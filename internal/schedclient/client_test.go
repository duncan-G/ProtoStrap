package schedclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
)

var identity = Identity{Document: `{"instanceId":"i-1"}`, Signature: "MIAGCSqG"}

func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c, err := New(strings.TrimPrefix(srv.URL, "https://"), &tls.Config{RootCAs: pool}, identity)
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func checkIdentity(t *testing.T, r *http.Request) {
	t.Helper()
	doc, err := base64.StdEncoding.DecodeString(r.Header.Get("X-ProtoStrap-Identity"))
	if err != nil || string(doc) != identity.Document {
		t.Errorf("identity header %q", r.Header.Get("X-ProtoStrap-Identity"))
	}
	if r.Header.Get("X-ProtoStrap-Signature") != identity.Signature {
		t.Errorf("signature header %q", r.Header.Get("X-ProtoStrap-Signature"))
	}
}

func TestJoin(t *testing.T) {
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/join" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		checkIdentity(t, r)
		w.Write([]byte(`{"swarm_id":"s1","token":"SWMTKN-1-abc","manager":"10.20.1.10:2377"}`))
	})
	j, err := c.Join(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if j != (api.Join{SwarmID: "s1", Token: "SWMTKN-1-abc", Manager: "10.20.1.10:2377"}) {
		t.Fatalf("got %+v", j)
	}
}

func TestSync(t *testing.T) {
	var got api.Report
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/sync" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		checkIdentity(t, r)
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"version":"1.4.0","replicas":[{"id":"api-1","component":"api","kind":"service","image":"i","request":{"cpu":500,"memory":512},"stop":30}]}`))
	})
	a, err := c.Sync(t.Context(), api.Report{VM: "i-1", Pool: "general", Replicas: []api.ReplicaReport{}})
	if err != nil {
		t.Fatal(err)
	}
	if got.VM != "i-1" || got.Pool != "general" {
		t.Errorf("report sent %+v", got)
	}
	if a.Version != "1.4.0" || len(a.Replicas) != 1 || a.Replicas[0].ID != "api-1" {
		t.Fatalf("got %+v", a)
	}
}

func TestSyncRejectsInvalidReplicas(t *testing.T) {
	// A bad assignment must not be half-applied: the agent keeps what runs.
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"1.4.0","replicas":[{"id":"api 1","component":"api","kind":"service","image":"i","request":{"cpu":500,"memory":512}}]}`))
	})
	if _, err := c.Sync(t.Context(), api.Report{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSyncRejectsDuplicateReplicas(t *testing.T) {
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		one := `{"id":"api-1","component":"api","kind":"service","image":"i","request":{"cpu":500,"memory":512}}`
		w.Write([]byte(`{"replicas":[` + one + `,` + one + `]}`))
	})
	if _, err := c.Sync(t.Context(), api.Report{}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestServerError(t *testing.T) {
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown VM i-1", http.StatusForbidden)
	})
	_, err := c.Sync(t.Context(), api.Report{})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "unknown VM") {
		t.Fatalf("got %v", err)
	}
}

func TestUntrustedCertificateIsRefused(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached an untrusted server")
	}))
	defer srv.Close()
	c, _ := New(strings.TrimPrefix(srv.URL, "https://"), &tls.Config{RootCAs: x509.NewCertPool()}, identity)
	if _, err := c.Join(t.Context()); err == nil {
		t.Fatal("expected a certificate error")
	}
}

func TestTimeout(t *testing.T) {
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	c.Timeout = 50 * time.Millisecond
	if _, err := c.Sync(t.Context(), api.Report{}); err == nil {
		t.Fatal("expected a timeout")
	}
}

func TestLoadCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o644)
	cfg, err := TLSConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil || cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("got %+v", cfg)
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	os.WriteFile(bad, []byte("not a certificate"), 0o644)
	if _, err := TLSConfig(bad); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := TLSConfig(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestConfig(t *testing.T) {
	files := configfiles.Files{"lds.yaml": []byte("listeners\n"), "conf.d/a.conf": []byte("a\n")}
	digest := configfiles.Digest(files)
	_, c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/config/"+digest {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		checkIdentity(t, r)
		json.NewEncoder(w).Encode(files)
	})
	got, err := c.Config(t.Context(), digest)
	if err != nil || string(got["lds.yaml"]) != "listeners\n" || string(got["conf.d/a.conf"]) != "a\n" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := c.Config(t.Context(), "../state"); err == nil {
		t.Errorf("a digest that isn't one was asked for")
	}
}
