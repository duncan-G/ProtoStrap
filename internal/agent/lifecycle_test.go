package agent

import (
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/probe"
)

func TestAWorkerIsReadyOnceItRuns(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(worker("w-1"))
	h.mustTick()
	h.mustTick()
	if got := h.state("w-1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
	if h.prober.count() != 0 {
		t.Fatal("a worker has no readiness check")
	}
}

func TestAServiceIsStartingUntilItsCheckPasses(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	if got := h.state("api-1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
	h.prober.setReady("api", true)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
	probes := h.prober.count()
	h.prober.setReady("api", false)
	h.mustTick()
	if h.prober.count() != probes {
		t.Fatal("only replicas that are starting are checked; Envoy watches ready ones")
	}
	if got := h.state("api-1"); got != api.StateReady {
		t.Fatalf("ready never goes back to starting, got %s", got)
	}
}

func TestReadinessCheckTarget(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	c, _ := h.rt.byName("api-1")
	h.prober.mu.Lock()
	got := h.prober.probes[0]
	h.prober.mu.Unlock()
	want := probe.Target{Addr: c.Addrs[0], Port: 8080, Check: "grpc", Service: "api"}
	if got != want {
		t.Fatalf("probe %+v, want %+v", got, want)
	}
}

func TestHTTPReadinessCheck(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.Ready = "/healthz"
	h.sched.assign(s)
	h.mustTick()
	h.mustTick()
	h.prober.mu.Lock()
	defer h.prober.mu.Unlock()
	if h.prober.probes[0].Check != "/healthz" {
		t.Fatalf("probe %+v", h.prober.probes[0])
	}
}

func TestStatefulIsReadyWhenItsPortAnswers(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[h.cfg.DataDir+"/postgres"] = true
	h.sched.assign(stateful("postgres-a1", "postgres", 1000, "/data"))
	h.mustTick()
	h.mustTick()
	if got := h.state("postgres-a1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
	h.prober.setReady("postgres", true)
	h.mustTick()
	if got := h.state("postgres-a1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
	h.prober.mu.Lock()
	defer h.prober.mu.Unlock()
	if p := h.prober.probes[0]; p.Check != probe.CheckTCP || p.Port != 5432 {
		t.Fatalf("probe %+v", p)
	}
}

// A worker with a check of its own is starting until it passes. Having no port, its check
// names one.
func TestAWorkerWithACheckIsStartingUntilItPasses(t *testing.T) {
	h := newHarness(t)
	w := worker("w-1")
	w.Ready = "8081/healthz"
	h.sched.assign(w)
	h.mustTick()
	h.mustTick()
	if got := h.state("w-1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
	h.prober.setReady("orders-worker", true)
	h.mustTick()
	if got := h.state("w-1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
	h.prober.mu.Lock()
	defer h.prober.mu.Unlock()
	if p := h.prober.probes[0]; p.Check != "8081/healthz" || p.Service != "orders-worker" {
		t.Fatalf("probe %+v", p)
	}
}

// A command check runs in the replica, within the probe's time, and passes when it exits 0.
func TestACommandCheckRunsInTheReplica(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[h.cfg.DataDir+"/postgres"] = true
	pg := stateful("postgres-a1", "postgres", 1000, "/data")
	pg.ReadyCommand = []string{"pg_isready", "-U", "postgres"}
	code := 1
	h.rt.mu.Lock()
	h.rt.exec = func(ctx context.Context, name string, cmd []string, _, _ io.Writer) (int, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a command check must have a time limit: a hung command would hold the sync")
		}
		h.rt.mu.Lock()
		defer h.rt.mu.Unlock()
		return code, nil
	}
	h.rt.mu.Unlock()
	h.sched.assign(pg)
	h.mustTick()
	h.mustTick()
	if got := h.state("postgres-a1"); got != api.StateStarting {
		t.Fatalf("state %s while the command fails", got)
	}
	h.rt.mu.Lock()
	code = 0
	execs := slices.Clone(h.rt.execs)
	h.rt.mu.Unlock()
	if !slices.Contains(execs, "postgres-a1: pg_isready -U postgres") {
		t.Fatalf("execs %q", execs)
	}
	h.mustTick()
	if got := h.state("postgres-a1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
	if h.prober.count() != 0 {
		t.Error("a command check replaces the port check")
	}
}

// A replica whose port speaks cleartext HTTP/2 gets its HTTP check over it.
func TestH2CReachesTheCheck(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.Ready, s.H2C = "/healthz", true
	h.sched.assign(s)
	h.mustTick()
	h.mustTick()
	h.prober.mu.Lock()
	defer h.prober.mu.Unlock()
	if p := h.prober.probes[0]; !p.H2C || p.Check != "/healthz" {
		t.Fatalf("probe %+v", p)
	}
}

func TestNotReadyWithinTheTimeoutIsFailed(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.clock.advance(4*time.Minute + 59*time.Second)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateStarting {
		t.Fatalf("state %s before the timeout", got)
	}
	h.clock.advance(2 * time.Second)
	h.mustTick()
	r := h.replica("api-1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "not ready within 5m0s") {
		t.Fatalf("got %+v", r)
	}
	// Failed is final, even if the replica becomes ready later.
	h.prober.setReady("api", true)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateFailed {
		t.Fatalf("state %s", got)
	}
	if _, ok := h.rt.byName("api-1"); !ok {
		t.Fatal("the scheduler decides when it goes: it stays until it's left out")
	}
	h.sched.assign()
	h.mustTick()
	if _, ok := h.rt.byName("api-1"); ok {
		t.Fatal("left out, so stopped")
	}
}

func TestReadyTimeoutRestartsWithTheAgent(t *testing.T) {
	// An agent that restarts doesn't know how long a replica has been starting. It gives the
	// replica a full timeout from when it first sees it, rather than failing a healthy one.
	h := newHarness(t)
	s := service("api-1")
	h.rt.addContainer(container.Container{Name: "api-1", Status: container.Running, Labels: labelsFor(s),
		Created: epoch.Add(-time.Hour)})
	h.sched.assign(s)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
	h.clock.advance(4 * time.Minute)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateStarting {
		t.Fatalf("state %s", got)
	}
	h.clock.advance(2 * time.Minute)
	h.mustTick()
	if got := h.state("api-1"); got != api.StateFailed {
		t.Fatalf("state %s", got)
	}
}

func TestExitingBeforeReadyIsFailed(t *testing.T) {
	// Docker restarts the replica, but it never became ready, so it is failed, not starting.
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.rt.set("api-1", func(c *container.Container) { c.Restarts = 1 })
	h.mustTick()
	h.mustTick()
	r := h.replica("api-1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "before it was ready") || r.Restarts != 1 {
		t.Fatalf("got %+v", r)
	}
}

func TestRestartingBeforeReadyIsFailed(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.rt.set("api-1", func(c *container.Container) { c.Status = container.Restarting; c.ExitCode = 139; c.Restarts = 1 })
	h.mustTick()
	h.mustTick()
	if r := h.replica("api-1"); r.State != api.StateFailed || !strings.Contains(r.Reason, "139") {
		t.Fatalf("got %+v", r)
	}
}

func TestACrashAfterReadyStaysReady(t *testing.T) {
	// Docker restarts the crashed replica; the agent reports the restarts and Envoy routes
	// around it.
	h := newHarness(t)
	h.running(service("api-1"))
	h.rt.set("api-1", func(c *container.Container) { c.Status = container.Restarting; c.Restarts = 2 })
	h.mustTick()
	r := h.replica("api-1")
	if r.State != api.StateReady || r.Restarts != 2 {
		t.Fatalf("got %+v", r)
	}
	if _, starts, _, _, _ := h.rt.snapshot(); len(starts) != 1 {
		t.Fatal("Docker restarts it, not the agent")
	}
}

func TestTaskStates(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(task("m-ok", "migrate"), task("m-bad", "migrate"))
	h.mustTick()
	h.mustTick()
	if h.state("m-ok") != api.StateStarting || h.state("m-bad") != api.StateStarting {
		t.Fatalf("a running task is starting: %+v", h.report().Replicas)
	}
	h.rt.set("m-ok", func(c *container.Container) { c.Status = container.Exited; c.ExitCode = 0 })
	h.rt.set("m-bad", func(c *container.Container) { c.Status = container.Exited; c.ExitCode = 3 })
	h.mustTick()
	ok, bad := h.replica("m-ok"), h.replica("m-bad")
	if ok.State != api.StateSucceeded || ok.ExitCode == nil || *ok.ExitCode != 0 {
		t.Fatalf("ok %+v", ok)
	}
	if bad.State != api.StateFailed || bad.ExitCode == nil || *bad.ExitCode != 3 || !strings.Contains(bad.Reason, "3") {
		t.Fatalf("bad %+v", bad)
	}
	if h.prober.count() != 0 {
		t.Fatal("tasks have no readiness check")
	}
}

func TestATaskStateSurvivesAnAgentRestart(t *testing.T) {
	h := newHarness(t)
	s := task("m-1", "migrate")
	h.rt.addContainer(container.Container{Name: "m-1", Status: container.Exited, ExitCode: 0, Labels: labelsFor(s)})
	h.sched.assign(s)
	h.mustTick()
	if got := h.state("m-1"); got != api.StateSucceeded {
		t.Fatalf("state %s", got)
	}
	if _, starts, _, _, _ := h.rt.snapshot(); len(starts) != 0 {
		t.Fatal("a finished task must not run again after an agent restart")
	}
}

func TestFailedStateSurvivesAnAgentRestart(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	h.rt.addContainer(container.Container{Name: "api-1", Status: container.Exited, ExitCode: 1, Labels: labelsFor(s)})
	h.sched.assign(s)
	h.mustTick()
	h.mustTick()
	if got := h.state("api-1"); got != api.StateFailed {
		t.Fatalf("state %s", got)
	}
	if _, starts, _, _, _ := h.rt.snapshot(); len(starts) != 0 {
		t.Fatal("a replica that exited before it was ready must not be started again")
	}
}

func TestCPUAndMemoryUsage(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	h.rt.mu.Lock()
	h.rt.stats["api-1"] = container.Stats{CPUNanos: 10e9, MemoryBytes: 402 << 20, Read: epoch}
	h.rt.mu.Unlock()
	h.mustTick()
	r := h.replica("api-1")
	if r.CPU != nil {
		t.Fatalf("CPU needs two samples, got %d", *r.CPU)
	}
	if r.Memory == nil || *r.Memory != 402 {
		t.Fatalf("memory %v", r.Memory)
	}
	// The next sample adds 1.55 seconds of CPU in 5 seconds, which is 310 millicores.
	h.rt.mu.Lock()
	h.rt.stats["api-1"] = container.Stats{CPUNanos: 11.55e9, MemoryBytes: 410 << 20, Read: epoch.Add(5 * time.Second)}
	h.rt.mu.Unlock()
	h.mustTick()
	r = h.replica("api-1")
	if r.CPU == nil || *r.CPU != 310 {
		t.Fatalf("cpu %v", r.CPU)
	}
}

func TestCPUCounterResetIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	set := func(ns uint64, at time.Time) {
		h.rt.mu.Lock()
		h.rt.stats["w-1"] = container.Stats{CPUNanos: ns, MemoryBytes: 1 << 20, Read: at}
		h.rt.mu.Unlock()
	}
	set(50e9, epoch)
	h.mustTick()
	set(1e9, epoch.Add(5*time.Second)) // Docker restarted the container.
	h.mustTick()
	if r := h.replica("w-1"); r.CPU != nil {
		t.Fatalf("a reset counter gives no usage, got %d", *r.CPU)
	}
	set(2e9, epoch.Add(10*time.Second))
	h.mustTick()
	if r := h.replica("w-1"); r.CPU == nil || *r.CPU != 200 {
		t.Fatalf("cpu %v", r.CPU)
	}
}

func TestStatsFailureOmitsUsage(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	h.mustTick() // The fake has no stats for w-1.
	if r := h.replica("w-1"); r.CPU != nil || r.Memory != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestMetricsTheSchedulerAskedFor(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	c, _ := h.rt.byName("api-1")
	h.scraper.values[c.Addrs[0]] = map[string]float64{"llm_budget_cap": -1, "other": 5}
	h.mustTick()
	r := h.replica("api-1")
	if !maps.Equal(r.Metrics, map[string]float64{"llm_budget_cap": -1}) {
		t.Fatalf("metrics %v", r.Metrics)
	}
	h.scraper.mu.Lock()
	call := h.scraper.calls[len(h.scraper.calls)-1]
	h.scraper.mu.Unlock()
	if call != c.Addrs[0]+" 9090/metrics llm_budget_cap" {
		t.Fatalf("scraped %q", call)
	}
}

func TestMetricsOnlyFromReadyReplicas(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	if len(h.scraper.calls) != 0 {
		t.Fatalf("a starting replica isn't scraped: %v", h.scraper.calls)
	}
}

func TestMetricsFailureOmitsMetrics(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	h.scraper.err = errors.New("connection refused")
	if err := h.tick(); err != nil {
		t.Fatalf("a failed scrape must not fail the cycle: %v", err)
	}
	if r := h.replica("api-1"); r.Metrics != nil {
		t.Fatalf("metrics %v", r.Metrics)
	}
}

func TestNoMetricsWhenNoneAreAskedFor(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.Metrics = &api.MetricsSpec{Endpoint: "9090/metrics"}
	h.running(s)
	h.mustTick()
	if len(h.scraper.calls) != 0 {
		t.Fatalf("scraped %v", h.scraper.calls)
	}
}

func TestReportCarriesFingerprints(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	if r := h.replica("api-1"); r.Fingerprint != "fp-api" || r.Kind != api.KindService || r.Release != "9f2c1e7" {
		t.Fatalf("got %+v", r)
	}
}
