package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These tests scale replicas (README.md#scaling-replicas, docs/scaling-examples.md).

func desired(h *harness, component string) int {
	for _, c := range h.s.Status().Components {
		if c.Name == component {
			return c.Desired
		}
	}
	return -1
}

func TestCPUDemand(t *testing.T) {
	// As in docs/scaling-examples.md#replicas-from-cpu, 5 replicas of 500m at 105% of their
	// request against a 70% target need ceil(5 × 105 / 70) = 8.
	h := newHarness(t)
	five := strings.Replace(testSpec, "      min: 2\n      max: 10\n", "      min: 5\n      max: 10\n", 1)
	h.deploy("c1", five, nil)
	h.until("5 api replicas", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone && h.ready("api") == 5 })
	h.run(2 * time.Minute)

	h.totalCPU["api"] = 5 * 500 * 105 / 100
	h.until("demand reaches 8", 2*time.Minute, func() bool { return desired(h, "api") == 8 })
	h.until("8 api replicas ready", 5*time.Minute, func() bool { return h.ready("api") == 8 })
}

func TestCPUWithinTenPercentOfTargetChangesNothing(t *testing.T) {
	h := newHarness(t)
	five := strings.Replace(testSpec, "      min: 2\n      max: 10\n", "      min: 2\n      max: 10\n", 1)
	h.deploy("c1", five, nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.run(6 * time.Minute) // This runs past the hold after a start.

	// With 2 replicas at 75% of 500m, ceil(2 × 75 / 70) would be 3, but 75% is within 10% of 70%.
	h.totalCPU["api"] = 2 * 500 * 75 / 100
	h.run(3 * time.Minute)
	if got := desired(h, "api"); got != 2 {
		t.Errorf("desired %d at 75%% against a 70%% target, want 2: small swings are ignored", got)
	}
	h.totalCPU["api"] = 2 * 500 * 90 / 100
	h.until("90% scales out", 2*time.Minute, func() bool { return desired(h, "api") == 3 })
}

func TestScalingDownWaitsForThePeakToPass(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 2 // At 200% on 2 replicas, demand is ceil(2 × 2 / 0.7) = 6.
	h.until("scaled out to 6", 3*time.Minute, func() bool { return desired(h, "api") >= 6 })
	peak := desired(h, "api")
	h.totalCPU["api"] = 0
	h.run(3 * time.Minute)
	if got := desired(h, "api"); got != peak {
		t.Errorf("desired fell to %d within 3 minutes of a peak of %d: scale down only on the highest demand of the last few minutes", got, peak)
	}
	h.until("scaled back to the minimum", 5*time.Minute, func() bool { return desired(h, "api") == 2 })
	h.until("the extra replicas stop", 2*time.Minute, func() bool { return len(h.running("api")) == 2 })
}

func TestStartingReplicasDontDragTheAverageDown(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.readyAfter["api"] = 24 // Replicas take two minutes to start.
	h.totalCPU["api"] = 2 * 500 * 2
	h.until("scaling out", 2*time.Minute, func() bool { return desired(h, "api") > 2 })
	want := desired(h, "api")
	h.run(time.Minute)
	if got := desired(h, "api"); got < want {
		t.Errorf("desired dropped from %d to %d while new replicas were starting: count only ready ones", want, got)
	}
}

func TestRequestsDemand(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	// web handles 20 requests a second per replica; the edge counts 70 a second.
	h.edgeRate["web"] = 70
	h.until("ceil(70 / 20) = 4 web replicas wanted", 3*time.Minute, func() bool { return desired(h, "web") == 4 })
	h.until("4 web replicas ready", 3*time.Minute, func() bool { return h.ready("web") == 4 })
}

func TestTheEdgeIsAskedForRequestCounts(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, a := range h.vms("ingress") {
		for _, s := range a.assignment.Replicas {
			if s.Kind != api.KindEdge {
				continue
			}
			if s.Metrics == nil || !containsString(s.Metrics.Names, EdgeRequestsMetric("web")) {
				t.Errorf("the edge isn't asked for web's request count: %+v", s.Metrics)
			}
		}
	}
}

func TestMetricDemand(t *testing.T) {
	h := newHarness(t)
	spec := strings.Replace(testSpec, "      demand: [{ cpu: 70% }]", "      demand: [{ metric: queue_lag, target: 2 }]", 1)
	h.deploy("c1", spec, nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.run(6 * time.Minute)
	// Each of 2 replicas reports 5: ceil(2 × 5 / 2) = 5.
	h.metrics["api"] = map[string]float64{"queue_lag": 5}
	h.until("demand of 5", 3*time.Minute, func() bool { return desired(h, "api") == 5 })
}

func TestTheHighestSignalWins(t *testing.T) {
	h := newHarness(t)
	spec := strings.Replace(testSpec, "      demand: [{ cpu: 70% }]", "      demand: [{ cpu: 70% }, { metric: queue_lag, target: 2 }]", 1)
	h.deploy("c1", spec, nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 140 / 100               // Demand is ceil(1400 / 350) = 4.
	h.metrics["api"] = map[string]float64{"queue_lag": 1} // Demand is ceil(2 × 1 / 2) = 1.
	h.until("the higher, CPU's 4", 3*time.Minute, func() bool { return desired(h, "api") == 4 })
	h.run(2 * time.Minute)
	if got := desired(h, "api"); got != 4 {
		t.Errorf("desired %d, want CPU's 4 to hold", got)
	}
	h.metrics["api"] = map[string]float64{"queue_lag": 4} // Demand is ceil(4 × 4 / 2) = 8.
	h.until("the metric, now higher", 3*time.Minute, func() bool { return desired(h, "api") >= 8 })
}

func TestWorkerFromZero(t *testing.T) {
	// This follows docs/protostrap.md#scale-a-worker-from-zero.
	h := newHarness(t)
	h.settle()
	h.until("general grows to two", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	if got := len(h.vms("compute")); got != 0 {
		t.Fatalf("compute has %d VMs before any job", got)
	}

	// One job gets one worker, on a general VM with room, as an ephemeral replica.
	h.queues.set("jobs", 1, 0)
	h.until("a worker runs", 2*time.Minute, func() bool { return h.ready("worker") == 1 })
	w := h.replicas("worker")[0]
	if !w.Ephemeral || h.agentOf(w.ID).pool != "general" {
		t.Errorf("the first worker %+v isn't an ephemeral replica on a general VM", w)
	}
	h.run(time.Minute)
	if got := len(h.vms("compute")); got != 0 {
		t.Errorf("compute started %d VMs for one job that fits on general", got)
	}
	if n := h.groups.countCalls("desired protostrap-general"); n > 1 {
		t.Errorf("an ephemeral worker grew the general pool")
	}

	// A backlog of 960 needs ceil(960 / (2 × 60)) = 8. General's room fits some; the rest
	// wait on compute, which grows to fit them.
	h.queues.set("jobs", 900, 60)
	h.until("demand of 8", time.Minute, func() bool { return desired(h, "worker") == 8 })
	h.until("compute grows", 2*time.Minute, func() bool { return len(h.vms("compute")) > 0 })
	h.until("8 workers ready", 5*time.Minute, func() bool { return h.ready("worker") == 8 })

	// The queue drains: workers come off compute first, compute empties and goes to zero.
	h.queues.set("jobs", 0, 0)
	h.until("no workers", 8*time.Minute, func() bool { return len(h.running("worker")) == 0 })
	h.until("compute back at zero", 15*time.Minute, func() bool { return len(h.vms("compute")) == 0 })
}

func TestTheLastJobsKeepOneWorker(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.queues.set("jobs", 0, 2) // With nothing waiting and 2 in flight, demand is ceil(2 / 120) = 1.
	h.until("one worker", 2*time.Minute, func() bool { return desired(h, "worker") == 1 })
	h.run(8 * time.Minute)
	if desired(h, "worker") != 1 {
		t.Errorf("the worker went to %d with 2 jobs in flight", desired(h, "worker"))
	}
}

func TestWorkersComeOffComputeBeforeGeneral(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.until("general grows to two", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	h.queues.set("jobs", 960, 0)
	h.until("8 workers", 8*time.Minute, func() bool { return h.ready("worker") == 8 })
	onGeneral := 0
	for _, r := range h.replicas("worker") {
		if h.agentOf(r.ID).pool == "general" {
			onGeneral++
		}
	}
	if onGeneral == 0 {
		t.Fatalf("no worker landed on general; the test needs some there")
	}
	h.queues.set("jobs", 240, 0) // Demand is 2.
	h.until("down to 2", 8*time.Minute, func() bool { return len(h.running("worker")) == 2 })
	for _, id := range h.running("worker") {
		if a := h.agentOf(id); a.pool != "general" {
			t.Errorf("worker %s kept on %s while workers on general went: compute empties first", id, a.pool)
		}
	}
}

func TestAServiceTakesRoomBackFromAnEphemeralWorker(t *testing.T) {
	h := newHarness(t)
	tight := strings.Replace(testSpec, "    min: 1\n    max: 6\n    spare: 1\n", "    min: 1\n    max: 1\n    spare: 0\n", 1)
	h.deploy("c1", tight, nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	// General is one VM, capped. Fill it with workers.
	h.queues.set("jobs", 960, 0)
	h.until("workers on general", 3*time.Minute, func() bool {
		for _, r := range h.replicas("worker") {
			if a := h.agentOf(r.ID); a != nil && a.pool == "general" && r.State == api.StateReady {
				return true
			}
		}
		return false
	})
	h.run(6 * time.Minute)
	// The api needs room: the scheduler stops an ephemeral worker and places the api there.
	h.totalCPU["api"] = 2 * 500 * 3
	h.until("the api scales out on general", 3*time.Minute, func() bool { return h.ready("api") > 2 })
}

func TestAReplicaLimitCapsDemand(t *testing.T) {
	h := newHarness(t)
	spec := strings.Replace(testSpec, "limits: [{ metric: jobs_budget_cap }]", "limits: [{ replicas: 1 }]", 1)
	h.deploy("c1", spec, nil)
	h.until("c1", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.queues.set("jobs", 960, 0) // Demand is 8, capped at 1.
	h.run(3 * time.Minute)
	if got := desired(h, "worker"); got != 1 {
		t.Errorf("desired %d, want the limit's 1", got)
	}
}

func TestAMetricLimitHoldsAComponentBack(t *testing.T) {
	// As in docs/scaling-examples.md#a-monthly-budget, the api exports the worker's cap.
	h := newHarness(t)
	h.settle()
	h.queues.set("jobs", 960, 0)
	h.metrics["api"] = map[string]float64{"jobs_budget_cap": -1}
	h.until("no cap: 8", 3*time.Minute, func() bool { return desired(h, "worker") == 8 })

	h.metrics["api"] = map[string]float64{"jobs_budget_cap": 0}
	h.until("the budget is spent: 0", 2*time.Minute, func() bool { return desired(h, "worker") == 0 })
	h.until("in-flight jobs finish and the workers stop", 3*time.Minute, func() bool { return len(h.running("worker")) == 0 })

	h.metrics["api"] = map[string]float64{"jobs_budget_cap": 3}
	h.until("a cap of 3", 2*time.Minute, func() bool { return desired(h, "worker") == 3 })
}

func TestAStaleLimitCapsNothing(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.queues.set("jobs", 960, 0)
	h.metrics["api"] = map[string]float64{"jobs_budget_cap": 0}
	h.run(time.Minute)
	if got := desired(h, "worker"); got != 0 {
		t.Fatalf("desired %d with a cap of 0", got)
	}
	// The api stops exporting it: after 5 minutes without a value, no cap applies.
	h.metrics["api"] = nil
	h.run(4 * time.Minute)
	if desired(h, "worker") != 0 {
		t.Errorf("the cap lapsed early")
	}
	h.until("the cap lapses", 2*time.Minute, func() bool { return desired(h, "worker") == 8 })
}

func TestReplicasAreAskedForTheMetricsTheSchedulerNeeds(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, a := range h.agents {
		for _, s := range a.assignment.Replicas {
			if s.Component != "api" {
				continue
			}
			if s.Metrics == nil || s.Metrics.Endpoint != "9090/metrics" || !containsString(s.Metrics.Names, "jobs_budget_cap") {
				t.Errorf("api asked for %+v, want jobs_budget_cap from 9090/metrics", s.Metrics)
			}
		}
	}
}

func TestDesiredStaysWithinMinAndMax(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 20 // Demand is far above the maximum.
	h.until("at the maximum", 2*time.Minute, func() bool { return desired(h, "api") == 10 })
	h.run(time.Minute)
	if got := desired(h, "api"); got != 10 {
		t.Errorf("desired %d, want the maximum, 10", got)
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
