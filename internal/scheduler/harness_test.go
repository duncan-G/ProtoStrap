package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/spec"
)

// The harness runs a whole cluster in memory, tick by tick: the scheduler, Auto Scaling
// groups that launch VMs after a boot time, simulated agents that sync every 5 seconds
// and run what they are assigned, SQS queues and the deploy bucket. Each step is one agent
// sync interval; the scheduler cycles every other step.

const (
	testAccount  = "123456789012"
	testRegion   = "us-east-1"
	testRegistry = "123456789012.dkr.ecr.us-east-1.amazonaws.com"
	step         = 5 * time.Second
)

var epoch = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// testSpec is the sample's shape, small enough to reason about: a tunnel and a gateway,
// telemetry, two stateful components, a migration task, two services and a worker.
const testSpec = `app: shop
protostrap: 1.4.0
pools:
  ingress: { vm: c8g.medium }
  general:
    vm: [m7g.xlarge]
    min: 1
    max: 6
    spare: 1
    target: 70%
    reserved: { cpu: 400m, memory: 1Gi }
  compute:
    vm: [c7g.xlarge]
    min: 0
    max: 6
    spare: 0
    target: 85%
    reserved: { cpu: 400m, memory: 1Gi }
  data: { vm: m7g.xlarge, count: 1 }
tunnel: {}
gateway:
  request: { cpu: 500m, memory: 256Mi }
  upstreams:
    api:
      routes: [/api/]
    web:
      routes: [/web/]
telemetry: {}
queues:
  jobs: {}
buckets:
  reports: { expire: 30d }
components:
  api:
    kind: service
    build: api
    pool: general
    request: { cpu: 500m, memory: 512Mi }
    port: 8080
    ready: grpc
    metrics: 9090/metrics
    depends_on: [postgres, redis]
    uses: [jobs, files, reports]
    migrate: migrate
    env: { MODE: prod }
    stop: 30s
    scale:
      min: 2
      max: 10
      demand: [{ cpu: 70% }]
  web:
    kind: service
    build: web
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    port: 3000
    ready: /healthz
    depends_on: [api]
    scale:
      min: 1
      max: 8
      demand: [{ requests: 20/s }]
  worker:
    kind: worker
    build: worker
    pool: compute
    request: { cpu: 1000m, memory: 1Gi }
    depends_on: [postgres]
    stop: 60s
    scale:
      min: 0
      max: 12
      demand: [{ queue: jobs, rate: 2/s, drain: 60s }]
      limits: [{ metric: jobs_budget_cap }]
  migrate:
    kind: task
    build: migrate
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    depends_on: [postgres]
  postgres:
    kind: stateful
    image: postgres:17.2
    vm: data-1
    request: { cpu: 2000m, memory: 8Gi }
    port: 5432
    volume: { path: /var/lib/postgresql/data, size: 100Gi }
    backup: [pg_dumpall, -U, postgres]
  redis:
    kind: stateful
    image: redis:7.4
    vm: data-1
    request: { cpu: 1000m, memory: 4Gi }
    port: 6379
    volume: { path: /data, size: 20Gi }
`

type harness struct {
	t      *testing.T
	cfg    Config
	s      *Scheduler
	clock  *fakeClock
	store  *fakeStore
	queues *fakeQueues
	groups *fakeGroups
	swarm  *fakeSwarm
	secret *fakeSecrets
	agents map[string]*simAgent
	steps  int
	ids    map[string]int
	log    *logBuffer

	// The fields below set what the simulated replicas do, by component.
	readyAfter  map[string]int   // It is how many syncs a replica takes to get ready, 2 by default.
	neverReady  map[string]bool  // The replicas never get ready: the agent fails them after 5 minutes.
	failOnStart map[string]bool  // The replicas exit before they are ready.
	taskFails   map[string]bool  // The tasks exit 1.
	totalCPU    map[string]int64 // It is the total millicores, spread across ready replicas.
	metrics     map[string]map[string]float64
	edgeCounts  map[string]float64 // It is the requests the edge has counted so far, per component.
	edgeRate    map[string]float64 // It is the requests per second added to edgeCounts each step.
	secretKey   []byte             // It is the scheduler's key for hashing keys.
	badSecret   map[string]string  // A replica of the component started with this version of its keys fails.
	hookFails   map[string]bool    // A follower can't apply a version, keyed by component@version.

	configHookFails map[string]bool            // A follower can't take new config files, keyed by component@digest.
	failingImages   map[string]bool            // A task run in this image exits 1, keyed by component:tag.
	taskSteps       map[string]int             // It is how many syncs a task takes to finish, 2 by default.
	taskRuns        map[string][]taskRunRecord // They are each task's runs, in the order they started.
}

// taskRunRecord is one run of a task, as the simulated agent started it.
type taskRunRecord struct {
	spec  api.ReplicaSpec
	image string
}

type option func(*harness)

func withConfig(f func(*Config)) option { return func(h *harness) { f(&h.cfg) } }

// newHarness starts a cluster with the groups the spec's pools need: each at its minimum,
// already booted, plus data-1 and an ingress VM per zone.
func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		clock:  &fakeClock{now: epoch},
		store:  &fakeStore{releases: map[string]release.Release{}},
		queues: &fakeQueues{depth: map[string]Depth{}},
		swarm:  &fakeSwarm{nodes: map[string]Node{}},
		agents: map[string]*simAgent{}, ids: map[string]int{}, log: &logBuffer{},
		readyAfter: map[string]int{}, neverReady: map[string]bool{}, failOnStart: map[string]bool{},
		taskFails: map[string]bool{}, totalCPU: map[string]int64{}, metrics: map[string]map[string]float64{},
		edgeCounts: map[string]float64{}, edgeRate: map[string]float64{},
		secret: &fakeSecrets{versions: map[string]map[string]string{}}, badSecret: map[string]string{}, hookFails: map[string]bool{},
		secretKey:       []byte("test digest key"),
		configHookFails: map[string]bool{}, failingImages: map[string]bool{}, taskSteps: map[string]int{},
		taskRuns: map[string][]taskRunRecord{},
	}
	h.cfg = Config{App: "shop", Environment: "dev", Region: testRegion, Account: testAccount,
		Registry: testRegistry, FilesBucket: "protostrap-123456789012-us-east-1-files", Address: "10.20.1.10", Version: "1.4.0"}
	h.groups = newFakeGroups(h)
	for _, o := range opts {
		o(h)
	}
	// It adds data-1, an ingress group and a general group in zone a, each with its VMs
	// booted, and two empty compute groups.
	h.addAgent("i-data1", "data-1", "data", "us-east-1a", "m7g.xlarge")
	h.groups.add(&fakeGroup{Group: Group{Name: "protostrap-ingress-us-east-1a", Pool: "ingress", Zone: "us-east-1a", Min: 1, Max: 2}, vm: "c8g.medium"}, 1)
	h.groups.add(&fakeGroup{Group: Group{Name: "protostrap-general-us-east-1a", Pool: "general", Zone: "us-east-1a", Min: 1, Max: 7}, vm: "m7g.xlarge"}, 1)
	h.groups.add(&fakeGroup{Group: Group{Name: "protostrap-compute-spot-us-east-1a", Pool: "compute", Zone: "us-east-1a", Spot: true, Max: 7}, vm: "c7g.xlarge"}, 0)
	h.groups.add(&fakeGroup{Group: Group{Name: "protostrap-compute-ondemand-us-east-1a", Pool: "compute", Zone: "us-east-1a", Max: 7}, vm: "c7g.xlarge"}, 0)
	h.start()
	h.run(20 * time.Second) // This runs past the scheduler's warm-up.
	return h
}

// start makes a scheduler over the cluster, as systemd would after data-1 boots or the
// scheduler restarts. The cluster, its agents and the bucket carry on.
func (h *harness) start() {
	h.t.Helper()
	var secrets Secrets
	if h.secret != nil {
		secrets = h.secret
	}
	s, err := New(h.cfg, Deps{
		Store: h.store, Queues: h.queues, Groups: h.groups, Swarm: h.swarm, Secrets: secrets,
		SecretKey: h.secretKey, Clock: h.clock,
		Logger: slog.New(slog.NewJSONHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		NewID: func(component string) string {
			h.ids[component]++
			return fmt.Sprintf("%s-%d", component, h.ids[component])
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.s = s
}

// publish writes a release to the deploy bucket. Every component's image is tagged v1
// unless tags says otherwise, so a component keeps its fingerprint until a test changes it.
func (h *harness) publish(commit, specText string, tags map[string]string) {
	h.t.Helper()
	h.publishWith(commit, specText, tags, nil)
}

// publishWith is publish with config files: config holds each component's, as the content
// of one file, conf.
func (h *harness) publishWith(commit, specText string, tags, config map[string]string) {
	h.t.Helper()
	s, err := spec.Parse([]byte(specText))
	if err != nil {
		h.t.Fatal(err)
	}
	if problems := s.Check(); len(problems) > 0 {
		h.t.Fatalf("test spec: %v", spec.Problems(problems))
	}
	r := release.Release{Commit: commit, App: s.App, Version: s.ProtoStrap, Spec: specText,
		Components: map[string]release.Component{}, Created: h.clock.Now()}
	for name, c := range s.Components {
		tag := "v1"
		if t, ok := tags[name]; ok {
			tag = t
		}
		img := testRegistry + "/protostrap/" + name + ":" + tag
		digest := ""
		if c.Config != nil {
			files := configfiles.Files{"conf": []byte(config[name])}
			digest = configfiles.Digest(files)
			if r.Config == nil {
				r.Config = map[string]configfiles.Files{}
			}
			r.Config[digest] = files
		}
		r.Components[name] = release.Component{Image: img, Fingerprint: c.Fingerprint(img, digest), Config: digest}
	}
	if s.Gateway != nil {
		edgeImg := testRegistry + "/protostrap/edge:" + s.ProtoStrap
		r.Edge = release.Component{Image: edgeImg, Fingerprint: release.EdgeFingerprint(edgeImg, s.Gateway.Request, s.Tunnel != nil)}
	}
	if s.Telemetry != nil {
		telImg := testRegistry + "/protostrap/telemetry:" + s.ProtoStrap
		r.Telemetry = release.Component{Image: telImg, Fingerprint: release.TelemetryFingerprint(telImg, *s.Telemetry)}
	}
	h.store.mu.Lock()
	h.store.releases[commit] = r
	h.store.mu.Unlock()
}

// deploy publishes a release and makes it the target, as `protostrap deploy` does.
func (h *harness) deploy(commit, specText string, tags map[string]string) {
	h.t.Helper()
	h.deployWith(commit, specText, tags, nil)
}

// deployWith is deploy with config files, as publishWith has them.
func (h *harness) deployWith(commit, specText string, tags, config map[string]string) {
	h.t.Helper()
	h.publishWith(commit, specText, tags, config)
	if err := h.s.Deploy(context.Background(), commit); err != nil {
		h.t.Fatalf("deploy %s: %v", commit, err)
	}
}

// step advances the clock by one sync interval: every agent syncs, the groups boot VMs,
// and every other step the scheduler runs a cycle.
func (h *harness) step() {
	h.t.Helper()
	h.clock.advance(step)
	h.steps++
	for c, rate := range h.edgeRate {
		h.edgeCounts[c] += rate * step.Seconds()
	}
	h.groups.tick()
	if h.steps%2 == 0 {
		if err := h.s.Cycle(context.Background()); err != nil {
			h.t.Fatalf("cycle: %v", err)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(h.agents)) {
		if a, ok := h.agents[id]; ok {
			a.sync()
		}
	}
}

func (h *harness) run(d time.Duration) {
	h.t.Helper()
	for range int(d / step) {
		h.step()
	}
}

// until steps until cond holds, failing after max.
func (h *harness) until(what string, max time.Duration, cond func() bool) {
	h.t.Helper()
	for i := 0; i < int(max/step); i++ {
		if cond() {
			return
		}
		h.step()
	}
	if !cond() {
		h.t.Fatalf("after %v: %s never happened\n%s", max, what, h.describe())
	}
}

// settle deploys the test spec into the empty cluster and waits for it to finish.
func (h *harness) settle() {
	h.t.Helper()
	h.deploy("c1", testSpec, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
}

func (h *harness) rollout(commit string) string {
	st := h.s.Status()
	for _, r := range st.Rollouts {
		if r.Commit == commit {
			return r.State
		}
	}
	return ""
}

func (h *harness) rolloutReason(commit string) string {
	for _, r := range h.s.Status().Rollouts {
		if r.Commit == commit {
			return r.Reason
		}
	}
	return ""
}

// replicas lists the scheduler's replicas of a component that aren't being stopped.
func (h *harness) replicas(component string) []ReplicaStatus {
	var out []ReplicaStatus
	for _, r := range h.s.Status().Replicas {
		if r.Component == component && !r.Stopping {
			out = append(out, r)
		}
	}
	return out
}

func (h *harness) count(component, state string) int {
	n := 0
	for _, r := range h.replicas(component) {
		if r.State == state {
			n++
		}
	}
	return n
}

func (h *harness) ready(component string) int { return h.count(component, api.StateReady) }

// running is what the agents actually run of a component, in any state.
func (h *harness) running(component string) []string {
	var ids []string
	for _, a := range h.agents {
		for id, r := range a.replicas {
			if r.spec.Component == component {
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids
}

// runningReady is how many of a component's replicas agents run and report ready.
func (h *harness) runningReady(component string) int {
	n := 0
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == component && r.state == api.StateReady {
				n++
			}
		}
	}
	return n
}

func (h *harness) vms(pool string) []*simAgent {
	var out []*simAgent
	for _, id := range slices.Sorted(maps.Keys(h.agents)) {
		if a := h.agents[id]; a.pool == pool {
			out = append(out, a)
		}
	}
	return out
}

func (h *harness) agentOf(replica string) *simAgent {
	for _, a := range h.agents {
		if _, ok := a.replicas[replica]; ok {
			return a
		}
	}
	return nil
}

func (h *harness) describe() string {
	var b strings.Builder
	st := h.s.Status()
	fmt.Fprintf(&b, "target %s, last good %s\n", st.Target, st.LastGood)
	for _, r := range st.Rollouts {
		fmt.Fprintf(&b, "rollout %s: %s %s\n", r.Commit, r.State, r.Reason)
	}
	for _, p := range st.Pools {
		fmt.Fprintf(&b, "pool %s: %d VMs, %d booting, wants %d\n", p.Name, p.VMs, p.Booting, p.Wanted)
	}
	for _, c := range st.Components {
		fmt.Fprintf(&b, "%s: desired %d, ready %d, starting %d, pending %d\n", c.Name, c.Desired, c.Ready, c.Starting, c.Pending)
	}
	for _, id := range slices.Sorted(maps.Keys(h.agents)) {
		a := h.agents[id]
		fmt.Fprintf(&b, "vm %s (%s %s) silent=%v:", id, a.pool, a.name, a.silent)
		for _, rid := range slices.Sorted(maps.Keys(a.replicas)) {
			fmt.Fprintf(&b, " %s[%s]", rid, a.replicas[rid].state)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// The fake clock below moves only when a test advances it.

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// The fake deploy bucket below keeps releases and state.json in memory.

type fakeStore struct {
	mu       sync.Mutex
	releases map[string]release.Release
	state    *release.State
	saves    int

	// hosts is hosts.json, or nil without one; hostsErr fails reading it.
	hosts     map[string]string
	hostsErr  error
	hostsRead int
}

func (s *fakeStore) Release(_ context.Context, commit string) (release.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.releases[commit]
	if !ok {
		return release.Release{}, fmt.Errorf("releases/%s.json: %w", commit, ErrNotFound)
	}
	return r, nil
}

func (s *fakeStore) State(context.Context) (release.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return release.State{}, nil
	}
	return *s.state, nil
}

func (s *fakeStore) SaveState(_ context.Context, st release.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = &st
	s.saves++
	return nil
}

// The fake SQS below reports the queue depths a test sets.

type fakeQueues struct {
	mu    sync.Mutex
	depth map[string]Depth
}

func (q *fakeQueues) Depth(_ context.Context, queue, _ string) (Depth, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.depth[queue], nil
}

func (q *fakeQueues) set(queue string, visible, inFlight int) {
	q.mu.Lock()
	q.depth[queue] = Depth{Visible: visible, InFlight: inFlight}
	q.mu.Unlock()
}

// The fake swarm below keeps its nodes in memory.

type fakeSwarm struct {
	mu      sync.Mutex
	nodes   map[string]Node
	removed []string
	joins   int
}

func (s *fakeSwarm) Join(context.Context) (api.Join, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joins++
	return api.Join{SwarmID: "swarm-1", Token: "SWMTKN-1-test", Manager: "10.20.1.10:2377"}, nil
}

func (s *fakeSwarm) Nodes(context.Context) ([]Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Node
	for _, id := range slices.Sorted(maps.Keys(s.nodes)) {
		out = append(out, s.nodes[id])
	}
	return out, nil
}

func (s *fakeSwarm) RemoveNode(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, id)
	s.removed = append(s.removed, id)
	return nil
}

// In the fake Auto Scaling below, a group finishes launching a VM bootSteps steps after its
// desired capacity rises, unless it has no capacity, and the VM's agent starts syncing then.

type fakeGroup struct {
	Group
	vm         string // It is the instance type.
	noCapacity bool
	booting    []bootingVM
}

type bootingVM struct {
	id    string
	ready int // It is the step at which the VM finishes booting.
}

type fakeGroups struct {
	mu        sync.Mutex
	h         *harness
	groups    []*fakeGroup
	next      int
	bootSteps int
	calls     []string
}

func newFakeGroups(h *harness) *fakeGroups { return &fakeGroups{h: h, bootSteps: 12} } // Booting takes a minute.

func (g *fakeGroups) add(fg *fakeGroup, booted int) {
	g.mu.Lock()
	g.groups = append(g.groups, fg)
	g.mu.Unlock()
	for range booted {
		id := g.newID()
		fg.Desired++
		fg.Instances = append(fg.Instances, Instance{ID: id, State: "InService", Launched: g.h.clock.Now()})
		g.h.addAgent(id, "", fg.Pool, fg.Zone, fg.vm)
	}
}

func (g *fakeGroups) newID() string {
	g.next++
	return fmt.Sprintf("i-%s%02d", "vm", g.next)
}

func (g *fakeGroups) group(name string) *fakeGroup {
	for _, fg := range g.groups {
		if fg.Name == name {
			return fg
		}
	}
	return nil
}

func (g *fakeGroups) Groups(context.Context) ([]Group, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Group
	for _, fg := range g.groups {
		c := fg.Group
		c.Instances = slices.Clone(fg.Instances)
		out = append(out, c)
	}
	return out, nil
}

func (g *fakeGroups) SetDesired(_ context.Context, name string, desired int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	fg := g.group(name)
	if fg == nil {
		return fmt.Errorf("no group %s", name)
	}
	if desired > fg.Max || desired < fg.Min {
		return fmt.Errorf("desired %d outside %d..%d", desired, fg.Min, fg.Max)
	}
	g.calls = append(g.calls, fmt.Sprintf("desired %s %d", name, desired))
	fg.Desired = desired
	return nil
}

func (g *fakeGroups) Terminate(_ context.Context, instance string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, fg := range g.groups {
		for i, in := range fg.Instances {
			if in.ID == instance {
				g.calls = append(g.calls, "terminate "+instance)
				fg.Instances = slices.Delete(fg.Instances, i, i+1)
				fg.Desired--
				delete(g.h.agents, instance)
				return nil
			}
		}
	}
	return fmt.Errorf("no instance %s", instance)
}

func (g *fakeGroups) SetUnhealthy(_ context.Context, instance string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, fg := range g.groups {
		for i, in := range fg.Instances {
			if in.ID == instance {
				g.calls = append(g.calls, "unhealthy "+instance)
				// The group replaces it: the old one goes, a new one boots.
				fg.Instances = slices.Delete(fg.Instances, i, i+1)
				delete(g.h.agents, instance)
				return nil
			}
		}
	}
	return fmt.Errorf("no instance %s", instance)
}

func (g *fakeGroups) countCalls(prefix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// tick launches instances up to each group's desired capacity, and finishes booting those
// whose time has come.
func (g *fakeGroups) tick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, fg := range g.groups {
		for len(fg.Instances) < fg.Desired && !fg.noCapacity {
			id := g.newID()
			fg.Instances = append(fg.Instances, Instance{ID: id, State: "Pending:Wait", Launched: g.h.clock.Now()})
			fg.booting = append(fg.booting, bootingVM{id: id, ready: g.h.steps + g.bootSteps})
		}
		var still []bootingVM
		for _, b := range fg.booting {
			if b.ready > g.h.steps {
				still = append(still, b)
				continue
			}
			for i := range fg.Instances {
				if fg.Instances[i].ID == b.id {
					fg.Instances[i].State = "InService"
					g.h.addAgent(b.id, "", fg.Pool, fg.Zone, fg.vm)
				}
			}
		}
		fg.booting = still
	}
}

// The simulated agents below run what they are assigned.

type simAgent struct {
	h          *harness
	id, name   string
	pool, zone string
	ip         string
	capacity   api.Resources
	version    string
	replicas   map[string]*simReplica
	assignment api.Assignment
	silent     bool
	term       *api.Termination
	syncErrs   int
	lastErr    error
}

type simReplica struct {
	spec    api.ReplicaSpec
	state   string
	age     int
	gone    bool   // The replica is reported stopping once, then removed.
	applied string // It is the version of its keys a follower has applied.
	failure *api.SecretFailure

	configApplied string // It is the digest of the config files the replica has.
	configFailure *api.ConfigFailure
}

func (h *harness) addAgent(id, name, pool, zone, vmType string) *simAgent {
	t, err := spec.LookupVM(vmType)
	if err != nil {
		h.t.Fatal(err)
	}
	if name == "" {
		name = id
	}
	a := &simAgent{h: h, id: id, name: name, pool: pool, zone: zone, ip: fmt.Sprintf("10.20.%d.%d", 16+len(h.agents)/200, 10+len(h.agents)%200),
		capacity: api.Resources{CPU: t.CPU, Memory: t.Memory}, version: "1.4.0", replicas: map[string]*simReplica{}}
	if pool == "data" {
		a.ip = "10.20.1.10"
	}
	h.agents[id] = a
	h.swarm.mu.Lock()
	h.swarm.nodes["node-"+id] = Node{ID: "node-" + id, Addr: a.ip, State: "ready"}
	h.swarm.mu.Unlock()
	return a
}

// sync reports the VM's replicas, then converges on the assignment it gets back, as the
// real agent does. Replicas move on one state per sync.
func (a *simAgent) sync() {
	if a.silent {
		return
	}
	h := a.h
	// Advance what's running.
	for id, r := range a.replicas {
		if r.gone {
			delete(a.replicas, id)
			continue
		}
		r.age++
		c := r.spec.Component
		switch {
		case r.state == api.StateStopping:
			r.gone = true
		case r.spec.Kind == api.KindTask && r.state == api.StateStarting && r.age >= h.taskStepsOf(c):
			if h.taskFails[c] || h.failingImages[c+":"+r.spec.Image[strings.LastIndex(r.spec.Image, ":")+1:]] {
				r.state = api.StateFailed
			} else {
				r.state = api.StateSucceeded
			}
		case r.spec.Kind == api.KindTask:
			// A task runs until it exits: it is never ready.
		case r.state == api.StateStarting && h.failOnStart[c]:
			r.state = api.StateFailed
		case r.state == api.StateStarting && !r.spec.SecretFollows && r.spec.SecretVersion != "" && h.badSecret[c] == r.spec.SecretVersion:
			r.state, r.failure = api.StateFailed, nil
		case r.state == api.StateStarting && h.neverReady[c] && time.Duration(r.age)*step > 5*time.Minute:
			r.state = api.StateFailed
		case r.state == api.StateStarting && !h.neverReady[c] && r.age >= h.readyAfterOf(c):
			r.state = api.StateReady
		}
	}

	report := api.Report{VM: a.id, Name: a.name, Pool: a.pool, Zone: a.zone, Version: a.version, Capacity: a.capacity, Terminating: a.term}
	readyCounts := map[string]int{}
	for _, r := range a.replicas {
		if r.state == api.StateReady {
			readyCounts[r.spec.Component]++
		}
	}
	for _, id := range slices.Sorted(maps.Keys(a.replicas)) {
		r := a.replicas[id]
		rr := api.ReplicaReport{ID: id, Component: r.spec.Component, Release: r.spec.Release, Kind: r.spec.Kind,
			Fingerprint: r.spec.Fingerprint, State: r.state, SecretDigest: r.spec.SecretDigest, SecretVersion: r.spec.SecretVersion}
		if r.spec.SecretFollows {
			rr.SecretVersion, rr.SecretFailure = r.applied, r.failure
		}
		if r.spec.ConfigPath != "" {
			rr.ConfigDigest, rr.ConfigFailure = r.configApplied, r.configFailure
		}
		if r.state == api.StateReady {
			cpu := h.cpuOf(r.spec.Component)
			mem := r.spec.Request.Memory / 2
			rr.CPU, rr.Memory = &cpu, &mem
			rr.Metrics = h.metricsOf(r.spec)
		}
		report.Replicas = append(report.Replicas, rr)
	}
	as, err := h.s.Sync(context.Background(), VMIdentity{InstanceID: a.id, PrivateIP: a.ip}, report)
	if err != nil {
		a.syncErrs++
		a.lastErr = err
		return // A failed sync leaves the agent frozen: it starts and stops nothing.
	}
	a.lastErr = nil
	if err := validateAssignment(as); err != nil {
		h.t.Fatalf("vm %s got an invalid assignment: %v", a.id, err)
	}
	a.assignment = as
	// A ready follower applies the version its assignment names, unless its hook fails.
	for _, r := range a.replicas {
		v := as.SecretVersions[r.spec.Component]
		if !r.spec.SecretFollows || r.state != api.StateReady || v == "" || v == r.applied {
			continue
		}
		if h.hookFails[r.spec.Component+"@"+v] {
			r.failure = &api.SecretFailure{Version: v, Reason: "the hook exited with 1"}
			continue
		}
		r.applied, r.failure = v, nil
	}
	// A ready follower takes the config files its assignment names, which it fetches from
	// the scheduler, unless its hook fails.
	for id, r := range a.replicas {
		d := as.ConfigDigests[id]
		if !r.spec.ConfigFollows || r.state != api.StateReady || d == "" || d == r.configApplied {
			continue
		}
		if _, ok := h.s.ConfigFiles(d); !ok {
			h.t.Fatalf("vm %s can't fetch config %s for %s", a.id, d, id)
		}
		if h.configHookFails[r.spec.Component+"@"+d] {
			r.configFailure = &api.ConfigFailure{Digest: d, Reason: "the hook exited with 1"}
			continue
		}
		r.configApplied, r.configFailure = d, nil
	}
	want := map[string]api.ReplicaSpec{}
	for _, s := range as.Replicas {
		want[s.ID] = s
	}
	for id, r := range a.replicas {
		if _, ok := want[id]; !ok && r.state != api.StateStopping {
			r.state = api.StateStopping
		}
	}
	for id, s := range want {
		if _, ok := a.replicas[id]; !ok {
			if a.term != nil {
				continue
			}
			// A stateful or telemetry replica waits for the old one to release the volume.
			if (s.Kind == api.KindStateful || s.Kind == api.KindTelemetry) && a.hasOther(s.Component, id) {
				continue
			}
			r := &simReplica{spec: s, state: api.StateStarting, configApplied: s.ConfigDigest}
			if d := as.ConfigDigests[id]; s.ConfigFollows && d != "" {
				r.configApplied = d
			}
			if s.ConfigPath != "" {
				if _, ok := h.s.ConfigFiles(r.configApplied); !ok {
					h.t.Fatalf("vm %s can't fetch config %s for %s", a.id, r.configApplied, id)
				}
			}
			if s.Kind == api.KindTask {
				h.taskRuns[s.Component] = append(h.taskRuns[s.Component], taskRunRecord{spec: s, image: s.Image})
			}
			a.replicas[id] = r
		}
	}
}

func (a *simAgent) hasOther(component, id string) bool {
	for rid, r := range a.replicas {
		if rid != id && r.spec.Component == component {
			return true
		}
	}
	return false
}

func validateAssignment(as api.Assignment) error {
	seen := map[string]bool{}
	for _, s := range as.Replicas {
		if err := s.Validate(); err != nil {
			return err
		}
		if seen[s.ID] {
			return fmt.Errorf("replica %s twice", s.ID)
		}
		seen[s.ID] = true
	}
	return nil
}

func (h *harness) taskStepsOf(c string) int {
	if n, ok := h.taskSteps[c]; ok {
		return n
	}
	return 2
}

func (h *harness) readyAfterOf(c string) int {
	if n, ok := h.readyAfter[c]; ok {
		return n
	}
	return 2
}

// cpuOf is one ready replica's CPU: the component's load spread across its ready
// replicas, as a balanced service's would be.
func (h *harness) cpuOf(component string) int64 {
	total := h.totalCPU[component]
	n := int64(h.runningReady(component))
	if n == 0 {
		return 0
	}
	return total / n
}

func (h *harness) metricsOf(s api.ReplicaSpec) map[string]float64 {
	if s.Metrics == nil {
		return nil
	}
	out := map[string]float64{}
	for _, name := range s.Metrics.Names {
		if s.Kind == api.KindEdge {
			for c, v := range h.edgeCounts {
				if name == EdgeRequestsMetric(c) {
					// The count is split across edges, as each counts its own requests.
					out[name] = v / float64(max(1, len(h.vms("ingress"))))
				}
			}
			continue
		}
		if v, ok := h.metrics[s.Component][name]; ok {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// The fake secret below is the environment's secret: versions of its keys, one current.

type fakeSecrets struct {
	mu        sync.Mutex
	versions  map[string]map[string]string
	current   string
	err       error
	describes int
	reads     int
	ids       []string // They are the secret IDs asked for.
}

func (f *fakeSecrets) CurrentVersion(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describes++
	f.ids = append(f.ids, id)
	return f.current, f.err
}

func (f *fakeSecrets) Read(_ context.Context, id, version string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	f.ids = append(f.ids, id)
	if f.err != nil {
		return "", f.err
	}
	keys, ok := f.versions[version]
	if !ok {
		return "", fmt.Errorf("no version %s", version)
	}
	b, _ := json.Marshal(keys)
	return string(b), nil
}

// writeSecret writes a new version of the secret, the current one with changes made, as
// scripts/set-secret.sh does, and returns its ID.
func (h *harness) writeSecret(changes map[string]string) string {
	f := h.secret
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := maps.Clone(f.versions[f.current])
	if keys == nil {
		keys = map[string]string{}
	}
	maps.Copy(keys, changes)
	id := fmt.Sprintf("v-%d", len(f.versions)+1)
	f.versions[id] = keys
	f.current = id
	return id
}

func (f *fakeSecrets) counts() (describes, reads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.describes, f.reads
}

// The log buffer below collects the scheduler's log for tests to search.

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

var _ io.Writer = (*logBuffer)(nil)

func isNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
