package telemetry

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover Grafana's proxy, which the scheduler serves on data-1's loopback for
// the protostrap-telemetry port forward (docs/telemetry.md#reading-telemetry).

// grafana serves a fake Grafana on a Unix socket, which echoes what it gets.
func grafana(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gf") // A socket's path is short: t.TempDir's can be too long.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "grafana.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(body))
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock
}

func serve(t *testing.T, p *Proxy, method, target, host, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Host = host
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

func TestProxyPassesRequestsToGrafanasSocket(t *testing.T) {
	p := &Proxy{Socket: grafana(t)}
	w := serve(t, p, "GET", "/explore?left=%7B%7D", "localhost:7482", "127.0.0.1:50000", "")
	if w.Code != 200 || w.Body.String() != "GET /explore?left=%7B%7D " {
		t.Errorf("GET: %d %q", w.Code, w.Body.String())
	}
	// Grafana queries its data sources with POST.
	w = serve(t, p, "POST", "/api/ds/query", "localhost:7482", "127.0.0.1:50000", `{"queries":[]}`)
	if w.Code != 200 || w.Body.String() != `POST /api/ds/query {"queries":[]}` {
		t.Errorf("POST: %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Seen-Host"); got != "localhost:7482" {
		t.Errorf("Grafana saw host %q", got)
	}
}

func TestProxyRefusesOtherHosts(t *testing.T) {
	// A page elsewhere can't read Grafana through a person's forward by pointing its own
	// name at 127.0.0.1.
	p := &Proxy{Socket: grafana(t)}
	for _, host := range []string{"localhost:7482", "127.0.0.1:7482", "[::1]:7482", "localhost"} {
		if w := serve(t, p, "GET", "/", host, "127.0.0.1:50000", ""); w.Code != 200 {
			t.Errorf("%s: %d", host, w.Code)
		}
	}
	for _, host := range []string{"evil.example.com", "evil.example.com:7482", "10.20.1.10:7482"} {
		if w := serve(t, p, "GET", "/", host, "127.0.0.1:50000", ""); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", host, w.Code)
		}
	}
}

func TestProxyRefusesRequestsFromElsewhere(t *testing.T) {
	p := &Proxy{Socket: grafana(t)}
	if w := serve(t, p, "GET", "/", "localhost:7482", "10.20.16.5:40000", ""); w.Code != http.StatusForbidden {
		t.Errorf("a request from another address: %d, want 403", w.Code)
	}
}

func TestProxyHelpsAnAgent(t *testing.T) {
	p := &Proxy{Socket: grafana(t)}
	w := serve(t, p, "GET", "/protostrap/help", "localhost:7482", "127.0.0.1:50000", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("%d %q", w.Code, w.Header().Get("Content-Type"))
	}
	body := w.Body.String()
	for _, want := range []string{
		"/api/datasources/proxy/uid/loki/loki/api/v1/query_range",
		"/api/datasources/proxy/uid/tempo/api/search",
		"/api/datasources/proxy/uid/prometheus/api/v1/query",
		"service_name", "service_instance_id", "log_iostream",
		"infra log", // It says where ProtoStrap's own records are not.
	} {
		if !strings.Contains(body, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

func TestProxyWhenTelemetryIsDown(t *testing.T) {
	p := &Proxy{Socket: filepath.Join(t.TempDir(), "missing.sock")}
	w := serve(t, p, "GET", "/", "localhost:7482", "127.0.0.1:50000", "")
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "telemetry") {
		t.Errorf("%d %q: say that telemetry isn't answering", w.Code, w.Body.String())
	}
}
