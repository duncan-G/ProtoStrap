package infralog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// The infra log holds ProtoStrap's own records, the scheduler's and every agent's, for the
// operator UI, the logs API and the reporter. Services' own logs are elsewhere.

var t0 = time.Date(2026, 10, 29, 10, 30, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newLog(c *clock) *Log { return &Log{Now: c.Now} }

// rec is an agent's record, as a sync carries it.
func rec(at time.Time, level, msg string, kv ...string) api.LogRecord {
	r := api.LogRecord{Time: at, Level: level, Msg: msg}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Attrs = append(r.Attrs, api.LogAttr{Key: kv[i], Value: kv[i+1]})
	}
	return r
}

func attr(r Record, key string) string {
	for _, a := range r.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

func msgs(rs []Record) string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Msg)
	}
	return strings.Join(out, ",")
}

func query(t *testing.T, l *Log, f Filter) Result {
	t.Helper()
	res, err := l.Query(context.Background(), f)
	if err != nil {
		t.Fatalf("query %+v: %v", f, err)
	}
	return res
}

func TestTheSchedulersRecordsAreKept(t *testing.T) {
	c := &clock{now: t0}
	l := newLog(c)
	var out bytes.Buffer
	log := slog.New(l.Handler(slog.NewJSONHandler(&out, nil), SourceScheduler, "i-0data1", "data-1"))
	log.Info("desired replicas", "component", "api", "from", 2, "to", 3, "reason", "cpu 85% of 70%")
	log.Warn("VM is silent: placing its replicas elsewhere", "vm", "i-0a1b", "pool", "general")
	log.Debug("not kept")

	res := query(t, l, Filter{})
	if len(res.Records) != 2 || res.Latest != 2 {
		t.Fatalf("records %+v, latest %d", res.Records, res.Latest)
	}
	first := res.Records[0]
	if first.Seq != 1 || first.Source != SourceScheduler || first.VM != "i-0data1" || first.VMName != "data-1" ||
		first.Level != "INFO" || first.Msg != "desired replicas" || first.Component != "api" || first.Time.IsZero() {
		t.Errorf("first %+v", first)
	}
	var keys []string
	for _, a := range first.Attrs {
		keys = append(keys, a.Key)
	}
	if strings.Join(keys, ",") != "component,from,to,reason" || attr(first, "to") != "3" {
		t.Errorf("attrs %+v: they keep the order they were logged in, as text", first.Attrs)
	}
	if res.Records[1].Level != "WARN" || res.Records[1].Component != "" {
		t.Errorf("second %+v", res.Records[1])
	}
	// The scheduler's own log still gets every record, debug included if it wants it.
	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Errorf("the scheduler's own log got %d lines:\n%s", n, out.String())
	}
}

func TestTheHandlerFlattensGroupsAndKeepsWithAttrs(t *testing.T) {
	l := newLog(&clock{now: t0})
	log := slog.New(l.Handler(slog.DiscardHandler, SourceScheduler, "", "data-1")).With("release", "9f2c1e7")
	log.WithGroup("signal").Info("reading", "name", "cpu", slog.Group("value", "now", 85))
	log.Info("plain", slog.Group("", "inline", "yes"))

	res := query(t, l, Filter{})
	if len(res.Records) != 2 {
		t.Fatalf("records %+v", res.Records)
	}
	r := res.Records[0]
	if attr(r, "release") != "9f2c1e7" || attr(r, "signal.name") != "cpu" || attr(r, "signal.value.now") != "85" {
		t.Errorf("attrs %+v", r.Attrs)
	}
	if attr(res.Records[1], "inline") != "yes" || attr(res.Records[1], "release") != "9f2c1e7" {
		t.Errorf("attrs %+v", res.Records[1].Attrs)
	}
}

func TestTheHandlerIsSafeFromManyGoroutines(t *testing.T) {
	l := newLog(&clock{now: t0})
	log := slog.New(l.Handler(slog.DiscardHandler, SourceScheduler, "", "data-1"))
	done := make(chan bool)
	for g := range 8 {
		go func() {
			for i := range 50 {
				log.Info("m", "g", g, "i", i)
			}
			done <- true
		}()
	}
	for range 8 {
		<-done
	}
	if res := query(t, l, Filter{Limit: 5000}); len(res.Records) != 400 || res.Latest != 400 {
		t.Errorf("%d records, latest %d", len(res.Records), res.Latest)
	}
}

func TestAgentRecordsAreAttributedToTheirVMAndComponent(t *testing.T) {
	c := &clock{now: t0}
	l := newLog(c)
	l.AddAgent("i-0a1b2c3d", "general-3", []api.LogRecord{
		rec(t0.Add(-3*time.Second), "INFO", "starting a replica", "replica", "api-x7k2qa", "image", "api:c-91f0"),
		rec(t0.Add(-2*time.Second), "ERROR", "replica failed", "replica", "document-import-ab3d5f", "reason", "exited with code 137"),
		rec(t0.Add(-time.Second), "ERROR", "backup failed", "component", "postgres", "err", "disk full"),
		rec(t0, "WARN", "firewall rules were missing and have been put back"),
	})
	res := query(t, l, Filter{})
	if msgs(res.Records) != "starting a replica,replica failed,backup failed,firewall rules were missing and have been put back" {
		t.Fatalf("records %s", msgs(res.Records))
	}
	for _, r := range res.Records {
		if r.Source != SourceAgent || r.VM != "i-0a1b2c3d" || r.VMName != "general-3" {
			t.Errorf("%q: source %q vm %q %q", r.Msg, r.Source, r.VM, r.VMName)
		}
	}
	// A replica's ID is its component's name and six characters, so its records belong to
	// the component even when the agent didn't name it.
	for i, want := range []string{"api", "document-import", "postgres", ""} {
		if got := res.Records[i].Component; got != want {
			t.Errorf("%q: component %q, want %q", res.Records[i].Msg, got, want)
		}
	}
}

func TestAComponentAttributeWinsOverTheReplicaID(t *testing.T) {
	l := newLog(&clock{now: t0})
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "m", "replica", "api-x7k2qa", "component", "edge")})
	if r := query(t, l, Filter{}).Records[0]; r.Component != "edge" {
		t.Errorf("component %q", r.Component)
	}
}

func TestAgentRecordsAreTrimmedToSize(t *testing.T) {
	// Agents run as root on VMs that run the application's containers, so what they send is
	// kept within bounds, whatever it is.
	c := &clock{now: t0}
	l := newLog(c)
	long := strings.Repeat("x", 10000)
	var many []string
	for i := range 40 {
		many = append(many, fmt.Sprintf("k%d", i), "v")
	}
	l.AddAgent("i-1", "general-1", []api.LogRecord{
		rec(t0, "info", long, "err", long),
		rec(t0, "LOUD", "unknown level", many...),
		rec(time.Time{}, "INFO", "no time"),
		rec(t0.Add(time.Hour), "INFO", "from the future"),
		rec(t0.Add(-25*time.Hour), "INFO", "older than the window"),
	})
	res := query(t, l, Filter{Since: t0.Add(-24 * time.Hour)})
	if msgs(res.Records) == "" || len(res.Records) != 4 {
		t.Fatalf("records %d: a record older than the memory's window is dropped", len(res.Records))
	}
	byMsg := map[string]Record{}
	for _, r := range res.Records {
		byMsg[r.Msg[:min(len(r.Msg), 20)]] = r
	}
	first := res.Records[0]
	if len(first.Msg) > 4096+len("…") || len(attr(first, "err")) > 2048+len("…") || first.Level != "INFO" {
		t.Errorf("msg %d bytes, err %d bytes, level %q", len(first.Msg), len(attr(first, "err")), first.Level)
	}
	if r := byMsg["unknown level"]; r.Level != "INFO" || len(r.Attrs) > 32 {
		t.Errorf("level %q, %d attrs", r.Level, len(r.Attrs))
	}
	if r := byMsg["no time"]; !r.Time.Equal(t0) {
		t.Errorf("a record with no time is stamped when it arrives: %v", r.Time)
	}
	if r := byMsg["from the future"]; !r.Time.Equal(t0) {
		t.Errorf("a record from a fast clock is stamped when it arrives: %v", r.Time)
	}
}

func TestOneSyncCarriesAtMostAThousandRecords(t *testing.T) {
	l := newLog(&clock{now: t0})
	var recs []api.LogRecord
	for i := range 1500 {
		recs = append(recs, rec(t0, "INFO", fmt.Sprint(i)))
	}
	l.AddAgent("i-1", "general-1", recs)
	if res := query(t, l, Filter{Limit: 5000}); len(res.Records) != 1000 || res.Records[0].Msg != "0" {
		t.Errorf("%d records, first %q", len(res.Records), res.Records[0].Msg)
	}
}

// world is a day's worth of records from the scheduler and two agents.
func world(t *testing.T) (*Log, *clock) {
	t.Helper()
	c := &clock{now: t0}
	l := newLog(c)
	sched := slog.New(l.Handler(slog.DiscardHandler, SourceScheduler, "i-0data1", "data-1"))
	at := func(d time.Duration) time.Time { return t0.Add(-d) }

	c.now = at(3 * time.Hour)
	sched.Info("replica placed", "replica", "api-aaaaaa", "component", "api", "vm", "i-0a1b", "pool", "general")
	c.now = at(2 * time.Hour)
	l.AddAgent("i-0a1b", "general-3", []api.LogRecord{rec(c.now, "INFO", "starting a replica", "replica", "api-aaaaaa", "image", "api:c-1")})
	c.now = at(90 * time.Minute)
	l.AddAgent("i-0a1b", "general-3", []api.LogRecord{rec(c.now, "ERROR", "replica failed", "replica", "api-aaaaaa", "reason", "OOMKilled: exited with code 137")})
	c.now = at(80 * time.Minute)
	sched.Warn("replica failed", "replica", "api-aaaaaa", "component", "api", "release", "9f2c1e7", "reason", "exited with code 137")
	c.now = at(30 * time.Minute)
	l.AddAgent("i-0c3d", "compute-1", []api.LogRecord{rec(c.now, "WARN", "not enough free cores for a stateful replica; it shares the VM's", "want", "2")})
	c.now = at(20 * time.Minute)
	sched.Info("adding VMs", "pool", "general", "group", "protostrap-shop-general", "from", 1, "to", 2, "reason", "1 replica waiting")
	c.now = at(10 * time.Minute)
	sched.Info("desired replicas", "component", "jobs", "from", 0, "to", 1, "reason", "queue jobs 12 at 2/s")
	c.now = t0
	return l, c
}

func TestQueryFilters(t *testing.T) {
	l, _ := world(t)
	day := t0.Add(-24 * time.Hour)
	for name, tc := range map[string]struct {
		f    Filter
		want string
	}{
		"everything": {Filter{Since: day},
			"replica placed,starting a replica,replica failed,replica failed,not enough free cores for a stateful replica; it shares the VM's,adding VMs,desired replicas"},
		"a component, from the scheduler and its agents": {Filter{Since: day, Component: "api"},
			"replica placed,starting a replica,replica failed,replica failed"},
		"the scheduler only":             {Filter{Since: day, Source: SourceScheduler}, "replica placed,replica failed,adding VMs,desired replicas"},
		"agents only":                    {Filter{Since: day, Source: SourceAgent}, "starting a replica,replica failed,not enough free cores for a stateful replica; it shares the VM's"},
		"a VM by name":                   {Filter{Since: day, VM: "compute-1"}, "not enough free cores for a stateful replica; it shares the VM's"},
		"a VM by ID":                     {Filter{Since: day, VM: "i-0a1b"}, "replica placed,starting a replica,replica failed"},
		"warnings and errors":            {Filter{Since: day, Level: "warn"}, "replica failed,replica failed,not enough free cores for a stateful replica; it shares the VM's"},
		"errors":                         {Filter{Since: day, Level: "ERROR"}, "replica failed"},
		"text in an attribute, any case": {Filter{Since: day, Contains: "oomkilled"}, "replica failed"},
		"text in a message":              {Filter{Since: day, Contains: "Free Cores"}, "not enough free cores for a stateful replica; it shares the VM's"},
		"the last hour":                  {Filter{Since: t0.Add(-time.Hour)}, "not enough free cores for a stateful replica; it shares the VM's,adding VMs,desired replicas"},
		"a window":                       {Filter{Since: t0.Add(-100 * time.Minute), Until: t0.Add(-25 * time.Minute)}, "replica failed,replica failed,not enough free cores for a stateful replica; it shares the VM's"},
		"a component and a level":        {Filter{Since: day, Component: "api", Level: "error", Source: SourceAgent}, "replica failed"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := msgs(query(t, l, tc.f).Records); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestQueryRefusesAnUnknownLevel(t *testing.T) {
	l, _ := world(t)
	if _, err := l.Query(context.Background(), Filter{Level: "loud"}); err == nil {
		t.Error("no error for level loud")
	}
}

func TestQueryReturnsTheNewestRecordsUpToItsLimitOldestFirst(t *testing.T) {
	c := &clock{now: t0}
	l := newLog(c)
	for i := range 10 {
		l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(time.Duration(i-10)*time.Second), "INFO", fmt.Sprint(i))})
	}
	res := query(t, l, Filter{Limit: 3})
	if msgs(res.Records) != "7,8,9" || !res.More || res.Latest != 10 {
		t.Errorf("records %s, more %v, latest %d", msgs(res.Records), res.More, res.Latest)
	}
	if res := query(t, l, Filter{Limit: 10}); res.More {
		t.Error("more, with every record returned")
	}
	if res := query(t, l, Filter{}); len(res.Records) != 10 {
		t.Errorf("the default limit is 200: %d", len(res.Records))
	}
}

func TestQueryOrdersByTimeNotArrival(t *testing.T) {
	// An agent that couldn't reach the scheduler sends its records late.
	c := &clock{now: t0}
	l := newLog(c)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(-time.Second), "INFO", "second")})
	l.AddAgent("i-2", "general-2", []api.LogRecord{rec(t0.Add(-time.Minute), "INFO", "first")})
	if got := msgs(query(t, l, Filter{}).Records); got != "first,second" {
		t.Errorf("got %s", got)
	}
}

func TestQueryAfterASequenceNumberTails(t *testing.T) {
	c := &clock{now: t0}
	l := newLog(c)
	for i := range 10 {
		l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", fmt.Sprint(i+1))})
	}
	res := query(t, l, Filter{After: 5, Limit: 2})
	if msgs(res.Records) != "6,7" || !res.More || res.Latest != 10 {
		t.Errorf("records %s, more %v, latest %d: a tail takes the oldest after the cursor, so it misses none", msgs(res.Records), res.More, res.Latest)
	}
	res = query(t, l, Filter{After: 10})
	if len(res.Records) != 0 || res.Latest != 10 || res.More {
		t.Errorf("records %s, latest %d", msgs(res.Records), res.Latest)
	}
	// A tail still applies the other filters.
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "WARN", "11")})
	if got := msgs(query(t, l, Filter{After: 10, Level: "warn"}).Records); got != "11" {
		t.Errorf("got %s", got)
	}
}

func snapshotLog(t *testing.T) *Log {
	t.Helper()
	l := newLog(&clock{now: t0})
	log := slog.New(l.Handler(slog.DiscardHandler, SourceScheduler, "", "data-1"))
	log.Info("signals",
		"components", "api desired=2 ready=2 cpu=40%; api-gateway desired=1 ready=1 cpu=5%; jobs desired=1 ready=0 queue[jobs]=12+1",
		"pools", "general vms=2 booting=0 wanted=2 cpu=1500m",
		"waiting", "api=1 jobs=1")
	log.Info("desired replicas", "component", "api", "from", 2, "to", 3, "reason", "cpu 85% of 70%")
	return l
}

func TestSignalSnapshotsAreLeftOutUnlessAsked(t *testing.T) {
	// The scheduler logs one a minute: they would bury everything else.
	l := snapshotLog(t)
	if got := msgs(query(t, l, Filter{}).Records); got != "desired replicas" {
		t.Errorf("got %s", got)
	}
	if got := msgs(query(t, l, Filter{Snapshots: true}).Records); got != "signals,desired replicas" {
		t.Errorf("got %s", got)
	}
}

func TestAComponentsSnapshotsShowOnlyThatComponent(t *testing.T) {
	l := snapshotLog(t)
	res := query(t, l, Filter{Component: "api", Snapshots: true})
	if msgs(res.Records) != "signals,desired replicas" {
		t.Fatalf("got %s", msgs(res.Records))
	}
	s := res.Records[0]
	if s.Component != "api" || attr(s, "components") != "api desired=2 ready=2 cpu=40%" || attr(s, "waiting") != "api=1" || attr(s, "pools") != "" {
		t.Errorf("snapshot %+v: api's part only, not api-gateway's or the pools'", s)
	}
	res = query(t, l, Filter{Component: "jobs", Snapshots: true})
	if msgs(res.Records) != "signals" || attr(res.Records[0], "components") != "jobs desired=1 ready=0 queue[jobs]=12+1" {
		t.Errorf("jobs %+v", res.Records)
	}
	if res := query(t, l, Filter{Component: "web", Snapshots: true}); len(res.Records) != 0 {
		t.Errorf("a component in no snapshot: %+v", res.Records)
	}
	// The snapshot in memory is untouched.
	if r := query(t, l, Filter{Snapshots: true}).Records[0]; !strings.Contains(attr(r, "components"), "api-gateway") {
		t.Errorf("snapshot %+v", r)
	}
}

func TestMemoryKeepsADay(t *testing.T) {
	c := &clock{now: t0}
	l := newLog(c)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "old")})
	c.advance(23 * time.Hour)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(c.now, "INFO", "new")})
	if got := msgs(query(t, l, Filter{Since: t0.Add(-time.Hour)}).Records); got != "old,new" {
		t.Errorf("got %s", got)
	}
	c.advance(2 * time.Hour)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(c.now, "INFO", "newer")})
	if got := msgs(query(t, l, Filter{Since: t0.Add(-time.Hour)}).Records); got != "new,newer" {
		t.Errorf("got %s: without an archive, records older than a day are gone", got)
	}
}

func TestAnHourHoldsAtMostItsLimit(t *testing.T) {
	// A replica in a crash loop can't push everything else out of memory.
	c := &clock{now: t0}
	l := &Log{Now: c.Now, HourLimit: 5}
	for i := range 8 {
		l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "ERROR", fmt.Sprint(i))})
	}
	res := query(t, l, Filter{Limit: 100})
	if msgs(res.Records) != "0,1,2,3,4,infra log hour full: dropping its records until the next hour" {
		t.Errorf("got %s", msgs(res.Records))
	}
	if last := res.Records[len(res.Records)-1]; last.Level != "WARN" || last.Source != SourceScheduler {
		t.Errorf("notice %+v", last)
	}
	c.advance(time.Hour)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(c.now, "ERROR", "next hour")})
	if got := query(t, l, Filter{Since: c.now.Add(-time.Minute)}); msgs(got.Records) != "next hour" {
		t.Errorf("got %s", msgs(got.Records))
	}
}

func TestAQueryCoversAtMostAWeek(t *testing.T) {
	l, _ := world(t)
	_, err := l.Query(context.Background(), Filter{Since: t0.Add(-8 * 24 * time.Hour)})
	if !errors.Is(err, ErrRange) {
		t.Errorf("err %v, want ErrRange", err)
	}
	if _, err := l.Query(context.Background(), Filter{Since: t0.Add(-7 * 24 * time.Hour)}); err != nil {
		t.Errorf("a week: %v", err)
	}
}

func TestText(t *testing.T) {
	// Agents read logs as lines: one record each, the fields an operator scans first.
	l, _ := world(t)
	res := query(t, l, Filter{Since: t0.Add(-24 * time.Hour), Component: "api", Limit: 3})
	var b strings.Builder
	if err := WriteText(&b, res); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines:\n%s", b.String())
	}
	want := `2026-10-29T08:30:00.000Z INFO  agent     general-3 api starting a replica replica=api-aaaaaa image=api:c-1`
	if lines[0] != want {
		t.Errorf("got  %s\nwant %s", lines[0], want)
	}
	if !strings.Contains(lines[1], `ERROR agent     general-3 api replica failed`) || !strings.Contains(lines[1], `reason="OOMKilled: exited with code 137"`) {
		t.Errorf("line %s: a value with spaces is quoted", lines[1])
	}
	if !strings.Contains(lines[2], "WARN  scheduler data-1") {
		t.Errorf("line %s", lines[2])
	}
	if !strings.HasPrefix(lines[3], "# ") || !strings.Contains(lines[3], "more") {
		t.Errorf("last line %q: it says when more records matched than were shown", lines[3])
	}

	b.Reset()
	WriteText(&b, query(t, l, Filter{Since: t0.Add(-24 * time.Hour), Source: SourceScheduler, Contains: "adding"}))
	if !strings.Contains(b.String(), "INFO  scheduler data-1 - adding VMs pool=general") {
		t.Errorf("a record of no component shows -:\n%s", b.String())
	}
	b.Reset()
	WriteText(&b, Result{})
	if !strings.Contains(b.String(), "# no records") {
		t.Errorf("an empty result says so: %q", b.String())
	}
}
