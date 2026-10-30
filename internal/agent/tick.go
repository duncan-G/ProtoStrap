package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/credentials"
	"github.com/duncan-G/protostrap/internal/probe"
)

const envCredentialsURI = "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"

type failure struct {
	component string
	release   string
	kind      api.Kind
	reason    string
}

// sighting is when the agent first saw a replica, and its restart count then. A replica
// started by this agent is seen when it starts; one adopted after the agent restarts is seen
// when the agent first lists it, which gives it a full ready timeout rather than fail a
// healthy replica.
type sighting struct {
	at       time.Time
	restarts int
}

type cpuSample struct {
	nanos uint64
	at    time.Time
}

// Tick runs one cycle: check the firewall, observe the VM, sync with the scheduler, and
// converge on the assignment. Starts and stops run in the background; Wait waits for them.
//
// If the scheduler doesn't answer, nothing is started or stopped: "down means frozen, not
// broken". Only a drain, when AWS is removing the VM, goes ahead without it. The error is
// returned after the rest of the cycle.
func (a *Agent) Tick(ctx context.Context) error {
	if a.done {
		return ErrTerminated
	}
	defer a.notifier.Notify("WATCHDOG=1")
	now := a.clock.Now()
	if a.capacity == (api.Resources{}) {
		if err := a.loadCapacity(ctx); err != nil {
			a.log.Warn("reading the VM's capacity", "err", err)
		}
	}

	repaired, fwErr := a.fw.Ensure(ctx)
	if fwErr != nil {
		a.log.Error("container firewall", "err", fwErr)
	} else if len(repaired) > 0 {
		a.log.Warn("firewall rules were missing and have been put back", "rules", repaired)
	}
	a.checkTermination(ctx, now)

	inFlight := a.opIDs()
	containers, err := a.rt.Containers(ctx)
	if err != nil {
		// Without seeing its containers, the agent stays silent: an empty report would tell
		// the scheduler nothing runs here.
		return fmt.Errorf("listing containers: %w", err)
	}
	if a.restoreSecretFiles(ctx, now, containers) {
		if fresh, err := a.rt.Containers(ctx); err == nil {
			containers = fresh
		}
	}
	a.sweepSecretFiles(containers, inFlight)
	a.sweepConfig(containers, inFlight)
	a.serveCredentials(containers, inFlight)
	report := api.Report{
		VM: a.cfg.InstanceID, Name: a.cfg.Name, Pool: a.cfg.Pool, Zone: a.cfg.Zone, Version: a.cfg.Version,
		Capacity: a.capacity, Replicas: a.observe(ctx, now, containers, inFlight), Repairs: repaired,
	}
	if a.term != nil {
		report.Terminating = &api.Termination{Reason: a.term.reason(), Deadline: a.term.deadline}
	}
	if a.outbox != nil {
		report.Logs = a.outbox.Take(syncRecords)
	}

	assignment, syncErr := a.sched.Sync(ctx, report)
	if syncErr != nil && a.outbox != nil {
		a.outbox.Return(report.Logs)
	}
	if syncErr == nil {
		a.lastSync = now
		a.last = assignment
		if a.update(ctx, assignment.Version) {
			return ErrUpdated
		}
		a.converge(ctx, assignment, containers, fwErr == nil)
		a.followSecrets(ctx, now, assignment, containers)
		a.followConfig(ctx, now, assignment, containers)
	}
	a.backup(ctx, now, containers)
	if a.term != nil {
		if err := a.drain(ctx, now, containers); err != nil {
			return err
		}
	}
	if syncErr != nil {
		return fmt.Errorf("sync: %w", syncErr)
	}
	return nil
}

// update installs the version the scheduler names, and reports whether it did.
func (a *Agent) update(ctx context.Context, version string) bool {
	if version == "" || version == a.cfg.Version || a.term != nil {
		return false
	}
	a.log.Info("updating the agent", "from", a.cfg.Version, "to", version)
	if err := a.updater.Update(ctx, version); err != nil {
		a.log.Error("updating the agent", "to", version, "err", err)
		return false
	}
	return true
}

func (a *Agent) opIDs() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make(map[string]bool, len(a.ops))
	for id := range a.ops {
		ids[id] = true
	}
	return ids
}

// serveCredentials makes sure every replica with a role is served its credentials, taking
// its token from its container, and that nothing else is. Replicas being started or
// stopped are left to that operation.
func (a *Agent) serveCredentials(containers []container.Container, inFlight map[string]bool) {
	live := map[string]bool{}
	for id := range inFlight {
		live[id] = true
	}
	for _, c := range containers {
		id := c.Replica()
		live[id] = true
		token, ok := strings.CutPrefix(c.Env[envCredentialsURI], a.cfg.CredentialsPath)
		role := c.Label(container.LabelRole)
		if !ok || token == "" || role == "" {
			continue
		}
		a.creds.Put(credentials.Entry{Token: token, Replica: id, Component: c.Label(container.LabelComponent), Role: role, Addrs: c.Addrs})
	}
	for _, id := range a.creds.Replicas() {
		if !live[id] {
			a.creds.Remove(id)
		}
	}
}

// readyCheck is what a replica must pass to be ready: a probe of Target, or Command run in it.
type readyCheck struct {
	probe.Target
	Command []string
}

func (c readyCheck) none() bool { return c.Check == "" && len(c.Command) == 0 }

// readiness is the check a replica must pass to be ready, from its labels: its `ready`, or
// for a stateful component without one, its port accepting connections. Workers and
// anything else without a check are ready once they run.
func readiness(c container.Container) readyCheck {
	port, _ := strconv.Atoi(c.Label(container.LabelPort))
	t := probe.Target{Port: port, Service: c.Label(container.LabelComponent), H2C: c.Label(container.LabelH2C) == "true"}
	if len(c.Addrs) > 0 {
		t.Addr = c.Addrs[0]
	}
	if cmd := c.Label(container.LabelReadyCommand); cmd != "" {
		var command []string
		if err := json.Unmarshal([]byte(cmd), &command); err == nil && len(command) > 0 {
			return readyCheck{Command: command}
		}
	}
	switch ready := c.Label(container.LabelReady); {
	case ready != "":
		t.Check = ready
	case api.Kind(c.Label(container.LabelKind)) == api.KindStateful && port > 0:
		t.Check = probe.CheckTCP
	default:
		return readyCheck{}
	}
	return readyCheck{Target: t}
}

// observe builds the report's replicas: each container's state, usage and metrics, and
// the replicas that failed before they had a container.
func (a *Agent) observe(ctx context.Context, now time.Time, containers []container.Container, inFlight map[string]bool) []api.ReplicaReport {
	live := map[string]bool{}
	for id := range inFlight {
		live[id] = true
	}
	a.mu.Lock()
	for _, c := range containers {
		id := c.Replica()
		live[id] = true
		if _, ok := a.seen[id]; !ok {
			a.seen[id] = sighting{at: now, restarts: c.Restarts}
		}
	}
	for id := range a.seen {
		if !live[id] {
			delete(a.seen, id)
			delete(a.ready, id)
		}
	}
	a.mu.Unlock()
	ids := map[string]bool{}
	for _, c := range containers {
		ids[c.ID] = true
	}
	for id := range a.cpu {
		if !ids[id] {
			delete(a.cpu, id)
		}
	}

	passed := a.probeStarting(ctx, containers)
	reports := make([]api.ReplicaReport, 0, len(containers))
	for _, c := range containers {
		r := api.ReplicaReport{
			ID: c.Replica(), Component: c.Label(container.LabelComponent), Release: c.Label(container.LabelRelease),
			Kind: api.Kind(c.Label(container.LabelKind)), Fingerprint: c.Label(container.LabelFingerprint),
			Restarts: c.Restarts,
		}
		r.State, r.Reason, r.ExitCode = a.classify(now, c, passed[c.ID])
		r.SecretVersion, r.SecretFailure = a.secretStatus(c)
		r.SecretDigest = c.Label(container.LabelSecretDigest)
		r.ConfigDigest, r.ConfigFailure = a.configStatus(c)
		if c.Status.Up() {
			r.CPU, r.Memory = a.usage(ctx, now, c)
			if r.State == api.StateReady {
				r.Metrics = a.metrics(ctx, c)
			}
		}
		reports = append(reports, r)
	}

	// Replicas that failed without a container are reported too: their image wouldn't pull,
	// the container wouldn't create or start, or their volume isn't mounted.
	a.mu.Lock()
	for id, f := range a.failed {
		if !slices.ContainsFunc(containers, func(c container.Container) bool { return c.Replica() == id }) {
			reports = append(reports, api.ReplicaReport{ID: id, Component: f.component, Release: f.release, Kind: f.kind,
				State: api.StateFailed, Reason: f.reason})
		}
	}
	a.mu.Unlock()
	slices.SortFunc(reports, func(x, y api.ReplicaReport) int { return strings.Compare(x.ID, y.ID) })
	return reports
}

// probeStarting runs the readiness checks of replicas that are starting, in parallel, and
// returns which container IDs passed.
func (a *Agent) probeStarting(ctx context.Context, containers []container.Container) map[string]bool {
	var targets []container.Container
	a.mu.Lock()
	for _, c := range containers {
		id := c.Replica()
		_, failed := a.failed[id]
		if c.Status == container.Running && !readiness(c).none() && !a.ready[id] && !failed && a.ops[id] == nil &&
			api.Kind(c.Label(container.LabelKind)) != api.KindTask {
			targets = append(targets, c)
		}
	}
	a.mu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	passed := map[string]bool{}
	for _, c := range targets {
		wg.Go(func() {
			if a.passes(ctx, c, readiness(c)) {
				mu.Lock()
				passed[c.ID] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return passed
}

// passes runs one check: a command in the replica, which must exit 0 within the probe
// timeout, or a probe.
func (a *Agent) passes(ctx context.Context, c container.Container, ch readyCheck) bool {
	if len(ch.Command) == 0 {
		return a.prober.Probe(ctx, ch.Target) == nil
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ProbeTimeout)
	defer cancel()
	code, err := a.rt.Exec(ctx, c.ID, ch.Command, io.Discard, io.Discard)
	return err == nil && code == 0
}

// classify is a replica's state (docs/protostrap.md#replica-lifecycle). Ready and failed
// are remembered: ready never goes back to starting, and failed is final.
func (a *Agent) classify(now time.Time, c container.Container, passed bool) (state, reason string, exitCode *int) {
	id := c.Replica()
	kind := api.Kind(c.Label(container.LabelKind))
	a.mu.Lock()
	defer a.mu.Unlock()
	fail := func(why string) (string, string, *int) {
		a.failed[id] = failure{component: c.Label(container.LabelComponent), release: c.Label(container.LabelRelease), kind: kind, reason: why}
		return api.StateFailed, why, nil
	}
	if o := a.ops[id]; (o != nil && o.kind == opStop) || c.Status == container.Removing {
		return api.StateStopping, "", nil
	}
	if f, ok := a.failed[id]; ok {
		return api.StateFailed, f.reason, nil
	}
	if kind == api.KindTask {
		// A task runs once: it succeeds or fails, and is never restarted.
		if c.Status != container.Exited && c.Status != container.Dead {
			return api.StateStarting, "", nil
		}
		code := c.ExitCode
		if code == 0 {
			return api.StateSucceeded, "", &code
		}
		return api.StateFailed, fmt.Sprintf("exited with code %d", code), &code
	}
	if u, ok := a.unrestored[id]; ok && !c.Status.Up() {
		// A reboot took its secret files, and Docker couldn't start it without them.
		if now.Sub(u.since) > a.cfg.ReadyTimeout {
			return fail("its secret files couldn't be written again after a reboot: " + u.reason)
		}
		return api.StateStarting, "", nil
	}
	if a.ready[id] {
		return api.StateReady, "", nil
	}
	switch c.Status {
	case container.Running, container.Paused:
		if c.Restarts > a.seen[id].restarts {
			return fail("exited and was restarted before it was ready")
		}
		if readiness(c).none() || passed {
			a.ready[id] = true
			return api.StateReady, "", nil
		}
		if now.Sub(a.seen[id].at) > a.cfg.ReadyTimeout {
			return fail(fmt.Sprintf("not ready within %v", a.cfg.ReadyTimeout))
		}
		return api.StateStarting, "", nil
	case container.Created:
		return api.StateStarting, "", nil
	default:
		return fail(fmt.Sprintf("exited with code %d before it was ready", c.ExitCode))
	}
}

// usage is a running container's CPU in millicores, averaged since the last cycle, and
// its memory in MiB. Either is nil when unknown.
func (a *Agent) usage(ctx context.Context, now time.Time, c container.Container) (cpu, mem *int64) {
	s, err := a.rt.Stats(ctx, c.ID)
	if err != nil {
		return nil, nil
	}
	m := s.MemoryBytes >> 20
	mem = &m
	at := s.Read
	if at.IsZero() {
		at = now
	}
	if prev, ok := a.cpu[c.ID]; ok && s.CPUNanos >= prev.nanos && at.After(prev.at) {
		millicores := int64((s.CPUNanos - prev.nanos) * 1000 / uint64(at.Sub(prev.at).Nanoseconds()))
		cpu = &millicores
	}
	a.cpu[c.ID] = cpuSample{nanos: s.CPUNanos, at: at}
	return cpu, mem
}

// metrics scrapes the metrics the scheduler asked for from a ready replica. A failed
// scrape leaves them out.
func (a *Agent) metrics(ctx context.Context, c container.Container) map[string]float64 {
	endpoint := c.Label(container.LabelMetrics)
	if endpoint == "" || len(c.Addrs) == 0 {
		return nil
	}
	var names []string
	for _, s := range a.last.Replicas {
		if s.ID == c.Replica() && s.Metrics != nil {
			names = s.Metrics.Names
		}
	}
	if len(names) == 0 {
		return nil
	}
	values, err := a.scraper.Scrape(ctx, c.Addrs[0], endpoint, names)
	if err != nil {
		a.log.Debug("scraping metrics", "replica", c.Replica(), "err", err)
		return nil
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

// stopTime is how long a replica has between SIGTERM and SIGKILL: its own stop time, from
// its label, cut to fit inside a Spot interruption's warning.
func (a *Agent) stopTime(c container.Container) time.Duration {
	d := defaultStop
	if s, err := strconv.Atoi(c.Label(container.LabelStop)); err == nil && s > 0 {
		d = time.Duration(s) * time.Second
	}
	if a.term != nil && a.term.spot && d > a.cfg.SpotStopLimit {
		d = a.cfg.SpotStopLimit
	}
	return d
}

const defaultStop = 10 * time.Second
