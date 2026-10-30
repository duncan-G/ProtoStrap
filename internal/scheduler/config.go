package scheduler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/spec"
)

// This file applies config files and configure tasks (docs/config.md).
//
// Config files come with a release, so they roll back with it. A component that restarts
// for new files has their digest in its fingerprint, and a release replaces its replicas as
// it would for a new image. A follower's replicas stay: the scheduler names the files each
// replica should have, a quarter of the component's replicas at a time, and their agents
// swap them in and run the component's command. A follower that can't take a release's
// files fails the release, and the files it had are swapped in again.
//
// A configure task configures a component once it is ready, such as through its API, and
// runs again whenever what it applies changes: the task's image, config or env, its keys,
// or the component itself. What the last run that succeeded applied is kept in state.json,
// hashed, and a run marks it dirty until it succeeds. So after a run fails, or a release
// rolls back, the task of the release that runs is applied over again, and what is
// configured matches what runs. Only one run of a task is in progress at a time.

// configureRetry is the wait after a configure task fails outside a release, doubling with
// each failure in a row up to configureRetryMax.
const (
	configureRetry    = time.Minute
	configureRetryMax = 10 * time.Minute
)

// configureRun is a component's configure task.
type configureRun struct {
	release.ConfigureState
	replica   string // It is the run in progress.
	key       string // It is what the run in progress applies.
	failed    string // It is why the last run failed.
	failedKey string // It is what the run that failed applied.
	failedIn  string // It is the release whose rollout the run failed in.
	failures  int    // It counts runs that failed in a row.
	retryAt   time.Time
	waiting   string
}

// ConfigFiles returns the config files a digest names, from any release the scheduler
// holds, for agents to fetch.
func (s *Scheduler) ConfigFiles(digest string) (configfiles.Files, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.st.target; t != nil {
		if f, ok := t.rel.Config[digest]; ok {
			return f, true
		}
	}
	for _, t := range s.st.releases {
		if f, ok := t.rel.Config[digest]; ok {
			return f, true
		}
	}
	return nil, false
}

func (s *Scheduler) loadConfigures(saved release.State) {
	for name, cs := range saved.Configured {
		s.st.configures[name] = &configureRun{ConfigureState: cs}
	}
}

func (s *Scheduler) saveConfigures(st *release.State) {
	for name, run := range s.st.configures {
		if run.ConfigureState == (release.ConfigureState{}) {
			continue
		}
		if st.Configured == nil {
			st.Configured = map[string]release.ConfigureState{}
		}
		st.Configured[name] = run.ConfigureState
	}
}

// stageConfig names the config files each follower replica should have: the target
// release's, swapped in a quarter of the component's replicas at a time, at least one. The
// next replicas get them once those before have taken them and are ready.
func (s *Scheduler) stageConfig(tgt *target) {
	for _, c := range tgt.spec.Sorted() {
		if !c.ConfigFollows() {
			continue
		}
		want := tgt.rel.Components[c.Name].Config
		var live []*Replica
		for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
			if r := s.st.replicas[id]; r.Component == c.Name && r.Kind != api.KindTask && r.live() && r.Spec.ConfigFollows {
				live = append(live, r)
			}
		}
		inFlight := 0
		for _, r := range live {
			if r.ConfigWant == want && (r.ConfigDigest != want || r.State != api.StateReady) {
				inFlight++
			}
		}
		batch := max(1, ceil(float64(len(live))/4))
		for _, r := range live {
			if inFlight >= batch {
				break
			}
			if r.ConfigWant != want {
				s.log.Info("swapping in new config files", "replica", r.ID, "component", c.Name, "config", want)
				r.ConfigWant = want
				inFlight++
			}
		}
	}
}

// configFollowersDone reports whether every follower replica of the target has its config
// files.
func (s *Scheduler) configFollowersDone(tgt *target) bool {
	for _, r := range s.st.replicas {
		if !r.live() || r.Kind == api.KindTask || !r.Spec.ConfigFollows {
			continue
		}
		if c, ok := tgt.rel.Components[r.Component]; ok && r.ConfigDigest != c.Config {
			return false
		}
	}
	return true
}

// configFailure is why a follower can't take the config files the target release changes,
// or "".
func (s *Scheduler) configFailure(tgt *target) string {
	prev := s.st.releases[s.st.lastGood]
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.ConfigFailure == nil || !r.live() {
			continue
		}
		c, ok := tgt.rel.Components[r.Component]
		if !ok || r.ConfigFailure.Digest != c.Config {
			continue
		}
		if prev != nil && prev.rel.Components[r.Component].Config == c.Config {
			continue // This release doesn't change its files: it isn't to blame.
		}
		return fmt.Sprintf("%s replica %s can't take its new config files: %s", r.Component, r.ID, r.ConfigFailure.Reason)
	}
	for _, name := range slices.Sorted(maps.Keys(s.st.configures)) {
		run := s.st.configures[name]
		if run.failedIn == tgt.rel.Commit {
			c := tgt.spec.Components[name]
			task := ""
			if c != nil {
				task = c.Configure
			}
			return fmt.Sprintf("%s's configure task %s failed: %s", name, task, run.failed)
		}
	}
	return ""
}

func (s *Scheduler) configureOf(component string) *configureRun {
	run := s.st.configures[component]
	if run == nil {
		run = &configureRun{}
		s.st.configures[component] = run
	}
	return run
}

// configureKey names what a component's configure task applies at the target release: the
// task's fingerprint, which covers its image, env and config files, its keys, hashed, and
// the component's fingerprint. It is "" with the reason when it isn't known yet.
func (s *Scheduler) configureKey(tgt *target, c *spec.Component) (string, string) {
	keys := ""
	if s.secrets != nil && s.secretName() != "" && s.st.secret.current != "" {
		cs := s.st.secrets[c.Configure]
		if cs == nil {
			return "", "the keys of " + c.Configure + " haven't been read yet"
		}
		keys = cs.Digest
	}
	b, _ := json.Marshal([]string{c.Configure, s.fingerprint(tgt, c.Configure), keys, c.Name, s.fingerprint(tgt, c.Name)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), ""
}

// configures runs each component's configure task when what it applies has changed, once
// the component has rolled out and has a ready replica.
func (s *Scheduler) configures(tgt *target, now time.Time) {
	for name, run := range s.st.configures {
		if c := tgt.spec.Components[name]; c == nil || c.Configure == "" || tgt.spec.Components[c.Configure] == nil {
			delete(s.st.configures, name)
			continue
		}
		if run.replica != "" && s.st.replicas[run.replica] == nil {
			run.replica = "" // The run was lost with its VM: it is still dirty, so it runs again.
		}
	}
	ro := s.running()
	inRelease := ro != nil && ro.Commit == tgt.rel.Commit
	for _, c := range tgt.spec.Sorted() {
		if c.Configure == "" || tgt.spec.Components[c.Configure] == nil {
			continue
		}
		task := tgt.spec.Components[c.Configure]
		run := s.configureOf(c.Name)
		run.waiting = ""
		if run.replica != "" {
			continue
		}
		key, why := s.configureKey(tgt, c)
		switch {
		case key == "":
			run.waiting = why
			continue
		case run.Applied == key && !run.Dirty:
			continue
		case run.failedKey == key && inRelease && run.failedIn == tgt.rel.Commit:
			continue // The release fails.
		case run.failedKey == key && now.Before(run.retryAt):
			run.waiting = fmt.Sprintf("it failed: it runs again in %v", run.retryAt.Sub(now).Round(time.Second))
			continue
		}
		if s.taskRunning(task.Name) {
			run.waiting = "a run of " + task.Name + " is still in progress"
			continue
		}
		if why := s.settled(tgt, c); why != "" {
			run.waiting = why
			continue
		}
		if !s.depsReady(task) {
			run.waiting = "what " + task.Name + " depends on isn't ready"
			continue
		}
		r := s.newReplica(tgt, task, now)
		r.Kind, r.Spec.Kind = api.KindTask, api.KindTask
		r.Fingerprint, r.Spec.Fingerprint = "", ""
		run.replica, run.key, run.Dirty = r.ID, key, true
		s.log.Info("configuring a component", "component", c.Name, "task", task.Name, "replica", r.ID, "image", r.Spec.Image)
	}
}

// configureFinished records how a configure task's run ended.
func (s *Scheduler) configureFinished(rep *Replica, now time.Time) {
	for name, run := range s.st.configures {
		if run.replica != rep.ID {
			continue
		}
		switch rep.State {
		case api.StateSucceeded:
			run.Applied, run.Dirty = run.key, false
			run.replica, run.failed, run.failedKey, run.failedIn, run.failures, run.retryAt = "", "", "", "", 0, time.Time{}
			s.log.Info("component configured", "component", name, "replica", rep.ID)
		case api.StateFailed:
			reason := rep.Reason
			if reason == "" {
				reason = "it didn't exit with 0"
			}
			run.failures++
			run.replica, run.failed, run.failedKey = "", reason, run.key
			run.retryAt = now.Add(min(configureRetry<<(run.failures-1), configureRetryMax))
			run.failedIn = ""
			if ro := s.running(); ro != nil && s.st.target != nil && ro.Commit == s.st.target.rel.Commit {
				run.failedIn = ro.Commit
			}
			s.log.Error("a configure task failed", "component", name, "replica", rep.ID, "reason", rep.Reason)
		}
	}
}

// taskRunning reports whether a run of a task is in progress, including one the scheduler
// adopted after a restart.
func (s *Scheduler) taskRunning(task string) bool {
	for _, r := range s.st.replicas {
		if r.Component == task && r.Kind == api.KindTask && r.live() {
			return true
		}
	}
	return false
}

// settled says why a component isn't ready to be configured, or "": it has rolled out, its
// replicas have its keys and config files, and one is ready.
func (s *Scheduler) settled(tgt *target, c *spec.Component) string {
	rc := tgt.rel.Components[c.Name]
	ready := 0
	for _, r := range s.st.replicas {
		if r.Component != c.Name || r.Kind == api.KindTask || !r.live() {
			continue
		}
		switch {
		case r.Fingerprint != s.fingerprint(tgt, c.Name) || !s.keysCurrent(r):
			return c.Name + " is rolling out"
		case r.Spec.ConfigFollows && r.ConfigDigest != rc.Config:
			return c.Name + " is taking new config files"
		case r.State == api.StateReady:
			ready++
		}
	}
	if s.secretInProgress(c.Name) {
		return c.Name + " is taking new keys"
	}
	desired := s.st.desired[c.Name]
	if c.Kind == api.KindStateful {
		desired = 1
	}
	if ready == 0 && desired > 0 {
		return c.Name + " has no ready replica"
	}
	return ""
}

// configured reports whether dep, a component that c depends on, has been configured, if
// it has a configure task: once at all, and during a release, with what the release
// applies. dep's own configure task never waits for it.
func (s *Scheduler) configured(dep, c string) bool {
	tgt := s.st.target
	if tgt == nil {
		return true
	}
	d := tgt.spec.Components[dep]
	if d == nil || d.Configure == "" || d.Configure == c || tgt.spec.Components[d.Configure] == nil {
		return true
	}
	run := s.st.configures[dep]
	if run == nil || run.Applied == "" {
		return false
	}
	if ro := s.running(); ro != nil && ro.Commit == tgt.rel.Commit {
		key, _ := s.configureKey(tgt, d)
		return run.Applied == key && !run.Dirty
	}
	return true
}

// configureBusy reports whether dep, a component that name depends on, has a configure task
// still to apply new keys or anything else. dep's own configure task never waits for it.
func (s *Scheduler) configureBusy(tgt *target, dep, name string) bool {
	d := tgt.spec.Components[dep]
	if d == nil || d.Configure == "" || d.Configure == name || tgt.spec.Components[d.Configure] == nil {
		return false
	}
	if cs := s.st.secrets[d.Configure]; cs != nil && cs.pending != "" {
		return true
	}
	run := s.st.configures[dep]
	if run == nil {
		return false
	}
	if run.replica != "" || run.Dirty {
		return true
	}
	key, _ := s.configureKey(tgt, d)
	return key != "" && run.Applied != key
}

// configStatus is a follower's config files, for the status.
func (s *Scheduler) configStatus(tgt *target, c *spec.Component) *ConfigStatus {
	if !c.ConfigFollows() {
		return nil
	}
	cs := &ConfigStatus{Digest: tgt.rel.Components[c.Name].Config}
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.Component != c.Name || r.Kind == api.KindTask || !r.live() || !r.Spec.ConfigFollows {
			continue
		}
		if r.ConfigDigest != cs.Digest {
			cs.Behind++
		}
		if f := r.ConfigFailure; f != nil && f.Digest == cs.Digest && cs.Failed == "" {
			cs.Failed = fmt.Sprintf("%s can't take them: %s", r.ID, f.Reason)
		}
	}
	return cs
}

// configureStatus is a component's configure task, for the status.
func (s *Scheduler) configureStatus(tgt *target, c *spec.Component) *ConfigureStatus {
	if c.Configure == "" {
		return nil
	}
	st := &ConfigureStatus{Task: c.Configure, State: "pending"}
	run := s.st.configures[c.Name]
	if run == nil {
		return st
	}
	key, _ := s.configureKey(tgt, c)
	st.Waiting = run.waiting
	switch {
	case run.replica != "":
		st.State = "running"
	case run.Applied == key && !run.Dirty:
		st.State = "done"
	case run.failed != "" && run.failedKey == key:
		st.State, st.Failed = "failed", run.failed
	}
	return st
}
