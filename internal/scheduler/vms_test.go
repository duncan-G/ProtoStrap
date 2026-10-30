package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These tests scale VMs (README.md#scaling-vms, docs/vms.md#scaling-with-auto-scaling-groups).

func TestSizingFormula(t *testing.T) {
	// As in README.md#scaling-vms, general VMs of 4 vCPU, 400m reserved and a 70% target
	// have 2,520m usable each. Replicas requesting 8,200m need ceil(8200 / 2520) = 4, plus 1
	// spare: 5.
	shape := poolShape{vm: api.Resources{CPU: 4000, Memory: 16384}, reserved: api.Resources{CPU: 400, Memory: 1024},
		target: 0.70, spare: 1, min: 1, max: 10}
	if got := shape.size(api.Resources{CPU: 8200, Memory: 4096}); got != 5 {
		t.Errorf("size = %d, want 5", got)
	}
	// Memory can be what needs the VMs: (16384 − 1024) × 70% = 10752 MiB usable.
	if got := shape.size(api.Resources{CPU: 100, Memory: 30000}); got != 4 {
		t.Errorf("size by memory = %d, want ceil(30000 / 10752) + 1 = 4", got)
	}
	if got := shape.size(api.Resources{}); got != 1 {
		t.Errorf("nothing requested = %d, want the spare, within the minimum", got)
	}
	shape.max = 3
	if got := shape.size(api.Resources{CPU: 8200}); got != 3 {
		t.Errorf("size = %d, want the maximum, 3", got)
	}
	compute := poolShape{vm: api.Resources{CPU: 4000, Memory: 8192}, reserved: api.Resources{CPU: 400, Memory: 1024}, target: 0.85, spare: 0, min: 0, max: 20}
	if got := compute.size(api.Resources{}); got != 0 {
		t.Errorf("compute with nothing to run = %d, want 0", got)
	}
}

func TestSizingGrowsThePool(t *testing.T) {
	h := newHarness(t)
	h.settle()
	// The api's 2 × 500m and web's 250m make 1250m: one VM, plus the spare.
	h.until("general has its spare VM", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	h.run(5 * time.Minute)
	if got := len(h.vms("general")); got != 2 {
		t.Errorf("general has %d VMs, want 2", got)
	}
}

func TestAPendingReplicaAddsAVMOnce(t *testing.T) {
	h := newHarness(t)
	h.groups.bootSteps = 60 // VMs take five minutes to boot.
	big := strings.Replace(testSpec, "request: { cpu: 500m, memory: 512Mi }", "request: { cpu: 3000m, memory: 512Mi }", 1)
	big = strings.Replace(big, "    spare: 1\n    target: 70%", "    spare: 0\n    target: 100%", 1)
	h.deploy("c1", big, nil)
	// Two api replicas of 3000m fit one per VM. The second is pending until a second VM boots.
	h.until("one api replica pending", 3*time.Minute, func() bool { return h.count("api", StatePending) == 1 })
	h.run(2 * time.Minute)
	g := h.groups.group("protostrap-general-us-east-1a")
	if g.Desired != 2 {
		t.Errorf("general desired %d, want 2: the booting VM counts with all its room", g.Desired)
	}
	h.until("both api replicas ready", 6*time.Minute, func() bool { return h.ready("api") == 2 })
}

func TestTheBiggestReplicasArePlacedFirst(t *testing.T) {
	// Two VMs each have 1,000m free; a 1,000m replica and two 500m replicas arrive. Placing
	// the big one first fits all three.
	rooms := []*room{
		{vm: &VM{ID: "a", Pool: "general"}, free: api.Resources{CPU: 1000, Memory: 4096}, perComponent: map[string]int{}},
		{vm: &VM{ID: "b", Pool: "general"}, free: api.Resources{CPU: 1000, Memory: 4096}, perComponent: map[string]int{}},
	}
	pending := []*Replica{
		{ID: "small-1", Component: "small", Request: api.Resources{CPU: 500, Memory: 64}},
		{ID: "big-1", Component: "big", Request: api.Resources{CPU: 1000, Memory: 64}},
		{ID: "small-2", Component: "small", Request: api.Resources{CPU: 500, Memory: 64}},
	}
	placed := packAll(pending, rooms)
	if len(placed) != 3 {
		t.Fatalf("placed %v, want all three", placed)
	}
}

func TestPlacementPrefersFewestOfTheComponentThenMostFreeCPU(t *testing.T) {
	r := &Replica{ID: "api-9", Component: "api", Request: api.Resources{CPU: 500, Memory: 512}}
	rooms := []*room{
		{vm: &VM{ID: "a"}, free: api.Resources{CPU: 3000, Memory: 8192}, perComponent: map[string]int{"api": 2}},
		{vm: &VM{ID: "b"}, free: api.Resources{CPU: 1000, Memory: 8192}, perComponent: map[string]int{"api": 1}},
		{vm: &VM{ID: "c"}, free: api.Resources{CPU: 2000, Memory: 8192}, perComponent: map[string]int{"api": 1}},
		{vm: &VM{ID: "d"}, free: api.Resources{CPU: 400, Memory: 8192}, perComponent: map[string]int{}},
	}
	if got := choose(rooms, r); got == nil || got.vm.ID != "c" {
		t.Errorf("chose %v, want c: d is too full, then fewest api replicas (b, c), then most free CPU", got)
	}
}

func TestStoppingPrefersUnservingReplicasThenTheBusiestVM(t *testing.T) {
	rooms := map[string]*room{
		"a": {vm: &VM{ID: "a"}, free: api.Resources{CPU: 500}, perComponent: map[string]int{"api": 3}},
		"b": {vm: &VM{ID: "b"}, free: api.Resources{CPU: 3000}, perComponent: map[string]int{"api": 1}},
		"c": {vm: &VM{ID: "c"}, free: api.Resources{CPU: 2000}, perComponent: map[string]int{"api": 3}},
	}
	replicas := []*Replica{
		{ID: "api-1", Component: "api", VM: "a", State: api.StateReady},
		{ID: "api-2", Component: "api", VM: "a", State: api.StateReady},
		{ID: "api-3", Component: "api", VM: "a", State: api.StateReady},
		{ID: "api-4", Component: "api", VM: "b", State: api.StateReady},
		{ID: "api-5", Component: "api", VM: "c", State: api.StateReady},
		{ID: "api-6", Component: "api", VM: "c", State: api.StateReady},
		{ID: "api-7", Component: "api", VM: "c", State: api.StateStarting},
		{ID: "api-8", Component: "api", State: StatePending},
	}
	got := pickToStop(replicas, rooms, 4)
	ids := []string{}
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	// The pending replica goes first, then the starting one (c is down to 2), then one from
	// the VM with the most api replicas (a, 3), then on a tie (a and c, 2 each) one from the
	// VM with the most free CPU (c).
	want := "api-8 api-7 api-1 api-5"
	if strings.Join(ids, " ") != want {
		t.Errorf("stopped %v, want %s", ids, want)
	}
}

func TestRemovingAVMDrainsItFirst(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 4 // The api runs 10 replicas, and the 5250m requested needs 4 VMs.
	h.until("general grows to 4", 6*time.Minute, func() bool { return len(h.vms("general")) == 4 })
	grown := len(h.vms("general"))
	h.totalCPU["api"] = 0

	// Sizing wants fewer VMs; ten minutes later, the emptiest goes, after its replicas move.
	h.run(8 * time.Minute)
	if got := len(h.vms("general")); got != grown {
		t.Errorf("a VM went %d minutes after load fell, before sizing had wanted fewer for 10", 8)
	}
	terminated := h.groups.countCalls("terminate")
	h.until("a VM is removed", 10*time.Minute, func() bool { return h.groups.countCalls("terminate") > terminated })
	// Nothing was lost on the way: the api and web still have their minimums ready.
	if h.ready("api") < 2 || h.ready("web") < 1 {
		t.Errorf("api %d, web %d ready after a VM was removed", h.ready("api"), h.ready("web"))
	}
	if strings.Contains(h.log.String(), `"lowered desired capacity"`) {
		t.Errorf("the scheduler lowered a group's desired capacity, letting AWS choose a VM")
	}
}

func TestVMsAreRemovedOneAtATime(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 6
	h.until("general grows to 4", 8*time.Minute, func() bool { return len(h.vms("general")) >= 4 })
	h.totalCPU["api"] = 0
	h.run(10 * time.Minute)
	draining := 0
	for i := 0; i < 240; i++ {
		h.step()
		n := 0
		for _, v := range h.s.Status().VMs {
			if v.Cordoned {
				n++
			}
		}
		draining = max(draining, n)
	}
	if draining > 1 {
		t.Errorf("%d VMs drained at once, want one at a time", draining)
	}
}

func TestASilentAgentsReplicasAreReplaced(t *testing.T) {
	// This follows docs/protostrap.md#lose-a-vm.
	h := newHarness(t)
	h.settle()
	h.until("general has two VMs", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	var victim *simAgent
	for _, a := range h.vms("general") {
		for _, r := range a.replicas {
			if r.spec.Component == "api" {
				victim = a
			}
		}
	}
	if victim == nil {
		t.Fatal("no general VM runs the api")
	}
	victim.silent = true
	h.run(20 * time.Second)
	if h.ready("api") < 2 {
		t.Errorf("replicas stopped counting before 30 seconds of silence")
	}
	h.until("replacements are ready elsewhere", 2*time.Minute, func() bool {
		n := 0
		for _, a := range h.agents {
			if a == victim {
				continue
			}
			for _, r := range a.replicas {
				if r.spec.Component == "api" && r.state == api.StateReady {
					n++
				}
			}
		}
		return n >= 2
	})

	// After 10 minutes the scheduler marks it unhealthy, so its group replaces it, and its
	// swarm node is removed.
	h.until("marked unhealthy", 11*time.Minute, func() bool { return h.groups.countCalls("unhealthy "+victim.id) == 1 })
	h.until("its swarm node is removed", 12*time.Minute, func() bool {
		for _, id := range h.swarm.removed {
			if id == "node-"+victim.id {
				return true
			}
		}
		return false
	})
}

func TestASilentVMThatComesBackStopsItsReplacedReplicas(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.until("two general VMs", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	victim := h.agentOf(h.running("api")[0])
	victim.silent = true
	h.run(2 * time.Minute)
	victim.silent = false
	h.run(30 * time.Second)
	for id, r := range victim.replicas {
		if r.spec.Component == "api" && r.state != api.StateStopping {
			t.Errorf("replica %s on the returning VM wasn't stopped: it was replaced", id)
		}
	}
}

func TestNoMoreThanOneVMPerPoolIsReplacedEveryTenMinutes(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 6
	h.until("general grows to 4", 8*time.Minute, func() bool { return len(h.vms("general")) >= 4 })
	vms := h.vms("general")
	vms[0].silent = true
	vms[1].silent = true
	h.run(11 * time.Minute)
	if n := h.groups.countCalls("unhealthy"); n != 1 {
		t.Errorf("%d VMs marked unhealthy within 10 minutes, want 1", n)
	}
}

func TestMoreThanHalfAPoolSilentReplacesNone(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.until("two general VMs", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	for _, a := range h.vms("general") {
		a.silent = true
	}
	h.run(15 * time.Minute)
	if n := h.groups.countCalls("unhealthy"); n != 0 {
		t.Errorf("%d VMs marked unhealthy when the whole pool went silent: that points at the scheduler's side", n)
	}
}

func TestASilentDataVMIsOnlyReported(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.agents["i-data1"].silent = true
	h.run(15 * time.Minute)
	if n := h.groups.countCalls("unhealthy"); n != 0 {
		t.Errorf("a data VM was marked unhealthy")
	}
	for _, c := range []string{"postgres", "redis"} {
		for _, r := range h.replicas(c) {
			if r.VM != "i-data1" {
				t.Errorf("%s replica %s placed off its data VM", c, r.ID)
			}
		}
	}
	if !strings.Contains(h.log.String(), "data VM is silent") {
		t.Errorf("a silent data VM wasn't reported")
	}
}

func TestATerminatingVMsReplicasMoveOff(t *testing.T) {
	// This follows docs/vms.md#when-aws-removes-a-vm.
	h := newHarness(t)
	h.settle()
	h.until("two general VMs", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	victim := h.agentOf(h.running("api")[0])
	victim.term = &api.Termination{Reason: api.TerminationLifecycle, Deadline: h.clock.Now().Add(10 * time.Minute)}
	h.until("its replicas run elsewhere", 4*time.Minute, func() bool {
		for _, r := range h.replicas("api") {
			if r.VM == victim.id {
				return false
			}
		}
		return h.ready("api") >= 2
	})
	if n := h.groups.countCalls("terminate " + victim.id); n != 0 {
		t.Errorf("the scheduler terminated a VM AWS is already removing")
	}
}

func TestComputeFallsBackToOnDemandWhenSpotHasNoCapacity(t *testing.T) {
	// This follows docs/vms.md#when-a-launch-fails.
	h := newHarness(t)
	h.settle()
	h.groups.group("protostrap-compute-spot-us-east-1a").noCapacity = true
	tight := 960 * 4
	h.queues.set("jobs", tight, 0)
	h.until("spot asked for VMs", 3*time.Minute, func() bool { return h.groups.group("protostrap-compute-spot-us-east-1a").Desired > 0 })
	h.run(8 * time.Minute)
	if h.groups.group("protostrap-compute-ondemand-us-east-1a").Desired != 0 {
		t.Errorf("fell back to On-Demand before launches went unfilled for 10 minutes")
	}
	h.until("On-Demand takes over", 4*time.Minute, func() bool { return h.groups.group("protostrap-compute-ondemand-us-east-1a").Desired > 0 })
	if got := h.groups.group("protostrap-compute-spot-us-east-1a").Desired; got != 0 {
		t.Errorf("spot's unfilled launches weren't cancelled: desired %d", got)
	}
	h.until("workers run on On-Demand VMs", 4*time.Minute, func() bool { return len(h.vms("compute")) > 0 })
}

func TestEveryIngressVMRunsAnEdge(t *testing.T) {
	h := newHarness(t)
	h.groups.add(&fakeGroup{Group: Group{Name: "protostrap-ingress-us-east-1b", Pool: "ingress", Zone: "us-east-1b", Min: 1, Max: 2}, vm: "c8g.medium"}, 1)
	h.settle()
	for _, a := range h.vms("ingress") {
		n := 0
		for _, r := range a.replicas {
			if r.spec.Kind == api.KindEdge {
				n++
			}
		}
		if n != 1 {
			t.Errorf("ingress VM %s in %s runs %d edges, want 1", a.id, a.zone, n)
		}
	}
	for _, a := range h.vms("ingress") {
		for _, r := range a.replicas {
			if r.spec.Kind != api.KindEdge {
				t.Errorf("%s runs on ingress VM %s, where nothing but the edge runs", r.spec.Component, a.id)
			}
		}
	}
}

func TestAnIngressReplacementGetsItsEdgeFirst(t *testing.T) {
	h := newHarness(t)
	h.settle()
	old := h.vms("ingress")[0]
	g := h.groups.group("protostrap-ingress-us-east-1a")
	g.Desired = 2 // A planned replacement starts beside the old VM.
	h.until("the new ingress VM runs an edge", 3*time.Minute, func() bool {
		for _, a := range h.vms("ingress") {
			if a != old && h.agentRunsReady(a, api.KindEdge) {
				return true
			}
		}
		return false
	})
	if !h.agentRunsReady(old, api.KindEdge) {
		t.Errorf("the old edge stopped before its replacement was ready")
	}
}

func (h *harness) agentRunsReady(a *simAgent, kind api.Kind) bool {
	for _, r := range a.replicas {
		if r.spec.Kind == kind && r.state == api.StateReady {
			return true
		}
	}
	return false
}

func TestStatefulReplicasStayOnTheirDataVM(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, c := range []string{"postgres", "redis"} {
		ids := h.running(c)
		if len(ids) != 1 || h.agentOf(ids[0]).name != "data-1" {
			t.Errorf("%s runs %v", c, ids)
		}
	}
	// The general pool never counts them.
	for _, p := range h.s.Status().Pools {
		if p.Name == "general" && p.Requested.CPU >= 3000 {
			t.Errorf("general counts %dm requested, which includes stateful components", p.Requested.CPU)
		}
	}
}
