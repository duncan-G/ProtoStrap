// Package scheduler is `protostrap scheduler`, the one component that decides
// (docs/protostrap.md#scheduler): how many replicas each component runs, on which VM, how
// many VMs each pool has, and how releases roll out. It runs on data-1, exactly once.
//
// Decisions are recomputed, not remembered. Every cycle compares what should run with
// what agents report running, from scratch: it observes (agents' reports, queue depths,
// the Auto Scaling groups), counts each component's desired replicas, rolls out the target
// release, places replicas and chooses which to stop, sizes the pools, and leaves each
// VM's assignment for its agent to collect on its next sync. The scheduler's only state
// outside memory is state.json in the deploy bucket: the target release, the last good one
// and each component's last desired count. Restarting it loses nothing: it adopts what runs.
//
// If it stops, every replica keeps running and serving. Only deploys, scaling and
// recovery wait for it to return.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/release"
)

var (
	// ErrNotFound is a release or object that doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrWarmingUp is the answer to a sync in the scheduler's first seconds, while it
	// listens to the agents before deciding anything. An agent that gets it starts and
	// stops nothing, as when the scheduler is down.
	ErrWarmingUp = errors.New("the scheduler is starting: it decides nothing until it has heard from the agents")
)

// Config is the scheduler's application and its timings (docs/protostrap.md#timings).
type Config struct {
	App         string
	Environment string
	Region      string
	Account     string
	Registry    string // It is the ECR registry the images are in.
	FilesBucket string
	Address     string // It is data-1's fixed address, where the swarm manager is.
	Version     string // It is this binary's ProtoStrap version.

	// Cycle is how often the scheduler decides. It defaults to 10s.
	Cycle time.Duration
	// Warmup is how long the scheduler listens to agents before deciding. It defaults to 15s.
	Warmup time.Duration
	// SilentAfter is how long an agent can go without syncing before it is silent and its
	// replicas stop counting. It defaults to 30s.
	SilentAfter time.Duration
	// UnhealthyAfter is how long a pool VM's agent stays silent before the VM is marked
	// unhealthy. It defaults to 10m.
	UnhealthyAfter time.Duration
	// UnhealthyEvery is how often, at most, a VM of each pool is marked unhealthy. It
	// defaults to 10m.
	UnhealthyEvery time.Duration
	// ScaleInAfter is how long sizing must want fewer VMs before one is removed. It defaults
	// to 10m.
	ScaleInAfter time.Duration
	// LaunchTimeout is how long launches can go unfilled before they are cancelled. It
	// defaults to 10m.
	LaunchTimeout time.Duration
	// SpotRetry is how long after Spot had no capacity the scheduler tries it again. It
	// defaults to 30m.
	SpotRetry time.Duration
	// StartupHold is how long after a start the scheduler scales nothing down. It defaults
	// to 5m.
	StartupHold time.Duration
	// CPUWindow is how long signals are averaged over to scale up. It defaults to 1m.
	CPUWindow time.Duration
	// PeakWindow is how far back the scheduler looks for the highest demand, which is as
	// low as it scales down. It defaults to 5m.
	PeakWindow time.Duration
	// SignalWindow is how long signals are kept. It defaults to 10m.
	SignalWindow time.Duration
	// LimitTTL is how long a limit can go without a value before it caps nothing. It
	// defaults to 5m.
	LimitTTL time.Duration
	// SnapshotEvery is how often every signal is logged. It defaults to 1m.
	SnapshotEvery time.Duration
	// Hysteresis is the share of CPU's target within which nothing changes. It defaults to
	// 0.10.
	Hysteresis float64
	// SecretCheck is how often the environment's secret's version is read. It defaults to 1m.
	SecretCheck time.Duration
	// SecretSettle is how long the secret must go unchanged before a change is applied, so
	// keys written together are applied together. It defaults to 2m.
	SecretSettle time.Duration
	// HostsCheck is how often hosts.json, the account's hostnames, is read. It defaults to 1m.
	HostsCheck time.Duration
}

// WithDefaults fills in the timings.
func (c Config) WithDefaults() Config {
	set := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	set(&c.Cycle, 10*time.Second)
	set(&c.Warmup, 15*time.Second)
	set(&c.SilentAfter, 30*time.Second)
	set(&c.UnhealthyAfter, 10*time.Minute)
	set(&c.UnhealthyEvery, 10*time.Minute)
	set(&c.ScaleInAfter, 10*time.Minute)
	set(&c.LaunchTimeout, 10*time.Minute)
	set(&c.SpotRetry, 30*time.Minute)
	set(&c.StartupHold, 5*time.Minute)
	set(&c.CPUWindow, time.Minute)
	set(&c.PeakWindow, 5*time.Minute)
	set(&c.SignalWindow, 10*time.Minute)
	set(&c.LimitTTL, 5*time.Minute)
	set(&c.SnapshotEvery, time.Minute)
	set(&c.SecretCheck, time.Minute)
	set(&c.SecretSettle, 2*time.Minute)
	set(&c.HostsCheck, time.Minute)
	if c.Hysteresis == 0 {
		c.Hysteresis = 0.10
	}
	return c
}

func (c Config) validate() error {
	var errs []error
	for name, v := range map[string]string{"app": c.App, "region": c.Region, "account": c.Account, "registry": c.Registry, "version": c.Version} {
		if v == "" {
			errs = append(errs, fmt.Errorf("no %s", name))
		}
	}
	return errors.Join(errs...)
}

// Store is the deploy bucket.
type Store interface {
	Release(ctx context.Context, commit string) (release.Release, error)
	State(ctx context.Context) (release.State, error) // It returns the zero State when there is none.
	SaveState(ctx context.Context, s release.State) error
	// Hosts returns hosts.json, the account's hostnames by the spec's names, or nil when
	// there is none.
	Hosts(ctx context.Context) (map[string]string, error)
}

// Depth is a queue's jobs: waiting, and received but not yet deleted.
type Depth struct{ Visible, InFlight int }

// Queues reads queue depths from SQS. Depth takes a queue's name in the spec, and its URL.
type Queues interface {
	Depth(ctx context.Context, queue, url string) (Depth, error)
}

// Group is one Auto Scaling group of a pool, found by its tags.
type Group struct {
	Name      string
	Pool      string // It is ingress, general or compute.
	Zone      string
	Spot      bool
	Min, Max  int
	Desired   int
	Instances []Instance
}

// Instance is one VM in a group.
type Instance struct {
	ID string
	// State is the group's lifecycle state, such as Pending, Pending:Wait, InService or
	// Terminating.
	State    string
	Launched time.Time
}

// Groups is Auto Scaling. The scheduler adds a VM by raising a group's desired capacity,
// and removes the VM it chose by terminating it and lowering the capacity in one call. It
// never just lowers the capacity, which would let AWS choose, except to cancel launches
// that can't be filled.
type Groups interface {
	Groups(ctx context.Context) ([]Group, error)
	SetDesired(ctx context.Context, group string, desired int) error
	Terminate(ctx context.Context, instance string) error // It also lowers the desired capacity.
	SetUnhealthy(ctx context.Context, instance string) error
}

// Node is a VM in the swarm.
type Node struct {
	ID    string
	Addr  string
	State string // It is the node's state, such as ready or down.
}

// Swarm is the local Docker on data-1, where the swarm manager runs.
type Swarm interface {
	// Join makes the swarm and the overlay network if they don't exist, and returns what an
	// agent needs to join.
	Join(ctx context.Context) (api.Join, error)
	Nodes(ctx context.Context) ([]Node, error)
	RemoveNode(ctx context.Context, id string) error
}

// Secrets reads the environment's secret (docs/secrets.md).
type Secrets interface {
	// CurrentVersion returns the ID of the secret's current version, or "" if it has no
	// value yet. It reads the secret's metadata, never its value.
	CurrentVersion(ctx context.Context, id string) (string, error)
	// Read returns a version of the secret's value, which the scheduler only hashes.
	Read(ctx context.Context, id, version string) (string, error)
}

// Clock is time, replaceable in tests.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Deps are what the scheduler works through.
type Deps struct {
	Store  Store
	Queues Queues
	Groups Groups
	Swarm  Swarm
	// Secrets is the environment's secret. Without it nothing changes with the secret.
	Secrets Secrets
	// SecretKey is the key the components' keys are hashed with. It must stay the same
	// across restarts, or every component's keys look changed.
	SecretKey []byte
	Clock     Clock                         // It defaults to the system clock.
	Logger    *slog.Logger                  // It defaults to slog.Default().
	NewID     func(component string) string // It makes a replica's id, <component>-<random> by default.
}

// VMIdentity is who a sync comes from, as the API proved it.
type VMIdentity struct {
	InstanceID string
	PrivateIP  string
}

// Scheduler is the scheduler. Its methods are safe to call from the API's goroutines and
// the cycle's at once.
type Scheduler struct {
	cfg   Config
	store Store
	queue Queues
	asg   Groups
	swarm Swarm
	// secrets and secretKey are the environment's secret, and the key its keys are hashed with.
	secrets   Secrets
	secretKey []byte
	keyID     string
	clock     Clock
	log       *slog.Logger
	newID     func(string) string

	mu sync.Mutex
	st state
}

// New returns a scheduler. Call Start before anything else.
func New(cfg Config, d Deps) (*Scheduler, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("scheduler config: %w", err)
	}
	if d.Store == nil || d.Queues == nil || d.Groups == nil || d.Swarm == nil {
		return nil, errors.New("scheduler: a store, queues, groups and swarm are required")
	}
	if d.Secrets != nil && len(d.SecretKey) == 0 {
		return nil, errors.New("scheduler: the secret needs a key to hash its keys with")
	}
	s := &Scheduler{cfg: cfg, store: d.Store, queue: d.Queues, asg: d.Groups, swarm: d.Swarm,
		secrets: d.Secrets, secretKey: d.SecretKey, keyID: keyID(d.SecretKey), clock: d.Clock, log: d.Logger, newID: d.NewID}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.newID == nil {
		s.newID = randomID
	}
	s.st = newState()
	return s, nil
}

// Start reads state.json and the target release, and begins the warm-up: for its first
// seconds the scheduler only listens to agents.
func (s *Scheduler) Start(ctx context.Context) error {
	saved, err := s.store.State(ctx)
	if err != nil {
		return fmt.Errorf("reading state.json: %w", err)
	}
	var tgt *target
	if saved.Target != "" {
		if tgt, err = s.loadTarget(ctx, saved.Target); err != nil {
			s.log.Error("the target release can't be read; trying the last good one", "target", saved.Target, "err", err)
			if saved.LastGood != "" && saved.LastGood != saved.Target {
				tgt, err = s.loadTarget(ctx, saved.LastGood)
			}
			if err != nil {
				s.log.Error("no release can be read: running nothing until one is deployed", "err", err)
				tgt = nil
			}
		}
	}
	// The last good release is loaded too, to roll back to if the target fails.
	var good *target
	if saved.LastGood != "" && (tgt == nil || tgt.rel.Commit != saved.LastGood) {
		if good, err = s.loadTarget(ctx, saved.LastGood); err != nil {
			s.log.Warn("the last good release can't be read", "release", saved.LastGood, "err", err)
		}
	}
	if _, err := s.swarm.Join(ctx); err != nil {
		s.log.Warn("the swarm isn't ready yet", "err", err)
	}

	s.mu.Lock()
	// The hostnames last read stay until hosts.json can be read again, so a restart while
	// it can't be replaces nothing.
	s.st.hosts.account = saved.Hosts
	s.mu.Unlock()
	s.checkHosts(ctx, s.clock.Now())

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	s.st.started = now
	s.st.lastGood = saved.LastGood
	s.st.saved = saved
	s.st.lastDesired = saved.Desired
	s.loadSecrets(saved)
	s.loadConfigures(saved)
	if good != nil {
		s.st.releases[saved.LastGood] = good
	}
	if tgt != nil {
		s.st.target = tgt
		s.st.releases[tgt.rel.Commit] = tgt
		if tgt.rel.Commit != saved.LastGood {
			s.st.rollouts = append(s.st.rollouts, &rollout{Commit: tgt.rel.Commit, State: RolloutRunning, Started: now})
		}
	}
	s.log.Info("scheduler started", "version", s.cfg.Version, "target", saved.Target, "last_good", saved.LastGood)
	return nil
}

// Run cycles until ctx is done.
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Cycle)
	defer t.Stop()
	for {
		if err := s.Cycle(ctx); err != nil && ctx.Err() == nil {
			s.log.Error("cycle", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Join is how an agent joins the swarm.
func (s *Scheduler) Join(ctx context.Context) (api.Join, error) {
	s.mu.Lock()
	j := s.st.join
	s.mu.Unlock()
	if j != nil {
		return *j, nil
	}
	got, err := s.swarm.Join(ctx)
	if err != nil {
		return api.Join{}, err
	}
	s.mu.Lock()
	s.st.join = &got
	s.mu.Unlock()
	return got, nil
}

// Deploy makes a release the target. The rollout happens over the next cycles; Status
// says how it went. A newer release supersedes one rolling out.
func (s *Scheduler) Deploy(ctx context.Context, commit string) error {
	tgt, err := s.loadTarget(ctx, commit)
	if err != nil {
		return err
	}
	s.mu.Lock()
	now := s.clock.Now()
	for _, r := range s.st.rollouts {
		if r.State == RolloutRunning && r.Commit != commit {
			r.State, r.Reason, r.Finished = RolloutSuperseded, "superseded by "+commit, now
			s.log.Info("release superseded", "release", r.Commit, "by", commit)
		}
	}
	s.st.releases[commit] = tgt
	s.st.target = tgt
	s.st.rollouts = slices.DeleteFunc(s.st.rollouts, func(r *rollout) bool { return r.Commit == commit })
	s.st.rollouts = append(s.st.rollouts, &rollout{Commit: commit, State: RolloutRunning, Started: now})
	if len(s.st.rollouts) > 20 {
		s.st.rollouts = s.st.rollouts[len(s.st.rollouts)-20:]
	}
	s.log.Info("release deployed", "release", commit, "version", tgt.rel.Version)
	toSave, save := s.stateToSave(now)
	s.mu.Unlock()
	// The target is durable before the deploy is acknowledged, so a restart doesn't lose it.
	if save {
		if err := s.store.SaveState(ctx, toSave); err != nil {
			s.mu.Lock()
			s.st.saved = release.State{}
			s.mu.Unlock()
			return fmt.Errorf("saving state.json: %w", err)
		}
	}
	return nil
}

// loadTarget reads and parses a release, from the cache when it can.
func (s *Scheduler) loadTarget(ctx context.Context, commit string) (*target, error) {
	s.mu.Lock()
	cached := s.st.releases[commit]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	rel, err := s.store.Release(ctx, commit)
	if err != nil {
		return nil, err
	}
	sp, err := rel.ParseSpec()
	if err != nil {
		return nil, err
	}
	return &target{rel: rel, spec: sp}, nil
}

// Sync records a VM's report and returns its assignment (docs/protostrap.md#sync).
func (s *Scheduler) Sync(_ context.Context, id VMIdentity, r api.Report) (api.Assignment, error) {
	if r.VM != id.InstanceID {
		return api.Assignment{}, fmt.Errorf("report from %s claims to be %s", id.InstanceID, r.VM)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.clock.Now()
	vm := s.st.observe(id, r, now)
	if !s.st.warm {
		return api.Assignment{}, ErrWarmingUp
	}
	s.reconcileReport(vm, r, now)
	return s.assignment(vm), nil
}

// Cycle runs one decision cycle.
func (s *Scheduler) Cycle(ctx context.Context) error {
	s.mu.Lock()
	now := s.clock.Now()
	warm, started := s.st.warm, s.st.started
	tgt := s.st.target
	s.mu.Unlock()

	if !warm {
		if now.Sub(started) < s.cfg.Warmup {
			return nil
		}
		s.adopt(ctx)
	}

	// Observe what lives outside the agents' reports.
	groups, gerr := s.asg.Groups(ctx)
	if gerr != nil {
		s.log.Warn("describing the Auto Scaling groups", "err", gerr)
	}
	depths := map[string]Depth{}
	if tgt != nil {
		for _, q := range tgt.spec.QueueNames() {
			d, err := s.queue.Depth(ctx, q, s.queueURL(q, tgt.spec.Queues[q].FIFO))
			if err != nil {
				s.log.Warn("reading a queue's depth", "queue", q, "err", err)
				continue
			}
			depths[q] = d
		}
	}
	nodes, nerr := s.swarm.Nodes(ctx)
	if nerr != nil {
		s.log.Debug("listing swarm nodes", "err", nerr)
	}
	s.checkSecret(ctx, now, tgt)
	s.checkHosts(ctx, now)

	s.mu.Lock()
	if gerr == nil {
		s.st.groups = groups
	}
	s.st.depths = depths
	acts := s.decide(now, nodes)
	toSave, save := s.stateToSave(now)
	s.mu.Unlock()

	s.act(ctx, acts)
	if save {
		if err := s.store.SaveState(ctx, toSave); err != nil {
			s.log.Error("saving state.json", "err", err)
			s.mu.Lock()
			s.st.saved = release.State{} // The next cycle tries again.
			s.mu.Unlock()
		}
	}
	return nil
}

// decide makes the cycle's decisions, under the lock.
func (s *Scheduler) decide(now time.Time, nodes []Node) []action {
	s.markSilent(now)
	s.forgetGone(now)
	s.count(now)
	s.secretChanges(now)
	s.reconcile(now)
	s.place(now)
	acts := s.size(now)
	acts = append(acts, s.cleanSwarm(now, nodes)...)
	s.snapshot(now)
	return acts
}

// action is a call to AWS or Docker the cycle decided on, made after the lock is released.
type action struct {
	kind     string // It is desired, terminate, unhealthy or node.
	group    string
	instance string
	desired  int
	node     string
	reason   string
}

func (s *Scheduler) act(ctx context.Context, acts []action) {
	for _, a := range acts {
		var err error
		switch a.kind {
		case "desired":
			err = s.asg.SetDesired(ctx, a.group, a.desired)
		case "terminate":
			err = s.asg.Terminate(ctx, a.instance)
		case "unhealthy":
			err = s.asg.SetUnhealthy(ctx, a.instance)
		case "node":
			err = s.swarm.RemoveNode(ctx, a.node)
		}
		if err != nil {
			s.log.Error("carrying out a decision", "action", a.kind, "group", a.group, "instance", a.instance, "node", a.node, "err", err)
		}
	}
}

// stateToSave is state.json as it should be, and whether it changed.
func (s *Scheduler) stateToSave(now time.Time) (release.State, bool) {
	st := release.State{LastGood: s.st.lastGood, Desired: map[string]int{}}
	if s.st.target != nil {
		st.Target = s.st.target.rel.Commit
	}
	for c, n := range s.st.desired {
		st.Desired[c] = n
	}
	s.saveSecrets(&st)
	s.saveConfigures(&st)
	st.Hosts = s.st.hosts.account
	old := s.st.saved
	same := old.Target == st.Target && old.LastGood == st.LastGood && len(old.Desired) == len(st.Desired) &&
		old.SecretVersion == st.SecretVersion && old.SecretChanged.Equal(st.SecretChanged) && maps.Equal(old.Secrets, st.Secrets) &&
		maps.Equal(old.Configured, st.Configured) && maps.Equal(old.Hosts, st.Hosts)
	if same {
		for c, n := range st.Desired {
			if old.Desired[c] != n {
				same = false
			}
		}
	}
	if same {
		return st, false
	}
	st.Updated = now.UTC()
	s.st.saved = st
	return st, true
}

// version is the ProtoStrap version agents should run: the target release's.
func (s *Scheduler) version() string {
	if s.st.target != nil {
		return s.st.target.rel.Version
	}
	return s.cfg.Version
}
