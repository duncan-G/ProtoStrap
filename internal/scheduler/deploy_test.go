package scheduler

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These tests deploy releases (docs/protostrap.md#deploy-a-release).

func TestFirstReleaseIntoAnEmptyCluster(t *testing.T) {
	h := newHarness(t)
	h.deploy("c1", testSpec, nil)

	// In cycle one, only what depends on nothing can start. The edge goes on the ingress VM,
	// PostgreSQL and Redis on data-1; the migration waits for PostgreSQL, the api for both
	// and for the migration, web for the api.
	h.step()
	h.step()
	if got := len(h.running("edge")); got != 1 {
		t.Errorf("edge replicas %d, want 1 on the ingress VM", got)
	}
	data1 := h.agents["i-data1"]
	for _, c := range []string{"postgres", "redis"} {
		ids := h.running(c)
		if len(ids) != 1 || h.agentOf(ids[0]) != data1 {
			t.Errorf("%s runs on %v, want one replica pinned to data-1", c, ids)
		}
	}
	for _, c := range []string{"migrate", "api", "web"} {
		if got := h.running(c); len(got) > 0 {
			t.Errorf("%s started before what it depends on was ready: %v", c, got)
		}
	}

	h.until("the migration runs", time.Minute, func() bool { return len(h.running("migrate")) == 1 })
	if len(h.running("api")) > 0 {
		t.Errorf("the api started before its migration finished")
	}
	h.until("the api starts", time.Minute, func() bool { return len(h.running("api")) > 0 })
	h.until("web starts after the api is ready", time.Minute, func() bool { return len(h.running("web")) > 0 })
	h.until("the release finishes", 5*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })

	if got := h.ready("api"); got != 2 {
		t.Errorf("api ready %d, want its minimum, 2", got)
	}
	if got := h.ready("web"); got != 1 {
		t.Errorf("web ready %d, want 1", got)
	}
	if got := len(h.running("worker")); got != 0 {
		t.Errorf("the worker runs %d replicas with an empty queue, want 0", got)
	}
	h.until("the finished migration's container is removed", time.Minute, func() bool { return len(h.running("migrate")) == 0 })
	st := h.s.Status()
	if st.Target != "c1" || st.LastGood != "c1" {
		t.Errorf("target %q, last good %q", st.Target, st.LastGood)
	}
	saved, _ := h.store.State(context.Background())
	if saved.Target != "c1" || saved.LastGood != "c1" || saved.Desired["api"] != 2 {
		t.Errorf("state.json %+v", saved)
	}
}

func TestReplicasSpreadAcrossVMs(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.until("general grows to its sized two VMs", 3*time.Minute, func() bool { return len(h.vms("general")) == 2 })
	h.totalCPU["api"] = 2100 // Demand is ceil(2100 / 350) = 6.
	h.until("the api scales out to 6", 5*time.Minute, func() bool { return h.ready("api") == 6 && len(h.replicas("api")) == 6 })
	per := map[string]int{}
	for _, r := range h.replicas("api") {
		per[r.VM]++
	}
	for vm, n := range per {
		if n > 4 {
			t.Errorf("vm %s has %d of 6 api replicas: placement prefers the VM with the fewest", vm, n)
		}
	}
}

func TestRolloutReplacesAQuarterAtATime(t *testing.T) {
	h := newHarness(t)
	big := strings.Replace(testSpec, "      min: 2\n      max: 10\n", "      min: 8\n      max: 10\n", 1)
	h.deploy("c1", big, nil)
	h.until("8 api replicas", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone && h.ready("api") == 8 })
	old := h.running("api")

	h.deploy("c2", big, map[string]string{"api": "v2"})
	maxNew := 0
	for i := 0; i < 400 && h.rollout("c2") == RolloutRunning; i++ {
		h.step()
		var newStarting, ready int
		for _, a := range h.agents {
			for _, r := range a.replicas {
				if r.spec.Component != "api" {
					continue
				}
				if r.state == api.StateReady {
					ready++
				}
				if r.spec.Release == "c2" && r.state == api.StateStarting {
					newStarting++
				}
			}
		}
		maxNew = max(maxNew, newStarting)
		if ready < 8 && h.steps > 4 {
			// Old replicas stop only once as many new ones are ready.
			t.Fatalf("step %d: only %d api replicas ready during the rollout, want at least 8\n%s", h.steps, ready, h.describe())
		}
	}
	if h.rollout("c2") != RolloutDone {
		t.Fatalf("the rollout didn't finish: %s\n%s", h.rollout("c2"), h.describe())
	}
	if maxNew > 2 {
		t.Errorf("up to %d new replicas started at once, want a quarter of 8: 2", maxNew)
	}
	h.until("the old replicas are gone", time.Minute, func() bool { return len(h.running("api")) == 8 })
	for _, id := range h.running("api") {
		if slices.Contains(old, id) {
			t.Errorf("old replica %s survived the rollout", id)
		}
	}
}

func TestUnchangedComponentsKeepTheirReplicas(t *testing.T) {
	h := newHarness(t)
	h.settle()
	api1, pg1, redis1, edge1 := h.running("api"), h.running("postgres"), h.running("redis"), h.running("edge")

	// Only web's image changes.
	h.deploy("c2", testSpec, map[string]string{"web": "v2"})
	h.until("c2 finishes", 5*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.run(30 * time.Second)
	for name, before := range map[string][]string{"api": api1, "postgres": pg1, "redis": redis1, "edge": edge1} {
		if got := h.running(name); !slices.Equal(got, before) {
			t.Errorf("%s replicas %v became %v: a matching fingerprint keeps a replica of an older release", name, before, got)
		}
	}
	if slices.Equal(h.running("web"), []string{"web-1"}) {
		t.Errorf("web wasn't replaced")
	}
	if len(h.running("migrate")) != 0 || h.ids["migrate"] != 1 {
		t.Errorf("the migration ran again though its tag didn't change: %d runs", h.ids["migrate"])
	}
}

func TestAScaleChangeReplacesNothing(t *testing.T) {
	h := newHarness(t)
	h.settle()
	before := h.running("api")
	h.deploy("c2", strings.Replace(testSpec, "      min: 2\n      max: 10\n", "      min: 3\n      max: 10\n", 1), nil)
	h.until("the api runs its new minimum", 3*time.Minute, func() bool { return h.ready("api") == 3 })
	for _, id := range before {
		if !slices.Contains(h.running("api"), id) {
			t.Errorf("replica %s was replaced by a scale change", id)
		}
	}
	h.until("c2 finishes", time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
}

func TestAnEnvChangeReplacesOnlyThatComponent(t *testing.T) {
	h := newHarness(t)
	h.settle()
	web := h.running("web")
	h.deploy("c2", strings.Replace(testSpec, "env: { MODE: prod }", "env: { MODE: canary }", 1), nil)
	h.until("c2 finishes", 5*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.run(30 * time.Second)
	if !slices.Equal(h.running("web"), web) {
		t.Errorf("web was replaced by a change to the api's env")
	}
	for _, id := range h.running("api") {
		if id == "api-1" || id == "api-2" {
			t.Errorf("api replica %s kept its old env", id)
		}
	}
}

func TestATaskRunsWhenItsTagIsNewAndReplacesNothing(t *testing.T) {
	h := newHarness(t)
	h.settle()
	apiBefore := h.running("api")
	h.deploy("c2", testSpec, map[string]string{"migrate": "v2"})
	h.until("the migration runs again", 2*time.Minute, func() bool { return h.ids["migrate"] == 2 })
	h.until("c2 finishes", 3*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	if !slices.Equal(h.running("api"), apiBefore) {
		t.Errorf("the api was replaced by a release that only changed its migration")
	}
}

func TestMigrationsRunBeforeTheFirstNewReplica(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.readyAfter["migrate"] = 6
	h.deploy("c2", testSpec, map[string]string{"api": "v2", "migrate": "v2"})
	h.until("the migration runs", time.Minute, func() bool { return len(h.running("migrate")) == 1 })
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == "api" && r.spec.Release == "c2" {
				t.Fatalf("a new api replica started before the migration finished")
			}
		}
	}
	h.until("c2 finishes", 5*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
}

func TestAMigrateCommandRunsInTheComponentsImage(t *testing.T) {
	h := newHarness(t)
	withCommand := strings.Replace(testSpec, "    migrate: migrate\n", "    migrate: [dotnet, Api.dll, migrate]\n", 1)
	h.deploy("c1", withCommand, nil)
	var task api.ReplicaSpec
	h.until("the api's migration runs", 2*time.Minute, func() bool {
		for _, a := range h.agents {
			for _, r := range a.replicas {
				if r.spec.Kind == api.KindTask && r.spec.Component == "api" {
					task = r.spec
					return true
				}
			}
		}
		return false
	})
	if !strings.HasSuffix(task.Image, "/protostrap/api:v1") || strings.Join(task.Command, " ") != "dotnet Api.dll migrate" {
		t.Errorf("migration task %+v, want the api's image with the migrate command", task)
	}
	h.until("c1 finishes", 5*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })

	// A release that changes the api's image migrates again; one that doesn't, doesn't.
	h.deploy("c2", withCommand, map[string]string{"web": "v2"})
	h.until("c2 finishes", 5*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.deploy("c3", withCommand, map[string]string{"web": "v2", "api": "v3"})
	h.until("c3 finishes", 5*time.Minute, func() bool { return h.rollout("c3") == RolloutDone })
	if got := h.ids["api"]; got < 2 {
		t.Errorf("api ids %d", got)
	}
	if n := countLog(h, `"migration"`); n < 2 {
		t.Errorf("migrations logged %d times, want one for c1 and one for c3", n)
	}
}

func TestAFailedMigrationRollsBack(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.taskFails["migrate"] = true
	h.deploy("c2", testSpec, map[string]string{"api": "v2", "migrate": "v2"})
	h.until("c2 fails", 3*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if !strings.Contains(h.rolloutReason("c2"), "migrate") {
		t.Errorf("reason %q doesn't name the migration", h.rolloutReason("c2"))
	}
	st := h.s.Status()
	if st.Target != "c1" || st.LastGood != "c1" {
		t.Errorf("target %q, last good %q, want both back on c1", st.Target, st.LastGood)
	}
	h.run(time.Minute)
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == "api" && r.spec.Release == "c2" {
				t.Errorf("a c2 api replica runs after the rollback")
			}
		}
	}
	if h.ready("api") != 2 {
		t.Errorf("api ready %d after the rollback, want 2", h.ready("api"))
	}
}

func TestAFailingReplicaRollsBackTheWholeRelease(t *testing.T) {
	h := newHarness(t)
	h.settle()
	webBefore := h.running("web")
	h.failOnStart["api"] = true
	h.deploy("c2", testSpec, map[string]string{"api": "v2", "web": "v2"})
	h.until("c2 fails", 3*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if !strings.Contains(h.rolloutReason("c2"), "api") {
		t.Errorf("reason %q doesn't name the api", h.rolloutReason("c2"))
	}
	h.failOnStart["api"] = false
	h.until("everything converges to c1", 3*time.Minute, func() bool {
		for _, c := range []string{"api", "web"} {
			for _, r := range h.replicas(c) {
				if r.Release != "c1" && r.Fingerprint != "" {
					if r.State != api.StateReady {
						return false
					}
				}
			}
		}
		return h.ready("api") == 2 && h.ready("web") == 1
	})
	// web was replaced with c2, which counts as healthy, but the whole release goes back.
	for _, r := range h.replicas("web") {
		if r.Release == "c2" {
			t.Errorf("web replica %s of c2 survived the rollback", r.ID)
		}
	}
	_ = webBefore
}

func TestAReplicaNeverReadyFailsAfterFiveMinutes(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.neverReady["api"] = true
	h.deploy("c2", testSpec, map[string]string{"api": "v2"})
	h.run(4 * time.Minute)
	if h.rollout("c2") != RolloutRunning {
		t.Fatalf("rollout %s before the ready timeout", h.rollout("c2"))
	}
	h.until("c2 fails", 3*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
}

func TestTheFirstReleaseFailingLeavesNothingRunning(t *testing.T) {
	h := newHarness(t)
	h.failOnStart["api"] = true
	h.deploy("c1", testSpec, nil)
	h.until("c1 fails", 5*time.Minute, func() bool { return h.rollout("c1") == RolloutFailed })
	st := h.s.Status()
	if st.LastGood != "" || st.Target != "" {
		t.Errorf("target %q, last good %q: with nothing to roll back to, nothing runs", st.Target, st.LastGood)
	}
	h.until("everything stops", time.Minute, func() bool {
		return len(h.running("api")) == 0 && len(h.running("postgres")) == 0 && len(h.running("edge")) == 0
	})
}

func TestANewerReleaseSupersedesOneRollingOut(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.readyAfter["api"] = 20
	h.deploy("c2", testSpec, map[string]string{"api": "v2"})
	h.run(20 * time.Second)
	h.readyAfter["api"] = 2
	h.deploy("c3", testSpec, map[string]string{"api": "v3"})
	if got := h.rollout("c2"); got != RolloutSuperseded {
		t.Errorf("c2 is %s, want superseded", got)
	}
	h.until("c3 finishes", 5*time.Minute, func() bool { return h.rollout("c3") == RolloutDone })
	h.until("only c3's api runs", time.Minute, func() bool {
		for _, r := range h.replicas("api") {
			if r.Release != "c3" {
				return false
			}
		}
		return len(h.running("api")) == 2
	})
}

func TestDeployingTheCurrentReleaseAgainIsDoneAtOnce(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.deploy("c1", testSpec, nil)
	h.step()
	h.step()
	if got := h.rollout("c1"); got != RolloutDone {
		t.Errorf("redeploying the running release is %s", got)
	}
}

func TestRollingBackIsDeployingAnEarlierRelease(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.deploy("c2", testSpec, map[string]string{"api": "v2"})
	h.until("c2 finishes", 5*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	if err := h.s.Deploy(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	h.until("c1 runs again", 5*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.until("the api is back on v1", time.Minute, func() bool {
		for _, a := range h.agents {
			for _, r := range a.replicas {
				if r.spec.Component == "api" && !strings.HasSuffix(r.spec.Image, ":v1") {
					return false
				}
			}
		}
		return true
	})
}

func TestDeployingAMissingReleaseFails(t *testing.T) {
	h := newHarness(t)
	err := h.s.Deploy(context.Background(), "abcdef1")
	if err == nil || !isNotFound(err) {
		t.Errorf("err %v, want not found", err)
	}
	if h.s.Status().Target != "" {
		t.Errorf("a missing release became the target")
	}
}

func TestAStatefulComponentStopsBeforeItsReplacementStarts(t *testing.T) {
	h := newHarness(t)
	h.settle()
	old := h.running("postgres")[0]
	h.deploy("c2", testSpec, map[string]string{"postgres": "17.3"})
	sawBoth := false
	h.until("c2 finishes", 5*time.Minute, func() bool {
		ids := h.running("postgres")
		running := 0
		for _, id := range ids {
			if r := h.agentOf(id).replicas[id]; r.state != api.StateStopping {
				running++
			}
		}
		if running > 1 {
			sawBoth = true
		}
		return h.rollout("c2") == RolloutDone
	})
	if sawBoth {
		t.Errorf("two PostgreSQL replicas ran at once: only one can own the volume")
	}
	if ids := h.running("postgres"); len(ids) != 1 || ids[0] == old || h.agentOf(ids[0]) != h.agents["i-data1"] {
		t.Errorf("postgres runs %v, want one new replica on data-1", ids)
	}
}

func TestANewProtoStrapVersionReachesTheAgents(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.deploy("c2", strings.Replace(testSpec, "protostrap: 1.4.0", "protostrap: 1.5.0", 1), nil)
	h.step()
	h.step()
	for _, a := range h.agents {
		if a.assignment.Version != "1.5.0" {
			t.Errorf("vm %s told version %q, want 1.5.0", a.id, a.assignment.Version)
		}
	}
	h.until("the new edge replaces the old", 3*time.Minute, func() bool {
		for _, a := range h.agents {
			for _, r := range a.replicas {
				if r.spec.Kind == api.KindEdge && !strings.HasSuffix(r.spec.Image, ":1.5.0") {
					return false
				}
			}
		}
		return len(h.running("edge")) == 1
	})
}

func TestRemovedComponentsStop(t *testing.T) {
	h := newHarness(t)
	h.settle()
	without := strings.Replace(testSpec, "    depends_on: [api]\n", "", 1)
	without = strings.Replace(without, "    web:\n      routes: [/web/]\n", "", 1)
	start := strings.Index(without, "\n  web:\n") + 1
	end := strings.Index(without, "  worker:\n")
	without = without[:start] + without[end:]
	h.deploy("c2", without, nil)
	h.until("web stops", 2*time.Minute, func() bool { return len(h.running("web")) == 0 })
	h.until("c2 finishes", time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
}

func countLog(h *harness, needle string) int { return strings.Count(h.log.String(), needle) }
