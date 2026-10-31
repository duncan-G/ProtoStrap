package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/container"
)

func check(name string, err error, order *[]string) Check {
	return Check{Name: name, Run: func(context.Context) error {
		if order != nil {
			*order = append(*order, "check "+name)
		}
		return err
	}}
}

// ordered wires the firewall and hooks into one call log.
func ordered(order *[]string) func(*harness, *Config, *Deps) {
	return func(h *harness, cfg *Config, d *Deps) {
		h.fw.order = order
		h.hooks.order = order
	}
}

func TestBootChecksTheVMCompletesTheHookAndJoinsTheSwarm(t *testing.T) {
	var order []string
	h := newHarness(t, ordered(&order), func(h *harness, cfg *Config, d *Deps) {
		d.Checks = []Check{check("credential helper", nil, &order), check("ssm agent", nil, &order)}
	})
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"check credential helper", "check ssm agent", "hook launch CONTINUE", "firewall"}
	if !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	if len(h.rt.joins) != 1 {
		t.Fatalf("joins %v", h.rt.joins)
	}
	j := h.rt.joins[0]
	if j.Token != "SWMTKN-1-abc" || !slices.Equal(j.Managers, []string{"10.20.1.10:2377"}) || j.AdvertiseAddr != "10.20.16.5" {
		t.Fatalf("join %+v", j)
	}
	if h.notify.count("READY=1") != 1 {
		t.Fatalf("systemd must be told the agent is ready: %v", h.notify.states)
	}
	if h.rt.calls()[0] != "ping" {
		t.Fatalf("Docker is checked first: %v", h.rt.calls())
	}
}

func TestBootAbandonsTheVMWhenDockerIsDown(t *testing.T) {
	h := newHarness(t)
	h.rt.pingErr = errors.New("cannot connect to the Docker daemon")
	if err := h.a.Boot(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if !slices.Equal(h.hooks.completes, []string{"launch ABANDON"}) {
		t.Fatalf("the ASG must discard the VM: %v", h.hooks.completes)
	}
	if h.fw.calls != 0 || h.sched.joinCalls != 0 {
		t.Fatal("nothing else may run on a VM that failed its checks")
	}
}

func TestBootAbandonsTheVMWhenACheckFails(t *testing.T) {
	for _, failing := range []string{"credential helper", "ssm agent"} {
		t.Run(failing, func(t *testing.T) {
			h := newHarness(t, func(h *harness, cfg *Config, d *Deps) {
				for _, name := range []string{"credential helper", "ssm agent"} {
					var err error
					if name == failing {
						err = errors.New(name + " missing")
					}
					d.Checks = append(d.Checks, check(name, err, nil))
				}
			})
			err := h.a.Boot(t.Context())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !slices.Equal(h.hooks.completes, []string{"launch ABANDON"}) {
				t.Fatalf("completes %v", h.hooks.completes)
			}
		})
	}
}

func TestBootCompletesTheLaunchHookWithoutTheScheduler(t *testing.T) {
	// data-1 is down: the VM must still finish launching, or the ASG would replace it.
	h := newHarness(t)
	down := errors.New("connection refused")
	h.sched.joinErrs = []error{down, down, down}
	var hookDoneBeforeJoin bool
	h.clock.onSleep = func() {
		h.hooks.mu.Lock()
		hookDoneBeforeJoin = slices.Contains(h.hooks.completes, "launch CONTINUE")
		h.hooks.mu.Unlock()
	}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !hookDoneBeforeJoin {
		t.Fatal("the launch hook must be completed before the scheduler answers")
	}
	if h.sched.joinCalls != 4 {
		t.Fatalf("join calls %d, want 3 failures then success", h.sched.joinCalls)
	}
	for _, s := range h.clock.sleeps {
		if s != h.cfg.JoinRetry {
			t.Fatalf("join retries every %v, slept %v", h.cfg.JoinRetry, h.clock.sleeps)
		}
	}
	if len(h.rt.joins) != 1 {
		t.Fatalf("joins %v", h.rt.joins)
	}
}

func TestBootGivesUpWhenCancelled(t *testing.T) {
	h := newHarness(t)
	h.sched.joinErrs = make([]error, 1000)
	for i := range h.sched.joinErrs {
		h.sched.joinErrs[i] = errors.New("down")
	}
	ctx, cancel := context.WithCancel(t.Context())
	h.clock.onSleep = func() {
		if len(h.clock.sleeps) == 3 {
			cancel()
		}
	}
	if err := h.a.Boot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestBootRetriesAFailedSwarmJoin(t *testing.T) {
	h := newHarness(t)
	h.rt.joinErrs = []error{errors.New("manager unreachable")}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.joins) != 2 || h.sched.joinCalls != 2 {
		t.Fatalf("joins %d, join calls %d", len(h.rt.joins), h.sched.joinCalls)
	}
}

func TestBootOnADataVMHasNoHook(t *testing.T) {
	h := newHarness(t, dataVM)
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestBootAfterAnAgentRestartIgnoresTheCompletedHook(t *testing.T) {
	h := newHarness(t)
	h.hooks.err = fmt.Errorf("%w: ValidationError: No active Lifecycle Action found", ErrHookDone)
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatalf("an already completed hook must not stop the agent: %v", err)
	}
	waitForLaunchHook(t, h)
	if got := h.hooks.completed(); !slices.Equal(got, []string{"launch CONTINUE"}) {
		t.Fatalf("a hook AWS says is done is tried once: %v", got)
	}
}

// waitForLaunchHook waits until the agent has stopped trying to complete the launch hook.
func waitForLaunchHook(t *testing.T, h *harness) {
	t.Helper()
	select {
	case <-h.a.launchHookDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("the agent is still completing the launch hook: %v", h.hooks.completed())
	}
}

// A pool VM can boot before its zone's ingress VM has made the route out, so Auto Scaling
// is out of reach for a while. The agent keeps trying without holding up its boot: if it
// gave up, the hook would time out and the group would replace a VM whose agent works.
func TestBootKeepsCompletingTheLaunchHookUntilAWSAnswers(t *testing.T) {
	h := newHarness(t)
	unreachable := errors.New("dial tcp 3.5.0.1:443: i/o timeout")
	h.hooks.errs = []error{unreachable, unreachable, unreachable}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.notify.count("READY=1") != 1 {
		t.Fatal("the agent is ready while it keeps trying the hook")
	}
	waitForLaunchHook(t, h)
	if got := h.hooks.completed(); len(got) != 4 || got[3] != "launch CONTINUE" {
		t.Fatalf("three failures, then the hook completes: %v", got)
	}
	h.clock.mu.Lock()
	defer h.clock.mu.Unlock()
	waits := 0
	for _, d := range h.clock.sleeps {
		if d == h.cfg.HookRetry {
			waits++
		}
	}
	if waits != 3 {
		t.Fatalf("it waits HookRetry (%v) before each retry: %v", h.cfg.HookRetry, h.clock.sleeps)
	}
}

func TestBootStopsCompletingTheLaunchHookWhenAWSSaysItIsDone(t *testing.T) {
	h := newHarness(t)
	h.hooks.errs = []error{errors.New("dial tcp: i/o timeout"), fmt.Errorf("%w: it timed out", ErrHookDone)}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitForLaunchHook(t, h)
	if got := h.hooks.completed(); len(got) != 2 {
		t.Fatalf("it stops once AWS has no hook in progress: %v", got)
	}
}

func TestBootStopsCompletingTheLaunchHookWhenCancelled(t *testing.T) {
	h := newHarness(t)
	h.hooks.err = errors.New("dial tcp: i/o timeout")
	ctx, cancel := context.WithCancel(t.Context())
	if err := h.a.Boot(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitForLaunchHook(t, h)
}

func TestBootOnADataVMHasNoHookToComplete(t *testing.T) {
	h := newHarness(t, dataVM)
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitForLaunchHook(t, h)
}

func TestBootFailsWithoutAFirewall(t *testing.T) {
	h := newHarness(t)
	h.fw.err = errors.New("iptables: resource unavailable")
	if err := h.a.Boot(t.Context()); err == nil {
		t.Fatal("no replica may start before the firewall is up")
	}
	if h.sched.joinCalls != 0 {
		t.Fatal("must not join before the firewall is up")
	}
}

func TestBootSkipsJoiningTheSwarmItIsIn(t *testing.T) {
	h := newHarness(t)
	h.rt.info.Swarm = container.Swarm{State: "active", Managers: []string{"10.20.1.10:2377"}}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.rt.joins) != 0 || h.rt.leaves != 0 {
		t.Fatalf("joins %v leaves %d", h.rt.joins, h.rt.leaves)
	}
}

func TestBootRejoinsWhenTheSwarmMoved(t *testing.T) {
	// The data VM was rebuilt in another zone with a new address, so there is a new swarm.
	h := newHarness(t)
	h.rt.info.Swarm = container.Swarm{State: "active", Managers: []string{"10.20.1.74:2377"}}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := h.rt.calls()
	leave, join := slices.Index(calls, "leave-swarm"), slices.Index(calls, "join-swarm")
	if leave < 0 || join < 0 || leave > join {
		t.Fatalf("must leave the old swarm then join: %v", calls)
	}
}

func TestBootLeavesAStuckJoin(t *testing.T) {
	for _, state := range []string{"pending", "error", "locked"} {
		h := newHarness(t)
		h.rt.info.Swarm = container.Swarm{State: state}
		if err := h.a.Boot(t.Context()); err != nil {
			t.Fatal(err)
		}
		if h.rt.leaves != 1 || len(h.rt.joins) != 1 {
			t.Fatalf("%s: leaves %d joins %d", state, h.rt.leaves, len(h.rt.joins))
		}
	}
}

func TestBootOnTheSwarmManagerNeverJoins(t *testing.T) {
	h := newHarness(t, dataVM, func(h *harness, cfg *Config, d *Deps) { cfg.SwarmManager = true })
	h.rt.info.Swarm = container.Swarm{State: "active", Manager: true}
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.sched.joinCalls != 0 || len(h.rt.joins) != 0 || h.rt.leaves != 0 {
		t.Fatalf("data-1 runs the manager: join calls %d, joins %d, leaves %d", h.sched.joinCalls, len(h.rt.joins), h.rt.leaves)
	}
}

func TestBootOnAnIngressVMTakesOverTheZone(t *testing.T) {
	var order []string
	zone := &fakeZone{order: &order}
	h := newHarness(t, ordered(&order), func(h *harness, cfg *Config, d *Deps) {
		cfg.Pool, cfg.Name = "ingress", "ingress-a"
		d.Zone = zone
	})
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	// NAT rules come with the firewall, so they're in place before routes point here.
	if !slices.Equal(order, []string{"hook launch CONTINUE", "firewall", "takeover"}) {
		t.Fatalf("order %v", order)
	}
	if len(h.rt.joins) != 1 {
		t.Fatal("the ingress VM joins the swarm too")
	}
}

func TestBootFailsWhenTheTakeoverFails(t *testing.T) {
	zone := &fakeZone{err: errors.New("UnauthorizedOperation")}
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) {
		cfg.Pool = "ingress"
		d.Zone = zone
	})
	if err := h.a.Boot(t.Context()); err == nil {
		t.Fatal("an ingress VM that can't take its zone must not carry on")
	}
}

func TestBootWhileTerminatingDoesNotJoin(t *testing.T) {
	// The agent restarted on a VM that is being drained.
	h := newHarness(t)
	h.meta.set(&Notice{})
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	if h.sched.joinCalls != 0 || len(h.rt.joins) != 0 {
		t.Fatal("a VM on its way out must not join the swarm")
	}
}

func TestBootReadsCapacity(t *testing.T) {
	h := newHarness(t)
	h.rt.info.CPUs, h.rt.info.MemoryBytes = 8, 32<<30
	if err := h.a.Boot(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.mustTick()
	if c := h.report().Capacity; c.CPU != 8000 || c.Memory != 32768 {
		t.Fatalf("capacity %+v", c)
	}
}

func TestBootFailsWhenDockerInfoFails(t *testing.T) {
	h := newHarness(t)
	h.rt.infoErr = errors.New("boom")
	if err := h.a.Boot(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
	if !slices.Equal(h.hooks.completes, []string{"launch ABANDON"}) {
		t.Fatalf("completes %v", h.hooks.completes)
	}
}

func TestNewRequiresItsDependencies(t *testing.T) {
	h := newHarness(t)
	base := Deps{Runtime: h.rt, Scheduler: h.sched, Metadata: h.meta, Firewall: h.fw, Credentials: h.creds,
		Prober: h.prober, Scraper: h.scraper, Volumes: h.volumes, Updater: h.updater, Notifier: h.notify}
	cfg := h.cfg
	if _, err := New(cfg, base); err != nil {
		t.Fatalf("hooks, zone, backups, clock, token and logger are optional: %v", err)
	}
	for name, clear := range map[string]func(*Deps){
		"runtime":     func(d *Deps) { d.Runtime = nil },
		"scheduler":   func(d *Deps) { d.Scheduler = nil },
		"metadata":    func(d *Deps) { d.Metadata = nil },
		"firewall":    func(d *Deps) { d.Firewall = nil },
		"credentials": func(d *Deps) { d.Credentials = nil },
		"prober":      func(d *Deps) { d.Prober = nil },
		"scraper":     func(d *Deps) { d.Scraper = nil },
		"volumes":     func(d *Deps) { d.Volumes = nil },
		"updater":     func(d *Deps) { d.Updater = nil },
		"notifier":    func(d *Deps) { d.Notifier = nil },
	} {
		d := base
		clear(&d)
		if _, err := New(cfg, d); err == nil {
			t.Errorf("missing %s should be an error", name)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{App: "shop", InstanceID: "i-1", Pool: "general", Region: "eu-west-1", Version: "1.4.0"}.WithDefaults()
	checks := map[string]bool{
		"network":       c.Network == "protostrap",
		"data dir":      c.DataDir == "/data",
		"files dir":     c.FilesDir == "/var/lib/protostrap/files",
		"edge mount":    c.EdgeFilesMount == "/etc/protostrap",
		"socket dir":    c.TelemetrySocketDir == "/run/protostrap/telemetry",
		"credentials":   c.CredentialsPath == "/v1/credentials/",
		"sync":          c.SyncInterval == 5*time.Second,
		"ready timeout": c.ReadyTimeout == 5*time.Minute,
		"silent":        c.SilentAfter == 30*time.Second,
		"hook timeout":  c.HookTimeout == 10*time.Minute,
		"drain margin":  c.DrainMargin == 15*time.Second,
		"spot stop":     c.SpotStopLimit == 90*time.Second,
		"spot warning":  c.SpotWarning == 2*time.Minute,
		"backup retry":  c.BackupRetry == time.Hour,
		"join retry":    c.JoinRetry == 5*time.Second,
		"hook retry":    c.HookRetry == 10*time.Second,
		"secrets dir":   c.SecretsDir == "/run/protostrap/secrets",
		"secrets mount": c.SecretsMount == "/run/secrets",
		"secret hook":   c.SecretHookTimeout == 2*time.Minute,
		"secret retry":  c.SecretRetry == time.Minute,
		"name from id":  c.Name == "i-1",
		"backup hour":   c.BackupHour == 0, // Midnight is a valid hour, so there is no default.
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("default %s wrong: %+v", name, c)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	good := Config{App: "shop", InstanceID: "i-1", Pool: "general", Region: "eu-west-1", Version: "1.4.0", TelemetryAddress: "10.20.1.10:24224"}.WithDefaults()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"no app":                   func(c *Config) { c.App = "" },
		"no instance":              func(c *Config) { c.InstanceID = "" },
		"bad pool":                 func(c *Config) { c.Pool = "gpu" },
		"no region":                func(c *Config) { c.Region = "" },
		"no version":               func(c *Config) { c.Version = "" },
		"backup hour":              func(c *Config) { c.BackupHour = 24 },
		"relative dir":             func(c *Config) { c.DataDir = "data" },
		"bad creds":                func(c *Config) { c.CredentialsPath = "v1/credentials" },
		"zero sync":                func(c *Config) { c.SyncInterval = 0 },
		"relative secrets dir":     func(c *Config) { c.SecretsDir = "run/secrets" },
		"relative secrets mount":   func(c *Config) { c.SecretsMount = "secrets" },
		"secrets under files":      func(c *Config) { c.SecretsDir = c.FilesDir + "/secrets" },
		"secrets dir is files dir": func(c *Config) { c.SecretsDir = c.FilesDir },
		"zero secret hook timeout": func(c *Config) { c.SecretHookTimeout = -1 },
		"no telemetry address":     func(c *Config) { c.TelemetryAddress = "" },
		"telemetry without a port": func(c *Config) { c.TelemetryAddress = "10.20.1.10" },
		"relative socket dir":      func(c *Config) { c.TelemetrySocketDir = "run/telemetry" },
	} {
		c := good
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
