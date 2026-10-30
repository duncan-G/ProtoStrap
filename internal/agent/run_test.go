package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunBootsThenSyncsEveryInterval(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	h.clock.onSleep = func() {
		if h.sched.syncs() >= 3 {
			cancel()
		}
	}
	err := h.a.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if len(h.rt.joins) != 1 {
		t.Fatal("Run boots first")
	}
	if h.sched.syncs() != 3 {
		t.Fatalf("syncs %d", h.sched.syncs())
	}
	for _, d := range h.clock.sleeps {
		if d != 5*time.Second {
			t.Fatalf("cycles are 5 seconds apart, slept %v", h.clock.sleeps)
		}
	}
}

func TestRunKeepsGoingWhenTheSchedulerIsDown(t *testing.T) {
	h := newHarness(t)
	h.sched.syncErr = errors.New("connection refused")
	ctx, cancel := context.WithCancel(t.Context())
	h.clock.onSleep = func() {
		if h.sched.syncs() >= 5 {
			cancel()
		}
	}
	h.a.Run(ctx)
	if h.sched.syncs() != 5 {
		t.Fatalf("syncs %d", h.sched.syncs())
	}
}

func TestRunStopsWhenBootFails(t *testing.T) {
	h := newHarness(t)
	h.rt.pingErr = errors.New("docker down")
	if err := h.a.Run(t.Context()); err == nil {
		t.Fatal("expected an error, so systemd restarts the agent")
	}
	if h.sched.syncs() != 0 {
		t.Fatal("no sync without a boot")
	}
}

func TestRunReturnsAfterAnUpdate(t *testing.T) {
	h := newHarness(t)
	h.sched.assignment.Version = "1.4.1"
	err := h.a.Run(t.Context())
	if !errors.Is(err, ErrUpdated) {
		t.Fatalf("got %v", err)
	}
}

func TestRunIdlesAfterTermination(t *testing.T) {
	// systemd would restart an agent that exits, and it would join the swarm again.
	h := newHarness(t)
	h.meta.set(&Notice{Spot: true, At: epoch.Add(time.Minute)})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- h.a.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before being cancelled", err)
	default:
	}
	syncs := h.sched.syncs()
	time.Sleep(20 * time.Millisecond)
	if h.sched.syncs() != syncs {
		t.Fatal("no more cycles after termination")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run didn't return when cancelled")
	}
}
