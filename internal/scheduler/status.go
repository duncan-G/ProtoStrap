package scheduler

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// Status is a snapshot of everything the scheduler knows.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	tgt := s.st.target
	st := Status{App: s.cfg.App, Environment: s.cfg.Environment, Version: s.cfg.Version, Now: now,
		Started: s.st.started, Warming: !s.st.warm, Holding: now.Sub(s.st.started) < s.cfg.StartupHold, LastGood: s.st.lastGood}
	if tgt != nil {
		st.Target = tgt.rel.Commit
	}
	for _, r := range s.st.rollouts {
		st.Rollouts = append(st.Rollouts, RolloutStatus{Commit: r.Commit, State: r.State, Reason: r.Reason, Started: r.Started, Finished: r.Finished})
	}
	pending := map[string]int{}
	for _, r := range s.st.replicas {
		if r.State == StatePending && !r.Stop {
			pending[r.Pool]++
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.st.pools)) {
		p := s.st.pools[name]
		ps := PoolStatus{Name: name, VMs: p.vms, Booting: p.booting, Desired: p.desired, Wanted: p.wanted, Requested: p.requested, Pending: pending[name]}
		if tgt != nil && (name == "general" || name == "compute") {
			if sh, ok := s.shape(name); ok {
				room := sh.room()
				ps.Scale = &PoolScale{VMType: tgt.spec.Pool(name).VM[0], Min: sh.min, Max: sh.max, Spare: sh.spare, Target: sh.target,
					Usable: api.Resources{CPU: int64(math.Round(float64(room.CPU) * sh.target)), Memory: int64(math.Round(float64(room.Memory) * sh.target))}}
			}
		}
		st.Pools = append(st.Pools, ps)
	}
	rooms := s.rooms()
	for _, id := range slices.Sorted(maps.Keys(s.st.vms)) {
		vm := s.st.vms[id]
		n := 0
		for _, r := range s.st.replicas {
			if r.VM == id && r.live() {
				n++
			}
		}
		st.VMs = append(st.VMs, VMStatus{ID: id, Name: vm.Name, Pool: vm.Pool, Zone: vm.Zone, IP: vm.IP, Version: vm.Version,
			Capacity: vm.Capacity, Reserved: s.reserve(vm.Pool), Free: rooms[id].free, Replicas: n, FirstSync: vm.FirstSync,
			LastSync: vm.LastSync, Lost: vm.Lost, Cordoned: vm.Cordoned, Terminating: vm.Terminating != nil, Unhealthy: vm.Unhealthy})
	}
	comps := map[string]*ComponentStatus{}
	if tgt != nil {
		for _, c := range tgt.spec.Sorted() {
			cs := &ComponentStatus{Name: c.Name, Kind: string(c.Kind), Pool: c.Pool, Request: c.Request.API(), Desired: s.st.desired[c.Name]}
			if c.Kind == api.KindStateful {
				cs.Desired, cs.Pool = 1, c.VM
			}
			if why := s.st.why[c.Name]; why != nil {
				copied := *why
				cs.Scale = &copied
			} else if c.Scale != nil && (c.Kind == api.KindService || c.Kind == api.KindWorker) {
				cs.Scale = &ScaleStatus{Min: c.Scale.Min, Max: c.Scale.Max, Reason: "not counted yet"}
			}
			cs.Secret = s.secretStatus(c.Name)
			cs.Config = s.configStatus(tgt, c)
			cs.Configure = s.configureStatus(tgt, c)
			comps[c.Name] = cs
		}
	}
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		rs := ReplicaStatus{ID: id, Component: r.Component, Kind: string(r.Kind), Release: r.Release, Fingerprint: r.Fingerprint,
			Pool: r.Pool, VM: r.VM, State: r.State, Stopping: r.Stop, Ephemeral: r.Ephemeral, Old: s.old(r), Reason: r.Reason,
			Request: r.Request, Created: r.Created, SecretVersion: r.SecretVersion, ConfigDigest: r.ConfigDigest}
		if vm := s.st.vms[r.VM]; vm != nil {
			if rep, ok := vm.Reported[id]; ok {
				rs.CPU, rs.Memory, rs.Restarts = rep.CPU, rep.Memory, rep.Restarts
			}
		}
		st.Replicas = append(st.Replicas, rs)
		c := comps[r.Component]
		if c == nil || r.Kind == api.KindTask || !r.live() {
			continue
		}
		if rs.Old {
			c.Old++
		}
		switch r.State {
		case api.StateReady:
			c.Ready++
		case StatePending:
			c.Pending++
		default:
			c.Starting++
		}
	}
	for _, name := range slices.Sorted(maps.Keys(comps)) {
		st.Components = append(st.Components, *comps[name])
	}
	if tgt != nil {
		for _, q := range tgt.spec.QueueNames() {
			if d, ok := s.st.depths[q]; ok {
				st.Queues = append(st.Queues, QueueStatus{Name: q, Visible: d.Visible, InFlight: d.InFlight})
			}
		}
	}
	if name := s.secretName(); name != "" && s.secrets != nil {
		w := s.st.secret
		st.Secret = &SecretWatch{Name: name, Version: w.current, Changed: w.changed, Checked: w.checked, Error: w.err}
	}
	return st
}

// old reports whether a replica is of an older fingerprint than the target release's, or
// without its component's keys: one a rollout will replace.
func (s *Scheduler) old(r *Replica) bool {
	tgt := s.st.target
	switch {
	case tgt == nil || r.Kind == api.KindTask:
		return false
	case r.Kind == api.KindEdge:
		return !hasEdge(tgt) || r.Fingerprint != s.edgeFingerprint(tgt) || !s.keysCurrent(r)
	case r.Kind == api.KindTelemetry:
		return r.Fingerprint != tgt.rel.Telemetry.Fingerprint
	}
	_, ok := tgt.rel.Components[r.Component]
	return ok && (r.Fingerprint != s.fingerprint(tgt, r.Component) || !s.keysCurrent(r))
}

// snapshot logs every component's signals once a minute, for the reporter.
func (s *Scheduler) snapshot(now time.Time) {
	if s.st.target == nil || now.Sub(s.st.lastSnapshot) < s.cfg.SnapshotEvery {
		return
	}
	s.st.lastSnapshot = now
	var parts []string
	for _, c := range s.st.target.spec.Sorted() {
		if c.Scale == nil || (c.Kind != api.KindService && c.Kind != api.KindWorker) {
			continue
		}
		ready := 0
		var cpu float64
		var withCPU int
		for _, r := range s.st.replicas {
			if r.Component != c.Name || !r.live() || r.State != api.StateReady {
				continue
			}
			ready++
			if avg, ok := average(r.cpu, now, s.cfg.CPUWindow); ok && r.Request.CPU > 0 {
				cpu += avg / float64(r.Request.CPU)
				withCPU++
			}
		}
		part := fmt.Sprintf("%s desired=%d ready=%d", c.Name, s.st.desired[c.Name], ready)
		if withCPU > 0 {
			part += fmt.Sprintf(" cpu=%.0f%%", cpu/float64(withCPU)*100)
		}
		if rate, ok := s.requestRate(c.Name, now); ok {
			part += fmt.Sprintf(" requests=%.1f/s", rate)
		}
		for _, q := range c.QueueSignals() {
			d := s.st.depths[q]
			part += fmt.Sprintf(" queue[%s]=%d+%d", q, d.Visible, d.InFlight)
		}
		if limit, _, _ := s.limitCap(c, now); limit >= 0 {
			part += fmt.Sprintf(" limit=%d", limit)
		}
		parts = append(parts, part)
	}
	pools := []string{}
	for _, name := range slices.Sorted(maps.Keys(s.st.pools)) {
		p := s.st.pools[name]
		pools = append(pools, fmt.Sprintf("%s vms=%d booting=%d wanted=%d cpu=%dm", name, p.vms, p.booting, p.wanted, p.requested.CPU))
	}
	// The snapshot counts replicas waiting for a VM, of every kind: a stateful one waits
	// while its data VM is missing or silent.
	pending := map[string]int{}
	for _, r := range s.st.replicas {
		if r.State == StatePending {
			pending[r.Component]++
		}
	}
	waiting := []string{}
	for _, name := range slices.Sorted(maps.Keys(pending)) {
		waiting = append(waiting, fmt.Sprintf("%s=%d", name, pending[name]))
	}
	s.log.Info("signals", "components", strings.Join(parts, "; "), "pools", strings.Join(pools, "; "), "waiting", strings.Join(waiting, " "))
}
