package edge

import (
	"context"
	"encoding/json"
	"errors"
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

func TestBootstrapReadsTheAgentsFiles(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal([]byte(Bootstrap("/etc/protostrap", 9901)), &v); err != nil {
		t.Fatal(err)
	}
	b := Bootstrap("/etc/protostrap", 9901)
	for _, want := range []string{`"/etc/protostrap/lds.json"`, `"/etc/protostrap/cds.json"`, `"watched_directory"`, `"port_value": 9901`, `"127.0.0.1"`} {
		if !strings.Contains(b, want) {
			t.Errorf("bootstrap lacks %s", want)
		}
	}
}

func TestBootstrapCapsConnectionsWithAResourceMonitor(t *testing.T) {
	b := Bootstrap("/etc/protostrap", 9901)
	for _, want := range []string{
		`"envoy.resource_monitors.global_downstream_max_connections"`,
		`"type.googleapis.com/envoy.extensions.resource_monitors.downstream_connections.v3.DownstreamConnectionsConfig"`,
		`"max_active_downstream_connections": 50000`,
	} {
		if !strings.Contains(b, want) {
			t.Errorf("bootstrap lacks %s", want)
		}
	}
	// Envoy 1.39 warns that the runtime key will be removed.
	if strings.Contains(b, "overload.global_downstream_max_connections") {
		t.Error("bootstrap caps connections with the deprecated runtime key")
	}
}

// Envoy compiles a pattern up to 1000 in RE2's program size, not 100, so 30 skipped
// extensions fit in a route's pattern.
func TestBootstrapRaisesRE2sLimit(t *testing.T) {
	if b := Bootstrap("/etc/protostrap", 9901); !strings.Contains(b, `"re2.max_program_size.error_level": 1000`) {
		t.Errorf("bootstrap keeps RE2's limit of 100:\n%s", b)
	}
}

func TestTunnelToken(t *testing.T) {
	tests := []struct {
		secret, prefix, want string
		ok                   bool
	}{
		// The environment's secret holds the token under the edge's prefix, among others' keys.
		{`{"Edge_TUNNEL_TOKEN":"abc","Api_StripeKey":"sk"}`, "Edge_", "abc", true},
		{`{"Edge_TUNNEL_TOKEN":""}`, "Edge_", "", false},
		{`{"TUNNEL_TOKEN":"abc"}`, "Edge_", "", false},
		{`{}`, "Edge_", "", false},
		{`not json`, "Edge_", "", false},
		// An agent from before the environment's secret gives no prefix: the edge's own secret.
		{`{"TUNNEL_TOKEN":"abc"}`, "", "abc", true},
	}
	for _, tt := range tests {
		got, err := tunnelToken(tt.secret, tt.prefix)
		if (err == nil) != tt.ok || got != tt.want {
			t.Errorf("tunnelToken(%s, %q) = %q, %v", tt.secret, tt.prefix, got, err)
		}
		if err != nil && strings.Contains(err.Error(), "sk") {
			t.Errorf("the error quotes the secret: %v", err)
		}
	}
	if _, err := tunnelToken(`{}`, "Edge_"); err == nil || !strings.Contains(err.Error(), "Edge_TUNNEL_TOKEN") {
		t.Errorf("the error should name the key to set: %v", err)
	}
}

// upstreams are Envoy's admin and cloudflared's metrics server, faked.
func upstreams(t *testing.T, envoyReady, tunnelReady *atomic.Bool) (string, string) {
	envoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ready":
			if envoyReady.Load() {
				w.Write([]byte("LIVE\n"))
				return
			}
			w.WriteHeader(503)
		case "/stats/prometheus":
			w.Write([]byte("# TYPE envoy_cluster_upstream_rq_total counter\nenvoy_cluster_upstream_rq_total{envoy_cluster_name=\"api\"} 42\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ready" && tunnelReady.Load() {
			w.Write([]byte(`{"status":200,"readyConnections":2}`))
			return
		}
		w.WriteHeader(503)
	}))
	t.Cleanup(envoy.Close)
	t.Cleanup(tunnel.Close)
	return envoy.URL, tunnel.URL
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

func TestReadinessNeedsBothProgramsAndItsName(t *testing.T) {
	var envoyReady, tunnelReady atomic.Bool
	envoyURL, tunnelURL := upstreams(t, &envoyReady, &tunnelReady)
	st := &status{envoyAdmin: envoyURL, tunnelMetrics: tunnelURL, client: http.DefaultClient}
	if code, _ := get(t, st, "/ready", "edge"); code != 503 {
		t.Errorf("ready with neither program up: %d", code)
	}
	envoyReady.Store(true)
	if code, _ := get(t, st, "/ready", "edge"); code != 503 {
		t.Errorf("ready without a tunnel connection: %d", code)
	}
	tunnelReady.Store(true)
	if code, _ := get(t, st, "/ready", "edge"); code != 200 {
		t.Errorf("not ready with both up: %d", code)
	}
	if code, _ := get(t, st, "/ready", "api"); code == 200 {
		t.Errorf("ready when asked as another service: an address reused by another service must fail")
	}
	if code, _ := get(t, st, "/ready", "edge:9902"); code != 200 {
		t.Errorf("the port in the host isn't part of the name: %d", code)
	}
	st.draining.Store(true)
	if code, _ := get(t, st, "/ready", "edge"); code != 503 {
		t.Errorf("ready while draining: %d", code)
	}
}

func TestMetricsAreEnvoysCounts(t *testing.T) {
	var on atomic.Bool
	envoyURL, tunnelURL := upstreams(t, &on, &on)
	st := &status{envoyAdmin: envoyURL, tunnelMetrics: tunnelURL, client: http.DefaultClient}
	code, body := get(t, st, "/metrics", "")
	if code != 200 || !strings.Contains(body, `envoy_cluster_upstream_rq_total{envoy_cluster_name="api"} 42`) {
		t.Errorf("metrics %d %q", code, body)
	}
	if code, _ := get(t, st, "/quitquitquit", "edge"); code != 404 {
		t.Errorf("Envoy's admin is reachable through the status port: %d", code)
	}
}

// script writes an executable shell script and returns its path.
func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prog")
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

func TestRunStartsBothAndStopsOnTheSignal(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	envoy := script(t, `echo "envoy $*" >> `+log+`; trap 'echo envoy stopped >> `+log+`; exit 0' TERM; while :; do sleep 0.05; done`)
	tunnel := script(t, `echo "cloudflared $* token=$TUNNEL_TOKEN" >> `+log+`; trap 'echo cloudflared stopped >> `+log+`; exit 0' TERM; while :; do sleep 0.05; done`)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Envoy: envoy, Cloudflared: tunnel, FilesDir: dir, BootstrapPath: filepath.Join(dir, "bootstrap.json"),
			StatusAddr: "127.0.0.1:0", AdminPort: 9901, Drain: 50 * time.Millisecond,
			Token: func(context.Context) (string, error) { return "s3cr3t", nil },
		})
	}()
	waitFor(t, func() bool {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "\n") >= 2
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after the signal")
	}
	b, _ := os.ReadFile(log)
	got := string(b)
	for _, want := range []string{"envoy -c " + filepath.Join(dir, "bootstrap.json"), "cloudflared tunnel", "token=s3cr3t", "envoy stopped", "cloudflared stopped"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "run --token") {
		t.Errorf("the token is on cloudflared's command line, where ps shows it")
	}
	if strings.Index(got, "cloudflared stopped") > strings.Index(got, "envoy stopped") {
		t.Errorf("Envoy stopped before cloudflared: the tunnel must stop sending requests first\n%s", got)
	}
}

func TestRunFailsWhenAProgramExits(t *testing.T) {
	dir := t.TempDir()
	envoy := script(t, `sleep 0.1; exit 3`)
	tunnel := script(t, `trap 'exit 0' TERM; while :; do sleep 0.05; done`)
	err := Run(context.Background(), Config{
		Envoy: envoy, Cloudflared: tunnel, FilesDir: dir, BootstrapPath: filepath.Join(dir, "b.json"),
		StatusAddr: "127.0.0.1:0", Drain: time.Millisecond,
		Token: func(context.Context) (string, error) { return "t", nil },
	})
	if err == nil || !strings.Contains(err.Error(), "envoy") {
		t.Errorf("err %v: Docker restarts the edge when either program dies", err)
	}
}

func TestRunWaitsForTheTunnelToken(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := Run(ctx, Config{
		Envoy: script(t, "exit 0"), Cloudflared: script(t, "exit 0"), FilesDir: dir, BootstrapPath: filepath.Join(dir, "b.json"),
		StatusAddr: "127.0.0.1:0", TokenRetry: 20 * time.Millisecond,
		Token: func(context.Context) (string, error) { calls++; return "", errors.New("not set yet") },
	})
	if err != nil {
		t.Errorf("a stop while waiting for the token: %v", err)
	}
	if calls < 3 {
		t.Errorf("asked for the token %d times", calls)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("timed out")
}
