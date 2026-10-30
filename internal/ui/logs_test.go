package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/infralog"
)

// The logs API is the infra log, the scheduler's and the agents' records, for operators and
// for the agents they hand it to, such as Claude: curl through the same SSM port forward.

type fakeLogs struct {
	got []infralog.Filter
	res infralog.Result
	err error
}

func (f *fakeLogs) Query(_ context.Context, fl infralog.Filter) (infralog.Result, error) {
	f.got = append(f.got, fl)
	return f.res, f.err
}

func withFakeLogs() (*Server, *fakeLogs) {
	s, _, _ := testServer()
	f := &fakeLogs{}
	s.Logs = f
	return s, f
}

func TestLogsParameters(t *testing.T) {
	at := func(d time.Duration) time.Time { return now.Add(-d) }
	for q, want := range map[string]infralog.Filter{
		"": {Since: at(time.Hour), Limit: 200},
		"component=api&source=agent&vm=general-3&level=warn&q=oom&limit=50&snapshots=1&since=2h": {
			Component: "api", Source: infralog.SourceAgent, VM: "general-3", Level: "warn", Contains: "oom",
			Limit: 50, Snapshots: true, Since: at(2 * time.Hour)},
		"since=3d":                     {Since: at(72 * time.Hour), Limit: 200},
		"since=90m":                    {Since: at(90 * time.Minute), Limit: 200},
		"since=45s":                    {Since: at(45 * time.Second), Limit: 200},
		"source=scheduler&level=ERROR": {Source: infralog.SourceScheduler, Level: "ERROR", Since: at(time.Hour), Limit: 200},
		"since=2026-10-25T02:00:00Z&until=2026-10-25T03:00:00Z": {
			Since: time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC), Until: time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC), Limit: 200},
		"since=6h&until=30m":     {Since: at(6 * time.Hour), Until: at(30 * time.Minute), Limit: 200},
		"after=41":               {After: 41, Limit: 200},
		"after=41&since=10m":     {After: 41, Since: at(10 * time.Minute), Limit: 200},
		"snapshots=true":         {Snapshots: true, Since: at(time.Hour), Limit: 200},
		"snapshots=0":            {Since: at(time.Hour), Limit: 200},
		"limit=5000&format=text": {Since: at(time.Hour), Limit: 5000},
	} {
		t.Run(q, func(t *testing.T) {
			s, f := withFakeLogs()
			w := get(s, "/api/logs?"+q)
			if w.Code != 200 || len(f.got) != 1 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			got := f.got[0]
			if !got.Since.Equal(want.Since) || !got.Until.Equal(want.Until) {
				t.Errorf("window %v to %v, want %v to %v", got.Since, got.Until, want.Since, want.Until)
			}
			var zero time.Time
			got.Since, got.Until, want.Since, want.Until = zero, zero, zero, zero
			if got != want {
				t.Errorf("got  %+v\nwant %+v", got, want)
			}
		})
	}
}

func TestLogsRefusesBadParameters(t *testing.T) {
	// An agent reads the error and tries again, so each says what was wrong and with what.
	for q, wantInErr := range map[string]string{
		"since=yesterday":             "since",
		"since=-2h":                   "since",
		"since=2w":                    "since",
		"until=soon":                  "until",
		"since=1h&until=2h":           "until",
		"limit=0":                     "limit",
		"limit=5001":                  "limit",
		"limit=many":                  "limit",
		"after=-1":                    "after",
		"after=x":                     "after",
		"level=loud":                  "level",
		"source=edge":                 "source",
		"format=xml":                  "format",
		"snapshots=maybe":             "snapshots",
		"componet=api":                "componet",
		"component=api&component=web": "component",
	} {
		t.Run(q, func(t *testing.T) {
			s, f := withFakeLogs()
			w := get(s, "/api/logs?"+q)
			var body struct{ Error string }
			json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusBadRequest || !strings.Contains(body.Error, wantInErr) || len(f.got) != 0 {
				t.Errorf("%d %q, want 400 naming %q", w.Code, body.Error, wantInErr)
			}
			if !strings.Contains(body.Error, "/api/help") {
				t.Errorf("error %q: it points at the help", body.Error)
			}
		})
	}
}

// liveLog is a server with a real infra log: two records about api, one about the pool.
func liveLog(t *testing.T) *Server {
	t.Helper()
	s, l, _ := testServer()
	sched := slog.New(l.Handler(slog.DiscardHandler, infralog.SourceScheduler, "i-0data1", "data-1"))
	sched.Info("adding VMs", "pool", "general", "from", 1, "to", 2)
	l.AddAgent("i-0a1b", "general-3", []api.LogRecord{
		{Time: now.Add(-2 * time.Minute), Level: "ERROR", Msg: "replica failed", Attrs: []api.LogAttr{{Key: "replica", Value: "api-aaaaaa"}, {Key: "reason", Value: "exited with code 137"}}},
		{Time: now.Add(-time.Minute), Level: "INFO", Msg: "starting a replica", Attrs: []api.LogAttr{{Key: "replica", Value: "api-bbbbbb"}}},
	})
	return s
}

func TestLogsAsJSON(t *testing.T) {
	s := liveLog(t)
	w := get(s, "/api/logs?component=api")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var got struct {
		Records []map[string]any `json:"records"`
		Latest  int64            `json:"latest"`
		More    bool             `json:"more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 2 || got.Latest != 3 || got.More {
		t.Fatalf("%s", w.Body)
	}
	r := got.Records[0]
	for k, want := range map[string]any{"msg": "replica failed", "level": "ERROR", "source": "agent", "vm": "i-0a1b", "vm_name": "general-3", "component": "api"} {
		if r[k] != want {
			t.Errorf("%s = %v, want %v", k, r[k], want)
		}
	}
	if _, ok := r["seq"]; !ok {
		t.Error("no seq: the UI tails with after=<seq>")
	}
	if w := get(s, "/api/logs?component=web"); !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Errorf("no records is an empty list, not null: %s", w.Body)
	}
}

func TestLogsAsText(t *testing.T) {
	s := liveLog(t)
	for name, edit := range map[string]func(*http.Request){
		"format=text": func(r *http.Request) { r.URL.RawQuery += "&format=text" },
		"Accept":      func(r *http.Request) { r.Header.Set("Accept", "text/plain") },
	} {
		t.Run(name, func(t *testing.T) {
			w := get(s, "/api/logs?component=api", edit)
			if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
			}
			lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
			if len(lines) != 2 || !strings.Contains(lines[0], "ERROR agent     general-3 api replica failed") || !strings.Contains(lines[0], "reason=\"exited with code 137\"") {
				t.Errorf("body:\n%s", w.Body)
			}
		})
	}
}

func TestLogsErrors(t *testing.T) {
	s, f := withFakeLogs()
	f.err = fmt.Errorf("%w: 9 days", infralog.ErrRange)
	if w := get(s, "/api/logs?since=9d"); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "9 days") {
		t.Errorf("a range too long: %d %s", w.Code, w.Body)
	}
	f.err = errors.New("AccessDenied")
	if w := get(s, "/api/logs?since=3d"); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("the archive unreadable: %d %s", w.Code, w.Body)
	}
	s.Logs = nil
	if w := get(s, "/api/logs"); w.Code != 200 || !strings.Contains(w.Body.String(), `"records":[]`) {
		t.Errorf("without a log: %d %s", w.Code, w.Body)
	}
}

func TestHelpTellsAnAgentHowToQuery(t *testing.T) {
	s, _, _ := testServer()
	w := get(s, "/api/help", func(r *http.Request) { r.Host = "localhost:9100" })
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		"GET /api/logs", "GET /api/status", "GET /api/reports",
		"component", "source", "vm", "level", "q", "since", "until", "after", "limit", "snapshots", "format",
		"curl -s 'http://localhost:9100/api/logs?component=", "format=text",
		"read-only", "shop", "dev", "as platform_admin", "platform_admin or org_admin",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(body, "localhost:7480") {
		t.Error("help names a port other than the one it was reached on")
	}
}

// The infra log holds no service output, so the help and the page send a reader to
// telemetry's Grafana for it, where the replica in a record finds its lines.
func TestServiceOutputIsInTelemetry(t *testing.T) {
	s, _, _ := testServer()
	help := get(s, "/api/help").Body.String()
	for _, want := range []string{"scripts/telemetry.sh", "http://localhost:7482/protostrap/help", "service_instance_id"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(help, "own output is in CloudWatch") {
		t.Error("help sends service output to CloudWatch, which only replicas started before telemetry use")
	}
	page := get(s, "/app.js").Body.String()
	if strings.Contains(page, "in its CloudWatch log group") || !strings.Contains(page, "Grafana") {
		t.Error("the Logs view doesn't send service output to Grafana")
	}
}

func TestWhoAmI(t *testing.T) {
	for role, want := range map[Role]string{"": `"role":"platform_admin"`, RolePlatformAdmin: `"role":"platform_admin"`, RoleOrgAdmin: `"role":"org_admin"`} {
		s, _, _ := testServer()
		s.Role = role
		if w := get(s, "/api/whoami"); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("role %q: %d %s", role, w.Code, w.Body)
		}
	}
}

func TestEveryRoleSeesTheSameReadOnlyViewForNow(t *testing.T) {
	// Org Admins will get actions of their own on their own port; until then both ports
	// serve the same view, and neither changes anything.
	for _, role := range []Role{RolePlatformAdmin, RoleOrgAdmin} {
		s, _, _ := testServer()
		s.Role = role
		for _, path := range []string{"/", "/api/status", "/api/logs", "/api/help", "/api/reports", "/api/whoami"} {
			if w := get(s, path); w.Code != 200 {
				t.Errorf("%s %s: %d", role, path, w.Code)
			}
			if w := get(s, path, func(r *http.Request) { r.Method = http.MethodPost }); w.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s POST %s: %d", role, path, w.Code)
			}
		}
	}
}

func TestLogsAndHelpAreForLoopbackOnly(t *testing.T) {
	s := liveLog(t)
	for _, path := range []string{"/api/logs", "/api/help", "/api/whoami"} {
		if w := get(s, path, func(r *http.Request) { r.RemoteAddr = "10.20.16.5:40000" }); w.Code != http.StatusForbidden {
			t.Errorf("%s from a VM: %d", path, w.Code)
		}
		if w := get(s, path, func(r *http.Request) { r.Host = "evil.example:7480" }); w.Code != http.StatusForbidden {
			t.Errorf("%s for another host: %d", path, w.Code)
		}
	}
}
