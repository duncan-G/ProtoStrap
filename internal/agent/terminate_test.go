package agent

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

func TestTerminationIsReported(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"))
	h.meta.set(&Notice{})
	h.mustTick()
	term := h.report().Terminating
	if term == nil || term.Reason != api.TerminationLifecycle {
		t.Fatalf("terminating %+v", term)
	}
	if want := h.clock.Now().Add(h.cfg.HookTimeout); !term.Deadline.Equal(want) {
		t.Fatalf("deadline %v, want the terminate hook's timeout %v", term.Deadline, want)
	}
	// The deadline is fixed when the termination is first seen.
	h.clock.advance(5 * time.Second)
	h.mustTick()
	if got := h.report().Terminating.Deadline; !got.Equal(term.Deadline) {
		t.Fatalf("deadline moved to %v", got)
	}
}

func TestTerminationWaitsForTheSchedulerToMoveReplicas(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"))
	h.meta.set(&Notice{})
	h.rt.resetCalls()
	for range 3 {
		h.mustTick()
		h.clock.advance(5 * time.Second)
	}
	if _, _, _, stops, _ := h.rt.snapshot(); len(stops) != 0 {
		t.Fatal("with the deadline far off, the scheduler moves replicas, starting each new one first")
	}
	h.sched.assign(service("api-1")) // w-1's replacement is ready elsewhere.
	h.mustTick()
	if _, _, removes, _, _ := h.rt.snapshot(); !slices.Equal(removes, []string{"w-1"}) {
		t.Fatalf("removes %v", removes)
	}
}

func TestTerminationFinishesOnceEmpty(t *testing.T) {
	// In a scale-down, the scheduler has moved everything off before terminating the VM.
	h := newHarness(t)
	h.rt.info.Swarm.State = "active"
	h.running(worker("w-1"))
	h.sched.assign()
	h.mustTick()
	h.meta.set(&Notice{})
	err := h.tick()
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if h.rt.leaves != 1 {
		t.Fatal("must leave the swarm")
	}
	if !slices.Equal(h.hooks.completes, []string{"terminate CONTINUE"}) {
		t.Fatalf("completes %v", h.hooks.completes)
	}
	calls := h.rt.calls()
	if calls[len(calls)-1] != "leave-swarm" {
		t.Fatalf("leaving the swarm is the last thing done to Docker: %v", calls)
	}
	// Further cycles do nothing.
	syncs := h.sched.syncs()
	if err := h.tick(); !errors.Is(err, ErrTerminated) || h.sched.syncs() != syncs {
		t.Fatalf("after termination the agent is done: %v", err)
	}
}

func TestTerminationStopsWhatIsLeftNearTheDeadline(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1")) // Their stop times are 30s and 120s.
	h.meta.set(&Notice{})
	h.mustTick()
	deadline := h.report().Terminating.Deadline
	// Stop everything once the longest stop time and the margin no longer fit.
	h.clock.now = deadline.Add(-(120*time.Second + h.cfg.DrainMargin) - time.Second)
	h.mustTick()
	if _, _, _, stops, _ := h.rt.snapshot(); len(stops) != 0 {
		t.Fatalf("stopped too early: %v", stops)
	}
	h.clock.advance(2 * time.Second)
	err := h.tick()
	_, _, _, stops, _ := h.rt.snapshot()
	if len(stops) != 2 {
		t.Fatalf("stops %+v", stops)
	}
	for _, s := range stops {
		want := map[string]time.Duration{"api-1": 30 * time.Second, "w-1": 120 * time.Second}[s.Name]
		if s.Timeout != want {
			t.Errorf("%s stop %v, want its own stop time %v", s.Name, s.Timeout, want)
		}
	}
	if !errors.Is(err, ErrTerminated) {
		err = h.tick()
	}
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(h.hooks.completes, []string{"terminate CONTINUE"}) || h.rt.leaves != 1 {
		t.Fatalf("completes %v leaves %d", h.hooks.completes, h.rt.leaves)
	}
}

func TestTerminationWithTheSchedulerDownStopsEverything(t *testing.T) {
	h := newHarness(t)
	h.running(service("api-1"), worker("w-1"))
	h.sched.syncErr = errors.New("connection refused")
	h.meta.set(&Notice{})
	h.tick()
	h.clock.advance(20 * time.Second)
	h.tick()
	if _, _, _, stops, _ := h.rt.snapshot(); len(stops) != 0 {
		t.Fatal("a short silence is not the scheduler being down")
	}
	h.clock.advance(15 * time.Second)
	err := h.tick()
	if !errors.Is(err, ErrTerminated) {
		err = h.tick()
	}
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if names := h.rt.names(); len(names) != 0 {
		t.Fatalf("left running %v", names)
	}
	if !slices.Equal(h.hooks.completes, []string{"terminate CONTINUE"}) {
		t.Fatalf("completes %v", h.hooks.completes)
	}
}

func TestSpotInterruptionStopsWorkersInsideTheWarning(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool, cfg.Name = "compute", "compute-1" })
	long := worker("w-long")
	long.Stop = 300
	h.running(worker("w-1"), long)
	at := h.clock.Now().Add(2 * time.Minute)
	h.meta.set(&Notice{Spot: true, At: at})
	err := h.tick()
	_, _, _, stops, _ := h.rt.snapshot()
	if len(stops) != 2 {
		t.Fatalf("spot gives two minutes and no hook extends it: stop at once, got %+v", stops)
	}
	for _, s := range stops {
		if s.Timeout != 90*time.Second {
			t.Errorf("%s: stop cut to %v, want 90s", s.Name, s.Timeout)
		}
	}
	term := h.report().Terminating
	if term == nil || term.Reason != api.TerminationSpot || !term.Deadline.Equal(at) {
		t.Fatalf("terminating %+v", term)
	}
	if !errors.Is(err, ErrTerminated) {
		err = h.tick()
	}
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if h.hooks.heartbeats != 0 {
		t.Fatal("no hook can extend a spot interruption")
	}
}

func TestSpotStopNeverExceedsTheReplicasOwn(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool = "compute" })
	h.running(service("api-1")) // Its stop time is 30s.
	h.meta.set(&Notice{Spot: true, At: h.clock.Now().Add(2 * time.Minute)})
	h.tick()
	_, _, _, stops, _ := h.rt.snapshot()
	if len(stops) != 1 || stops[0].Timeout != 30*time.Second {
		t.Fatalf("stops %+v", stops)
	}
}

func TestSpotCapsStopsTheSchedulerAsksFor(t *testing.T) {
	// The scheduler drops the worker in the same cycle the Spot notice arrives: its stop
	// must still fit inside the warning.
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool = "compute" })
	h.running(worker("w-1"), worker("w-2")) // Each one's stop time is 120s.
	h.meta.set(&Notice{Spot: true, At: h.clock.Now().Add(2 * time.Minute)})
	h.sched.assign(worker("w-2"))
	h.tick()
	_, _, _, stops, _ := h.rt.snapshot()
	if len(stops) != 2 {
		t.Fatalf("stops %+v", stops)
	}
	for _, s := range stops {
		if s.Timeout != 90*time.Second {
			t.Errorf("%s: stop %v, want the spot limit of 90s", s.Name, s.Timeout)
		}
	}
}

func TestSpotNoticeWithoutATime(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool = "compute" })
	h.meta.set(&Notice{Spot: true})
	h.tick()
	if got := h.report().Terminating.Deadline; !got.Equal(epoch.Add(2 * time.Minute)) {
		t.Fatalf("deadline %v", got)
	}
}

func TestTerminateHookIsExtendedWhileWorkersFinish(t *testing.T) {
	// The hook timeout is shorter than the worker's stop time, so the deadline stretches while
	// jobs are still running, up to the longest stop time of the VM's workers.
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) {
		cfg.Pool = "compute"
		cfg.HookTimeout = time.Minute
	})
	h.running(worker("w-1")) // Its stop time is 120s.
	gate := make(chan struct{})
	h.rt.mu.Lock()
	h.rt.stopGate = gate
	h.rt.mu.Unlock()
	h.meta.set(&Notice{})
	h.a.Tick(t.Context()) // Too little time is left, so the agent stops the worker now.
	eventually(t, "the worker to be stopped", func() bool {
		_, _, _, stops, _ := h.rt.snapshot()
		return len(stops) == 1
	})
	for range 40 { // The 40 cycles cover 200 seconds.
		h.clock.advance(5 * time.Second)
		h.a.Tick(t.Context())
	}
	beats := h.hooks.heartbeats
	if beats == 0 {
		t.Fatal("the hook must be extended while the worker finishes")
	}
	if beats > 3 {
		t.Fatalf("extended %d times: no further than the worker's stop time needs", beats)
	}
	if h.report().Terminating.Deadline.Before(epoch.Add(120 * time.Second)) {
		t.Fatalf("reported deadline %v doesn't cover the stop", h.report().Terminating.Deadline)
	}
	close(gate)
	h.a.Wait()
	if err := h.tick(); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
}

func TestNoNewReplicasWhileTerminating(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	h.meta.set(&Notice{})
	h.sched.assign(worker("w-1"), worker("w-2"))
	h.rt.resetCalls()
	h.mustTick()
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatalf("a VM on its way out starts nothing: %v", creates)
	}
}

func TestTerminationCancelsPulls(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool = "compute" })
	gate := make(chan struct{})
	defer close(gate)
	h.rt.pullGate = gate
	h.sched.assign(worker("w-1"))
	h.a.Tick(t.Context()) // The agent is now pulling the worker's image.
	h.meta.set(&Notice{Spot: true, At: h.clock.Now().Add(2 * time.Minute)})
	err := h.tick()
	if !errors.Is(err, ErrTerminated) {
		err = h.tick()
	}
	if !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatal("a pull must not finish into a dying VM")
	}
}

func TestTheHookIsCompletedEvenIfLeavingFails(t *testing.T) {
	h := newHarness(t)
	h.meta.set(&Notice{})
	h.hooks.err = nil
	h.rt.mu.Lock()
	h.rt.info.Swarm.State = "active"
	h.rt.mu.Unlock()
	failing := &leaveFails{fakeRuntime: h.rt}
	h.a.rt = failing
	if err := h.tick(); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
	if !slices.Equal(h.hooks.completes, []string{"terminate CONTINUE"}) {
		t.Fatalf("completes %v", h.hooks.completes)
	}
}

type leaveFails struct{ *fakeRuntime }

func (l *leaveFails) LeaveSwarm(context.Context) error { return errors.New("node is a manager") }

func TestTerminationOnAVMWithoutHooks(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { d.Hooks = nil; h.hooks = nil })
	h.meta.set(&Notice{Spot: true, At: epoch.Add(time.Minute)})
	if err := h.tick(); !errors.Is(err, ErrTerminated) {
		t.Fatalf("got %v", err)
	}
}

func TestMetadataErrorsAreNotTerminations(t *testing.T) {
	h := newHarness(t)
	h.running(worker("w-1"))
	h.meta.err = errors.New("metadata timeout")
	h.mustTick()
	if h.report().Terminating != nil {
		t.Fatal("a metadata error is not a termination")
	}
	if names := h.rt.names(); !slices.Equal(names, []string{"w-1"}) {
		t.Fatalf("replicas %v", names)
	}
}
