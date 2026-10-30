package probe

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	u, _ := http.NewRequest("GET", rawURL, nil)
	host, port, _ := net.SplitHostPort(u.URL.Host)
	p, _ := strconv.Atoi(port)
	return host, p
}

func TestHTTPReady(t *testing.T) {
	var gotHost, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(200)
	}))
	defer srv.Close()
	addr, port := hostPort(t, srv.URL)
	p := New(time.Second)
	if err := p.Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/healthz" {
		t.Errorf("path %q", gotPath)
	}
	// "A copy answers its check only when it is asked for its own service by name."
	if gotHost != "api" && gotHost != "api:"+strconv.Itoa(port) {
		t.Errorf("the check must name the service, Host = %q", gotHost)
	}
}

func TestHTTPNotReady(t *testing.T) {
	for _, status := range []int{500, 503, 404} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		addr, port := hostPort(t, srv.URL)
		err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api"})
		srv.Close()
		if err == nil {
			t.Errorf("status %d should not be ready", status)
		}
	}
}

func TestHTTPRedirectIsNotFollowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		t.Error("redirect followed")
	}))
	defer srv.Close()
	addr, port := hostPort(t, srv.URL)
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api"}); err != nil {
		t.Fatalf("a 3xx answer counts as ready: %v", err)
	}
}

// One rule says which HTTP answers mean ready, for the agent's probe and the edge's own check
// alike, so a replica the agent calls ready is never one the edge refuses to send to.
func TestReadyStatusesAreOneRule(t *testing.T) {
	for status := 100; status < 600; status++ {
		want := status >= ReadyStatuses.Start && status < ReadyStatuses.End
		if got := ReadyStatus(status); got != want {
			t.Errorf("ReadyStatus(%d) = %v, want %v", status, got, want)
		}
	}
	if ReadyStatuses.Start != 200 || ReadyStatuses.End != 400 {
		t.Errorf("ready statuses are %d up to %d, want 2xx and 3xx", ReadyStatuses.Start, ReadyStatuses.End)
	}
}

func TestHTTPInformationalIsNotReady(t *testing.T) {
	if ReadyStatus(101) || ReadyStatus(199) {
		t.Error("a 1xx answer is not ready")
	}
}

func TestHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	addr, port := hostPort(t, srv.URL)
	start := time.Now()
	err := New(50*time.Millisecond).Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api"})
	if err == nil || time.Since(start) > 400*time.Millisecond {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestConnectionRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	for _, check := range []string{"/healthz", CheckGRPC, CheckTCP} {
		if err := New(time.Second).Probe(t.Context(), Target{Addr: "127.0.0.1", Port: port, Check: check, Service: "x"}); err == nil {
			t.Errorf("%s: nothing listening is not ready", check)
		}
	}
}

func TestTCPReady(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if err := New(time.Second).Probe(t.Context(), Target{Addr: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, Check: CheckTCP}); err != nil {
		t.Fatal(err)
	}
}

// grpcHealth serves grpc.health.v1.Health/Check over cleartext HTTP/2, answering status for
// the service named in the request.
func grpcHealth(t *testing.T, status map[string]byte, grpcStatus string) (string, int) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("gRPC needs HTTP/2, got %s", r.Proto)
		}
		if r.URL.Path != "/grpc.health.v1.Health/Check" || r.Header.Get("Content-Type") != "application/grpc" {
			t.Errorf("bad request %s %s", r.URL.Path, r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) < 5 || body[0] != 0 || int(binary.BigEndian.Uint32(body[1:5])) != len(body)-5 {
			t.Errorf("bad frame %x", body)
		}
		msg := body[5:]
		service := ""
		if len(msg) > 2 && msg[0] == 0x0a {
			service = string(msg[2 : 2+int(msg[1])])
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		if s, ok := status[service]; ok {
			resp := []byte{0x08, s}
			frame := make([]byte, 5)
			binary.BigEndian.PutUint32(frame[1:], uint32(len(resp)))
			w.Write(append(frame, resp...))
			w.Header().Set("Grpc-Status", grpcStatus)
			return
		}
		w.Header().Set("Grpc-Status", "5") // 5 is NOT_FOUND.
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return hostPort(t, srv.URL)
}

func TestGRPCServing(t *testing.T) {
	addr, port := grpcHealth(t, map[string]byte{"api": 1}, "0")
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: CheckGRPC, Service: "api"}); err != nil {
		t.Fatal(err)
	}
}

func TestGRPCNotServing(t *testing.T) {
	addr, port := grpcHealth(t, map[string]byte{"api": 2}, "0")
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: CheckGRPC, Service: "api"}); err == nil {
		t.Fatal("NOT_SERVING is not ready")
	}
}

func TestGRPCAsksForItsOwnService(t *testing.T) {
	// The replica only knows "api". Asked as another service, it must not count as ready,
	// which protects against an address reused by another service.
	addr, port := grpcHealth(t, map[string]byte{"api": 1}, "0")
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: CheckGRPC, Service: "orders"}); err == nil {
		t.Fatal("a check for another service must fail")
	}
}

func TestGRPCErrorStatus(t *testing.T) {
	addr, port := grpcHealth(t, map[string]byte{"api": 1}, "14")
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: CheckGRPC, Service: "api"}); err == nil {
		t.Fatal("a non-zero grpc-status is not ready")
	}
}

func TestUnknownCheck(t *testing.T) {
	if err := New(time.Second).Probe(t.Context(), Target{Addr: "127.0.0.1", Port: 1, Check: "healthz"}); err == nil {
		t.Fatal("expected an error")
	}
	if err := New(time.Second).Probe(t.Context(), Target{Addr: "", Port: 1, Check: CheckTCP}); err == nil {
		t.Fatal("no address is not ready")
	}
}

func TestParseCheck(t *testing.T) {
	for in, want := range map[string]Check{
		"grpc":              {Kind: CheckGRPC},
		"grpc:9001":         {Kind: CheckGRPC, Port: 9001},
		"tcp":               {Kind: CheckTCP},
		"tcp:5432":          {Kind: CheckTCP, Port: 5432},
		"/healthz":          {Kind: CheckHTTP, Path: "/healthz"},
		"/":                 {Kind: CheckHTTP, Path: "/"},
		"9000/health/ready": {Kind: CheckHTTP, Port: 9000, Path: "/health/ready"},
		"8081/":             {Kind: CheckHTTP, Port: 8081, Path: "/"},
	} {
		got, err := ParseCheck(in)
		if err != nil {
			t.Errorf("ParseCheck(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCheck(%q) = %+v, want %+v", in, got, want)
		}
		if got.String() != in {
			t.Errorf("ParseCheck(%q).String() = %q: the form doesn't survive a round trip", in, got.String())
		}
	}
	for _, in := range []string{
		"", "healthz", "9000", "grpc:", "grpc:0", "grpc:70000", "grpc:x", "tcp:", "tcp:-1",
		"http://x/healthz", ":9000/x", "0/healthz", "70000/healthz", "grpc:9001/x", "/health z", "GRPC",
	} {
		if c, err := ParseCheck(in); err == nil {
			t.Errorf("ParseCheck(%q) = %+v, want an error", in, c)
		}
	}
}

// A check that names a port runs there, not on the replica's own port: Keycloak answers its
// health on 9000 and its requests on 8080.
func TestCheckOnAnotherPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/ready" {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	addr, port := hostPort(t, srv.URL)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	own := l.Addr().(*net.TCPAddr).Port
	l.Close() // Nothing listens on the replica's own port.
	check := strconv.Itoa(port) + "/health/ready"
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: own, Check: check, Service: "keycloak"}); err != nil {
		t.Fatalf("%s: %v", check, err)
	}

	gaddr, gport := grpcHealth(t, map[string]byte{"api": 1}, "0")
	check = "grpc:" + strconv.Itoa(gport)
	if err := New(time.Second).Probe(t.Context(), Target{Addr: gaddr, Port: own, Check: check, Service: "api"}); err != nil {
		t.Fatalf("%s: %v", check, err)
	}
}

// A worker has no port of its own, so its check must name one.
func TestCheckWithoutAnyPort(t *testing.T) {
	if err := New(time.Second).Probe(t.Context(), Target{Addr: "127.0.0.1", Check: "/healthz", Service: "w"}); err == nil {
		t.Error("an HTTP check with no port anywhere is not ready")
	}
}

// h2cOnly serves HTTP over cleartext HTTP/2 alone, as a Kestrel set to Http2 does.
func h2cOnly(t *testing.T) (string, int) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return hostPort(t, srv.URL)
}

// On its own port, an HTTP check speaks the replica's protocol: a service that only speaks
// cleartext HTTP/2 gets its check over it.
func TestHTTPCheckOverH2C(t *testing.T) {
	addr, port := h2cOnly(t)
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api", H2C: true}); err != nil {
		t.Fatalf("over h2c: %v", err)
	}
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: port, Check: "/healthz", Service: "api"}); err == nil {
		t.Error("HTTP/1.1 to an h2c-only server should fail")
	}
	// On another port, a check speaks HTTP/1.1 whatever the replica's own port speaks.
	if err := New(time.Second).Probe(t.Context(), Target{Addr: addr, Port: 1, Check: strconv.Itoa(port) + "/healthz", Service: "api", H2C: true}); err == nil {
		t.Error("a check on another port should speak HTTP/1.1")
	}
}
