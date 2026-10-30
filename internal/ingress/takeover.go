// Package ingress takes over a zone's way in and out for a new ingress VM: the zone's
// Elastic IP and the private subnets' route to the internet (docs/vms.md#ingress).
package ingress

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// EC2 is what a takeover needs from AWS.
type EC2 interface {
	AddressHolder(ctx context.Context, allocationID string) (string, error)
	Instance(ctx context.Context, id string) (launched time.Time, state string, err error)
	AssociateAddress(ctx context.Context, allocationID, instanceID string) error
	RouteVia(ctx context.Context, routeTable, cidr, instanceID string) error
	DisableSourceDestCheck(ctx context.Context, instanceID string) error
}

// Takeover moves a zone to this VM.
type Takeover struct {
	EC2        EC2
	InstanceID string
	// AllocationID is the zone's Elastic IP.
	AllocationID string
	// RouteTables are the zone's private route tables.
	RouteTables []string
	Logger      *slog.Logger
}

// TakeOver makes this VM the zone's ingress VM. The newest ingress VM holds the zone, so
// a planned replacement takes it from the VM it replaces, and that VM's agent, if it
// restarts meanwhile, leaves the zone alone. A holder that isn't running loses the zone.
func (t *Takeover) TakeOver(ctx context.Context) error {
	holder, err := t.EC2.AddressHolder(ctx, t.AllocationID)
	if err != nil {
		return fmt.Errorf("takeover: %w", err)
	}
	if holder != t.InstanceID {
		if holder != "" {
			newer, err := t.newerThanMe(ctx, holder)
			if err != nil {
				return err
			}
			if newer {
				t.log().Info("a newer ingress VM holds the zone; leaving it", "holder", holder)
				return nil
			}
		}
		// NAT forwards traffic that isn't the VM's own.
		if err := t.EC2.DisableSourceDestCheck(ctx, t.InstanceID); err != nil {
			return fmt.Errorf("takeover: %w", err)
		}
		if err := t.EC2.AssociateAddress(ctx, t.AllocationID, t.InstanceID); err != nil {
			return fmt.Errorf("takeover: %w", err)
		}
		t.log().Info("took over the zone's address", "from", holder)
	}
	for _, rtb := range t.RouteTables {
		if err := t.EC2.RouteVia(ctx, rtb, "0.0.0.0/0", t.InstanceID); err != nil {
			return fmt.Errorf("takeover: route table %s: %w", rtb, err)
		}
	}
	return nil
}

func (t *Takeover) newerThanMe(ctx context.Context, holder string) (bool, error) {
	theirs, state, err := t.EC2.Instance(ctx, holder)
	if err != nil {
		return false, fmt.Errorf("takeover: %w", err)
	}
	if state != "running" && state != "pending" {
		return false, nil
	}
	mine, _, err := t.EC2.Instance(ctx, t.InstanceID)
	if err != nil {
		return false, fmt.Errorf("takeover: %w", err)
	}
	return theirs.After(mine), nil
}

func (t *Takeover) log() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
