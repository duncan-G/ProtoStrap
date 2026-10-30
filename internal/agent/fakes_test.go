package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/credentials"
	"github.com/duncan-G/protostrap/internal/probe"
)

// The fake Runtime below is an in-memory Docker.

type stopCall struct {
	ID      string
	Name    string
	Timeout time.Duration
}

type fakeRuntime struct {
	mu    sync.Mutex
	clock *fakeClock
	info  container.Info

	containers map[string]*container.Container // It is keyed by ID.
	specs      map[string]container.Spec
	images     map[string]bool
	stats      map[string]container.Stats // It is keyed by name, for convenience.
	nextIP     int

	pingErr   error
	infoErr   error
	listErr   error
	pullErr   map[string]error
	createErr error
	startErr  error
	joinErrs  []error

	pullGate chan struct{} // When set, Pull waits for it or for its context to end.
	stopGate chan struct{} // When set, Stop waits for it.
	onStart  func(name string)
	exec     func(ctx context.Context, name string, cmd []string, stdout, stderr io.Writer) (int, error)

	pulls   []string
	creates []container.Spec
	starts  []string // It holds container names.
	stops   []stopCall
	removes []string // It holds container names.
	joins   []container.Join
	leaves  int
	execs   []string
	order   []string // It holds every call, in order.
}

func newFakeRuntime(clock *fakeClock) *fakeRuntime {
	return &fakeRuntime{
		clock: clock,
		info: container.Info{CPUs: 4, MemoryBytes: 16 << 30,
			Swarm: container.Swarm{State: "inactive"}},
		containers: map[string]*container.Container{},
		specs:      map[string]container.Spec{},
		images:     map[string]bool{},
		stats:      map[string]container.Stats{},
		pullErr:    map[string]error{},
	}
}

func (r *fakeRuntime) record(call string) { r.order = append(r.order, call) }

func (r *fakeRuntime) Ping(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("ping")
	return r.pingErr
}

func (r *fakeRuntime) Info(context.Context) (container.Info, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.info, r.infoErr
}

func (r *fakeRuntime) Containers(context.Context) ([]container.Container, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []container.Container
	for _, c := range r.containers {
		if c.Labels[container.LabelReplica] == "" {
			continue // Docker filters on the label.
		}
		cp := *c
		cp.Labels = maps.Clone(c.Labels)
		cp.Env = maps.Clone(c.Env)
		cp.Addrs = slices.Clone(c.Addrs)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *fakeRuntime) Stats(_ context.Context, id string) (container.Stats, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.containers[id]
	if !ok {
		return container.Stats{}, errors.New("no such container")
	}
	s, ok := r.stats[c.Name]
	if !ok {
		return container.Stats{}, errors.New("no stats")
	}
	return s, nil
}

func (r *fakeRuntime) HasImage(_ context.Context, image string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.images[image], nil
}

func (r *fakeRuntime) Pull(ctx context.Context, image string) error {
	r.mu.Lock()
	r.pulls = append(r.pulls, image)
	r.record("pull " + image)
	gate := r.pullGate
	err := r.pullErr[image]
	r.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.images[image] = true
	r.mu.Unlock()
	return nil
}

func (r *fakeRuntime) Create(_ context.Context, spec container.Spec) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, spec)
	r.record("create " + spec.Name)
	if r.createErr != nil {
		return "", r.createErr
	}
	for _, c := range r.containers {
		if c.Name == spec.Name {
			return "", fmt.Errorf("conflict: name %s in use", spec.Name)
		}
	}
	env := map[string]string{}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	id := "c-" + spec.Name
	r.containers[id] = &container.Container{
		ID: id, Name: spec.Name, Image: spec.Image, Labels: maps.Clone(spec.Labels), Env: env,
		Status: container.Created, Created: r.clock.Now(), CPUSet: spec.CPUSet,
	}
	r.specs[id] = spec
	return id, nil
}

// addContainer puts a container in place as if an earlier agent had started it.
func (r *fakeRuntime) addContainer(c container.Container) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c.ID == "" {
		c.ID = "c-" + c.Name
	}
	if c.Created.IsZero() {
		c.Created = r.clock.Now()
	}
	if len(c.Addrs) == 0 && c.Status.Up() {
		r.nextIP++
		c.Addrs = []string{fmt.Sprintf("172.18.0.%d", 100+r.nextIP)}
	}
	r.containers[c.ID] = &c
}

func (r *fakeRuntime) Start(_ context.Context, id string) error {
	r.mu.Lock()
	c, ok := r.containers[id]
	if !ok {
		r.mu.Unlock()
		return errors.New("no such container")
	}
	r.starts = append(r.starts, c.Name)
	r.record("start " + c.Name)
	if r.startErr != nil {
		r.mu.Unlock()
		return r.startErr
	}
	c.Status = container.Running
	if len(c.Addrs) == 0 {
		r.nextIP++
		c.Addrs = []string{fmt.Sprintf("172.18.0.%d", r.nextIP)}
	}
	hook := r.onStart
	r.mu.Unlock()
	if hook != nil {
		hook(c.Name)
	}
	return nil
}

func (r *fakeRuntime) Stop(ctx context.Context, id string, timeout time.Duration) error {
	r.mu.Lock()
	c, ok := r.containers[id]
	if !ok {
		r.mu.Unlock()
		return nil
	}
	r.stops = append(r.stops, stopCall{ID: id, Name: c.Name, Timeout: timeout})
	r.record("stop " + c.Name)
	gate := r.stopGate
	r.mu.Unlock()
	if gate != nil {
		<-gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.containers[id]; ok && c.Status != container.Exited {
		c.Status = container.Exited
		c.ExitCode = 143
	}
	return nil
}

func (r *fakeRuntime) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.containers[id]; ok {
		r.removes = append(r.removes, c.Name)
		r.record("remove " + c.Name)
		delete(r.containers, id)
	}
	return nil
}

func (r *fakeRuntime) Exec(ctx context.Context, id string, cmd []string, stdout, stderr io.Writer) (int, error) {
	r.mu.Lock()
	c, ok := r.containers[id]
	name := ""
	if ok {
		name = c.Name
	}
	r.execs = append(r.execs, name+": "+strings.Join(cmd, " "))
	fn := r.exec
	r.mu.Unlock()
	if !ok {
		return -1, errors.New("no such container")
	}
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, name, cmd, stdout, stderr)
}

func (r *fakeRuntime) JoinSwarm(_ context.Context, j container.Join) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.joins = append(r.joins, j)
	r.record("join-swarm")
	if len(r.joinErrs) > 0 {
		err := r.joinErrs[0]
		r.joinErrs = r.joinErrs[1:]
		if err != nil {
			return err
		}
	}
	r.info.Swarm = container.Swarm{State: "active", Managers: slices.Clone(j.Managers)}
	return nil
}

func (r *fakeRuntime) LeaveSwarm(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leaves++
	r.record("leave-swarm")
	r.info.Swarm = container.Swarm{State: "inactive"}
	return nil
}

// The helpers below are for assertions, and each one takes the lock.

func (r *fakeRuntime) byName(name string) (container.Container, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.containers {
		if c.Name == name {
			return *c, true
		}
	}
	return container.Container{}, false
}

func (r *fakeRuntime) specOf(name string) container.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.creates {
		if s.Name == name {
			return s
		}
	}
	return container.Spec{}
}

func (r *fakeRuntime) set(name string, f func(c *container.Container)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.containers {
		if c.Name == name {
			f(c)
			return
		}
	}
	panic("no container " + name)
}

func (r *fakeRuntime) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.containers {
		out = append(out, c.Name)
	}
	sort.Strings(out)
	return out
}

func (r *fakeRuntime) snapshot() (creates, starts, removes []string, stops []stopCall, pulls []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.creates {
		creates = append(creates, s.Name)
	}
	return creates, slices.Clone(r.starts), slices.Clone(r.removes), slices.Clone(r.stops), slices.Clone(r.pulls)
}

func (r *fakeRuntime) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

func (r *fakeRuntime) resetCalls() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates, r.starts, r.stops, r.removes, r.pulls, r.order, r.execs = nil, nil, nil, nil, nil, nil, nil
}

// The fake Scheduler below records reports and returns a set assignment.

type fakeScheduler struct {
	mu         sync.Mutex
	assignment api.Assignment
	syncErr    error
	joinErrs   []error
	join       api.Join
	reports    []api.Report
	joinCalls  int
	onSync     func(api.Report)

	configs       map[string]configfiles.Files // They are the config files the scheduler serves, by digest.
	configErr     error
	configFetches []string
}

func (s *fakeScheduler) Config(_ context.Context, digest string) (configfiles.Files, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configFetches = append(s.configFetches, digest)
	if s.configErr != nil {
		return nil, s.configErr
	}
	files, ok := s.configs[digest]
	if !ok {
		return nil, fmt.Errorf("scheduler /v1/config/%s: HTTP 404: no such config", digest)
	}
	return files, nil
}

func (s *fakeScheduler) Join(context.Context) (api.Join, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.joinCalls++
	if len(s.joinErrs) > 0 {
		err := s.joinErrs[0]
		s.joinErrs = s.joinErrs[1:]
		return api.Join{}, err
	}
	return s.join, nil
}

func (s *fakeScheduler) Sync(_ context.Context, r api.Report) (api.Assignment, error) {
	s.mu.Lock()
	s.reports = append(s.reports, r)
	err, a, hook := s.syncErr, s.assignment, s.onSync
	s.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	if err != nil {
		return api.Assignment{}, err
	}
	return a, nil
}

func (s *fakeScheduler) assign(specs ...api.ReplicaSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.assignment.Replicas = specs
}

func (s *fakeScheduler) lastReport(t *testing.T) api.Report {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.reports) == 0 {
		t.Fatal("no report was sent")
	}
	return s.reports[len(s.reports)-1]
}

func (s *fakeScheduler) syncs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reports)
}

// The fakes below stand in for the Auto Scaling hooks, metadata, firewall and zone.

type fakeHooks struct {
	mu         sync.Mutex
	completes  []string // Each is a hook and its result, such as "launch CONTINUE".
	heartbeats int
	err        error
	order      *[]string
}

func (h *fakeHooks) Complete(_ context.Context, hook Hook, result string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completes = append(h.completes, string(hook)+" "+result)
	if h.order != nil {
		*h.order = append(*h.order, "hook "+string(hook)+" "+result)
	}
	return h.err
}

func (h *fakeHooks) Heartbeat(_ context.Context, hook Hook) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hook == TerminateHook {
		h.heartbeats++
	}
	return h.err
}

type fakeMetadata struct {
	mu     sync.Mutex
	notice *Notice
	err    error
}

func (m *fakeMetadata) Termination(context.Context) (*Notice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notice, m.err
}

func (m *fakeMetadata) set(n *Notice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notice = n
}

type fakeFirewall struct {
	mu       sync.Mutex
	calls    int
	repaired []string
	err      error
	order    *[]string
}

func (f *fakeFirewall) Ensure(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.order != nil {
		*f.order = append(*f.order, "firewall")
	}
	r := f.repaired
	f.repaired = nil
	return r, f.err
}

type fakeZone struct {
	calls int
	err   error
	order *[]string
}

func (z *fakeZone) TakeOver(context.Context) error {
	z.calls++
	if z.order != nil {
		*z.order = append(*z.order, "takeover")
	}
	return z.err
}

// The fakes below stand in for credentials, readiness checks, metrics and volumes.

type fakeCredentials struct {
	mu      sync.Mutex
	entries map[string]credentials.Entry
}

func (c *fakeCredentials) Put(e credentials.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[e.Replica] = e
}

func (c *fakeCredentials) Remove(replica string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, replica)
}

func (c *fakeCredentials) Replicas() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Sorted(maps.Keys(c.entries))
}

func (c *fakeCredentials) get(replica string) (credentials.Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[replica]
	return e, ok
}

// fakeProber answers ready for services listed in ready.
type fakeProber struct {
	mu     sync.Mutex
	ready  map[string]bool // It is keyed by service name.
	probes []probe.Target
}

func (p *fakeProber) Probe(_ context.Context, t probe.Target) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes = append(p.probes, t)
	if p.ready[t.Service] {
		return nil
	}
	return errors.New("not ready")
}

func (p *fakeProber) setReady(service string, ready bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready[service] = ready
}

func (p *fakeProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.probes)
}

type fakeScraper struct {
	mu     sync.Mutex
	values map[string]map[string]float64 // It is keyed by address.
	err    error
	calls  []string
}

func (s *fakeScraper) Scrape(_ context.Context, addr, endpoint string, names []string) (map[string]float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, addr+" "+endpoint+" "+strings.Join(names, ","))
	if s.err != nil {
		return nil, s.err
	}
	out := map[string]float64{}
	for _, n := range names {
		if v, ok := s.values[addr][n]; ok {
			out[n] = v
		}
	}
	return out, nil
}

type fakeVolumes struct{ mounted map[string]bool }

func (v *fakeVolumes) Mounted(path string) (bool, error) {
	return v.mounted[filepath.Clean(path)], nil
}

// fakeSecrets is the environment's secret: versions of its value, one of them current.
type fakeSecrets struct {
	mu       sync.Mutex
	versions map[string]string // It is keyed by version ID.
	current  string
	err      error
	reads    []string // Each read is recorded as secret|role|component|version.
}

func (s *fakeSecrets) Read(_ context.Context, secretID, version, role, component string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, secretID+"|"+role+"|"+component+"|"+version)
	if s.err != nil {
		return "", "", s.err
	}
	if version == "" {
		version = s.current
	}
	value, ok := s.versions[version]
	if !ok {
		return "", "", fmt.Errorf("no version %s", version)
	}
	return value, version, nil
}

// put adds a version of the secret, as JSON of its keys, and makes it current.
func (s *fakeSecrets) put(version string, keys map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(keys)
	s.versions[version] = string(b)
	s.current = version
}

func (s *fakeSecrets) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reads)
}

// The fakes below stand in for the backups bucket, updater, notifier and clock.

type fakeBackups struct {
	mu        sync.Mutex
	objects   map[string][]byte
	existsErr error
	uploads   []string
	failed    []string
}

func (b *fakeBackups) Exists(_ context.Context, key string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.objects[key]
	return ok, b.existsErr
}

func (b *fakeBackups) Upload(_ context.Context, key string, body io.Reader) error {
	data, err := io.ReadAll(body)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.uploads = append(b.uploads, key)
	if err != nil {
		b.failed = append(b.failed, key)
		return err // The upload is aborted, so nothing is stored.
	}
	b.objects[key] = data
	return nil
}

func (b *fakeBackups) object(key string) ([]byte, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[key]
	return o, ok
}

type fakeUpdater struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (u *fakeUpdater) Update(_ context.Context, v string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, v)
	return u.err
}

type fakeNotifier struct {
	mu     sync.Mutex
	states []string
}

func (n *fakeNotifier) Notify(s string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.states = append(n.states, s)
	return nil
}

func (n *fakeNotifier) count(state string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.states {
		if s == state {
			c++
		}
	}
	return c
}

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func()
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.sleeps = append(c.sleeps, d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ctx.Err()
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// The harness below runs an agent against the fakes.

type harness struct {
	t       *testing.T
	a       *Agent
	cfg     Config
	clock   *fakeClock
	rt      *fakeRuntime
	sched   *fakeScheduler
	hooks   *fakeHooks
	meta    *fakeMetadata
	fw      *fakeFirewall
	creds   *fakeCredentials
	prober  *fakeProber
	scraper *fakeScraper
	volumes *fakeVolumes
	backups *fakeBackups
	secrets *fakeSecrets
	updater *fakeUpdater
	notify  *fakeNotifier
	tokens  int
}

var epoch = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)

// newHarness builds an agent on a general VM. options adjust the config and dependencies
// before the agent is created.
func newHarness(t *testing.T, options ...func(*harness, *Config, *Deps)) *harness {
	t.Helper()
	clock := &fakeClock{now: epoch}
	h := &harness{
		t:       t,
		clock:   clock,
		rt:      newFakeRuntime(clock),
		sched:   &fakeScheduler{join: api.Join{SwarmID: "swarm-1", Token: "SWMTKN-1-abc", Manager: "10.20.1.10:2377"}},
		hooks:   &fakeHooks{},
		meta:    &fakeMetadata{},
		fw:      &fakeFirewall{},
		creds:   &fakeCredentials{entries: map[string]credentials.Entry{}},
		prober:  &fakeProber{ready: map[string]bool{}},
		scraper: &fakeScraper{values: map[string]map[string]float64{}},
		volumes: &fakeVolumes{mounted: map[string]bool{}},
		secrets: &fakeSecrets{versions: map[string]string{}},
		updater: &fakeUpdater{},
		notify:  &fakeNotifier{},
	}
	h.sched.assignment.Version = "1.4.0"
	h.sched.assignment.Secret = "shop/prod"
	dir := t.TempDir()
	cfg := Config{
		App: "shop", InstanceID: "i-0a1b2c3d", Name: "general-3", Pool: "general", Zone: "eu-west-1a",
		Region: "eu-west-1", Version: "1.4.0", PrivateIP: "10.20.16.5", TelemetryAddress: "10.20.1.10:24224",
		DataDir: filepath.Join(dir, "data"), FilesDir: filepath.Join(dir, "files"),
		SecretsDir: filepath.Join(dir, "secrets"), ConfigDir: filepath.Join(dir, "config"),
		TelemetrySocketDir: filepath.Join(dir, "telemetry"),
	}
	deps := Deps{
		Runtime: h.rt, Scheduler: h.sched, Hooks: h.hooks, Metadata: h.meta, Firewall: h.fw,
		Credentials: h.creds, Prober: h.prober, Scraper: h.scraper, Volumes: h.volumes,
		Secrets: h.secrets, Updater: h.updater, Notifier: h.notify, Clock: clock,
		NewToken: func() string { h.tokens++; return fmt.Sprintf("token-%d", h.tokens) },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, o := range options {
		o(h, &cfg, &deps)
	}
	a, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	h.a, h.cfg = a, a.cfg
	return h
}

// dataVM makes the harness a data VM, which has backups and no Auto Scaling hooks.
func dataVM(h *harness, cfg *Config, d *Deps) {
	cfg.Pool, cfg.Name = "data", "data-1"
	cfg.BackupHour = 3
	d.Hooks = nil
	h.hooks = nil
	h.backups = &fakeBackups{objects: map[string][]byte{}}
	d.Backups = h.backups
}

// tick runs one cycle and waits for the starts and stops it began.
func (h *harness) tick() error {
	h.t.Helper()
	err := h.a.Tick(h.t.Context())
	h.a.Wait()
	return err
}

// mustTick runs one cycle that must not fail.
func (h *harness) mustTick() {
	h.t.Helper()
	if err := h.tick(); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

func (h *harness) report() api.Report { return h.sched.lastReport(h.t) }

// replica finds a replica in the last report.
func (h *harness) replica(id string) api.ReplicaReport {
	h.t.Helper()
	for _, r := range h.report().Replicas {
		if r.ID == id {
			return r
		}
	}
	h.t.Fatalf("replica %s not in the report: %+v", id, h.report().Replicas)
	return api.ReplicaReport{}
}

func (h *harness) state(id string) string {
	h.t.Helper()
	return h.replica(id).State
}

func (h *harness) noReplica(id string) {
	h.t.Helper()
	for _, r := range h.report().Replicas {
		if r.ID == id {
			h.t.Fatalf("replica %s should not be reported: %+v", id, r)
		}
	}
}

// The helpers below build replica specs.

const registry = "123456789012.dkr.ecr.eu-west-1.amazonaws.com/protostrap/"

func service(id string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "api", Kind: api.KindService, Release: "9f2c1e7", Fingerprint: "fp-api",
		Image: registry + "api:c-4e1a90b2", Request: api.Resources{CPU: 500, Memory: 512},
		Port: 8080, Ready: "grpc", Stop: 30,
		Env:          map[string]string{"QUEUE_ORDERS_URL": "https://sqs/orders", "LOG_LEVEL": "info"},
		Role:         "arn:aws:iam::123456789012:role/protostrap-shop-api",
		Metrics:      &api.MetricsSpec{Endpoint: "9090/metrics", Names: []string{"llm_budget_cap"}},
		SecretPrefix: "Api_",
	}
}

// postgres is a stateful replica that reads its password from a secret file, and applies a
// new one with a hook rather than be replaced.
func postgres(id string) api.ReplicaSpec {
	s := stateful(id, "postgres", 1000, "/var/lib/postgresql")
	s.SecretPrefix, s.SecretFiles = "Postgres_", []string{"POSTGRES_PASSWORD"}
	s.SecretFollows, s.SecretHook = true, []string{"sh", "-c", "alter-role"}
	return s
}

// keycloak is a stateful replica that reads its secret files when it starts, and is
// replaced when they change.
func keycloak(id, version string) api.ReplicaSpec {
	s := stateful(id, "keycloak", 1000, "/opt/keycloak/data")
	s.Port = 8080
	s.SecretPrefix, s.SecretFiles, s.SecretVersion = "Keycloak_", []string{"KC_DB_PASSWORD", "SMTP_PASSWORD"}, version
	return s
}

func worker(id string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "orders-worker", Kind: api.KindWorker, Release: "9f2c1e7",
		Image: registry + "orders-worker:c-77", Request: api.Resources{CPU: 1000, Memory: 1024},
		Stop: 120, Role: "arn:aws:iam::123456789012:role/protostrap-shop-orders-worker",
	}
}

func stateful(id, component string, cpu int64, volume string, backup ...string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: component, Kind: api.KindStateful, Release: "9f2c1e7",
		Image: registry + component + ":pinned", Request: api.Resources{CPU: cpu, Memory: 4096},
		Port: 5432, Stop: 60, Volume: volume, Backup: backup,
		Role: "arn:aws:iam::123456789012:role/protostrap-shop-" + component,
	}
}

func task(id string, command ...string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "api-migrate", Kind: api.KindTask, Release: "9f2c1e7",
		Image: registry + "api:c-4e1a90b2", Command: command, Request: api.Resources{CPU: 250, Memory: 256},
		Stop: 30, Role: "arn:aws:iam::123456789012:role/protostrap-shop-api",
	}
}

// telemetryReplica is the telemetry replica, as the scheduler assigns it to data-1.
func telemetryReplica(id string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "telemetry", Kind: api.KindTelemetry, Release: "9f2c1e7", Fingerprint: "fp-telemetry",
		Image: registry + "telemetry:1.4.0", Request: api.Resources{CPU: 500, Memory: 1536},
		Port: 13133, Ready: "/ready", Stop: 60, Volume: "/var/lib/telemetry",
		Env:  map[string]string{"TELEMETRY_BUCKET": "protostrap-123456789012-eu-west-1-telemetry", "TELEMETRY_LOGS_DAYS": "30"},
		Role: "arn:aws:iam::123456789012:role/protostrap/protostrap-c-telemetry",
	}
}

func edge(id string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "edge", Kind: api.KindEdge, Release: "9f2c1e7",
		Image: registry + "edge:1.4.0", Request: api.Resources{CPU: 500, Memory: 256},
		Stop: 30, Ready: "/ready", Port: 9901, Role: "arn:aws:iam::123456789012:role/protostrap-shop-edge",
	}
}

// running starts the replicas through the agent and makes their readiness checks pass.
func (h *harness) running(specs ...api.ReplicaSpec) {
	h.t.Helper()
	for _, s := range specs {
		h.prober.setReady(s.Component, true)
	}
	h.sched.assign(specs...)
	h.mustTick() // The first cycle starts them.
	h.mustTick() // The second sees them ready.
	for _, s := range specs {
		if s.Kind == api.KindTask {
			continue
		}
		if got := h.state(s.ID); got != api.StateReady {
			h.t.Fatalf("%s is %s, want ready", s.ID, got)
		}
	}
}

// eventually waits for a background operation to reach a point the test can't wait on
// with Wait, such as a stop held at a gate.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func fileList(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, rel)
		}
		return nil
	})
	return out
}
