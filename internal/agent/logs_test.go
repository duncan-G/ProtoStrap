package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/infralog"
)

// An agent's log records go to the scheduler with its reports, so they outlive the VM and
// show in the operator UI. The VM's journal still gets every record.

func withOutbox(o *infralog.Outbox) func(*harness, *Config, *Deps) {
	return func(_ *harness, _ *Config, d *Deps) {
		d.Logs = o
		d.Logger = slog.New(o.Handler(slog.DiscardHandler))
	}
}

// logged is the messages a report carried, of those named.
func logged(r api.Report, names ...string) []string {
	var out []string
	for _, rec := range r.Logs {
		if len(names) == 0 || slices.Contains(names, rec.Msg) {
			out = append(out, rec.Msg)
		}
	}
	return out
}

func TestASyncCarriesTheRecordsLoggedSinceTheLastOne(t *testing.T) {
	o := &infralog.Outbox{}
	h := newHarness(t, withOutbox(o))
	h.a.log.Info("one", "component", "api")
	h.mustTick()
	if got := logged(h.report(), "one"); len(got) != 1 {
		t.Fatalf("report logs %v", logged(h.report()))
	}
	if a := h.report().Logs[slices.IndexFunc(h.report().Logs, func(r api.LogRecord) bool { return r.Msg == "one" })]; a.Level != "INFO" || len(a.Attrs) != 1 {
		t.Errorf("record %+v", a)
	}
	h.mustTick()
	if got := logged(h.report(), "one"); len(got) != 0 {
		t.Errorf("sent again: %v", got)
	}
}

func TestTheAgentsOwnRecordsAboutAReplicaGoToTheScheduler(t *testing.T) {
	o := &infralog.Outbox{}
	h := newHarness(t, withOutbox(o))
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	var found bool
	for _, r := range h.report().Logs {
		if r.Msg == "starting a replica" && slices.Contains(r.Attrs, api.LogAttr{Key: "replica", Value: "api-1"}) {
			found = true
		}
	}
	if !found {
		t.Errorf("report logs %v: starting a replica goes with the next sync", logged(h.report()))
	}
}

func TestRecordsWaitWhileTheSchedulerIsDown(t *testing.T) {
	o := &infralog.Outbox{}
	h := newHarness(t, withOutbox(o))
	h.sched.syncErr = errors.New("connection refused")
	h.a.log.Warn("two")
	h.tick()
	if got := logged(h.report(), "two"); len(got) != 1 {
		t.Fatalf("the failed sync carried %v", logged(h.report()))
	}
	h.sched.syncErr = nil
	h.a.log.Info("three")
	h.mustTick()
	if got := logged(h.report(), "two", "three"); fmt.Sprint(got) != "[two three]" {
		t.Errorf("got %v: a failed sync's records go again, before the newer ones", got)
	}
}

func TestOneSyncCarriesAtMost500Records(t *testing.T) {
	o := &infralog.Outbox{}
	h := newHarness(t, withOutbox(o))
	for i := range 600 {
		h.a.log.Info(fmt.Sprint(i))
	}
	h.mustTick()
	if n := len(h.report().Logs); n != 500 || h.report().Logs[0].Msg != "0" {
		t.Fatalf("%d records", n)
	}
	h.mustTick()
	if first := h.report().Logs[0].Msg; first != "500" || len(logged(h.report())) < 100 {
		t.Errorf("second sync starts at %q with %d records", first, len(h.report().Logs))
	}
}

func TestWithoutAnOutboxReportsCarryNoRecords(t *testing.T) {
	h := newHarness(t)
	h.a.log.Info("one")
	h.mustTick()
	if h.report().Logs != nil {
		t.Errorf("logs %v", h.report().Logs)
	}
}
