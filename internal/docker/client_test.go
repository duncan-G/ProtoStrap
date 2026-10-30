package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/container"
)

// daemon is a fake Docker Engine API. Tests register handlers by "METHOD /path" with the
// version prefix stripped, and every request is recorded.
type daemon struct {
	t        *testing.T
	mu       sync.Mutex
	version  string
	handlers map[string]http.HandlerFunc
	requests []string
}

func newDaemon(t *testing.T) *daemon {
	return &daemon{t: t, version: "1.47", handlers: map[string]http.HandlerFunc{}}
}

func (d *daemon) on(route string, h http.HandlerFunc) { d.handlers[route] = h }

func (d *daemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if p, ok := strings.CutPrefix(path, "/v"+d.version); ok {
		path = p
	} else if path != "/_ping" {
		d.t.Errorf("request %s %s is not versioned", r.Method, r.URL.Path)
	}
	d.mu.Lock()
	d.requests = append(d.requests, r.Method+" "+path)
	d.mu.Unlock()
	if path == "/_ping" {
		w.Header().Set("API-Version", d.version)
		w.Write([]byte("OK"))
		return
	}
	for route, h := range d.handlers {
		method, pattern, _ := strings.Cut(route, " ")
		if method == r.Method && match(pattern, path) {
			h(w, r)
			return
		}
	}
	w.WriteHeader(404)
	fmt.Fprintf(w, `{"message":"no route for %s %s"}`, r.Method, path)
}

// match supports one trailing * in a pattern.
func match(pattern, path string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(path, p)
	}
	return pattern == path
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func newClient(t *testing.T, d *daemon, auth Auth) *Client {
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	c := NewHTTP(srv.URL, srv.Client(), auth)
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPingNegotiatesTheAPIVersion(t *testing.T) {
	d := newDaemon(t)
	d.version = "1.45"
	d.on("GET /info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"NCPU": 4}) })
	c := newClient(t, d, nil)
	if _, err := c.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestPingFailsWhenDockerIsDown(t *testing.T) {
	c := NewHTTP("http://127.0.0.1:1", http.DefaultClient, nil)
	if err := c.Ping(t.Context()); err == nil {
		t.Fatal("expected an error")
	}
}

func TestOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := newDaemon(t)
	srv := &http.Server{Handler: d}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	c := New(sock, nil)
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestInfo(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /info", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"NCPU":4,"MemTotal":16777216000,"Swarm":{"LocalNodeState":"active","ControlAvailable":false,
			"RemoteManagers":[{"NodeID":"n1","Addr":"10.20.1.10:2377"}]}}`))
	})
	c := newClient(t, d, nil)
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := container.Info{CPUs: 4, MemoryBytes: 16777216000,
		Swarm: container.Swarm{State: "active", Manager: false, Managers: []string{"10.20.1.10:2377"}}}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("got %+v", info)
	}
}

const inspectAPI = `{
  "Id": "c1", "Name": "/api-k3f9", "Created": "2026-10-24T10:00:00.123456789Z", "RestartCount": 2,
  "State": {"Status": "running", "ExitCode": 0},
  "Config": {"Image": "r/api:c-1", "Labels": {"protostrap.replica": "api-k3f9", "protostrap.component": "api"},
             "Env": ["A=1", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI=/v1/credentials/tok", "EQ=a=b"]},
  "HostConfig": {"CpusetCpus": ""},
  "NetworkSettings": {"Networks": {"protostrap": {"IPAddress": "10.200.0.7"}}}
}`

func TestContainersListsOnlyReplicas(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("all") != "1" && r.URL.Query().Get("all") != "true" {
			t.Errorf("stopped containers must be listed too: %s", r.URL.RawQuery)
		}
		var filters map[string][]string
		json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters)
		if !reflect.DeepEqual(filters["label"], []string{"protostrap.replica"}) {
			t.Errorf("filters = %v", filters)
		}
		w.Write([]byte(`[{"Id":"c1"},{"Id":"c2"}]`))
	})
	d.on("GET /containers/c1/json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(inspectAPI)) })
	d.on("GET /containers/c2/json", func(w http.ResponseWriter, r *http.Request) {
		// The container was removed between the list and the inspect.
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"No such container: c2"}`))
	})
	d.on("GET /networks/docker_gwbridge", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Containers":{"c1":{"IPv4Address":"172.18.0.5/24"},"gateway_x":{"IPv4Address":"172.18.0.2/24"}}}`))
	})
	c := newClient(t, d, nil)
	got, err := c.Containers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []container.Container{{
		ID: "c1", Name: "api-k3f9", Image: "r/api:c-1",
		Labels: map[string]string{"protostrap.replica": "api-k3f9", "protostrap.component": "api"},
		Env:    map[string]string{"A": "1", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v1/credentials/tok", "EQ": "a=b"},
		Status: container.Running, Restarts: 2,
		Created: time.Date(2026, 10, 24, 10, 0, 0, 123456789, time.UTC),
		Addrs:   []string{"172.18.0.5"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestContainersWithoutGatewayBridge(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /containers/json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[{"Id":"c1"}]`)) })
	d.on("GET /containers/c1/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"c1","Name":"/x","State":{"Status":"exited","ExitCode":3},"Config":{"Labels":{"protostrap.replica":"x"}},
			"NetworkSettings":{"Networks":{"bridge":{"IPAddress":"172.17.0.4"}}}}`))
	})
	c := newClient(t, d, nil) // docker_gwbridge has no handler, so the daemon answers 404.
	got, err := c.Containers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Status != container.Exited || got[0].ExitCode != 3 || !reflect.DeepEqual(got[0].Addrs, []string{"172.17.0.4"}) {
		t.Fatalf("got %+v", got)
	}
}

func TestStats(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /containers/c1/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stream") != "false" {
			t.Errorf("stats must not stream: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`{"read":"2026-10-24T10:00:05Z","cpu_stats":{"cpu_usage":{"total_usage":5000000000}},
			"memory_stats":{"usage":524288000,"stats":{"inactive_file":104857600}}}`))
	})
	c := newClient(t, d, nil)
	s, err := c.Stats(t.Context(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	want := container.Stats{CPUNanos: 5000000000, MemoryBytes: 524288000 - 104857600, Read: time.Date(2026, 10, 24, 10, 0, 5, 0, time.UTC)}
	if s != want {
		t.Fatalf("got %+v", s)
	}
}

func TestStatsCgroupV1(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /containers/c1/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"read":"2026-10-24T10:00:05Z","cpu_stats":{"cpu_usage":{"total_usage":1}},
			"memory_stats":{"usage":300,"stats":{"total_inactive_file":100}}}`))
	})
	c := newClient(t, d, nil)
	s, _ := c.Stats(t.Context(), "c1")
	if s.MemoryBytes != 200 {
		t.Fatalf("memory = %d", s.MemoryBytes)
	}
}

func TestHasImage(t *testing.T) {
	d := newDaemon(t)
	d.on("GET /images/1.dkr.ecr.eu-west-1.amazonaws.com/protostrap/api:c-1/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Id":"sha256:x"}`))
	})
	c := newClient(t, d, nil)
	if ok, err := c.HasImage(t.Context(), "1.dkr.ecr.eu-west-1.amazonaws.com/protostrap/api:c-1"); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, err := c.HasImage(t.Context(), "1.dkr.ecr.eu-west-1.amazonaws.com/protostrap/api:c-2"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

type fakeAuth struct {
	registries []string
	user, pass string
	ok         bool
	err        error
}

func (f *fakeAuth) Credentials(_ context.Context, registry string) (string, string, bool, error) {
	f.registries = append(f.registries, registry)
	return f.user, f.pass, f.ok, f.err
}

func TestPullSendsRegistryAuth(t *testing.T) {
	d := newDaemon(t)
	var gotAuth, gotImage string
	d.on("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		gotImage = r.URL.Query().Get("fromImage")
		gotAuth = r.Header.Get("X-Registry-Auth")
		w.Write([]byte(`{"status":"Pulling from protostrap/api"}` + "\n" + `{"status":"Download complete"}` + "\n"))
	})
	auth := &fakeAuth{user: "AWS", pass: "pw", ok: true}
	c := newClient(t, d, auth)
	img := "1.dkr.ecr.eu-west-1.amazonaws.com/protostrap/api:c-1"
	if err := c.Pull(t.Context(), img); err != nil {
		t.Fatal(err)
	}
	if gotImage != img {
		t.Errorf("fromImage = %q", gotImage)
	}
	if !reflect.DeepEqual(auth.registries, []string{"1.dkr.ecr.eu-west-1.amazonaws.com"}) {
		t.Errorf("asked for %v", auth.registries)
	}
	raw, err := base64.URLEncoding.DecodeString(gotAuth)
	if err != nil {
		t.Fatalf("auth header %q: %v", gotAuth, err)
	}
	var a map[string]string
	json.Unmarshal(raw, &a)
	if a["username"] != "AWS" || a["password"] != "pw" || a["serveraddress"] != "1.dkr.ecr.eu-west-1.amazonaws.com" {
		t.Errorf("auth = %v", a)
	}
}

func TestPullErrorInStream(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /images/create", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}` + "\n"))
	})
	c := newClient(t, d, &fakeAuth{})
	err := c.Pull(t.Context(), "r.example.com/x:1")
	if err == nil || !strings.Contains(err.Error(), "manifest unknown") {
		t.Fatalf("got %v", err)
	}
}

func TestPullAuthFailure(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /images/create", func(w http.ResponseWriter, r *http.Request) { t.Error("must not pull without credentials") })
	c := newClient(t, d, &fakeAuth{err: errors.New("helper broken")})
	if err := c.Pull(t.Context(), "1.dkr.ecr.eu-west-1.amazonaws.com/x:1"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCreateSendsTheWholeSpec(t *testing.T) {
	d := newDaemon(t)
	var name string
	var body map[string]any
	d.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		name = r.URL.Query().Get("name")
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"new-id","Warnings":[]}`))
	})
	c := newClient(t, d, nil)
	id, err := c.Create(t.Context(), container.Spec{
		Name: "postgres-a1", Image: "r/postgres:17.2", Cmd: []string{"postgres", "-c", "x=1"},
		Env:     []string{"A=1", "B=2"},
		Labels:  map[string]string{"protostrap.replica": "postgres-a1"},
		Network: "protostrap", Aliases: []string{"postgres"},
		MemoryBytes: 10 << 30, CPUShares: 3072, CPUSet: "0-2", StopTimeout: 30 * time.Second,
		Restart:   container.RestartUnlessStopped,
		LogDriver: "awslogs", LogOptions: map[string]string{"awslogs-group": "/protostrap/shop/postgres"},
		Mounts: []container.Mount{{Source: "/data/postgres", Target: "/var/lib/postgresql/data"}},
	})
	if err != nil || id != "new-id" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	if name != "postgres-a1" {
		t.Errorf("name = %q", name)
	}
	want := map[string]any{
		"Image":       "r/postgres:17.2",
		"Cmd":         []any{"postgres", "-c", "x=1"},
		"Env":         []any{"A=1", "B=2"},
		"Labels":      map[string]any{"protostrap.replica": "postgres-a1"},
		"StopTimeout": float64(30),
		"HostConfig": map[string]any{
			"NetworkMode":   "protostrap",
			"Memory":        float64(10 << 30),
			"MemorySwap":    float64(10 << 30),
			"CpuShares":     float64(3072),
			"CpusetCpus":    "0-2",
			"RestartPolicy": map[string]any{"Name": "unless-stopped"},
			"LogConfig":     map[string]any{"Type": "awslogs", "Config": map[string]any{"awslogs-group": "/protostrap/shop/postgres"}},
			"Mounts":        []any{map[string]any{"Type": "bind", "Source": "/data/postgres", "Target": "/var/lib/postgresql/data"}},
		},
		"NetworkingConfig": map[string]any{
			"EndpointsConfig": map[string]any{"protostrap": map[string]any{"Aliases": []any{"postgres"}}},
		},
	}
	if !reflect.DeepEqual(body, want) {
		got, _ := json.MarshalIndent(body, "", "  ")
		exp, _ := json.MarshalIndent(want, "", "  ")
		t.Fatalf("create body\n got %s\nwant %s", got, exp)
	}
}

func TestCreateReadOnlyMountAndNoRestart(t *testing.T) {
	d := newDaemon(t)
	var body struct {
		HostConfig struct {
			RestartPolicy struct{ Name string }
			Mounts        []struct{ ReadOnly bool }
		}
	}
	d.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"Id":"x"}`))
	})
	c := newClient(t, d, nil)
	_, err := c.Create(t.Context(), container.Spec{Name: "t", Image: "i", Restart: container.RestartNever,
		Mounts: []container.Mount{{Source: "/a", Target: "/b", ReadOnly: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if body.HostConfig.RestartPolicy.Name != "no" || !body.HostConfig.Mounts[0].ReadOnly {
		t.Fatalf("got %+v", body)
	}
}

// TestCreatePublishesAPortOnOneAddress checks the telemetry replica's port for Docker's
// output: published on data-1's address alone, never on every address.
func TestCreatePublishesAPortOnOneAddress(t *testing.T) {
	d := newDaemon(t)
	var body map[string]any
	d.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"Id":"x"}`))
	})
	c := newClient(t, d, nil)
	_, err := c.Create(t.Context(), container.Spec{Name: "telemetry-1", Image: "i",
		Publish: []container.Port{{HostIP: "10.20.1.10", HostPort: 24224, ContainerPort: 24224, Protocol: "tcp"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(body["ExposedPorts"]); got != "map[24224/tcp:map[]]" {
		t.Errorf("exposed ports %s", got)
	}
	host, _ := body["HostConfig"].(map[string]any)
	if got := fmt.Sprint(host["PortBindings"]); got != "map[24224/tcp:[map[HostIp:10.20.1.10 HostPort:24224]]]" {
		t.Errorf("port bindings %s", got)
	}

	// A container that publishes nothing sends neither.
	body = nil
	_, err = c.Create(t.Context(), container.Spec{Name: "api-1", Image: "i"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["ExposedPorts"]; ok {
		t.Errorf("exposed ports for a container that publishes none: %v", body)
	}
}

func TestCreateConflict(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"message":"Conflict. The container name \"/api-1\" is already in use"}`))
	})
	c := newClient(t, d, nil)
	_, err := c.Create(t.Context(), container.Spec{Name: "api-1", Image: "i"})
	var derr *Error
	if !errors.As(err, &derr) || derr.Status != 409 || !strings.Contains(derr.Message, "already in use") {
		t.Fatalf("got %v", err)
	}
}

func TestStartStopRemove(t *testing.T) {
	d := newDaemon(t)
	var stopT string
	d.on("POST /containers/c1/start", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	d.on("POST /containers/c2/start", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) })
	d.on("POST /containers/c1/stop", func(w http.ResponseWriter, r *http.Request) {
		stopT = r.URL.Query().Get("t")
		w.WriteHeader(204)
	})
	d.on("POST /containers/gone/stop", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	d.on("POST /containers/done/stop", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) })
	d.on("DELETE /containers/c1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") != "true" && r.URL.Query().Get("force") != "1" {
			t.Errorf("remove should force: %s", r.URL.RawQuery)
		}
		w.WriteHeader(204)
	})
	d.on("DELETE /containers/gone", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	c := newClient(t, d, nil)
	ctx := t.Context()
	if err := c.Start(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, "c2"); err != nil {
		t.Fatalf("already started is not an error: %v", err)
	}
	if err := c.Stop(ctx, "c1", 120*time.Second); err != nil || stopT != "120" {
		t.Fatalf("stop: %v t=%q", err, stopT)
	}
	if err := c.Stop(ctx, "gone", time.Second); err != nil {
		t.Fatalf("stopping a missing container is not an error: %v", err)
	}
	if err := c.Stop(ctx, "done", time.Second); err != nil {
		t.Fatalf("stopping a stopped container is not an error: %v", err)
	}
	if err := c.Remove(ctx, "c1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "gone"); err != nil {
		t.Fatalf("removing a missing container is not an error: %v", err)
	}
}

func TestStopOutlastsTheStopTimeout(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /containers/slow/stop", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(d)
	defer srv.Close()
	hc := srv.Client()
	hc.Timeout = 100 * time.Millisecond // A client-wide timeout must not cut a stop short.
	c := NewHTTP(srv.URL, hc, nil)
	c.Ping(t.Context())
	if err := c.Stop(t.Context(), "slow", time.Second); err != nil {
		t.Fatalf("stop was cut short: %v", err)
	}
}

func muxFrame(stream byte, payload string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
	return append(h, payload...)
}

func TestExecDemultiplexesOutput(t *testing.T) {
	d := newDaemon(t)
	var execBody map[string]any
	d.on("POST /containers/c1/exec", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&execBody)
		w.WriteHeader(201)
		w.Write([]byte(`{"Id":"e1"}`))
	})
	d.on("POST /exec/e1/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.docker.raw-stream")
		w.Write(muxFrame(1, "dump part 1,"))
		w.Write(muxFrame(2, "warning: x\n"))
		w.Write(muxFrame(1, "dump part 2"))
	})
	d.on("GET /exec/e1/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Running":false,"ExitCode":0}`))
	})
	c := newClient(t, d, nil)
	var out, errOut bytes.Buffer
	code, err := c.Exec(t.Context(), "c1", []string{"pg_dumpall", "-U", "postgres"}, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if out.String() != "dump part 1,dump part 2" || errOut.String() != "warning: x\n" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if execBody["AttachStdout"] != true || execBody["AttachStderr"] != true || execBody["Tty"] == true {
		t.Fatalf("exec body %v", execBody)
	}
}

func TestExecReportsExitCode(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /containers/c1/exec", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Id":"e2"}`)) })
	d.on("POST /exec/e2/start", func(w http.ResponseWriter, r *http.Request) { w.Write(muxFrame(2, "fatal")) })
	d.on("GET /exec/e2/json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Running":false,"ExitCode":2}`)) })
	c := newClient(t, d, nil)
	code, err := c.Exec(t.Context(), "c1", []string{"false"}, io.Discard, io.Discard)
	if err != nil || code != 2 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("upload failed") }

func TestExecStopsWhenTheWriterFails(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /containers/c1/exec", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"Id":"e3"}`)) })
	d.on("POST /exec/e3/start", func(w http.ResponseWriter, r *http.Request) { w.Write(muxFrame(1, "data")) })
	d.on("GET /exec/e3/json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"ExitCode":0}`)) })
	c := newClient(t, d, nil)
	if _, err := c.Exec(t.Context(), "c1", []string{"x"}, failWriter{}, io.Discard); err == nil {
		t.Fatal("expected the writer's error")
	}
}

func TestDemuxRejectsCorruptStream(t *testing.T) {
	var out bytes.Buffer
	err := demux(bytes.NewReader([]byte{9, 0, 0, 0, 0, 0, 0, 1, 'x'}), &out, &out)
	if err == nil {
		t.Fatal("an unknown stream type is an error")
	}
	if err := demux(bytes.NewReader(muxFrame(1, "abc")[:6]), &out, &out); err == nil {
		t.Fatal("a truncated header is an error")
	}
}

func TestJoinSwarm(t *testing.T) {
	d := newDaemon(t)
	var body map[string]any
	d.on("POST /swarm/join", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	})
	c := newClient(t, d, nil)
	err := c.JoinSwarm(t.Context(), container.Join{Token: "SWMTKN-1", Managers: []string{"10.20.1.10:2377"}, AdvertiseAddr: "10.20.16.5"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"ListenAddr": "0.0.0.0:2377", "AdvertiseAddr": "10.20.16.5",
		"RemoteAddrs": []any{"10.20.1.10:2377"}, "JoinToken": "SWMTKN-1"}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("join body %v", body)
	}
}

func TestLeaveSwarm(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /swarm/leave", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	c := newClient(t, d, nil)
	if err := c.LeaveSwarm(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestLeaveSwarmWhenNotInOne(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /swarm/leave", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte(`{"message":"This node is not part of a swarm"}`))
	})
	c := newClient(t, d, nil)
	if err := c.LeaveSwarm(t.Context()); err != nil {
		t.Fatalf("leaving when not in a swarm is not an error: %v", err)
	}
}

func TestErrorsCarryTheDaemonsMessage(t *testing.T) {
	d := newDaemon(t)
	d.on("POST /containers/c1/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"attaching to network failed, the swarm manager is down"}`))
	})
	c := newClient(t, d, nil)
	err := c.Start(t.Context(), "c1")
	if err == nil || !strings.Contains(err.Error(), "swarm manager is down") {
		t.Fatalf("got %v", err)
	}
}
