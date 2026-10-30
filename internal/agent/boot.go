package agent

import (
	"context"
	"fmt"
	"slices"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// Boot readies the VM (docs/protostrap.md#agent, "At boot"):
//
//  1. It checks Docker, the credential helper and the SSM Agent. If any fails, it abandons
//     the launch hook so the Auto Scaling group discards the VM.
//  2. It completes the launch hook without waiting for the scheduler, so VMs can launch
//     while data-1 is down.
//  3. It sets up the container firewall and the credentials redirect, and on an ingress
//     VM the NAT rules, then takes over the zone.
//  4. It joins the swarm, retrying until the scheduler answers.
func (a *Agent) Boot(ctx context.Context) error {
	abandon := func(err error) error {
		if a.hooks != nil {
			if herr := a.hooks.Complete(ctx, LaunchHook, HookAbandon); herr != nil {
				a.log.Error("abandoning the launch hook", "err", herr)
			}
		}
		return err
	}
	if err := a.rt.Ping(ctx); err != nil {
		return abandon(fmt.Errorf("boot check: docker: %w", err))
	}
	for _, c := range a.checks {
		if err := c.Run(ctx); err != nil {
			return abandon(fmt.Errorf("boot check: %s: %w", c.Name, err))
		}
	}
	if err := a.loadCapacity(ctx); err != nil {
		return abandon(fmt.Errorf("boot check: %w", err))
	}
	if a.hooks != nil {
		// After an agent restart the hook is long completed, and AWS says so, which is not
		// an error.
		if err := a.hooks.Complete(ctx, LaunchHook, HookContinue); err != nil {
			a.log.Info("launch hook not completed", "err", err)
		}
	}
	if _, err := a.fw.Ensure(ctx); err != nil {
		return fmt.Errorf("container firewall: %w", err)
	}
	if a.zone != nil {
		if err := a.zone.TakeOver(ctx); err != nil {
			return fmt.Errorf("taking over the zone: %w", err)
		}
	}
	switch notice, err := a.meta.Termination(ctx); {
	case err == nil && notice != nil:
		a.log.Info("the VM is being terminated: not joining the swarm")
	case a.cfg.SwarmManager:
		// data-1 runs the swarm's manager, which the scheduler looks after.
	default:
		if err := a.joinSwarm(ctx); err != nil {
			return err
		}
	}
	a.notifier.Notify("READY=1")
	a.log.Info("agent ready", "vm", a.cfg.InstanceID, "pool", a.cfg.Pool, "version", a.cfg.Version)
	return nil
}

func (a *Agent) loadCapacity(ctx context.Context) error {
	info, err := a.rt.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	a.capacity = api.Resources{CPU: int64(info.CPUs) * 1000, Memory: info.MemoryBytes >> 20}
	return nil
}

// joinSwarm asks the scheduler how to join the swarm and joins it, until both succeed.
func (a *Agent) joinSwarm(ctx context.Context) error {
	for {
		j, err := a.sched.Join(ctx)
		if err == nil {
			err = a.enterSwarm(ctx, j)
			if err == nil {
				return nil
			}
		}
		a.log.Warn("joining the swarm", "err", err)
		if err := a.clock.Sleep(ctx, a.cfg.JoinRetry); err != nil {
			return err
		}
	}
}

// enterSwarm joins the swarm j describes, unless the VM is in it already. A VM in another
// swarm, after data-1 was rebuilt at a new address, or stuck joining one, leaves it first.
func (a *Agent) enterSwarm(ctx context.Context, j api.Join) error {
	info, err := a.rt.Info(ctx)
	if err != nil {
		return fmt.Errorf("docker info: %w", err)
	}
	s := info.Swarm
	if s.State == "active" && slices.Contains(s.Managers, j.Manager) {
		return nil
	}
	if s.State != "" && s.State != "inactive" {
		a.log.Info("leaving a swarm to join another", "state", s.State, "managers", s.Managers, "manager", j.Manager)
		if err := a.rt.LeaveSwarm(ctx); err != nil {
			return fmt.Errorf("leaving the old swarm: %w", err)
		}
	}
	err = a.rt.JoinSwarm(ctx, container.Join{Token: j.Token, Managers: []string{j.Manager}, AdvertiseAddr: a.cfg.PrivateIP})
	if err != nil {
		return fmt.Errorf("joining the swarm: %w", err)
	}
	a.log.Info("joined the swarm", "manager", j.Manager)
	return nil
}
