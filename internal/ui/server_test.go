package ui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/scheduler"
)

// The operator UI is read-only and loopback-only: operators reach it on data-1 through an
// SSM port forward, so every request comes from 127.0.0.1 with a localhost Host.

type fakeReports struct {
	list  []Report
	texts map[string]string
	err   error
}

func (f *fakeReports) Reports(context.Context) ([]Report, error) { return f.list, f.err }

func (f *fakeReports) Report(_ context.Context, date string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	text, ok := f.texts[date]
	if !ok {
		return nil, ErrNotFound
	}
	return []byte(text), nil
}

var now = time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)

func testServer() (*Server, *infralog.Log, *fakeReports) {
	l := &infralog.Log{Now: func() time.Time { return now }}
	reports := &fakeReports{
		list: []Report{
			{Date: "2026-10-23", Size: 10},
			{Date: "2026-10-25", Size: 30},
			{Date: "2026-10-24", Size: 20},
		},
		texts: map[string]string{"2026-10-25": "# shop, 25 October\n\nNothing happened."},
	}
	s := &Server{
		Status: func() scheduler.Status {
			return scheduler.Status{App: "shop", Environment: "dev", Version: "1.4.0", Target: "9f2c1e7",
				Components: []scheduler.ComponentStatus{{Name: "api", Kind: "service", Desired: 2, Ready: 2}}}
		},
		Logs:    l,
		Reports: reports,
		Logger:  slog.New(slog.DiscardHandler),
		Now:     func() time.Time { return now },
	}
	return s, l, reports
}

// get sends a request as an operator's browser does through the port forward.
func get(s http.Handler, path string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://localhost:7480"+path, nil)
	r.RemoteAddr = "127.0.0.1:51234"
	for _, f := range edit {
		f(r)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestServesThePage(t *testing.T) {
	s, _, _ := testServer()
	w := get(s, "/")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "<title>") {
		t.Error("no title in the page")
	}
	for path, want := range map[string]string{"/app.js": "text/javascript", "/app.css": "text/css"} {
		w := get(s, path)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), want) || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q, %d bytes", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
}

func TestThePageLoadsNothingFromElsewhere(t *testing.T) {
	// Operators may have no route to the internet, the CSP wouldn't allow one anyway, and the
	// UI shows the cluster's state: everything comes from the scheduler.
	s, _, _ := testServer()
	external := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`)
	for _, path := range []string{"/", "/app.js", "/app.css"} {
		body := get(s, path).Body.String()
		if m := external.FindString(body); m != "" {
			t.Errorf("%s loads %q from elsewhere", path, m)
		}
		if strings.Contains(body, "@import") || strings.Contains(body, "fonts.googleapis") {
			t.Errorf("%s imports a stylesheet or font", path)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	s, _, _ := testServer()
	for _, path := range []string{"/", "/app.js", "/api/status"} {
		h := get(s, path).Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q has no %q", path, csp, want)
			}
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", path, h)
		}
		if h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", path, h.Get("Cache-Control"))
		}
	}
}

func TestOnlyFromLoopback(t *testing.T) {
	s, _, _ := testServer()
	for _, addr := range []string{"10.20.16.5:40000", "172.18.0.4:40000", "203.0.113.9:443"} {
		w := get(s, "/api/status", func(r *http.Request) { r.RemoteAddr = addr })
		if w.Code != http.StatusForbidden {
			t.Errorf("from %s: %d, want 403", addr, w.Code)
		}
	}
	if w := get(s, "/api/status", func(r *http.Request) { r.RemoteAddr = "[::1]:40000" }); w.Code != 200 {
		t.Errorf("from ::1: %d", w.Code)
	}
}

func TestOnlyForLocalhostNames(t *testing.T) {
	// A page elsewhere could point its own name at 127.0.0.1 (DNS rebinding) and read the
	// UI through the operator's port forward: only loopback names are served.
	s, _, _ := testServer()
	for _, host := range []string{"localhost:7480", "localhost", "127.0.0.1:7480", "[::1]:7480", "LOCALHOST:9000"} {
		if w := get(s, "/api/status", func(r *http.Request) { r.Host = host }); w.Code != 200 {
			t.Errorf("Host %s: %d, want 200", host, w.Code)
		}
	}
	for _, host := range []string{"evil.example:7480", "localhost.evil.example", "10.20.1.10:7480", ""} {
		if w := get(s, "/api/status", func(r *http.Request) { r.Host = host }); w.Code != http.StatusForbidden {
			t.Errorf("Host %q: %d, want 403", host, w.Code)
		}
	}
}

func TestRefusesCrossSiteRequests(t *testing.T) {
	s, _, _ := testServer()
	w := get(s, "/api/status", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") })
	if w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d, want 403", w.Code)
	}
	for _, site := range []string{"same-origin", "none"} {
		if w := get(s, "/api/status", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", site) }); w.Code != 200 {
			t.Errorf("%s: %d", site, w.Code)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	s, _, _ := testServer()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		for _, path := range []string{"/", "/api/status", "/api/releases"} {
			w := get(s, path, func(r *http.Request) { r.Method = method })
			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d, Allow %q", method, path, w.Code, w.Header().Get("Allow"))
			}
		}
	}
	if w := get(s, "/api/status", func(r *http.Request) { r.Method = http.MethodHead }); w.Code != 200 {
		t.Errorf("HEAD: %d", w.Code)
	}
}

func TestStatus(t *testing.T) {
	s, _, _ := testServer()
	w := get(s, "/api/status")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var st scheduler.Status
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.App != "shop" || st.Target != "9f2c1e7" || len(st.Components) != 1 {
		t.Errorf("status %+v", st)
	}
}

func TestReportsNewestFirst(t *testing.T) {
	s, _, _ := testServer()
	var got struct {
		Reports []Report `json:"reports"`
	}
	w := get(s, "/api/reports")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
		t.Fatalf("%d %s %v", w.Code, w.Body, err)
	}
	var dates []string
	for _, r := range got.Reports {
		dates = append(dates, r.Date)
	}
	if strings.Join(dates, " ") != "2026-10-25 2026-10-24 2026-10-23" {
		t.Errorf("dates %v, want newest first", dates)
	}
}

func TestAReport(t *testing.T) {
	s, _, _ := testServer()
	w := get(s, "/api/reports/2026-10-25")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(w.Body.String(), "Nothing happened.") {
		t.Errorf("body %q", w.Body)
	}
	if w := get(s, "/api/reports/2026-10-24"); w.Code != http.StatusNotFound {
		t.Errorf("a missing report: %d", w.Code)
	}
}

func TestAReportByDateOnly(t *testing.T) {
	// The date becomes an S3 key: nothing but a date gets near the bucket.
	s, _, reports := testServer()
	reports.texts["../state.json"] = "secret state"
	for _, path := range []string{"/api/reports/..%2Fstate.json", "/api/reports/2026-10-25.md", "/api/reports/2026-1-5", "/api/reports/latest", "/api/reports/2026-10-25/x"} {
		w := get(s, path)
		if w.Code == 200 {
			t.Errorf("%s: 200 %q", path, w.Body)
		}
	}
}

func TestReportsUnavailable(t *testing.T) {
	s, _, reports := testServer()
	reports.err = errors.New("AccessDenied")
	if w := get(s, "/api/reports"); w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "AccessDenied") {
		t.Errorf("list: %d %s", w.Code, w.Body)
	}
	if w := get(s, "/api/reports/2026-10-25"); w.Code != http.StatusBadGateway {
		t.Errorf("get: %d", w.Code)
	}
	s.Reports = nil
	if w := get(s, "/api/reports"); w.Code != 200 || !strings.Contains(w.Body.String(), `"reports":[]`) {
		t.Errorf("without reports: %d %s", w.Code, w.Body)
	}
}

func TestReportsTimeOut(t *testing.T) {
	s, _, _ := testServer()
	s.Reports = slowReports{}
	s.Timeout = 50 * time.Millisecond
	start := time.Now()
	w := get(s, "/api/reports")
	if w.Code != http.StatusBadGateway || time.Since(start) > 2*time.Second {
		t.Errorf("%d after %v", w.Code, time.Since(start))
	}
}

type slowReports struct{}

func (slowReports) Reports(ctx context.Context) ([]Report, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (slowReports) Report(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestUnknownPaths(t *testing.T) {
	s, _, _ := testServer()
	for _, path := range []string{"/v1/status", "/api/", "/api/releases", "/api/decisions", "/index.html/x", "/app.js/"} {
		if w := get(s, path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

func TestOverRealHTTP(t *testing.T) {
	// This takes the whole path, through a listener on loopback, as the port forward reaches it.
	s, _, _ := testServer()
	srv := httptest.NewServer(s)
	defer srv.Close()
	resp, err := http.Get(strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"app":"shop"`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}
