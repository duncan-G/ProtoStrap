package reporter

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/release"
)

// These tests cover reading the components' errors from CloudWatch Logs, where Docker sends
// their output when the application runs no telemetry (docs/telemetry.md, "Without
// telemetry").

// fakeInsights is CloudWatch Logs: the groups it has, and the rows a query answers.
type fakeInsights struct {
	groups   []string
	rows     []map[string]string
	prefix   string
	queried  []string
	query    string
	from, to time.Time
}

func (f *fakeInsights) Groups(_ context.Context, prefix string) ([]string, error) {
	f.prefix = prefix
	var out []string
	for _, g := range f.groups {
		if strings.HasPrefix(g, prefix) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeInsights) Query(_ context.Context, groups []string, query string, from, to time.Time, _ time.Duration) ([]map[string]string, error) {
	f.queried, f.query, f.from, f.to = groups, query, from, to
	return f.rows, nil
}

func TestCloudWatchReadsTheComponentsErrorLines(t *testing.T) {
	f := &fakeInsights{
		groups: []string{"/protostrap/shop/api", "/protostrap/shop/edge", "/protostrap/other/api"},
		rows: []map[string]string{
			{"@timestamp": "2026-10-25 11:00:00.000", "@message": "fail: boom", "@log": "123456789012:/protostrap/shop/api"},
			{"@timestamp": "2026-10-25 10:59:00.000", "@message": "Error: upstream reset", "@log": "123456789012:/protostrap/shop/edge"},
		},
	}
	cw := &CloudWatch{Logs: f, App: "shop"}
	from, to := day.Add(-24*time.Hour), day
	lines, err := cw.Errors(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if f.prefix != "/protostrap/shop/" || !slices.Equal(f.queried, []string{"/protostrap/shop/api", "/protostrap/shop/edge"}) {
		t.Errorf("queried %v with prefix %q, want the app's groups alone", f.queried, f.prefix)
	}
	if !f.from.Equal(from) || !f.to.Equal(to) || !strings.Contains(f.query, "(?i)(error|exception|panic|fatal|traceback|unhandled|critical)") {
		t.Errorf("query %q from %v to %v", f.query, f.from, f.to)
	}
	want := []LogLine{
		{Time: time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC), Component: "api", Message: "fail: boom"},
		{Time: time.Date(2026, 10, 25, 10, 59, 0, 0, time.UTC), Component: "edge", Message: "Error: upstream reset"},
	}
	if !slices.Equal(lines, want) {
		t.Errorf("lines\n got %+v\nwant %+v", lines, want)
	}

	// Without any of the app's groups, there is nothing to read.
	f = &fakeInsights{groups: []string{"/protostrap/other/api"}}
	if lines, err := (&CloudWatch{Logs: f, App: "shop"}).Errors(context.Background(), from, to); err != nil || lines != nil || f.queried != nil {
		t.Errorf("no groups: %v, %v, queried %v", lines, err, f.queried)
	}
}

// TestReportReadsCloudWatchWithoutTelemetry checks that the report reads the components'
// errors where the running release sends them: CloudWatch Logs when its spec has no
// telemetry, and telemetry's Loki otherwise.
func TestReportReadsCloudWatchWithoutTelemetry(t *testing.T) {
	w := newWorld(t)
	rel, _ := json.Marshal(release.Release{Commit: "abc1234", App: "shop", Version: "0.1.0", Spec: strings.Replace(testSpec, "telemetry: {}\n", "", 1)})
	w.bucket.objects["deploy/releases/abc1234.json"] = rel
	cw := &fakeLogs{errs: []LogLine{errLine(day.Add(-time.Hour), "api", "from cloudwatch")}}
	w.deps.CloudWatch = cw
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if len(cw.calls) != 1 || len(w.logs.calls) != 0 {
		t.Errorf("CloudWatch read %d times, Loki %d: without telemetry the errors are in CloudWatch", len(cw.calls), len(w.logs.calls))
	}
	if r := w.report(t); !strings.Contains(r, "from cloudwatch") {
		t.Errorf("the report lacks CloudWatch's error:\n%s", r)
	}

	w = newWorld(t)
	cw = &fakeLogs{}
	w.deps.CloudWatch = cw
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if len(cw.calls) != 0 || len(w.logs.calls) != 1 {
		t.Errorf("CloudWatch read %d times, Loki %d: with telemetry the errors are in Loki", len(cw.calls), len(w.logs.calls))
	}
}
