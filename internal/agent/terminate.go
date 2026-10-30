package agent

import (
	"context"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// termination is AWS removing the VM (docs/vms.md#when-aws-removes-a-vm).
type termination struct {
	spot     bool
	detected time.Time
	deadline time.Time // It is when the replicas must be gone.

	// The fields below track the final drain, when the agent stops whatever is left itself.
	final    bool
	drainEnd time.Time // It is when the longest stop the agent began will be over.
}

func (t *termination) reason() string {
	if t.spot {
		return api.TerminationSpot
	}
	return api.TerminationLifecycle
}

// checkTermination reads instance metadata for the VM's removal. The deadline is fixed
// when it is first seen: the terminate hook's timeout from then, or the Spot interruption's
// time. A Spot interruption during a hook-driven drain takes over, since no hook can
// extend it.
func (a *Agent) checkTermination(ctx context.Context, now time.Time) {
	if a.term != nil && a.term.spot {
		return
	}
	n, err := a.meta.Termination(ctx)
	if err != nil {
		a.log.Debug("reading instance metadata", "err", err)
		return
	}
	if n == nil || (a.term != nil && !n.Spot) {
		return
	}
	t := &termination{spot: n.Spot, detected: now, deadline: now.Add(a.cfg.HookTimeout)}
	if n.Spot {
		t.deadline = n.At
		if t.deadline.IsZero() {
			t.deadline = now.Add(a.cfg.SpotWarning)
		}
	}
	a.term = t
	a.log.Warn("the VM is being terminated", "reason", t.reason(), "deadline", t.deadline)
}

// drain empties the VM before it goes. While the deadline is far off, the scheduler moves
// replicas, starting each new one elsewhere before the old one stops. The agent stops
// whatever is left itself once most of the deadline has passed, when the scheduler has
// gone silent, or at once on a Spot interruption, whose warning can't be extended. Once
// nothing is left it leaves the swarm and completes the terminate hook.
func (a *Agent) drain(ctx context.Context, now time.Time, containers []container.Container) error {
	t := a.term
	var longest time.Duration
	for _, c := range containers {
		longest = max(longest, a.stopTime(c))
	}

	if !t.final {
		silentSince := a.lastSync
		if t.detected.After(silentSince) {
			silentSince = t.detected
		}
		why := ""
		switch {
		case t.spot:
			why = "spot interruption"
		case now.Sub(silentSince) >= a.cfg.SilentAfter:
			why = "the scheduler is silent"
		case t.deadline.Sub(now) <= longest+a.cfg.DrainMargin:
			why = "the deadline is near"
		}
		if why != "" {
			t.final = true
			t.drainEnd = now.Add(longest + a.cfg.DrainMargin)
			a.log.Warn("stopping every replica left", "why", why, "replicas", len(containers))
		}
	}
	if t.final {
		a.mu.Lock()
		for _, o := range a.ops {
			if o.kind == opStart {
				o.cancel()
			}
		}
		a.mu.Unlock()
		for _, c := range containers {
			a.stop(ctx, c, a.stopTime(c))
		}
	}

	busy := len(a.opIDs()) > 0
	if len(containers) == 0 && !busy {
		if !a.cfg.SwarmManager {
			if err := a.rt.LeaveSwarm(ctx); err != nil {
				a.log.Error("leaving the swarm", "err", err)
			}
		}
		if a.hooks != nil {
			if err := a.hooks.Complete(ctx, TerminateHook, HookContinue); err != nil {
				a.log.Error("completing the terminate hook", "err", err)
			}
		}
		a.done = true
		a.log.Info("VM drained")
		return ErrTerminated
	}

	// Stretch the hook while replicas finish stopping, but no further than their stop
	// times need.
	if t.final && !t.spot && a.hooks != nil && t.deadline.Before(t.drainEnd) &&
		t.deadline.Sub(now) <= a.cfg.DrainMargin+a.cfg.SyncInterval {
		if err := a.hooks.Heartbeat(ctx, TerminateHook); err != nil {
			a.log.Error("extending the terminate hook", "err", err)
		} else {
			t.deadline = now.Add(a.cfg.HookTimeout)
		}
	}
	return nil
}
