package reporter

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/spec"
)

var day = time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)

const testSpec = `app: shop
protostrap: 0.1.0
pools:
  general: { vm: [m7g.large], min: 1, max: 4 }
  compute: { vm: [c7g.large], min: 0, max: 3 }
  data: { vm: m7g.large }
components:
  api:
    kind: service
    pool: general
    request: { cpu: 500m, memory: 512Mi }
    scale: { min: 2, max: 6, demand: [{ cpu: 70% }] }
  jobs:
    kind: worker
    pool: compute
    request: { cpu: 500m, memory: 512Mi }
    scale: { min: 0, max: 3, demand: [{ queue: jobs, rate: 2/s }], limits: [{ metric: jobs_budget_cap }] }
  postgres:
    kind: stateful
    image: postgres:17.6
    vm: data-1
    request: { cpu: 1000m, memory: 3Gi }
`

func testSpecParsed(t *testing.T) *spec.Spec {
	t.Helper()
	s, err := spec.Parse([]byte(testSpec))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// line is one of the scheduler's records, as the infra log keeps it: with the component it
// is about.
func line(at time.Time, level, msg string, kv ...any) infralog.Record {
	r := infralog.Record{Time: at, Level: level, Source: infralog.SourceScheduler, VMName: "data-1", Msg: msg}
	for i := 0; i+1 < len(kv); i += 2 {
		a := api.LogAttr{Key: kv[i].(string), Value: fmt.Sprint(kv[i+1])}
		r.Attrs = append(r.Attrs, a)
		if a.Key == "component" {
			r.Component = a.Value
		}
	}
	return r
}

// agentLine is one of an agent's records.
func agentLine(at time.Time, vm, level, msg string, kv ...any) infralog.Record {
	r := line(at, level, msg, kv...)
	r.Source, r.VMName = infralog.SourceAgent, vm
	return r
}

// signals is a signal snapshot, which the scheduler logs once a minute.
func signals(at time.Time, components, pools, waiting string) infralog.Record {
	return line(at, "INFO", "signals", "components", components, "pools", pools, "waiting", waiting)
}

// errLine is a line from a component's logs in Loki that looks like an error.
func errLine(at time.Time, component, msg string) LogLine {
	return LogLine{Time: at, Component: component, Message: msg}
}

func gatherTest(t *testing.T, records []infralog.Record, errs []LogLine) *Facts {
	t.Helper()
	return gather(records, errs, testSpecParsed(t), day.Add(-24*time.Hour), day)
}

func hasFlag(f *Facts, substr string) bool {
	for _, fl := range f.Flags {
		if strings.Contains(fl, substr) {
			return true
		}
	}
	return false
}

func TestSchedulerEvents(t *testing.T) {
	at := day.Add(-time.Hour)
	ev := schedulerEvents([]infralog.Record{
		line(at, "INFO", "desired replicas", "component", "api", "from", 2, "to", 3, "reason", "cpu 85% of 70%"),
		agentLine(at, "general-3", "ERROR", "replica failed", "replica", "api-aaaaaa"),
		line(day.Add(-2*time.Hour), "WARN", "earlier"),
	})
	if len(ev) != 2 {
		t.Fatalf("events %v: the scheduler's records only", ev)
	}
	if ev[0].Msg != "earlier" || ev[1].Msg != "desired replicas" {
		t.Errorf("events %v: in time order", ev)
	}
	if ev[1].Level != "INFO" || ev[1].Attrs["from"] != "2" || ev[1].Attrs["component"] != "api" || !ev[1].Time.Equal(at) {
		t.Errorf("event %+v", ev[1])
	}
}

func TestAgentWarnings(t *testing.T) {
	var rows []infralog.Record
	for i := range 3 {
		rows = append(rows, agentLine(day.Add(-time.Duration(3-i)*time.Hour), "general-3", "ERROR", "starting a replica", "replica", "api-aaaaaa", "component", "api", "err", "pull access denied"))
	}
	rows = append(rows,
		agentLine(day.Add(-time.Hour), "data-1", "ERROR", "backup failed", "component", "postgres", "err", "disk full"),
		agentLine(day.Add(-time.Hour), "general-3", "WARN", "firewall rules were missing and have been put back"),
		agentLine(day.Add(-time.Hour), "general-3", "INFO", "starting a replica", "component", "api"),
		line(day.Add(-time.Hour), "WARN", "reading a queue's depth"),
	)
	f := gatherTest(t, rows, nil)
	if len(f.AgentWarnings) != 3 {
		t.Fatalf("agent warnings %+v", f.AgentWarnings)
	}
	if w := f.AgentWarnings[0]; w.Text != "starting a replica (api)" || w.N != 3 || !w.Last.Equal(day.Add(-time.Hour)) {
		t.Errorf("first %+v: by message and component, the commonest first", w)
	}
	if f.AgentWarnings[1].Text != "backup failed (postgres)" || f.AgentWarnings[2].Text != "firewall rules were missing and have been put back" {
		t.Errorf("warnings %+v", f.AgentWarnings)
	}
	if len(f.Warnings) != 1 || f.Warnings[0].Text != "reading a queue's depth" {
		t.Errorf("scheduler warnings %+v: they stay apart", f.Warnings)
	}
	if md := f.Markdown(); !strings.Contains(md, "### Agent warnings") || !strings.Contains(md, "starting a replica (api) (×3") {
		t.Errorf("markdown:\n%s", md)
	}
}

func TestReleases(t *testing.T) {
	at := day.Add(-5 * time.Hour)
	f := gatherTest(t, []infralog.Record{
		line(at, "INFO", "release deployed", "release", "abc1234", "version", "0.1.0"),
		line(at.Add(8*time.Minute), "INFO", "release finished", "release", "abc1234", "took", "8m0s"),
		line(at.Add(time.Hour), "INFO", "release deployed", "release", "def5678", "version", "0.1.0"),
		line(at.Add(time.Hour+3*time.Minute), "ERROR", "release failed: rolling back", "release", "def5678", "to", "abc1234", "reason", "api replica api-9 failed: exited with code 1"),
	}, nil)
	got := strings.Join(f.Releases, "\n")
	for _, want := range []string{"abc1234 deployed", "abc1234 finished in 8m0s", "def5678 failed", "rolled back to abc1234", "exited with code 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("releases lack %q:\n%s", want, got)
		}
	}
	if !hasFlag(f, "def5678") {
		t.Errorf("a failed release isn't flagged: %v", f.Flags)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("a release failure counted again as a warning: %v", f.Warnings)
	}
}

func TestComponentRangeAndTimeAtMax(t *testing.T) {
	var rows []infralog.Record
	for i := range 40 {
		desired := 2
		if i >= 20 {
			desired = 6 // api is at its max for half the snapshots.
		}
		rows = append(rows, signals(day.Add(-time.Duration(40-i)*time.Minute),
			fmt.Sprintf("api desired=%d ready=%d cpu=%d%% requests=12.5/s; jobs desired=0 ready=0 queue[jobs]=%d+0", desired, desired, 60+i/2, i),
			"general vms=2 booting=0 wanted=2 cpu=3000m", ""))
	}
	rows = append(rows, line(day.Add(-20*time.Minute), "INFO", "desired replicas", "component", "api", "from", 2, "to", 6, "reason", "cpu 95% of 70%"))
	f := gatherTest(t, rows, nil)
	api := f.component("api")
	if api == nil || api.Low != 2 || api.High != 6 || api.Min != 2 || api.Max != 6 || api.Changes != 1 {
		t.Fatalf("api %+v", api)
	}
	if api.AtMax != 20 || api.Snapshots != 40 {
		t.Errorf("api at max %d of %d", api.AtMax, api.Snapshots)
	}
	if api.CPU.Peak != 79 || api.Requests.Peak != 12.5 {
		t.Errorf("api cpu %+v requests %+v", api.CPU, api.Requests)
	}
	if jobs := f.component("jobs"); jobs == nil || jobs.QueuePeak["jobs"] != 39 {
		t.Errorf("jobs %+v", jobs)
	}
	if !hasFlag(f, "api was at its maximum of 6") {
		t.Errorf("flags %v", f.Flags)
	}
}

func TestLimitsThatHeldAComponentBack(t *testing.T) {
	var rows []infralog.Record
	for i := range 3 {
		rows = append(rows, line(day.Add(-time.Duration(3-i)*time.Hour), "INFO", "desired replicas", "component", "jobs", "from", 0, "to", 2, "reason", "limited by jobs_budget_cap to 2"))
	}
	rows = append(rows, line(day.Add(-time.Minute), "INFO", "desired replicas", "component", "jobs", "from", 2, "to", 0, "reason", "queue jobs 0 jobs"))
	f := gatherTest(t, rows, nil)
	jobs := f.component("jobs")
	if len(jobs.Limited) != 1 || jobs.Limited[0].Text != "limited by jobs_budget_cap to 2" || jobs.Limited[0].N != 3 {
		t.Errorf("limited %+v", jobs.Limited)
	}
	if !hasFlag(f, "jobs was held back by a limit 3 times") {
		t.Errorf("flags %v", f.Flags)
	}
}

func TestPendingReplicasAndShortDataVMs(t *testing.T) {
	var rows []infralog.Record
	for i := range 15 {
		rows = append(rows, signals(day.Add(-time.Duration(15-i)*time.Minute), "api desired=2 ready=2", "general vms=2 booting=0 wanted=2 cpu=1000m", "postgres=1"))
	}
	for i := range 3 {
		rows = append(rows, signals(day.Add(-time.Duration(60-i)*time.Minute), "api desired=4 ready=2", "general vms=2 booting=1 wanted=3 cpu=2000m", "api=2"))
	}
	f := gatherTest(t, rows, nil)
	pg := f.component("postgres")
	if pg == nil || pg.PendingPeak != 1 || pg.PendingSnapshots != 15 {
		t.Fatalf("postgres %+v", pg)
	}
	if !hasFlag(f, "postgres waited for a VM") || !hasFlag(f, "data VM") {
		t.Errorf("a stateful replica waiting is a data VM running short: %v", f.Flags)
	}
	if api := f.component("api"); api.PendingPeak != 2 || hasFlag(f, "api waited") {
		t.Errorf("api %+v flags %v: 3 minutes waiting for a booting VM is normal", api, f.Flags)
	}
}

func TestPools(t *testing.T) {
	var rows []infralog.Record
	for i := range 20 {
		vms := 1 + i/5 // vms steps through 1, 2, 3 and 4.
		rows = append(rows, signals(day.Add(-time.Duration(20-i)*time.Minute), "api desired=2 ready=2",
			fmt.Sprintf("compute vms=0 booting=0 wanted=0 cpu=0m; general vms=%d booting=0 wanted=%d cpu=1000m", vms, min(vms+1, 4)), ""))
	}
	rows = append(rows,
		line(day.Add(-19*time.Minute), "INFO", "adding VMs", "pool", "general", "group", "shop-general-a", "from", 1, "to", 2, "reason", "placing", "requested_cpu", 1500, "waiting", 1),
		line(day.Add(-2*time.Minute), "INFO", "removing a VM: moving its replicas off first", "vm", "i-0abc", "pool", "general"),
		line(day.Add(-time.Minute), "INFO", "VM gone", "vm", "i-0abc", "pool", "general"),
	)
	f := gatherTest(t, rows, nil)
	g := f.pool("general")
	if g == nil || g.Low != 1 || g.High != 4 || g.Max != 4 || g.AtMax != 5 || g.WantedPeak != 4 || g.Added != 1 || g.Removed != 1 {
		t.Fatalf("general %+v", g)
	}
	if !hasFlag(f, "general pool was at its maximum of 4 VMs") {
		t.Errorf("flags %v", f.Flags)
	}
	ev := strings.Join(f.VMEvents, "\n")
	if !strings.Contains(ev, "adding VMs") || !strings.Contains(ev, "pool=general") || !strings.Contains(ev, "VM gone") || !strings.Contains(ev, "i-0abc") {
		t.Errorf("vm events:\n%s", ev)
	}
}

func TestLostVMsAreFlagged(t *testing.T) {
	f := gatherTest(t, []infralog.Record{
		line(day.Add(-time.Hour), "WARN", "VM is silent: placing its replicas elsewhere", "vm", "i-0def", "pool", "compute", "last_sync", day.Add(-time.Hour-time.Minute).Format(time.RFC3339)),
		line(day.Add(-time.Hour+time.Minute), "WARN", "marking a silent VM unhealthy, so its group replaces it", "vm", "i-0def", "pool", "compute", "silent_for", "2m0s"),
	}, nil)
	if !hasFlag(f, "1 VM went silent") {
		t.Errorf("flags %v", f.Flags)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("VM events counted again as warnings: %v", f.Warnings)
	}
}

func TestReplicaFailures(t *testing.T) {
	var rows []infralog.Record
	for i, reason := range []string{"exited with code 1", "exited with code 1", "exited with code 137", "health check failed"} {
		rows = append(rows, line(day.Add(-time.Duration(10-i)*time.Hour), "WARN", "replica failed", "replica", fmt.Sprintf("api-%d", i), "component", "api", "release", "abc1234", "reason", reason))
	}
	f := gatherTest(t, rows, nil)
	api := f.component("api")
	if api.Failed != 4 || len(api.Failures) != 3 || api.Failures[0].Text != "exited with code 1" || api.Failures[0].N != 2 {
		t.Errorf("failures %+v", api.Failures)
	}
	if !hasFlag(f, "api had 4 replica failures") || !hasFlag(f, "137") {
		t.Errorf("flags %v: exit 137 is out of memory", f.Flags)
	}
}

func TestSchedulerWarnings(t *testing.T) {
	var rows []infralog.Record
	for i := range 4 {
		rows = append(rows, line(day.Add(-time.Duration(4-i)*time.Hour), "WARN", "reading a queue's depth", "queue", "jobs", "err", "timeout"))
	}
	rows = append(rows, line(day.Add(-time.Hour), "INFO", "replica placed", "replica", "api-1"))
	f := gatherTest(t, rows, nil)
	if len(f.Warnings) != 1 || f.Warnings[0].Text != "reading a queue's depth" || f.Warnings[0].N != 4 || !f.Warnings[0].Last.Equal(day.Add(-time.Hour)) {
		t.Errorf("warnings %+v", f.Warnings)
	}
}

func TestComponentLogErrors(t *testing.T) {
	var errs []LogLine
	for i := range 30 {
		errs = append(errs, errLine(day.Add(-time.Duration(i)*time.Minute), "api",
			fmt.Sprintf("2026-10-25T05:%02d:00Z fail: Npgsql.NpgsqlException (0x80004005): connection to 10.200.0.%d:5432 refused, request 4b1c2d3e-0000-4000-8000-%012d", i%60, i, i)))
	}
	errs = append(errs,
		errLine(day.Add(-time.Hour), "api", "Unhandled exception. System.InvalidOperationException: bad state\n   at Api.Program.Main()"),
		errLine(day.Add(-time.Hour), "web", "Error: ECONNREFUSED"),
	)
	f := gatherTest(t, nil, errs)
	if len(f.Errors) != 2 || f.Errors[0].Component != "api" || f.Errors[0].Lines != 31 {
		t.Fatalf("errors %+v", f.Errors)
	}
	top := f.Errors[0].Top
	if top[0].N != 30 || !strings.Contains(top[0].Text, "NpgsqlException") || strings.Contains(top[0].Text, "4b1c2d3e") {
		t.Errorf("top %+v: the same error with different numbers is one error", top[0])
	}
	if strings.Contains(top[1].Text, "at Api.Program") {
		t.Errorf("top %+v: only an error's first line", top[1])
	}
	if f.Errors[1].Component != "web" || f.Errors[1].Lines != 1 {
		t.Errorf("web %+v", f.Errors[1])
	}
}

func TestCPURequestFlags(t *testing.T) {
	var rows []infralog.Record
	for i := range 60 {
		rows = append(rows, signals(day.Add(-time.Duration(60-i)*time.Minute), "api desired=2 ready=2 cpu=8%; jobs desired=1 ready=1 cpu=140%", "", ""))
	}
	f := gatherTest(t, rows, nil)
	if !hasFlag(f, "api used at most 8% of its CPU request") {
		t.Errorf("flags %v: a request too large", f.Flags)
	}
	if !hasFlag(f, "jobs averaged 140% of its CPU request") {
		t.Errorf("flags %v: a request too small", f.Flags)
	}
}

func TestAQuietDay(t *testing.T) {
	var rows []infralog.Record
	for i := range 60 {
		rows = append(rows, signals(day.Add(-time.Duration(60-i)*time.Minute), "api desired=2 ready=2 cpu=50%", "general vms=1 booting=0 wanted=1 cpu=1000m", ""))
	}
	f := gatherTest(t, rows, nil)
	if len(f.Flags) != 0 {
		t.Errorf("flags on a quiet day: %v", f.Flags)
	}
	md := f.Markdown()
	for _, want := range []string{"## Facts", "### Releases", "None.", "### Components", "| api |", "### VMs", "| general |", "### Agent warnings", "### Errors in component logs"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestWithoutASpec(t *testing.T) {
	rows := []infralog.Record{signals(day.Add(-time.Minute), "api desired=6 ready=6 cpu=50%", "general vms=4 booting=0 wanted=4 cpu=1000m", "")}
	f := gather(rows, nil, nil, day.Add(-24*time.Hour), day)
	if api := f.component("api"); api == nil || api.Max != 0 || api.AtMax != 0 {
		t.Errorf("api %+v: no spec, no maximum", api)
	}
	if !strings.Contains(f.Markdown(), "| api |") {
		t.Errorf("markdown:\n%s", f.Markdown())
	}
}
