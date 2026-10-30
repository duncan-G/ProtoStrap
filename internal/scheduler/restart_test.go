package scheduler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/release"
)

// If the scheduler stops, replicas keep running (docs/protostrap.md#scheduler). On start
// it reads state.json, listens to the agents for 15 seconds, adopts every running replica
// from its labels, and resumes. Until it has 5 minutes of signals, it scales nothing down
// and runs each component at no fewer than its last desired count.

func TestARestartedSchedulerAdoptsWhatRuns(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 2 // Demand is ceil(2000 / 350) = 6.
	h.until("scaled out to 6", 3*time.Minute, func() bool { return h.ready("api") == 6 && len(h.running("api")) == 6 })
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge"} {
		before[c] = h.running(c)
	}

	h.start() // A new scheduler process starts; the agents and the bucket carry on.
	h.step()
	for _, a := range h.agents {
		if a.lastErr == nil || !errors.Is(a.lastErr, ErrWarmingUp) {
			t.Fatalf("vm %s synced during the warm-up: %v", a.id, a.lastErr)
		}
	}
	h.run(30 * time.Second)
	for c, ids := range before {
		if got := h.running(c); !slices.Equal(got, ids) {
			t.Errorf("%s replicas %v became %v across a restart", c, ids, got)
		}
	}
	if st := h.s.Status(); st.Target != "c1" || st.LastGood != "c1" {
		t.Errorf("target %q, last good %q after a restart", st.Target, st.LastGood)
	}
}

func TestARestartScalesNothingDownForFiveMinutes(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 2
	h.until("scaled out to 6", 3*time.Minute, func() bool { return h.ready("api") >= 6 })
	n := h.ready("api")

	// The scheduler restarts while load has gone: from nothing, demand would be the minimum.
	h.totalCPU["api"] = 0
	h.start()
	h.run(4 * time.Minute)
	if got := len(h.running("api")); got < n {
		t.Errorf("api fell from %d to %d within 5 minutes of a restart", n, got)
	}
	h.until("then it scales down", 8*time.Minute, func() bool { return len(h.running("api")) == 2 })
}

func TestARestartFromNothingRunsTheLastDesiredCount(t *testing.T) {
	// After an outage, a component starts at no fewer than its last desired count, so
	// services don't restart at their minimum into a wave of retries
	// (docs/vms.md#starting-from-nothing).
	h := newHarness(t)
	h.publish("c1", testSpec, nil)
	h.store.state = &release.State{Target: "c1", LastGood: "c1", Desired: map[string]int{"api": 6, "web": 3}}
	h.start()
	h.until("api and web run their last desired counts", 5*time.Minute, func() bool { return h.ready("api") == 6 && h.ready("web") == 3 })
}

func TestAReplicaOfAnUnknownReleaseIsAdoptedAndReplaced(t *testing.T) {
	h := newHarness(t)
	h.settle()
	// A replica whose release is gone from the bucket is adopted, so it doesn't vanish while
	// the scheduler warms up, then replaced since its fingerprint doesn't match.
	id := h.running("web")[0]
	a := h.agentOf(id)
	a.replicas[id].spec.Release = "deadbee"
	a.replicas[id].spec.Fingerprint = "0000000000000000"
	h.start()
	h.until("web is replaced", 3*time.Minute, func() bool {
		ids := h.running("web")
		return len(ids) == 1 && ids[0] != id
	})
}

func TestStateIsSavedWhenItChanges(t *testing.T) {
	h := newHarness(t)
	h.settle()
	saves := h.store.saves
	h.run(2 * time.Minute)
	if h.store.saves != saves {
		t.Errorf("state.json written %d times while nothing changed", h.store.saves-saves)
	}
	h.deploy("c2", testSpec, map[string]string{"web": "v2"})
	h.step()
	h.step()
	st, _ := h.store.State(context.Background())
	if st.Target != "c2" {
		t.Errorf("state.json's target %q after a deploy", st.Target)
	}
}

func TestSchedulerDecisionsAreLoggedWithReasons(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.run(6 * time.Minute)
	h.totalCPU["api"] = 2 * 500 * 2
	h.until("scaled out", 3*time.Minute, func() bool { return desired(h, "api") > 2 })
	log := h.log.String()
	for _, want := range []string{`"msg":"desired replicas"`, `"component":"api"`, `"reason":`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no %s", want)
		}
	}
	h.run(time.Minute)
	if !strings.Contains(h.log.String(), `"msg":"signals"`) {
		t.Errorf("no signal snapshot within a minute")
	}
}

func TestARestartMidRolloutCanStillRollBack(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.failOnStart["api"] = true
	h.deploy("c2", testSpec, map[string]string{"api": "v2"})
	h.step()
	h.start() // The scheduler restarts with c2 rolling out and c1 the last good release.
	h.until("c2 fails", 4*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if st := h.s.Status(); st.Target != "c1" {
		t.Errorf("target %q after the failure, want the last good c1", st.Target)
	}
}
