package scheduler

import (
	"context"
	"maps"
	"slices"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// reconcileReport brings the scheduler's replicas up to date with what a VM's agent
// reports: their states, their CPU and metrics, and which have gone.
func (s *Scheduler) reconcileReport(vm *VM, r api.Report, now time.Time) {
	for _, rr := range r.Replicas {
		rep := s.st.replicas[rr.ID]
		if rep == nil || rep.VM != vm.ID {
			continue // The replica isn't assigned here, so its agent stops it.
		}
		rep.Seen = true
		prev := rep.State
		switch rr.State {
		case api.StateStarting, api.StateReady, api.StateFailed, api.StateSucceeded, api.StateStopping:
			if prev != api.StateFailed && prev != api.StateSucceeded {
				rep.State = rr.State
			}
		}
		if rep.State == api.StateFailed && prev != api.StateFailed {
			rep.Reason, rep.ExitCode = rr.Reason, rr.ExitCode
			s.replicaFailed(rep, now)
		}
		if rep.Kind == api.KindTask && rep.State != prev {
			s.taskFinished(rep)
			s.configureFinished(rep, now)
		}
		if rep.Spec.SecretFollows {
			// A follower says which version of its keys it has applied. Other replicas have
			// those they were made with, which the scheduler knows.
			rep.SecretVersion, rep.SecretFailure = rr.SecretVersion, rr.SecretFailure
		}
		if rep.Spec.ConfigFollows && rr.ConfigDigest != "" {
			// So does a follower of its config files.
			rep.ConfigDigest, rep.ConfigFailure = rr.ConfigDigest, rr.ConfigFailure
		}
		if rr.CPU != nil {
			rep.cpu = appendSample(rep.cpu, sample{now, float64(*rr.CPU)}, now, s.cfg.SignalWindow)
		}
		for name, v := range rr.Metrics {
			if rep.Kind == api.KindEdge {
				if s.st.edgeCounts[rep.ID] == nil {
					s.st.edgeCounts[rep.ID] = map[string]float64{}
				}
				s.st.edgeCounts[rep.ID][name] = v
				continue
			}
			if rep.metrics == nil {
				rep.metrics = map[string][]sample{}
			}
			rep.metrics[name] = appendSample(rep.metrics[name], sample{now, v}, now, s.cfg.SignalWindow)
			// Any replica may export a limit; the latest value is the one that holds.
			s.st.limits[name] = sample{now, v}
		}
	}
	// Assigned replicas the agent no longer runs are forgotten if they were stopped or have
	// vanished.
	for id, rep := range s.st.replicas {
		if rep.VM != vm.ID {
			continue
		}
		if _, ok := vm.Reported[id]; ok {
			continue
		}
		switch {
		case rep.Stop:
			s.forget(id) // The replica was stopped and removed.
		case rep.Seen && rep.State != api.StateSucceeded && rep.State != api.StateFailed:
			s.log.Warn("a replica vanished from its VM", "replica", id, "component", rep.Component, "vm", vm.ID)
			s.forget(id)
		}
	}
}

func appendSample(list []sample, x sample, now time.Time, keep time.Duration) []sample {
	list = append(list, x)
	cut := 0
	for cut < len(list) && now.Sub(list[cut].at) > keep {
		cut++
	}
	return list[cut:]
}

func (s *Scheduler) forget(id string) {
	delete(s.st.replicas, id)
	delete(s.st.edgeCounts, id)
}

// replicaFailed records a failure. Outside a rollout, failures back off further starts of
// the component, so a broken replica isn't restarted in a tight loop.
func (s *Scheduler) replicaFailed(rep *Replica, now time.Time) {
	s.log.Warn("replica failed", "replica", rep.ID, "component", rep.Component, "release", rep.Release, "reason", rep.Reason)
	if ro := s.running(); ro != nil && rep.Release == ro.Commit {
		return // The rollout fails instead.
	}
	s.st.failures[rep.Component] = append(s.st.failures[rep.Component], now)
}

// taskFinished records a migration's result.
func (s *Scheduler) taskFinished(rep *Replica) {
	for key, run := range s.st.tasks {
		if run.replica != rep.ID {
			continue
		}
		switch rep.State {
		case api.StateSucceeded:
			run.state = "succeeded"
			s.log.Info("migration succeeded", "task", key, "replica", rep.ID)
		case api.StateFailed:
			run.state, run.reason = "failed", rep.Reason
			s.log.Error("migration failed", "task", key, "replica", rep.ID, "reason", rep.Reason)
		}
	}
}

// adopt ends the warm-up: every replica agents reported is taken on from its labels, so a
// restarted scheduler picks up where the last one left off instead of starting over.
func (s *Scheduler) adopt(ctx context.Context) {
	type found struct {
		vm *VM
		rr api.ReplicaReport
	}
	s.mu.Lock()
	var all []found
	commits := map[string]bool{}
	for _, vm := range s.st.vms {
		for _, rr := range vm.Reported {
			if _, known := s.st.replicas[rr.ID]; !known {
				all = append(all, found{vm, rr})
				if rr.Release != "" {
					commits[rr.Release] = true
				}
			}
		}
	}
	s.mu.Unlock()

	releases := map[string]*target{}
	for commit := range commits {
		t, err := s.loadTarget(ctx, commit)
		if err != nil {
			s.log.Warn("a running replica's release can't be read", "release", commit, "err", err)
			continue
		}
		releases[commit] = t
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	for commit, t := range releases {
		s.st.releases[commit] = t
	}
	if s.st.lastGood != "" && s.st.releases[s.st.lastGood] == nil {
		if t, ok := releases[s.st.lastGood]; ok {
			s.st.releases[s.st.lastGood] = t
		}
	}
	slices.SortFunc(all, func(a, b found) int {
		if a.rr.ID < b.rr.ID {
			return -1
		}
		return 1
	})
	for _, f := range all {
		rep := s.adoptOne(f.vm, f.rr, releases[f.rr.Release], now)
		s.st.replicas[rep.ID] = rep
	}
	s.st.warm = true
	s.log.Info("adopted the running replicas", "replicas", len(all), "vms", len(s.st.vms))
}

func (s *Scheduler) adoptOne(vm *VM, rr api.ReplicaReport, rel *target, now time.Time) *Replica {
	kind := rr.Kind
	if kind == "" {
		kind = api.KindService
	}
	rep := &Replica{ID: rr.ID, Component: rr.Component, Kind: kind, Release: rr.Release, Fingerprint: rr.Fingerprint,
		VM: vm.ID, State: rr.State, Seen: true, Created: now, Pool: vm.Pool}
	if rep.State == "" {
		rep.State = api.StateStarting
	}
	// The replica's settings come from its own release, or failing that the target's,
	// which only fills in the assignment: a replica's settings never change, and a
	// mismatched fingerprint gets it replaced.
	for _, t := range []*target{rel, s.st.target} {
		if t == nil {
			continue
		}
		if kind == api.KindEdge {
			if !hasEdge(t) {
				continue
			}
			rep.Spec = s.edgeSpec(t, rep.ID)
			break
		}
		if kind == api.KindTelemetry {
			if !hasTelemetry(t) {
				continue
			}
			rep.Spec = s.telemetrySpec(t, rep.ID)
			rep.Request, rep.Pool, rep.Pin = rep.Spec.Request, "data", telemetryVM
			break
		}
		if c := t.spec.Components[rep.Component]; c != nil {
			rep.Spec = s.buildSpec(t, c, rep.ID)
			rep.Request = rep.Spec.Request
			if c.Pool != "" {
				rep.Pool = c.Pool
			}
			rep.Pin = c.VM
			break
		}
	}
	if rep.Spec.ID == "" {
		rep.Spec = api.ReplicaSpec{ID: rep.ID, Component: rep.Component, Kind: kind, Release: rep.Release, Image: "unknown", Request: api.Resources{Memory: 1}}
		rep.Stop = true
	}
	rep.Spec.Release, rep.Spec.Fingerprint, rep.Spec.Kind = rep.Release, rep.Fingerprint, kind
	// Its keys are those its label says it was made with.
	rep.SecretDigest, rep.SecretVersion, rep.SecretFailure = rr.SecretDigest, rr.SecretVersion, rr.SecretFailure
	rep.Spec.SecretDigest = rr.SecretDigest
	if !rep.Spec.SecretFollows {
		rep.Spec.SecretVersion = rr.SecretVersion
	}
	// It has the config files it says it has, and a follower keeps them until it is told
	// to take others.
	rep.ConfigDigest, rep.ConfigWant, rep.ConfigFailure = rr.ConfigDigest, rr.ConfigDigest, rr.ConfigFailure
	if rep.Spec.ConfigPath != "" && rr.ConfigDigest != "" && !rep.Spec.ConfigFollows {
		rep.Spec.ConfigDigest = rr.ConfigDigest
	}
	rep.Ephemeral = kind == api.KindWorker && vm.Pool == "general" && rep.Pool == "compute"
	if kind == api.KindTask && rep.State != api.StateStarting {
		rep.Stop = true
	}
	return rep
}

// markSilent stops counting the replicas of VMs whose agents have gone silent, so they are
// placed elsewhere. A data VM's stateful replicas can't go elsewhere: a silent data VM is
// only reported.
func (s *Scheduler) markSilent(now time.Time) {
	for _, vm := range s.st.vms {
		if vm.Lost || now.Sub(vm.LastSync) <= s.cfg.SilentAfter {
			continue
		}
		vm.Lost = true
		if vm.Pool == "data" {
			s.log.Error("data VM is silent: its stateful components can't move, so they are only reported", "vm", vm.ID, "name", vm.Name, "last_sync", vm.LastSync)
			continue
		}
		s.log.Warn("VM is silent: placing its replicas elsewhere", "vm", vm.ID, "pool", vm.Pool, "last_sync", vm.LastSync)
		for id, rep := range s.st.replicas {
			if rep.VM == vm.ID {
				s.dropTask(id)
				s.forget(id)
			}
		}
	}
}

// dropTask forgets a migration whose replica was lost, so it runs again.
func (s *Scheduler) dropTask(replica string) {
	for key, run := range s.st.tasks {
		if run.replica == replica && run.state == "running" {
			delete(s.st.tasks, key)
		}
	}
}

// forgetGone forgets pool VMs that have left their groups: removed, replaced or terminated.
func (s *Scheduler) forgetGone(now time.Time) {
	if len(s.st.groups) == 0 {
		return
	}
	inGroups := map[string]bool{}
	for _, g := range s.st.groups {
		for _, in := range g.Instances {
			inGroups[in.ID] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.st.vms)) {
		vm := s.st.vms[id]
		if vm.Pool == "data" || inGroups[id] {
			continue
		}
		if !vm.Lost && !vm.Terminated && vm.Terminating == nil && !vm.Unhealthy {
			continue // The VM still syncs, so the groups we read are older than it.
		}
		s.log.Info("VM gone", "vm", id, "pool", vm.Pool)
		for rid, rep := range s.st.replicas {
			if rep.VM == id {
				s.dropTask(rid)
				s.forget(rid)
			}
		}
		if vm.IP != "" {
			s.st.goneIPs[vm.IP] = now
		}
		delete(s.st.vms, id)
	}
}

// cleanSwarm removes the swarm nodes of VMs that are gone, so the swarm doesn't fill with
// dead nodes when VMs die without leaving it.
func (s *Scheduler) cleanSwarm(now time.Time, nodes []Node) []action {
	live := map[string]bool{}
	for _, vm := range s.st.vms {
		if !vm.Lost && vm.IP != "" {
			live[vm.IP] = true
		}
	}
	var acts []action
	for _, n := range nodes {
		if n.Addr == s.cfg.Address {
			continue // This node is the manager.
		}
		_, gone := s.st.goneIPs[n.Addr]
		if gone && !live[n.Addr] || n.State == "down" && !live[n.Addr] && now.Sub(s.st.started) > s.cfg.UnhealthyAfter {
			acts = append(acts, action{kind: "node", node: n.ID})
			delete(s.st.goneIPs, n.Addr)
			s.log.Info("removing a gone VM's swarm node", "node", n.ID, "addr", n.Addr)
		}
	}
	for ip, at := range s.st.goneIPs {
		if now.Sub(at) > time.Hour {
			delete(s.st.goneIPs, ip)
		}
	}
	return acts
}
