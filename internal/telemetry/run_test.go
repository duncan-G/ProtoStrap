package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover `protostrap telemetry`, the telemetry container's entrypoint: it runs
// five programs as the edge runs two, answers the agent's readiness check, and reloads
// Prometheus when the scrape targets change.

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prog")
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

// fakeProgram is a fake program that logs its start, its arguments and its stop to log.
func fakeProgram(t *testing.T, name, log string) string {
	return script(t, `echo "`+name+` started $*" >> `+log+`; trap 'echo "`+name+` stopped" >> `+log+`; exit 0' TERM; trap 'echo "`+name+` reloaded" >> `+log+`' HUP; while :; do sleep 0.02; done`)
}

// ok is a readiness check that passes while ready holds.
func ok(t *testing.T, ready *atomic.Bool) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// runConfig is a Config whose programs are fake ones that log to log, and whose readiness
// checks pass.
func runConfig(t *testing.T, log string) Config {
	t.Helper()
	dir := t.TempDir()
	var ready atomic.Bool
	ready.Store(true)
	c := testConfig()
	c.DataDir, c.FilesDir, c.SocketDir, c.ConfigDir = filepath.Join(dir, "data"), filepath.Join(dir, "files"), filepath.Join(dir, "socket"), filepath.Join(dir, "config")
	os.MkdirAll(c.FilesDir, 0o755)
	c.StatusAddr = "127.0.0.1:0"
	c.UID, c.GID = -1, -1
	c.Drain, c.ScrapeEvery = 10*time.Millisecond, 20*time.Millisecond
	c.Bin = Binaries{
		Collector: fakeProgram(t, "collector", log), Loki: fakeProgram(t, "loki", log), Tempo: fakeProgram(t, "tempo", log),
		Prometheus: fakeProgram(t, "prometheus", log), Grafana: fakeProgram(t, "grafana", log), GrafanaHome: dir,
	}
	url := ok(t, &ready)
	c.Checks = Checks{Collector: url, Loki: url, Tempo: url, Prometheus: url, Grafana: url}
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func read(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestRunStartsEveryProgramAndStopsTheCollectorFirst(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	c := runConfig(t, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	waitFor(t, "five programs", func() bool { return strings.Count(read(log), " started") == 5 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't return after the signal")
	}
	got := read(log)
	for _, want := range []string{
		"collector started --config=" + filepath.Join(c.ConfigDir, "collector.json"),
		"loki started -config.file=" + filepath.Join(c.ConfigDir, "loki.json"),
		"tempo started -config.file=" + filepath.Join(c.ConfigDir, "tempo.json"),
		"prometheus started --config.file=" + filepath.Join(c.ConfigDir, "prometheus.json"),
		"grafana started server",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	// Intake stops first, so nothing arrives at a store that is shutting down; Loki and
	// Tempo, which flush what they hold, stop last.
	stops := []string{}
	for _, line := range strings.Split(got, "\n") {
		if name, ok := strings.CutSuffix(line, " stopped"); ok {
			stops = append(stops, name)
		}
	}
	if len(stops) != 5 || stops[0] != "collector" || (stops[4] != "loki" && stops[4] != "tempo") {
		t.Errorf("stopped in the order %v: the collector first, Loki and Tempo last", stops)
	}
}

func TestRunWritesEveryConfigAndDirectory(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	c := runConfig(t, log)
	os.WriteFile(filepath.Join(c.FilesDir, "scrape.json"), []byte(`[{"component":"api","port":9090,"path":"/metrics"}]`), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	waitFor(t, "five programs", func() bool { return strings.Count(read(log), " started") == 5 })
	for _, name := range []string{"collector.json", "loki.json", "tempo.json", "prometheus.json", "grafana.ini",
		"grafana/provisioning/datasources/protostrap.json"} {
		if _, err := os.Stat(filepath.Join(c.ConfigDir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !strings.Contains(read(filepath.Join(c.ConfigDir, "prometheus.json")), `"api"`) {
		t.Errorf("Prometheus's config lacks the targets the agent wrote")
	}
	for _, dir := range []string{"loki", "tempo", "prometheus"} {
		if info, err := os.Stat(filepath.Join(c.DataDir, dir)); err != nil || !info.IsDir() {
			t.Errorf("%s's directory on the volume: %v", dir, err)
		}
	}
	if info, err := os.Stat(filepath.Join(c.ConfigDir, "grafana", "data")); err != nil || !info.IsDir() {
		t.Errorf("Grafana's own data directory, in the container: %v", err)
	}
	if info, err := os.Stat(c.SocketDir); err != nil || !info.IsDir() {
		t.Errorf("the socket directory: %v", err)
	}
	cancel()
	<-done
}

func TestRunFailsWhenAProgramExits(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	c := runConfig(t, log)
	c.Bin.Tempo = script(t, `sleep 0.2; exit 3`)
	errc := make(chan error, 1)
	go func() { errc <- Run(context.Background(), c) }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "tempo") {
			t.Errorf("err %v: Docker restarts the container when any program dies", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run carried on without Tempo")
	}
	if got := read(log); strings.Count(got, " stopped") != 4 {
		t.Errorf("the others weren't stopped:\n%s", got)
	}
}

func TestRunReloadsPrometheusWhenTheTargetsChange(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	c := runConfig(t, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	waitFor(t, "five programs", func() bool { return strings.Count(read(log), " started") == 5 })
	if strings.Contains(read(log), "reloaded") {
		t.Fatal("reloaded before anything changed")
	}
	os.WriteFile(filepath.Join(c.FilesDir, "scrape.json"), []byte(`[{"component":"web","port":9100,"path":"/stats"}]`), 0o644)
	waitFor(t, "a reload", func() bool { return strings.Contains(read(log), "prometheus reloaded") })
	if !strings.Contains(read(filepath.Join(c.ConfigDir, "prometheus.json")), `"web"`) {
		t.Error("Prometheus was reloaded without the new targets")
	}
	// A broken file keeps the targets Prometheus has.
	os.WriteFile(filepath.Join(c.FilesDir, "scrape.json"), []byte(`not json`), 0o644)
	time.Sleep(100 * time.Millisecond)
	if !strings.Contains(read(filepath.Join(c.ConfigDir, "prometheus.json")), `"web"`) {
		t.Error("a broken targets file replaced the good ones")
	}
	cancel()
	<-done
}

func TestRunRemovesAStaleSocketBeforeGrafanaStarts(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	c := runConfig(t, log)
	os.MkdirAll(c.SocketDir, 0o755)
	sock := filepath.Join(c.SocketDir, "grafana.sock")
	os.WriteFile(sock, nil, 0o644)
	c.Bin.Grafana = script(t, `if [ -e `+sock+` ]; then echo "grafana saw a stale socket" >> `+log+`; fi; echo "grafana started" >> `+log+`; trap 'exit 0' TERM; while :; do sleep 0.02; done`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	waitFor(t, "grafana", func() bool { return strings.Contains(read(log), "grafana started") })
	if strings.Contains(read(log), "stale socket") {
		t.Error("Grafana can't listen on a socket left by the last run")
	}
	cancel()
	<-done
}

func get(t *testing.T, h http.Handler, path, host string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = host
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	b, _ := io.ReadAll(w.Body)
	return w.Code, string(b)
}

func TestReadinessNeedsEveryProgramAndItsName(t *testing.T) {
	var loki, rest atomic.Bool
	rest.Store(true)
	lokiURL, restURL := ok(t, &loki), ok(t, &rest)
	st := newStatus(Checks{Collector: restURL, Loki: lokiURL, Tempo: restURL, Prometheus: restURL, Grafana: restURL}, "", http.DefaultClient)

	if code, body := get(t, st, "/ready", "telemetry"); code != http.StatusServiceUnavailable || !strings.Contains(body, "loki") {
		t.Errorf("ready without Loki: %d %q", code, body)
	}
	loki.Store(true)
	if code, _ := get(t, st, "/ready", "telemetry:13133"); code != http.StatusOK {
		t.Errorf("not ready with every program up: %d", code)
	}
	// An address reused by another component never passes.
	if code, _ := get(t, st, "/ready", "api"); code != http.StatusNotFound {
		t.Errorf("ready for another name: %d", code)
	}
	st.draining.Store(true)
	if code, _ := get(t, st, "/ready", "telemetry"); code != http.StatusServiceUnavailable {
		t.Errorf("ready while stopping: %d", code)
	}
	// Nothing but readiness is served on the overlay.
	if code, _ := get(t, st, "/loki/api/v1/query_range", "telemetry"); code != http.StatusNotFound {
		t.Errorf("the status port passes other paths on: %d", code)
	}
}
