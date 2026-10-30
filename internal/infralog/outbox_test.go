package infralog

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/api"
)

// An agent's outbox holds its log records until a sync carries them to the scheduler. The
// agent's own log, in the VM's journal, still gets every record.

func outMsgs(rs []api.LogRecord) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Msg)
	}
	return strings.Join(out, ",")
}

func TestTheOutboxHoldsInfoAndAbove(t *testing.T) {
	o := &Outbox{}
	var journal bytes.Buffer
	log := slog.New(o.Handler(slog.NewJSONHandler(&journal, &slog.HandlerOptions{Level: slog.LevelDebug})))
	log.Debug("probing")
	log.Info("starting a replica", "replica", "api-aaaaaa", "component", "api")
	log.With("vm", "i-0a1b").WithGroup("op").Warn("slow", "took", "3s")

	got := o.Take(10)
	if outMsgs(got) != "starting a replica,slow" {
		t.Fatalf("got %s", outMsgs(got))
	}
	if got[0].Level != "INFO" || got[0].Time.IsZero() || len(got[0].Attrs) != 2 || got[0].Attrs[0] != (api.LogAttr{Key: "replica", Value: "api-aaaaaa"}) {
		t.Errorf("first %+v", got[0])
	}
	if got[1].Level != "WARN" || len(got[1].Attrs) != 2 || got[1].Attrs[0].Key != "vm" || got[1].Attrs[1].Key != "op.took" {
		t.Errorf("second %+v", got[1])
	}
	if n := strings.Count(journal.String(), "\n"); n != 3 {
		t.Errorf("the journal got %d lines, want all 3", n)
	}
	if len(o.Take(10)) != 0 {
		t.Error("taken records are gone")
	}
}

func TestTakeAndReturn(t *testing.T) {
	o := &Outbox{}
	log := slog.New(o.Handler(slog.DiscardHandler))
	for i := range 3 {
		log.Info(fmt.Sprint(i + 1))
	}
	first := o.Take(2)
	if outMsgs(first) != "1,2" {
		t.Fatalf("got %s", outMsgs(first))
	}
	log.Info("4")
	// The sync failed: the records go back in front of the ones logged since.
	o.Return(first)
	if got := outMsgs(o.Take(10)); got != "1,2,3,4" {
		t.Errorf("got %s", got)
	}
}

func TestAFullOutboxDropsTheOldestAndSaysSo(t *testing.T) {
	// While the scheduler is unreachable, the newest records matter most.
	o := &Outbox{Limit: 3}
	log := slog.New(o.Handler(slog.DiscardHandler))
	for i := range 5 {
		log.Info(fmt.Sprint(i + 1))
	}
	got := o.Take(10)
	if len(got) != 4 || outMsgs(got[1:]) != "3,4,5" {
		t.Fatalf("got %s", outMsgs(got))
	}
	if got[0].Level != "WARN" || !strings.Contains(got[0].Msg, "2 log records dropped") {
		t.Errorf("notice %+v", got[0])
	}
	log.Info("6")
	if got := outMsgs(o.Take(10)); got != "6" {
		t.Errorf("got %s: the notice goes once", got)
	}
}

func TestReturningToAFullOutboxKeepsTheNewest(t *testing.T) {
	o := &Outbox{Limit: 3}
	log := slog.New(o.Handler(slog.DiscardHandler))
	log.Info("1")
	log.Info("2")
	taken := o.Take(10)
	log.Info("3")
	log.Info("4")
	o.Return(taken)
	got := o.Take(10)
	if len(got) != 4 || outMsgs(got[1:]) != "2,3,4" || !strings.Contains(got[0].Msg, "1 log record dropped") {
		t.Errorf("got %s", outMsgs(got))
	}
}

func TestTakeNothing(t *testing.T) {
	o := &Outbox{}
	if got := o.Take(10); got != nil {
		t.Errorf("got %v: an empty outbox adds nothing to the report", got)
	}
	o.Return(nil)
	if got := o.Take(10); got != nil {
		t.Errorf("got %v", got)
	}
}
