package agent

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestServiceReplicaSettings(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-k3f9"))
	h.mustTick()
	s := h.rt.specOf("api-k3f9")

	if s.Name != "api-k3f9" || s.Image != registry+"api:c-4e1a90b2" {
		t.Errorf("name %q image %q", s.Name, s.Image)
	}
	wantLabels := map[string]string{
		container.LabelReplica:     "api-k3f9",
		container.LabelComponent:   "api",
		container.LabelRelease:     "9f2c1e7",
		container.LabelKind:        "service",
		container.LabelFingerprint: "fp-api",
		container.LabelApp:         "shop",
		container.LabelStop:        "30",
		container.LabelRole:        "arn:aws:iam::123456789012:role/protostrap-shop-api",
		container.LabelReady:       "grpc",
		container.LabelPort:        "8080",
		container.LabelMetrics:     "9090/metrics",
	}
	if !maps.Equal(s.Labels, wantLabels) {
		t.Errorf("labels\n got %v\nwant %v", s.Labels, wantLabels)
	}
	if s.Network != "protostrap" || !slices.Equal(s.Aliases, []string{"api"}) {
		t.Errorf("network %q aliases %v: the component's name must resolve to its replicas", s.Network, s.Aliases)
	}
	if s.MemoryBytes != 512<<20 {
		t.Errorf("memory limit %d, want a hard limit at the request", s.MemoryBytes)
	}
	if s.CPUShares != 512 || s.CPUSet != "" {
		t.Errorf("cpu shares %d cpuset %q: a share in proportion to the request, not a cap", s.CPUShares, s.CPUSet)
	}
	if s.StopTimeout != 30*time.Second {
		t.Errorf("stop timeout %v", s.StopTimeout)
	}
	if s.Restart != container.RestartUnlessStopped {
		t.Errorf("restart %q: Docker restarts a crashed replica even if the agent is down", s.Restart)
	}
	if s.LogDriver != "fluentd" || s.LogOptions["fluentd-address"] != "tcp://10.20.1.10:24224" {
		t.Errorf("log driver %q %v: output goes to the telemetry collector on data-1", s.LogDriver, s.LogOptions)
	}
	if len(s.Publish) != 0 {
		t.Errorf("a service publishes no port: %+v", s.Publish)
	}
	if len(s.Mounts) != 0 || len(s.Cmd) != 0 {
		t.Errorf("a service has no mounts and keeps its image's command: %+v %v", s.Mounts, s.Cmd)
	}
	if !slices.IsSorted(s.Env) {
		t.Errorf("env must be sorted so it is stable: %v", s.Env)
	}
	env := envMap(s.Env)
	wantEnv := map[string]string{
		"QUEUE_ORDERS_URL":                       "https://sqs/orders",
		"LOG_LEVEL":                              "info",
		"PROTOSTRAP_APP":                         "shop",
		"PROTOSTRAP_COMPONENT":                   "api",
		"PROTOSTRAP_REPLICA":                     "api-k3f9",
		"PROTOSTRAP_RELEASE":                     "9f2c1e7",
		"SECRET_ID":                              "shop/prod",
		"SECRET_PREFIX":                          "Api_",
		"AWS_REGION":                             "eu-west-1",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/v1/credentials/token-1",
	}
	if !maps.Equal(env, wantEnv) {
		t.Errorf("env\n got %v\nwant %v", env, wantEnv)
	}
}

func TestProtoStrapVariablesCannotBeOverridden(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.Env = map[string]string{
		"PROTOSTRAP_REPLICA": "spoofed", "AWS_REGION": "us-east-1",
		"AWS_CONTAINER_CREDENTIALS_RELATIVE_URI": "/steal", "SECRET_ID": "protostrap/other",
		"SECRET_PREFIX": "Other_",
	}
	h.sched.assign(s)
	h.mustTick()
	env := envMap(h.rt.specOf("api-1").Env)
	if env["PROTOSTRAP_REPLICA"] != "api-1" || env["AWS_REGION"] != "eu-west-1" ||
		env["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"] != "/v1/credentials/token-1" || env["SECRET_ID"] != "shop/prod" ||
		env["SECRET_PREFIX"] != "Api_" {
		t.Fatalf("env %v", env)
	}
}

func TestCredentialsAreServedBeforeTheReplicaStarts(t *testing.T) {
	// A replica reads its secret as soon as it starts, so its credentials must already be there.
	h := newHarness(t)
	var servedAtStart bool
	h.rt.onStart = func(name string) {
		e, ok := h.creds.get(name)
		servedAtStart = ok && e.Token == "token-1" && e.Role == service("x").Role && e.Component == "api"
	}
	h.sched.assign(service("api-1"))
	h.mustTick()
	if !servedAtStart {
		t.Fatal("credentials must be registered before the container starts")
	}
}

func TestCredentialsLearnTheReplicasAddress(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"))
	h.mustTick()
	h.mustTick()
	c, _ := h.rt.byName("api-1")
	e, _ := h.creds.get("api-1")
	if !slices.Equal(e.Addrs, c.Addrs) || len(e.Addrs) == 0 {
		t.Fatalf("credentials addrs %v, container addrs %v", e.Addrs, c.Addrs)
	}
}

func TestReplicaWithoutARoleGetsNoCredentials(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.Role = ""
	h.sched.assign(s)
	h.mustTick()
	if _, ok := envMap(h.rt.specOf("api-1").Env)["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"]; ok {
		t.Fatal("no role, no credentials URI")
	}
	if _, ok := h.creds.get("api-1"); ok {
		t.Fatal("no role, no credentials")
	}
}

func TestEachReplicaGetsItsOwnToken(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(service("api-1"), service("api-2"))
	h.mustTick()
	a, _ := h.creds.get("api-1")
	b, _ := h.creds.get("api-2")
	if a.Token == "" || a.Token == b.Token {
		t.Fatalf("tokens %q %q", a.Token, b.Token)
	}
}

func TestCPUSharesFollowTheRequest(t *testing.T) {
	h := newHarness(t)
	tiny, big := service("api-1"), worker("w-1")
	tiny.Request.CPU = 1
	big.Request.CPU = 2000
	h.sched.assign(tiny, big)
	h.mustTick()
	if got := h.rt.specOf("api-1").CPUShares; got != 2 {
		t.Errorf("tiny request: shares %d, Docker's minimum is 2", got)
	}
	if got := h.rt.specOf("w-1").CPUShares; got != 2048 {
		t.Errorf("2000m: shares %d", got)
	}
}

func TestStatefulReplicaGetsItsVolumeAndCoresOfItsOwn(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "redis")] = true
	pg := stateful("postgres-a1", "postgres", 3000, "/var/lib/postgresql/data", "pg_dumpall", "-U", "postgres")
	h.sched.assign(pg)
	h.mustTick()
	s := h.rt.specOf("postgres-a1")
	if s.CPUSet != "0-2" {
		t.Errorf("postgres cpuset %q", s.CPUSet)
	}
	wantMount := container.Mount{Source: filepath.Join(h.cfg.DataDir, "postgres"), Target: "/var/lib/postgresql/data"}
	if !slices.Equal(s.Mounts, []container.Mount{wantMount}) {
		t.Errorf("mounts %+v", s.Mounts)
	}
	var backup []string
	json.Unmarshal([]byte(s.Labels[container.LabelBackup]), &backup)
	if !slices.Equal(backup, []string{"pg_dumpall", "-U", "postgres"}) {
		t.Errorf("backup label %q", s.Labels[container.LabelBackup])
	}

	redis := stateful("redis-b2", "redis", 1000, "/data", "redis-cli", "--rdb", "-")
	redis.Port = 6379
	h.sched.assign(pg, redis)
	h.mustTick()
	if got := h.rt.specOf("redis-b2").CPUSet; got != "3" {
		t.Errorf("redis must get the core postgres doesn't use, got %q", got)
	}
}

func TestStatefulCoresWhenTheVMIsFull(t *testing.T) {
	h := newHarness(t, dataVM)
	h.rt.info.CPUs = 2
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "redis")] = true
	h.a.Boot(t.Context())
	h.sched.assign(stateful("postgres-a1", "postgres", 2000, "/data"), stateful("redis-b2", "redis", 1000, "/data"))
	h.mustTick()
	if got := h.rt.specOf("postgres-a1").CPUSet; got != "0-1" {
		t.Errorf("postgres cpuset %q", got)
	}
	if got := h.rt.specOf("redis-b2").CPUSet; got != "" {
		t.Errorf("with no free cores, redis falls back to a share, got %q", got)
	}
	if h.rt.specOf("redis-b2").CPUShares != 1024 {
		t.Errorf("shares %d", h.rt.specOf("redis-b2").CPUShares)
	}
}

func TestStatefulNeverStartsWithoutItsVolume(t *testing.T) {
	// Without its EBS volume, PostgreSQL would write to the root disk and lose it all.
	h := newHarness(t, dataVM)
	h.sched.assign(stateful("postgres-a1", "postgres", 1000, "/var/lib/postgresql/data"))
	h.mustTick()
	creates, _, _, _, _ := h.rt.snapshot()
	if len(creates) != 0 {
		t.Fatal("started without its volume")
	}
	h.mustTick()
	r := h.replica("postgres-a1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "not mounted") || r.Component != "postgres" {
		t.Fatalf("got %+v", r)
	}
}

func TestTaskSettings(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(task("api-migrate-1", "dotnet", "Api.dll", "migrate"))
	h.mustTick()
	s := h.rt.specOf("api-migrate-1")
	if s.Restart != container.RestartNever {
		t.Errorf("a task is never restarted, got %q", s.Restart)
	}
	if !slices.Equal(s.Cmd, []string{"dotnet", "Api.dll", "migrate"}) {
		t.Errorf("cmd %v", s.Cmd)
	}
	if s.Labels[container.LabelKind] != "task" {
		t.Errorf("labels %v", s.Labels)
	}
}

func TestEdgeSeesItsFilesReadOnly(t *testing.T) {
	h := newHarness(t, func(h *harness, cfg *Config, d *Deps) { cfg.Pool, cfg.Name = "ingress", "ingress-a" })
	h.sched.assignment.Files = map[string]string{"edge/routes.yaml": "routes: []\n"}
	h.sched.assign(edge("edge-1"))
	h.mustTick()
	s := h.rt.specOf("edge-1")
	want := container.Mount{Source: filepath.Join(h.cfg.FilesDir, "edge"), Target: "/etc/protostrap", ReadOnly: true}
	if !slices.Equal(s.Mounts, []container.Mount{want}) {
		t.Fatalf("mounts %+v", s.Mounts)
	}
	if b, err := os.ReadFile(filepath.Join(h.cfg.FilesDir, "edge", "routes.yaml")); err != nil || string(b) != "routes: []\n" {
		t.Fatalf("routes must be written before the edge starts: %q %v", b, err)
	}
}

func TestEdgeFilesDirExistsEvenWithoutFiles(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(edge("edge-1"))
	h.mustTick()
	if info, err := os.Stat(filepath.Join(h.cfg.FilesDir, "edge")); err != nil || !info.IsDir() {
		t.Fatalf("the bind mount's source must exist: %v", err)
	}
}

func TestReplicasJoinTheOverlayUnderTheirComponent(t *testing.T) {
	h := newHarness(t)
	h.sched.assign(worker("w-1"), task("m-1", "migrate"), edge("edge-1"))
	h.mustTick()
	for name, alias := range map[string]string{"w-1": "orders-worker", "edge-1": "edge"} {
		s := h.rt.specOf(name)
		if s.Network != "protostrap" || !slices.Equal(s.Aliases, []string{alias}) {
			t.Errorf("%s: network %q aliases %v", name, s.Network, s.Aliases)
		}
	}
	// A task reaches what it needs on the overlay, but answers to no name: a migration run
	// in a service's image mustn't receive the service's requests.
	if s := h.rt.specOf("m-1"); s.Network != "protostrap" || len(s.Aliases) != 0 {
		t.Errorf("task: network %q aliases %v", s.Network, s.Aliases)
	}
}
