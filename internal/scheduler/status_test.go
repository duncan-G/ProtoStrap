package scheduler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These tests check the status, as the operator UI and `protostrap status --json` show it:
// what runs where, and why each component wants the replicas it does.

func component(t *testing.T, h *harness, name string) ComponentStatus {
	t.Helper()
	for _, c := range h.s.Status().Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no component %s in the status", name)
	return ComponentStatus{}
}

func pool(t *testing.T, st Status, name string) PoolStatus {
	t.Helper()
	for _, p := range st.Pools {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no pool %s in the status", name)
	return PoolStatus{}
}

func signal(t *testing.T, c ComponentStatus, prefix string) SignalStatus {
	t.Helper()
	if c.Scale == nil {
		t.Fatalf("%s has no scale in the status", c.Name)
	}
	for _, s := range c.Scale.Signals {
		if strings.HasPrefix(s.Signal, prefix) {
			return s
		}
	}
	t.Fatalf("%s has no %q signal: %+v", c.Name, prefix, c.Scale.Signals)
	return SignalStatus{}
}

func TestStatusNamesTheApplication(t *testing.T) {
	h := newHarness(t)
	st := h.s.Status()
	if st.App != "shop" || st.Environment != "dev" || st.Version != "1.4.0" {
		t.Errorf("app %q, environment %q, version %q", st.App, st.Environment, st.Version)
	}
	if !st.Now.Equal(h.clock.Now()) {
		t.Errorf("now %v, want the scheduler's clock, %v", st.Now, h.clock.Now())
	}
	if !st.Started.Equal(epoch) {
		t.Errorf("started %v, want %v", st.Started, epoch)
	}
}

func TestStatusKeepsItsFieldNames(t *testing.T) {
	// `protostrap deploy` and `status` on an older version decode the same JSON.
	h := newHarness(t)
	h.settle()
	b, err := json.Marshal(h.s.Status())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	for _, key := range []string{"version", "target", "last_good", "rollouts", "pools", "vms", "components", "replicas"} {
		if _, ok := m[key]; !ok {
			t.Errorf("no %q in %s", key, b)
		}
	}
}

func TestStatusShowsTheHoldAfterAStart(t *testing.T) {
	h := newHarness(t)
	if !h.s.Status().Holding {
		t.Error("not holding 20 seconds after a start: nothing should scale down for 5 minutes")
	}
	h.run(5 * time.Minute)
	if h.s.Status().Holding {
		t.Error("still holding 5 minutes after the start")
	}
}

func TestStatusExplainsCPUDemand(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	// With 2000m across api's replicas against 70% of 500m, demand is ceil(2000 / 350) = 6,
	// once the minute's average has caught up with the load.
	h.totalCPU["api"] = 2000
	h.until("api wants 6", 3*time.Minute, func() bool { return desired(h, "api") == 6 })

	c := component(t, h, "api")
	if c.Pool != "general" || c.Request != (api.Resources{CPU: 500, Memory: 512}) {
		t.Errorf("pool %q, request %+v", c.Pool, c.Request)
	}
	if c.Scale == nil || c.Scale.Min != 2 || c.Scale.Max != 10 {
		t.Fatalf("scale %+v, want min 2 and max 10", c.Scale)
	}
	s := signal(t, c, "cpu")
	if s.Signal != "cpu: 70%" {
		t.Errorf("signal %q, want it as the spec writes it", s.Signal)
	}
	if s.Demand == nil || *s.Demand != 6 {
		t.Errorf("demand %v, want 6", s.Demand)
	}
	if !strings.Contains(s.Detail, "/ 350m)") {
		t.Errorf("detail %q, want how the demand is worked out", s.Detail)
	}
	if c.Scale.Reason != s.Detail {
		t.Errorf("reason %q, want the cpu signal's, %q", c.Scale.Reason, s.Detail)
	}

	// Six replicas share the 2000m at 67% each, within 10% of the target, so nothing changes.
	h.until("6 api replicas ready", 3*time.Minute, func() bool { return h.ready("api") == 6 })
	h.run(2 * time.Minute)
	s = signal(t, component(t, h, "api"), "cpu")
	if s.Demand == nil || *s.Demand != 6 || !strings.Contains(s.Detail, "within 10% of its 70% target") {
		t.Errorf("signal %+v at 67%%, want 6 and the hysteresis named", s)
	}
}

func TestStatusExplainsTheMinimum(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	// With no load, the cpu signal asks for nothing, and the minimum of 2 holds.
	c := component(t, h, "api")
	if c.Desired != 2 {
		t.Fatalf("desired %d, want the minimum, 2", c.Desired)
	}
	if s := signal(t, c, "cpu"); s.Demand == nil || *s.Demand != 0 {
		t.Errorf("cpu demand %v, want 0", s.Demand)
	}
	if !strings.Contains(c.Scale.Reason, "raised to its minimum, 2") {
		t.Errorf("reason %q, want the minimum named", c.Scale.Reason)
	}
}

func TestStatusExplainsTheMaximum(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 20000 // Demand is ceil(20000 / 350) = 58.
	h.until("api at its maximum", 3*time.Minute, func() bool { return desired(h, "api") == 10 })
	c := component(t, h, "api")
	if !strings.Contains(c.Scale.Reason, "capped at its maximum, 10") {
		t.Errorf("reason %q, want the maximum named", c.Scale.Reason)
	}
}

func TestStatusExplainsTheHoldAfterAStart(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2000
	h.until("api wants 6", 3*time.Minute, func() bool { return h.ready("api") == 6 })

	// The scheduler restarts and the load is gone: it keeps 6 until it has 5 minutes of
	// signals, and says so.
	h.totalCPU["api"] = 0
	h.start()
	h.run(time.Minute)
	c := component(t, h, "api")
	if c.Desired != 6 {
		t.Fatalf("desired %d after a restart, want the last desired, 6", c.Desired)
	}
	if !strings.Contains(c.Scale.Reason, "started recently") {
		t.Errorf("reason %q, want the hold after a start", c.Scale.Reason)
	}
}

func TestStatusExplainsQueueDemand(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.metrics["api"] = map[string]float64{"jobs_budget_cap": -1}
	// With 300 waiting and 60 in flight, at 2 a second within 60 seconds, demand is
	// ceil(360 / 120) = 3.
	h.queues.set("jobs", 300, 60)
	h.until("worker wants 3", 3*time.Minute, func() bool { return desired(h, "worker") == 3 })

	c := component(t, h, "worker")
	s := signal(t, c, "queue")
	if s.Signal != "queue: jobs, rate: 2/s, drain: 60s" {
		t.Errorf("signal %q", s.Signal)
	}
	if s.Demand == nil || *s.Demand != 3 {
		t.Errorf("demand %v, want 3", s.Demand)
	}
	if !strings.Contains(s.Detail, "300 waiting + 60 in flight") {
		t.Errorf("detail %q", s.Detail)
	}
	st := h.s.Status()
	if len(st.Queues) != 1 || st.Queues[0] != (QueueStatus{Name: "jobs", Visible: 300, InFlight: 60}) {
		t.Errorf("queues %+v, want jobs at 300 + 60", st.Queues)
	}
}

func TestStatusExplainsALimit(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.queues.set("jobs", 960, 0) // Demand is 8.

	c := component(t, h, "worker")
	if len(c.Scale.Limits) != 1 || c.Scale.Limits[0].Limit != "metric: jobs_budget_cap" {
		t.Fatalf("limits %+v, want the spec's one", c.Scale.Limits)
	}
	if l := c.Scale.Limits[0]; l.Cap != nil || !strings.Contains(l.Detail, "no value") {
		t.Errorf("limit %+v before any value: want no cap, and why", l)
	}

	h.metrics["api"] = map[string]float64{"jobs_budget_cap": 2}
	h.until("the cap holds the worker at 2", 3*time.Minute, func() bool { return desired(h, "worker") == 2 })
	c = component(t, h, "worker")
	l := c.Scale.Limits[0]
	if l.Cap == nil || *l.Cap != 2 {
		t.Errorf("cap %v, want 2", l.Cap)
	}
	if s := signal(t, c, "queue"); s.Demand == nil || *s.Demand != 8 {
		t.Errorf("queue demand %v, want 8 whatever the cap", s.Demand)
	}
	if c.Scale.Reason != "limited by jobs_budget_cap to 2" {
		t.Errorf("reason %q, want the limit's", c.Scale.Reason)
	}

	h.metrics["api"] = map[string]float64{"jobs_budget_cap": -1}
	h.until("no cap", 3*time.Minute, func() bool { return desired(h, "worker") == 8 })
	if l := component(t, h, "worker").Scale.Limits[0]; l.Cap != nil || !strings.Contains(l.Detail, "no cap") {
		t.Errorf("limit %+v at -1: want no cap", l)
	}
}

func TestStatusExplainsAFixedLimit(t *testing.T) {
	h := newHarness(t)
	h.deploy("c1", strings.Replace(testSpec, "limits: [{ metric: jobs_budget_cap }]", "limits: [{ replicas: 1 }]", 1), nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	l := component(t, h, "worker").Scale.Limits
	if len(l) != 1 || l[0].Limit != "replicas: 1" || l[0].Cap == nil || *l[0].Cap != 1 {
		t.Errorf("limits %+v, want a fixed cap of 1", l)
	}
}

func TestStatusShowsASignalWithoutAReading(t *testing.T) {
	h := newHarness(t)
	h.settle()
	s := signal(t, component(t, h, "web"), "requests")
	if s.Signal != "requests: 20/s" {
		t.Errorf("signal %q", s.Signal)
	}
	if s.Demand != nil || s.Detail == "" {
		t.Errorf("signal %+v with no request counts: want no demand, and why", s)
	}

	h.edgeRate["web"] = 70
	h.until("web wants 4", 3*time.Minute, func() bool { return desired(h, "web") == 4 })
	s = signal(t, component(t, h, "web"), "requests")
	if s.Demand == nil || *s.Demand != 4 || !strings.Contains(s.Detail, "70.0 requests/s") {
		t.Errorf("signal %+v, want 4 from 70 requests/s", s)
	}
}

func TestStatusShowsComponentsThatDontScale(t *testing.T) {
	h := newHarness(t)
	h.settle()
	c := component(t, h, "postgres")
	if c.Scale != nil {
		t.Errorf("postgres has scale %+v: a stateful component doesn't scale", c.Scale)
	}
	if c.Pool != "data-1" {
		t.Errorf("postgres pool %q, want the data VM it is pinned to", c.Pool)
	}
}

func TestStatusShowsEachVMsRoom(t *testing.T) {
	h := newHarness(t)
	h.settle()
	st := h.s.Status()
	// Every replica on a VM takes its request, until it is gone.
	requested := map[string]api.Resources{}
	for _, r := range st.Replicas {
		if r.VM != "" {
			q := requested[r.VM]
			q.CPU += r.Request.CPU
			q.Memory += r.Request.Memory
			requested[r.VM] = q
		}
	}
	for _, vm := range st.VMs {
		if vm.Pool != "general" {
			continue
		}
		if vm.Reserved != (api.Resources{CPU: 400, Memory: 1024}) {
			t.Errorf("%s reserved %+v, want the pool's 400m and 1Gi", vm.ID, vm.Reserved)
		}
		if want := vm.Capacity.CPU - vm.Reserved.CPU - requested[vm.ID].CPU; vm.Free.CPU != want {
			t.Errorf("%s free CPU %d, want capacity less reserve less requests, %d", vm.ID, vm.Free.CPU, want)
		}
		if vm.IP == "" || vm.FirstSync.IsZero() {
			t.Errorf("%s: ip %q, first sync %v", vm.ID, vm.IP, vm.FirstSync)
		}
	}
}

func TestStatusShowsEachReplicasUsage(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.totalCPU["api"] = 600
	h.run(30 * time.Second)
	n := 0
	for _, r := range h.s.Status().Replicas {
		if r.Component != "api" || r.State != api.StateReady {
			continue
		}
		n++
		if r.Pool != "general" || r.Request != (api.Resources{CPU: 500, Memory: 512}) {
			t.Errorf("%s: pool %q, request %+v", r.ID, r.Pool, r.Request)
		}
		if r.CPU == nil || *r.CPU != 300 || r.Memory == nil || *r.Memory != 256 {
			t.Errorf("%s: cpu %v, memory %v; want what its agent reported, 300m and 256Mi", r.ID, r.CPU, r.Memory)
		}
		if r.Created.IsZero() {
			t.Errorf("%s has no created time", r.ID)
		}
	}
	if n != 2 {
		t.Fatalf("%d ready api replicas, want 2", n)
	}
}

func TestStatusMarksOldReplicasDuringARollout(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.neverReady["api"] = true // The rollout stalls with old and new side by side.
	h.deploy("c2", testSpec, map[string]string{"api": "v2"})
	h.until("a new api replica starts", 2*time.Minute, func() bool { return h.count("api", api.StateStarting) > 0 })

	var old, fresh int
	for _, r := range h.s.Status().Replicas {
		if r.Component != "api" || r.Stopping {
			continue
		}
		switch {
		case r.Release == "c1" && r.Old:
			old++
		case r.Release == "c2" && !r.Old:
			fresh++
		default:
			t.Errorf("%s of %s: old %v", r.ID, r.Release, r.Old)
		}
	}
	if old != 2 || fresh == 0 {
		t.Errorf("%d old and %d new api replicas, want 2 old and some new", old, fresh)
	}
	if c := component(t, h, "api"); c.Old != 2 {
		t.Errorf("api counts %d old, want 2", c.Old)
	}
	for _, r := range h.s.Status().Replicas {
		if r.Component == "web" && r.Old {
			t.Errorf("%s is marked old, but web didn't change", r.ID)
		}
	}
}

func TestStatusShowsPoolsSizing(t *testing.T) {
	h := newHarness(t)
	h.settle()
	st := h.s.Status()
	g := pool(t, st, "general")
	if g.Scale == nil {
		t.Fatal("general has no scale")
	}
	want := PoolScale{VMType: "m7g.xlarge", Min: 1, Max: 6, Spare: 1, Target: 0.7,
		Usable: api.Resources{CPU: (4000 - 400) * 7 / 10, Memory: (16384 - 1024) * 7 / 10}}
	if *g.Scale != want {
		t.Errorf("general scale %+v, want %+v", *g.Scale, want)
	}
	c := pool(t, st, "compute")
	if c.Scale == nil || c.Scale.Min != 0 || c.Scale.Spare != 0 || c.Scale.Target != 0.85 || c.Scale.VMType != "c7g.xlarge" {
		t.Errorf("compute scale %+v", c.Scale)
	}
	for _, p := range st.Pools {
		if p.Name == "ingress" || p.Name == "data" {
			t.Errorf("pool %s in the status: fixed pools aren't sized", p.Name)
		}
	}
}

func TestStatusCountsPendingReplicasByPool(t *testing.T) {
	h := newHarness(t)
	h.groups.bootSteps = 1000 // General never gets a second VM.
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 20000 // Ten replicas of 500m need more than one VM's 3600m.
	h.until("api replicas wait for a VM", 3*time.Minute, func() bool { return h.count("api", StatePending) > 0 })
	st := h.s.Status()
	if got, want := pool(t, st, "general").Pending, h.count("api", StatePending)+h.count("web", StatePending); got != want {
		t.Errorf("general has %d pending, want api's and web's %d", got, want)
	}
	if got := pool(t, st, "compute").Pending; got != 0 {
		t.Errorf("compute has %d pending, want 0", got)
	}
}
