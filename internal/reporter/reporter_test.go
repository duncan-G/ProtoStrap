package reporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/release"
)

// fakeLogs is telemetry's Loki, which holds the components' own logs: it answers with
// errs, the lines that look like errors.
type fakeLogs struct {
	errs  []LogLine
	calls []window
	err   error
}

type window struct{ from, to time.Time }

func (f *fakeLogs) Errors(_ context.Context, from, to time.Time) ([]LogLine, error) {
	f.calls = append(f.calls, window{from, to})
	if f.err != nil {
		return nil, f.err
	}
	return f.errs, nil
}

// fakeInfra is the infra log's archive in the deploy bucket.
type fakeInfra struct {
	records  []infralog.Record
	from, to time.Time
	reads    int
	err      error
}

func (f *fakeInfra) Read(_ context.Context, from, to time.Time) ([]infralog.Record, error) {
	f.reads++
	f.from, f.to = from, to
	return f.records, f.err
}

type fakeBucket struct {
	objects map[string][]byte
	putErr  error
}

func (b *fakeBucket) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	v, ok := b.objects[bucket+"/"+key]
	if !ok {
		return nil, fmt.Errorf("s3 GET %s: %w", key, aws.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func (b *fakeBucket) Put(_ context.Context, bucket, key string, body []byte) error {
	if b.putErr != nil {
		return b.putErr
	}
	b.objects[bucket+"/"+key] = body
	return nil
}

type fakeWriter struct {
	key          string
	system, user string
	reply        Reply
	err          error
	calls        int
}

func (w *fakeWriter) Write(_ context.Context, system, user string) (Reply, error) {
	w.calls++
	w.system, w.user = system, user
	return w.reply, w.err
}

type world struct {
	logs   *fakeLogs
	infra  *fakeInfra
	bucket *fakeBucket
	writer *fakeWriter
	secret string
	secErr error
	deps   Deps
	cfg    Config
}

func newWorld(t *testing.T) *world {
	rel, _ := json.Marshal(release.Release{Commit: "abc1234", App: "shop", Version: "0.1.0", Spec: testSpec})
	state, _ := json.Marshal(release.State{Target: "abc1234", LastGood: "abc1234"})
	w := &world{
		logs: &fakeLogs{errs: []LogLine{errLine(day.Add(-time.Hour), "api", "fail: boom")}},
		infra: &fakeInfra{records: []infralog.Record{
			line(day.Add(-5*time.Hour), "INFO", "release deployed", "release", "abc1234", "version", "0.1.0"),
			agentLine(day.Add(-4*time.Hour), "general-3", "ERROR", "starting a replica", "component", "api", "err", "pull access denied"),
			signals(day.Add(-time.Minute), "api desired=2 ready=2 cpu=50%", "general vms=1 booting=0 wanted=1 cpu=1000m", ""),
		}},
		bucket: &fakeBucket{objects: map[string][]byte{"deploy/state.json": state, "deploy/releases/abc1234.json": rel}},
		writer: &fakeWriter{reply: Reply{Text: "## What happened\n\nOne release.\n\n## What to change\n\nNothing.", Model: "claude-opus-5"}},
		secret: `{"Reporter_ANTHROPIC_API_KEY":"sk-ant-test","Api_StripeKey":"sk_live_x"}`,
		cfg:    Config{App: "shop", Environment: "dev", DeployBucket: "deploy"},
	}
	w.deps = Deps{
		Logs: w.logs, Infra: w.infra, Bucket: w.bucket,
		Secret: func(context.Context) (string, error) { return w.secret, w.secErr },
		LLM:    func(key string) Writer { w.writer.key = key; return w.writer },
		Now:    func() time.Time { return day },
		Logger: slog.New(slog.DiscardHandler),
	}
	return w
}

func (w *world) report(t *testing.T) string {
	t.Helper()
	b, ok := w.bucket.objects["deploy/reports/2026-10-25.md"]
	if !ok {
		t.Fatalf("no report written: %v", slices.Collect(func(yield func(string) bool) {
			for k := range w.bucket.objects {
				yield(k)
			}
		}))
	}
	return string(b)
}

func TestReportWithTheLLM(t *testing.T) {
	w := newWorld(t)
	key, err := Run(context.Background(), w.cfg, w.deps)
	if err != nil || key != "reports/2026-10-25.md" {
		t.Fatalf("Run = %q, %v", key, err)
	}
	r := w.report(t)
	for _, want := range []string{"# shop dev: 2026-10-25", "One release.", "## What to change", "## Facts", "abc1234 deployed", "claude-opus-5"} {
		if !strings.Contains(r, want) {
			t.Errorf("report lacks %q:\n%s", want, r)
		}
	}
	if strings.Index(r, "One release.") > strings.Index(r, "## Facts") {
		t.Errorf("the facts come after the LLM's report")
	}
	if w.writer.key != "sk-ant-test" {
		t.Errorf("key %q", w.writer.key)
	}
	for _, want := range []string{"## Facts", "jobs_budget_cap", "abc1234"} {
		if !strings.Contains(w.writer.user, want) {
			t.Errorf("the prompt lacks %q: the LLM gets the facts and the spec", want)
		}
	}
	for _, want := range []string{"What happened", "What to change", "Move when"} {
		if !strings.Contains(w.writer.system, want) {
			t.Errorf("the system prompt lacks %q", want)
		}
	}
}

func TestReportReadsTheInfraLogFromTheDeployBucket(t *testing.T) {
	// The scheduler's and the agents' records are in the infra log's archive; telemetry's
	// Loki holds only the components' own logs.
	w := newWorld(t)
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if w.infra.reads != 1 || !w.infra.to.Equal(day) || w.infra.to.Sub(w.infra.from) != 24*time.Hour {
		t.Errorf("%d reads, window %v to %v", w.infra.reads, w.infra.from, w.infra.to)
	}
	r := w.report(t)
	for _, want := range []string{"abc1234 deployed", "### Agent warnings", "starting a replica (api)"} {
		if !strings.Contains(r, want) {
			t.Errorf("report lacks %q:\n%s", want, r)
		}
	}
	if !strings.Contains(w.writer.user, "starting a replica (api)") {
		t.Error("the LLM isn't given the agents' warnings")
	}
}

func TestReportReadsTheComponentsErrorsForItsDay(t *testing.T) {
	w := newWorld(t)
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if len(w.logs.calls) != 1 || !w.logs.calls[0].to.Equal(day) || w.logs.calls[0].to.Sub(w.logs.calls[0].from) != 24*time.Hour {
		t.Fatalf("reads %+v: one, for the day", w.logs.calls)
	}
	if r := w.report(t); !strings.Contains(r, "fail: boom") {
		t.Errorf("report lacks the api's error:\n%s", r)
	}
}

func TestReportWithoutAKey(t *testing.T) {
	for name, set := range map[string]func(*world){
		"no secret value": func(w *world) { w.secErr = fmt.Errorf("secretsmanager: %w", aws.ErrNotFound) },
		"no key in it":    func(w *world) { w.secret = `{}` },
		"an empty key":    func(w *world) { w.secret = `{"Reporter_ANTHROPIC_API_KEY":""}` },
		"another's key":   func(w *world) { w.secret = `{"Api_ANTHROPIC_API_KEY":"sk-ant-api"}` },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			set(w)
			if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
				t.Fatal(err)
			}
			if w.writer.calls != 0 {
				t.Errorf("called the LLM without a key")
			}
			r := w.report(t)
			if !strings.Contains(r, "facts only") || !strings.Contains(r, "Reporter_ANTHROPIC_API_KEY") || !strings.Contains(r, "## Facts") {
				t.Errorf("report:\n%s", r)
			}
		})
	}
}

func TestReportUsesTheKeyInTheReportersOwnSecret(t *testing.T) {
	// Before the environment's secret, the reporter had a secret of its own.
	w := newWorld(t)
	w.secret = `{"ANTHROPIC_API_KEY":"sk-ant-old"}`
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if w.writer.key != "sk-ant-old" {
		t.Errorf("key %q", w.writer.key)
	}
}

func TestReportWhenTheKeyCantBeRead(t *testing.T) {
	w := newWorld(t)
	w.secErr = errors.New("AccessDeniedException: not allowed")
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if r := w.report(t); !strings.Contains(r, "AccessDeniedException") || !strings.Contains(r, "## Facts") {
		t.Errorf("report:\n%s", r)
	}
}

func TestReportWhenTheLLMFails(t *testing.T) {
	for name, err := range map[string]error{
		"refused": fmt.Errorf("%w (category cyber)", ErrRefused),
		"down":    &APIError{Status: 529, Type: "overloaded_error", Message: "Overloaded"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.writer.err = err
			if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
				t.Fatalf("Run: %v: the facts are still worth writing", err)
			}
			r := w.report(t)
			if !strings.Contains(r, err.Error()) || !strings.Contains(r, "## Facts") {
				t.Errorf("report:\n%s", r)
			}
		})
	}
}

func TestReportNotesAFallbackAndATruncation(t *testing.T) {
	w := newWorld(t)
	w.writer.reply = Reply{Text: "## What happened\n\nCut", Model: "claude-opus-4-8", FellBack: true, Truncated: true}
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	r := w.report(t)
	if !strings.Contains(r, "claude-opus-4-8") || !strings.Contains(r, "fallback") || !strings.Contains(r, "cut short") {
		t.Errorf("report:\n%s", r)
	}
}

func TestReportBeforeAnyRelease(t *testing.T) {
	w := newWorld(t)
	delete(w.bucket.objects, "deploy/state.json")
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatal(err)
	}
	if r := w.report(t); !strings.Contains(r, "No release") {
		t.Errorf("report:\n%s", r)
	}
}

// TestReportNotesWhenTheLogsCantBeRead checks that a report is still written while
// telemetry is down: it says the components' errors are missing (docs/telemetry.md#when-telemetry-is-down).
func TestReportNotesWhenTheLogsCantBeRead(t *testing.T) {
	w := newWorld(t)
	w.logs.err = errors.New("telemetry isn't running")
	if _, err := Run(context.Background(), w.cfg, w.deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r := w.report(t)
	if !strings.Contains(r, "couldn't be read") || !strings.Contains(r, "telemetry isn't running") {
		t.Errorf("report doesn't say the errors are missing:\n%s", r)
	}
	if !strings.Contains(w.writer.user, "couldn't be read") {
		t.Errorf("the LLM isn't told the errors are missing")
	}
}

func TestReportFailsWhenTheInfraLogCantBeRead(t *testing.T) {
	w := newWorld(t)
	w.infra.err = errors.New("AccessDenied")
	if _, err := Run(context.Background(), w.cfg, w.deps); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err %v", err)
	}
	if _, ok := w.bucket.objects["deploy/reports/2026-10-25.md"]; ok {
		t.Errorf("wrote a report with no facts")
	}
}

func TestReportFailsWhenItCantBeWritten(t *testing.T) {
	w := newWorld(t)
	w.bucket.putErr = errors.New("AccessDenied")
	if _, err := Run(context.Background(), w.cfg, w.deps); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err %v", err)
	}
}

func TestReportNamesItsDayInUTC(t *testing.T) {
	w := newWorld(t)
	w.deps.Now = func() time.Time { return time.Date(2026, 10, 25, 23, 30, 0, 0, time.FixedZone("PDT", -7*3600)) }
	key, err := Run(context.Background(), w.cfg, w.deps)
	if err != nil || key != "reports/2026-10-26.md" {
		t.Errorf("key %q %v", key, err)
	}
}
