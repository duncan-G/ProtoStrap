package scheduler

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/spec"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// reconcile makes what runs converge on the target release (docs/protostrap.md#deploy-a-release):
//
//   - Replicas are replaced only where their fingerprint differs from the release's; a
//     replica of an older release with a matching fingerprint is kept as it is.
//   - Changed components roll out together, each replacing a quarter of its replicas at a
//     time, at least one: new replicas start, and as many old ones stop once they're ready.
//   - Migrations run first, once, and the rollout carries on only if they succeed.
//   - A stateful component stops before its replacement starts: only one can own the volume.
//   - A replica isn't started until everything in its depends_on has a ready replica.
//   - A follower's config files are swapped in a quarter of its replicas at a time, and a
//     configure task runs once its component has rolled out (config.go).
//   - A failed migration or configure task, a follower that can't take its config files, or
//     a new replica that fails or isn't ready within 5 minutes, rolls the whole release
//     back to the last good one.
func (s *Scheduler) reconcile(now time.Time) {
	if ro := s.running(); ro != nil && s.st.target != nil && ro.Commit == s.st.target.rel.Commit {
		if reason := s.rolloutFailure(); reason != "" {
			s.fail(ro, reason, now)
		}
	}
	// Failed replicas and finished tasks leave the assignment, so their agents remove them.
	for _, r := range s.st.replicas {
		if !r.Stop && (r.State == api.StateFailed || r.State == api.StateSucceeded) {
			r.Stop = true
		}
	}
	tgt := s.st.target
	if tgt == nil {
		for _, r := range s.st.replicas {
			r.Stop = true
		}
		return
	}

	for _, r := range s.st.replicas {
		if r.Kind != api.KindEdge && r.Kind != api.KindTelemetry && r.Kind != api.KindTask && tgt.spec.Components[r.Component] == nil && !r.Stop {
			r.Stop = true
			s.log.Info("stopping a replica of a removed component", "replica", r.ID, "component", r.Component)
		}
	}
	s.reconcileEdge(tgt, now)
	s.reconcileTelemetry(tgt, now)
	migrations := s.migrations(tgt, now)
	for _, c := range tgt.spec.Sorted() {
		switch c.Kind {
		case api.KindService, api.KindWorker:
			s.reconcileScaled(tgt, c, s.gate(tgt, c, migrations), now)
		case api.KindStateful:
			s.reconcileStateful(tgt, c, s.depsReady(c), now)
		}
	}
	s.stageConfig(tgt)
	s.configures(tgt, now)
	if ro := s.running(); ro != nil && ro.Commit == tgt.rel.Commit && s.rolloutDone(tgt, migrations) {
		ro.State, ro.Finished = RolloutDone, now
		s.st.lastGood = tgt.rel.Commit
		s.log.Info("release finished", "release", ro.Commit, "took", now.Sub(ro.Started).Round(time.Second))
		for key := range s.st.tasks {
			if !strings.HasPrefix(key, tgt.rel.Commit+"/") {
				delete(s.st.tasks, key)
			}
		}
	}
}

// running is the rollout in progress, if any.
func (s *Scheduler) running() *rollout {
	for i := len(s.st.rollouts) - 1; i >= 0; i-- {
		if r := s.st.rollouts[i]; r.State == RolloutRunning {
			return r
		}
	}
	return nil
}

// rolloutFailure is why the target release failed, or "".
func (s *Scheduler) rolloutFailure() string {
	commit := s.st.target.rel.Commit
	for _, key := range slices.Sorted(maps.Keys(s.st.tasks)) {
		if run := s.st.tasks[key]; strings.HasPrefix(key, commit+"/") && run.state == "failed" {
			return fmt.Sprintf("migration %s failed: %s", strings.TrimPrefix(key, commit+"/"), run.reason)
		}
	}
	if reason := s.configFailure(s.st.target); reason != "" {
		return reason
	}
	// A release is blamed only for the components it changes: a replica with the last good
	// release's fingerprint failed for another reason, such as its keys.
	unchanged := func(r *Replica) bool {
		prev := s.st.releases[s.st.lastGood]
		if prev == nil {
			return false
		}
		switch r.Kind {
		case api.KindEdge:
			return r.Fingerprint == prev.rel.Edge.Fingerprint
		case api.KindTelemetry:
			return r.Fingerprint == prev.rel.Telemetry.Fingerprint
		}
		_, ok := prev.rel.Components[r.Component]
		return ok && r.Fingerprint == s.fingerprint(prev, r.Component)
	}
	var ids []string
	for id, r := range s.st.replicas {
		if r.Release == commit && r.State == api.StateFailed && r.Kind != api.KindTask && !unchanged(r) {
			ids = append(ids, id)
		}
	}
	if len(ids) > 0 {
		slices.Sort(ids)
		r := s.st.replicas[ids[0]]
		return fmt.Sprintf("%s replica %s failed: %s", r.Component, r.ID, r.Reason)
	}
	return ""
}

// fail rolls a release back to the last good one, or with nothing to go back to, runs
// nothing.
func (s *Scheduler) fail(ro *rollout, reason string, now time.Time) {
	ro.State, ro.Reason, ro.Finished = RolloutFailed, reason, now
	prev := s.st.releases[s.st.lastGood]
	if s.st.lastGood == "" || s.st.lastGood == ro.Commit || prev == nil {
		s.log.Error("release failed, with no release to roll back to: running nothing", "release", ro.Commit, "reason", reason)
		s.st.target = nil
		return
	}
	s.log.Error("release failed: rolling back", "release", ro.Commit, "to", s.st.lastGood, "reason", reason)
	s.st.target = prev
}

// migration is a task to run once in this release, before the components it gates.
type migration struct {
	key       string
	component *spec.Component // It is the task, or the component whose migrate command it runs.
	command   []string        // It is a component's migrate command, or nil for a task.
	gates     []string        // It lists the components that wait for the migration.
}

// migrations are the release's tasks whose image is new since the last good release, and
// the migrate commands of components whose image is new: each starts once what it depends
// on is ready, and runs exactly once.
func (s *Scheduler) migrations(tgt *target, now time.Time) map[string]*migration {
	prev := s.st.releases[s.st.lastGood]
	changed := func(name string) bool {
		if prev == nil {
			return true
		}
		old, ok := prev.rel.Components[name]
		cur := tgt.rel.Components[name]
		return !ok || old.Image != cur.Image || old.Config != cur.Config
	}
	out := map[string]*migration{}
	for _, c := range tgt.spec.Sorted() {
		switch {
		case c.Kind == api.KindTask && tgt.spec.ConfiguredBy(c.Name) != nil:
			// A configure task runs after its component is ready, never as a migration.
		case c.Kind == api.KindTask && changed(c.Name):
			out[c.Name] = &migration{key: tgt.rel.Commit + "/" + c.Name, component: c}
		case c.Migrate != nil && len(c.Migrate.Command) > 0 && changed(c.Name):
			out[c.Name+"/migrate"] = &migration{key: tgt.rel.Commit + "/" + c.Name + "/migrate", component: c, command: c.Migrate.Command, gates: []string{c.Name}}
		}
	}
	for _, c := range tgt.spec.Components {
		if c.Migrate != nil && c.Migrate.Task != "" {
			if m := out[c.Migrate.Task]; m != nil {
				m.gates = append(m.gates, c.Name)
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(out)) {
		m := out[name]
		if run := s.st.tasks[m.key]; run != nil {
			if r := s.st.replicas[run.replica]; run.state == "running" && r == nil {
				delete(s.st.tasks, m.key) // The task was lost with its VM, so it runs again.
			} else {
				continue
			}
		}
		if !s.depsReady(m.component) {
			continue
		}
		r := s.newReplica(tgt, m.component, now)
		r.Kind = api.KindTask
		r.Spec.Kind = api.KindTask
		r.Fingerprint, r.Spec.Fingerprint = "", ""
		// A migration runs once: it never follows its component's keys or config.
		r.Spec.SecretFollows, r.Spec.SecretHook, r.Spec.ConfigFollows, r.Spec.ConfigHook = false, nil, false, nil
		if m.command != nil {
			r.Spec.Command = m.command
			r.Spec.Port, r.Spec.Ready, r.Spec.ReadyCommand, r.Spec.H2C, r.Spec.Metrics = 0, "", nil, false, nil
		}
		s.st.tasks[m.key] = &taskRun{replica: r.ID, state: "running"}
		s.log.Info("migration", "task", m.key, "replica", r.ID, "image", r.Spec.Image)
	}
	return out
}

// gate reports whether a component may start new replicas: what it depends on is ready,
// and its migration, if this release has one, has succeeded.
func (s *Scheduler) gate(tgt *target, c *spec.Component, migrations map[string]*migration) bool {
	if !s.depsReady(c) {
		return false
	}
	for _, m := range migrations {
		if !slices.Contains(m.gates, c.Name) {
			continue
		}
		if run := s.st.tasks[m.key]; run == nil || run.state != "succeeded" {
			return false
		}
	}
	return true
}

// depsReady reports whether every component c depends on has a ready replica, and has been
// configured if it has a configure task.
func (s *Scheduler) depsReady(c *spec.Component) bool {
	for _, dep := range c.DependsOn {
		if !s.configured(dep, c.Name) {
			return false
		}
		ok := false
		for _, r := range s.st.replicas {
			if r.Component == dep && r.Kind != api.KindTask && r.live() && r.State == api.StateReady {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// reconcileScaled converges a service or worker on its desired count at the release's
// fingerprint.
func (s *Scheduler) reconcileScaled(tgt *target, c *spec.Component, open bool, now time.Time) {
	want := s.st.desired[c.Name]
	fp := s.fingerprint(tgt, c.Name)
	var cur, old []*Replica
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.Component != c.Name || r.Kind == api.KindTask || !r.live() {
			continue
		}
		// A replica on a VM being drained counts as old: it is replaced, then stopped. So does
		// one without its component's keys.
		if r.Fingerprint == fp && s.keysCurrent(r) && !s.draining(r.VM) {
			cur = append(cur, r)
		} else {
			old = append(old, r)
		}
	}
	ready := 0
	for _, r := range cur {
		if r.State == api.StateReady {
			ready++
		}
	}
	notReady := len(cur) - ready

	if open && !s.backingOff(c.Name, now) {
		n := want - len(cur)
		if len(old) > 0 {
			batch := max(1, ceil(float64(want)/4))
			n = min(n, batch-notReady)
		}
		for range max(0, n) {
			s.newReplica(tgt, c, now)
		}
	}

	rooms := s.rooms()
	if keep := max(0, want-ready); len(old) > keep {
		for _, r := range pickToStop(old, rooms, len(old)-keep) {
			s.stop(r, "replaced")
		}
	}
	if len(cur) > want {
		for _, r := range pickToStop(cur, rooms, len(cur)-want) {
			s.stop(r, "scaled in")
		}
	}
}

// draining reports whether a VM is on its way out: cordoned, or being terminated by AWS.
func (s *Scheduler) draining(id string) bool {
	vm := s.st.vms[id]
	return vm != nil && (vm.Cordoned || vm.Terminating != nil)
}

// backingOff holds back new replicas of a component whose replicas keep failing outside a
// rollout: 10 seconds after a failure, doubling with each, up to 5 minutes.
func (s *Scheduler) backingOff(component string, now time.Time) bool {
	recent := slices.DeleteFunc(s.st.failures[component], func(t time.Time) bool { return now.Sub(t) > 10*time.Minute })
	s.st.failures[component] = recent
	if len(recent) == 0 {
		return false
	}
	wait := min(10*time.Second<<(len(recent)-1), 5*time.Minute)
	return now.Sub(recent[len(recent)-1]) < wait
}

// reconcileStateful runs one replica of a stateful component on its data VM, stopping an
// old one before its replacement starts.
func (s *Scheduler) reconcileStateful(tgt *target, c *spec.Component, open bool, now time.Time) {
	fp := s.fingerprint(tgt, c.Name)
	var cur []*Replica
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.Component != c.Name || !r.live() {
			continue
		}
		if r.Fingerprint == fp && s.keysCurrent(r) {
			cur = append(cur, r)
		} else {
			s.stop(r, "replaced: a stateful component stops before its replacement starts")
		}
	}
	if len(cur) == 0 && open && !s.backingOff(c.Name, now) {
		s.newReplica(tgt, c, now)
	}
	for _, r := range cur[min(1, len(cur)):] {
		s.stop(r, "one replica owns the volume")
	}
}

// reconcileEdge runs one edge on every ingress VM, and replaces an old edge by starting
// the new one beside it, on the same VM, and stopping the old once the new is ready.
func (s *Scheduler) reconcileEdge(tgt *target, now time.Time) {
	fp := tgt.rel.Edge.Fingerprint
	for _, vmID := range slices.Sorted(maps.Keys(s.st.vms)) {
		vm := s.st.vms[vmID]
		if vm.Pool != "ingress" || vm.Lost {
			continue
		}
		var cur, old []*Replica
		for _, r := range s.st.replicas {
			if r.Kind != api.KindEdge || r.VM != vmID || !r.live() {
				continue
			}
			if r.Fingerprint == fp && s.keysCurrent(r) {
				cur = append(cur, r)
			} else {
				old = append(old, r)
			}
		}
		if len(cur) == 0 && vm.Terminating == nil && !vm.Cordoned {
			r := s.newEdge(tgt, now)
			r.VM, r.State = vmID, StateAssigned
			cur = append(cur, r)
		}
		if slices.ContainsFunc(cur, func(r *Replica) bool { return r.State == api.StateReady }) {
			for _, r := range old {
				s.stop(r, "replaced by a newer edge")
			}
		}
		slices.SortFunc(cur, func(a, b *Replica) int { return strings.Compare(a.ID, b.ID) })
		for _, r := range cur[min(1, len(cur)):] {
			s.stop(r, "one edge per ingress VM")
		}
	}
}

func (s *Scheduler) stop(r *Replica, why string) {
	if r.Stop {
		return
	}
	r.Stop = true
	s.log.Info("stopping a replica", "replica", r.ID, "component", r.Component, "vm", r.VM, "reason", why)
	if r.VM == "" {
		s.forget(r.ID) // The replica never started.
	}
}

// rolloutDone reports whether everything runs the target release and is ready.
func (s *Scheduler) rolloutDone(tgt *target, migrations map[string]*migration) bool {
	for _, m := range migrations {
		if run := s.st.tasks[m.key]; run == nil || run.state != "succeeded" {
			return false
		}
	}
	for _, c := range tgt.spec.Components {
		if c.Configure == "" || tgt.spec.Components[c.Configure] == nil {
			continue
		}
		key, _ := s.configureKey(tgt, c)
		if run := s.st.configures[c.Name]; key == "" || run == nil || run.replica != "" || run.Dirty || run.Applied != key {
			return false
		}
	}
	if !s.configFollowersDone(tgt) {
		return false
	}
	readyOf := map[string]int{}
	for _, r := range s.st.replicas {
		if !r.live() || r.Kind == api.KindTask {
			continue
		}
		var fp string
		switch {
		case r.Kind == api.KindEdge:
			fp = tgt.rel.Edge.Fingerprint
		case r.Kind == api.KindTelemetry:
			fp = tgt.rel.Telemetry.Fingerprint
		case tgt.spec.Components[r.Component] == nil:
			return false // A removed component is still stopping.
		default:
			fp = s.fingerprint(tgt, r.Component)
		}
		if r.Fingerprint != fp || r.State == StateAssigned || r.State == api.StateStarting {
			return false
		}
		if r.State == api.StateReady {
			readyOf[r.Component]++
		}
	}
	if hasTelemetry(tgt) && readyOf[telemetry.Name] == 0 {
		return false
	}
	for _, c := range tgt.spec.Components {
		switch c.Kind {
		case api.KindStateful:
			if readyOf[c.Name] == 0 {
				return false
			}
		case api.KindService, api.KindWorker:
			if s.st.desired[c.Name] > 0 && readyOf[c.Name] == 0 {
				return false
			}
		}
	}
	for _, vm := range s.st.vms {
		if vm.Pool == "ingress" && !vm.Lost && vm.Terminating == nil && !vm.Cordoned {
			ok := false
			for _, r := range s.st.replicas {
				if r.Kind == api.KindEdge && r.VM == vm.ID && r.live() && r.State == api.StateReady {
					ok = true
				}
			}
			if !ok {
				return false
			}
		}
	}
	return true
}

// newReplica adds a pending replica of a component at the release.
func (s *Scheduler) newReplica(tgt *target, c *spec.Component, now time.Time) *Replica {
	id := s.newID(c.Name)
	r := &Replica{ID: id, Component: c.Name, Kind: c.Kind, Release: tgt.rel.Commit,
		Fingerprint: s.fingerprint(tgt, c.Name), Pool: c.Pool, State: StatePending,
		Request: c.Request.API(), Spec: s.buildSpec(tgt, c, id), Created: now}
	r.SecretDigest, r.SecretVersion = r.Spec.SecretDigest, r.Spec.SecretVersion
	r.ConfigDigest, r.ConfigWant = r.Spec.ConfigDigest, r.Spec.ConfigDigest
	if c.Kind == api.KindStateful {
		r.Pool, r.Pin = "data", c.VM
	}
	s.st.replicas[id] = r
	return r
}

func (s *Scheduler) newEdge(tgt *target, now time.Time) *Replica {
	id := s.newID("edge")
	r := &Replica{ID: id, Component: "edge", Kind: api.KindEdge, Release: tgt.rel.Commit,
		Fingerprint: tgt.rel.Edge.Fingerprint, Pool: "ingress", State: StatePending,
		Request: tgt.spec.Edge.Request.API(), Spec: s.edgeSpec(tgt, id), Created: now}
	r.SecretDigest, r.SecretVersion = r.Spec.SecretDigest, r.Spec.SecretVersion
	s.st.replicas[id] = r
	return r
}
