package ingress

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

type fakeEC2 struct {
	holder    string
	instances map[string]instance
	calls     []string
	failOn    string
}

type instance struct {
	launched time.Time
	state    string
}

func (f *fakeEC2) fail(call string) error {
	f.calls = append(f.calls, call)
	if f.failOn != "" && f.failOn == call {
		return errors.New(call + " failed")
	}
	return nil
}

func (f *fakeEC2) AddressHolder(_ context.Context, alloc string) (string, error) {
	return f.holder, f.fail("describe-address " + alloc)
}

func (f *fakeEC2) Instance(_ context.Context, id string) (time.Time, string, error) {
	i, ok := f.instances[id]
	if !ok {
		return time.Time{}, "terminated", f.fail("describe-instance " + id)
	}
	return i.launched, i.state, f.fail("describe-instance " + id)
}

func (f *fakeEC2) AssociateAddress(_ context.Context, alloc, id string) error {
	if err := f.fail("associate " + alloc + " " + id); err != nil {
		return err
	}
	f.holder = id
	return nil
}

func (f *fakeEC2) RouteVia(_ context.Context, rtb, cidr, id string) error {
	return f.fail("route " + rtb + " " + cidr + " " + id)
}

func (f *fakeEC2) DisableSourceDestCheck(_ context.Context, id string) error {
	return f.fail("source-dest-check " + id)
}

var (
	earlier = time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)
	later   = earlier.Add(time.Hour)
)

func takeover(ec2 *fakeEC2) *Takeover {
	return &Takeover{EC2: ec2, InstanceID: "i-new", AllocationID: "eipalloc-1", RouteTables: []string{"rtb-a", "rtb-b"}}
}

func has(calls []string, call string) bool { return slices.Contains(calls, call) }

func TestTakesOverAnUnheldZone(t *testing.T) {
	ec2 := &fakeEC2{instances: map[string]instance{"i-new": {later, "running"}}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"source-dest-check i-new", "associate eipalloc-1 i-new",
		"route rtb-a 0.0.0.0/0 i-new", "route rtb-b 0.0.0.0/0 i-new"} {
		if !has(ec2.calls, want) {
			t.Errorf("missing %q in %v", want, ec2.calls)
		}
	}
}

func TestTakesOverFromAnOlderIngressVM(t *testing.T) {
	// This is a planned replacement: the new VM starts beside the old one and takes the zone.
	ec2 := &fakeEC2{holder: "i-old", instances: map[string]instance{
		"i-old": {earlier, "running"}, "i-new": {later, "running"},
	}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ec2.holder != "i-new" || !has(ec2.calls, "route rtb-a 0.0.0.0/0 i-new") {
		t.Fatalf("calls %v", ec2.calls)
	}
}

func TestTakesOverFromAFailedIngressVM(t *testing.T) {
	ec2 := &fakeEC2{holder: "i-old", instances: map[string]instance{
		"i-old": {later.Add(time.Hour), "shutting-down"}, "i-new": {later, "running"},
	}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ec2.holder != "i-new" {
		t.Fatalf("a dying VM's zone must be taken, calls %v", ec2.calls)
	}
}

func TestTakesOverFromAVanishedIngressVM(t *testing.T) {
	ec2 := &fakeEC2{holder: "i-gone", instances: map[string]instance{"i-new": {later, "running"}}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ec2.holder != "i-new" {
		t.Fatalf("calls %v", ec2.calls)
	}
}

func TestLeavesTheZoneToANewerIngressVM(t *testing.T) {
	// The old VM's agent restarts during a planned replacement: it must not take the zone back.
	ec2 := &fakeEC2{holder: "i-newer", instances: map[string]instance{
		"i-newer": {later.Add(time.Hour), "running"}, "i-new": {later, "running"},
	}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ec2.holder != "i-newer" {
		t.Fatal("zone taken back from a newer VM")
	}
	for _, c := range ec2.calls {
		if c[:5] == "route" || c[:9] == "associate" {
			t.Errorf("must not touch the zone: %s", c)
		}
	}
}

func TestAlreadyHoldingTheZoneOnlyChecksRoutes(t *testing.T) {
	// The agent restarts on the VM that already holds the zone.
	ec2 := &fakeEC2{holder: "i-new", instances: map[string]instance{"i-new": {later, "running"}}}
	if err := takeover(ec2).TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range ec2.calls {
		if c[:9] == "associate" {
			t.Errorf("must not re-associate its own address: %v", ec2.calls)
		}
	}
	if !has(ec2.calls, "route rtb-a 0.0.0.0/0 i-new") {
		t.Fatalf("routes must still point here: %v", ec2.calls)
	}
}

func TestTakeoverFailuresAreReturned(t *testing.T) {
	for _, call := range []string{"describe-address eipalloc-1", "associate eipalloc-1 i-new", "route rtb-b 0.0.0.0/0 i-new", "source-dest-check i-new"} {
		ec2 := &fakeEC2{instances: map[string]instance{"i-new": {later, "running"}}, failOn: call}
		if err := takeover(ec2).TakeOver(t.Context()); err == nil {
			t.Errorf("%s failing should fail the takeover", call)
		}
	}
}

func TestRoutesMoveAfterTheAddress(t *testing.T) {
	// The NAT rules are in place before the routes move, and the address is ours first, so
	// forwarded traffic leaves from the zone's address.
	ec2 := &fakeEC2{instances: map[string]instance{"i-new": {later, "running"}}}
	takeover(ec2).TakeOver(t.Context())
	assoc := slices.Index(ec2.calls, "associate eipalloc-1 i-new")
	route := slices.Index(ec2.calls, "route rtb-a 0.0.0.0/0 i-new")
	sdc := slices.Index(ec2.calls, "source-dest-check i-new")
	if !(sdc < assoc && assoc < route) {
		t.Fatalf("order %v", ec2.calls)
	}
}
