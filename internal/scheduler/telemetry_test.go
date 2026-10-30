package scheduler

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// These tests cover the telemetry replica (docs/telemetry.md, docs/protostrap.md#telemetry):
// one on data-1, ProtoStrap's own, replaced like a stateful component, and the OTEL_
// variables every component's replicas get.

// telemetrySpec is the telemetry replica's spec as data-1's agent was given it.
func (h *harness) telemetrySpec() api.ReplicaSpec {
	h.t.Helper()
	ids := h.running("telemetry")
	if len(ids) != 1 {
		h.t.Fatalf("telemetry replicas running: %v, want one\n%s", ids, h.describe())
	}
	return h.agentOf(ids[0]).replicas[ids[0]].spec
}

// specOf is the spec a running replica was given.
func (h *harness) specOf(id string) api.ReplicaSpec {
	h.t.Helper()
	a := h.agentOf(id)
	if a == nil {
		h.t.Fatalf("no agent runs %s", id)
	}
	return a.replicas[id].spec
}

func TestTelemetryRunsOnceOnDataOne(t *testing.T) {
	h := newHarness(t)
	h.settle()
	ids := h.running("telemetry")
	if len(ids) != 1 || h.agentOf(ids[0]).id != "i-data1" {
		t.Fatalf("telemetry runs as %v, want one replica on data-1\n%s", ids, h.describe())
	}
	if h.ready("telemetry") != 1 {
		t.Fatalf("telemetry isn't ready: %+v", h.replicas("telemetry"))
	}
	s := h.telemetrySpec()
	want := api.ReplicaSpec{
		ID: ids[0], Component: "telemetry", Kind: api.KindTelemetry, Release: "c1",
		Image:   testRegistry + "/protostrap/telemetry:1.4.0",
		Request: api.Resources{CPU: 500, Memory: 1536}, Port: 13133, Ready: "/ready", Stop: 60,
		Volume: "/var/lib/telemetry",
		Role:   "arn:aws:iam::123456789012:role/protostrap/protostrap-c-telemetry",
	}
	if s.ID != want.ID || s.Component != want.Component || s.Kind != want.Kind || s.Release != want.Release ||
		s.Image != want.Image || s.Request != want.Request || s.Port != want.Port || s.Ready != want.Ready ||
		s.Stop != want.Stop || s.Volume != want.Volume || s.Role != want.Role {
		t.Errorf("telemetry spec\n got %+v\nwant %+v", s, want)
	}
	wantEnv := map[string]string{
		"TELEMETRY_BUCKET":       "protostrap-123456789012-us-east-1-telemetry",
		"TELEMETRY_LOGS_DAYS":    "30",
		"TELEMETRY_TRACES_DAYS":  "30",
		"TELEMETRY_METRICS_DAYS": "15",
	}
	if !maps.Equal(s.Env, wantEnv) {
		t.Errorf("telemetry env\n got %v\nwant %v", s.Env, wantEnv)
	}
	if s.SecretPrefix != "" || len(s.SecretFiles) > 0 || s.Metrics != nil {
		t.Errorf("telemetry reads no keys and exports no scaling metrics: %+v", s)
	}
	for _, a := range h.agents {
		for _, r := range a.assignment.Replicas {
			if r.Kind == api.KindTelemetry && a.id != "i-data1" {
				t.Errorf("vm %s was assigned telemetry", a.id)
			}
		}
	}
}

func TestTelemetrySettingsReachTheReplica(t *testing.T) {
	h := newHarness(t)
	spec := strings.Replace(testSpec, "queues:\n", "telemetry:\n  request: { cpu: 250m, memory: 1Gi }\n  retention: { logs: 7d, traces: 3d, metrics: 30d }\nqueues:\n", 1)
	h.deploy("c1", spec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	s := h.telemetrySpec()
	if s.Request != (api.Resources{CPU: 250, Memory: 1024}) {
		t.Errorf("request %+v", s.Request)
	}
	if s.Env["TELEMETRY_LOGS_DAYS"] != "7" || s.Env["TELEMETRY_TRACES_DAYS"] != "3" || s.Env["TELEMETRY_METRICS_DAYS"] != "30" {
		t.Errorf("env %v", s.Env)
	}
}

// TestDataOnesAssignmentListsTheScrapeTargets checks the file that tells Prometheus what
// to scrape: every component with a metrics endpoint, and the edge's request counts.
func TestDataOnesAssignmentListsTheScrapeTargets(t *testing.T) {
	h := newHarness(t)
	h.settle()
	data := h.agents["i-data1"].assignment
	got, err := telemetry.ParseTargets([]byte(data.Files[telemetry.ScrapeFile]))
	if err != nil {
		t.Fatalf("%s: %v\n%q", telemetry.ScrapeFile, err, data.Files[telemetry.ScrapeFile])
	}
	want := []telemetry.Target{
		{Component: "api", Port: 9090, Path: "/metrics"},
		{Component: "edge", Port: 9902, Path: "/metrics"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("targets\n got %+v\nwant %+v", got, want)
	}
	for _, a := range h.agents {
		if _, ok := a.assignment.Files[telemetry.ScrapeFile]; ok && a.id != "i-data1" {
			t.Errorf("vm %s got the scrape targets: only data-1 runs telemetry", a.id)
		}
	}

	// A new endpoint reaches Prometheus as a file, without replacing the replica.
	before := h.running("telemetry")
	spec := strings.Replace(testSpec, "    routes: [/web/]\n", "    routes: [/web/]\n    metrics: 9100/stats\n", 1)
	h.deploy("c2", spec, nil)
	h.until("c2 finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	got, _ = telemetry.ParseTargets([]byte(h.agents["i-data1"].assignment.Files[telemetry.ScrapeFile]))
	if !slices.Contains(got, telemetry.Target{Component: "web", Port: 9100, Path: "/stats"}) {
		t.Errorf("targets %+v lack web's endpoint", got)
	}
	if after := h.running("telemetry"); !slices.Equal(before, after) {
		t.Errorf("telemetry was replaced for a scrape target: %v then %v", before, after)
	}
}

func TestEveryComponentsReplicasGetTheOTELVariables(t *testing.T) {
	h := newHarness(t)
	h.settle()
	check := func(id, component string, s api.ReplicaSpec) {
		t.Helper()
		want := map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": "http://telemetry:4318",
			"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
			"OTEL_SERVICE_NAME":           component,
			"OTEL_RESOURCE_ATTRIBUTES":    "service.instance.id=" + id + ",service.version=c1,deployment.environment.name=dev",
		}
		for k, v := range want {
			if s.Env[k] != v {
				t.Errorf("%s: %s = %q, want %q", id, k, s.Env[k], v)
			}
		}
	}
	for _, component := range []string{"api", "web", "postgres", "redis"} {
		ids := h.running(component)
		if len(ids) == 0 {
			t.Fatalf("no %s running", component)
		}
		check(ids[0], component, h.specOf(ids[0]))
	}
	runs := h.taskRuns["migrate"]
	if len(runs) == 0 {
		t.Fatal("the migration never ran")
	}
	check(runs[0].spec.ID, "migrate", runs[0].spec)
	if s := h.specOf(h.running("api")[0]); s.Env["MODE"] != "prod" {
		t.Errorf("the spec's env is still there: %v", s.Env)
	}

	// ProtoStrap's own replicas get none: they don't use OpenTelemetry's SDK, and the
	// collector must not export to itself.
	for _, component := range []string{"edge", "telemetry"} {
		for _, id := range h.running(component) {
			for k := range h.specOf(id).Env {
				if strings.HasPrefix(k, "OTEL_") {
					t.Errorf("%s got %s", id, k)
				}
			}
		}
	}
}

// TestTheSpecsEnvWinsForTheOTELVariables checks that an application can point its
// services at its own collector, or name them itself. The endpoint and its protocol go
// together: a spec that sets either gets neither of ProtoStrap's.
func TestTheSpecsEnvWinsForTheOTELVariables(t *testing.T) {
	h := newHarness(t)
	spec := strings.Replace(testSpec, "    env: { MODE: prod }\n",
		"    env: { MODE: prod, OTEL_EXPORTER_OTLP_ENDPOINT: \"http://otel-collector:4317\", OTEL_SERVICE_NAME: shop-api }\n", 1)
	spec = strings.Replace(spec, "    routes: [/web/]\n", "    routes: [/web/]\n    env: { OTEL_EXPORTER_OTLP_PROTOCOL: grpc }\n", 1)
	h.deploy("c1", spec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })

	apiID := h.running("api")[0]
	env := h.specOf(apiID).Env
	if env["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://otel-collector:4317" || env["OTEL_SERVICE_NAME"] != "shop-api" {
		t.Errorf("api env %v: the spec's values win", env)
	}
	if v, ok := env["OTEL_EXPORTER_OTLP_PROTOCOL"]; ok {
		t.Errorf("api got ProtoStrap's protocol %q for an endpoint of its own", v)
	}
	if env["OTEL_RESOURCE_ATTRIBUTES"] != "service.instance.id="+apiID+",service.version=c1,deployment.environment.name=dev" {
		t.Errorf("api keeps ProtoStrap's resource attributes: %v", env)
	}

	web := h.specOf(h.running("web")[0]).Env
	if web["OTEL_EXPORTER_OTLP_PROTOCOL"] != "grpc" {
		t.Errorf("web env %v", web)
	}
	if v, ok := web["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
		t.Errorf("web got ProtoStrap's endpoint %q for a protocol of its own", v)
	}
	if web["OTEL_SERVICE_NAME"] != "web" {
		t.Errorf("web keeps ProtoStrap's service name: %v", web)
	}
}

// TestChangingTelemetryStopsTheOldReplicaFirst checks that, like a stateful component, the
// old telemetry replica is gone before its replacement starts: only one can own the volume.
func TestChangingTelemetryStopsTheOldReplicaFirst(t *testing.T) {
	h := newHarness(t)
	h.settle()
	old := h.running("telemetry")
	spec := strings.Replace(testSpec, "queues:\n", "telemetry:\n  retention: { logs: 7d }\nqueues:\n", 1)
	h.deploy("c2", spec, nil)
	for i := 0; i < 200 && h.rollout("c2") != RolloutDone; i++ {
		h.step()
		if n := len(h.running("telemetry")); n > 1 {
			t.Fatalf("%d telemetry replicas at once: %v", n, h.running("telemetry"))
		}
	}
	if h.rollout("c2") != RolloutDone {
		t.Fatalf("c2: %s %s\n%s", h.rollout("c2"), h.rolloutReason("c2"), h.describe())
	}
	now := h.running("telemetry")
	if len(now) != 1 || slices.Equal(now, old) {
		t.Fatalf("telemetry %v then %v: want a new replica", old, now)
	}
	if h.specOf(now[0]).Env["TELEMETRY_LOGS_DAYS"] != "7" {
		t.Errorf("the new replica has the new retention: %v", h.specOf(now[0]).Env)
	}
	// Nothing else restarted.
	for _, c := range []string{"api", "postgres", "redis", "edge"} {
		for _, r := range h.replicas(c) {
			if r.Release != "c1" {
				t.Errorf("%s was replaced for telemetry: %+v", r.ID, r)
			}
		}
	}
}

func TestAReleaseIsntDoneUntilTelemetryIsReady(t *testing.T) {
	h := newHarness(t)
	h.readyAfter["telemetry"] = 40 // It takes over three minutes.
	h.deploy("c1", testSpec, nil)
	h.until("everything else is ready", 10*time.Minute, func() bool {
		return h.ready("api") >= 2 && h.ready("postgres") == 1 && h.ready("edge") == 1
	})
	if h.ready("telemetry") == 0 && h.rollout("c1") == RolloutDone {
		t.Fatal("the release finished before telemetry was ready")
	}
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	if h.ready("telemetry") != 1 {
		t.Fatalf("telemetry: %+v", h.replicas("telemetry"))
	}
}

func TestTelemetryThatNeverGetsReadyRollsTheReleaseBack(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.neverReady["telemetry"] = true
	spec := strings.Replace(testSpec, "queues:\n", "telemetry:\n  retention: { logs: 7d }\nqueues:\n", 1)
	h.deploy("c2", spec, nil)
	h.until("c2 fails", 15*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if reason := h.rolloutReason("c2"); !strings.Contains(reason, "telemetry") {
		t.Errorf("reason %q should name telemetry", reason)
	}
	delete(h.neverReady, "telemetry")
	h.until("c1's telemetry is back", 10*time.Minute, func() bool {
		ids := h.running("telemetry")
		return len(ids) == 1 && h.specOf(ids[0]).Release == "c1" && h.ready("telemetry") == 1
	})
}

// TestRollingBackToAReleaseWithoutTelemetryStopsIt checks a rollback past the version that
// brought telemetry: the earlier release has no telemetry image, so none runs.
func TestRollingBackToAReleaseWithoutTelemetryStopsIt(t *testing.T) {
	h := newHarness(t)
	h.settle()
	h.publish("c0", testSpec, nil)
	h.store.mu.Lock()
	r := h.store.releases["c0"]
	r.Telemetry = release.Component{}
	h.store.releases["c0"] = r
	h.store.mu.Unlock()
	if err := h.s.Deploy(t.Context(), "c0"); err != nil {
		t.Fatal(err)
	}
	h.until("c0 finishes", 10*time.Minute, func() bool { return h.rollout("c0") == RolloutDone })
	h.until("telemetry stops", time.Minute, func() bool { return len(h.running("telemetry")) == 0 })
	if got := h.agents["i-data1"].assignment.Files[telemetry.ScrapeFile]; got != "" {
		t.Errorf("data-1 still gets scrape targets: %q", got)
	}
	// A replica made for the earlier release isn't pointed at a collector that isn't there.
	h.totalCPU["api"] = 5000
	h.until("api scales out", 5*time.Minute, func() bool { return h.ready("api") > 2 })
	made := 0
	for _, id := range h.running("api") {
		if s := h.specOf(id); s.Release == "c0" {
			made++
			if _, ok := s.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]; ok {
				t.Errorf("%s points at a collector that isn't there", id)
			}
		}
	}
	if made == 0 {
		t.Fatalf("no api replica was made for c0: %v", h.running("api"))
	}
}

func TestARestartedSchedulerAdoptsTelemetry(t *testing.T) {
	h := newHarness(t)
	h.settle()
	before := h.running("telemetry")
	h.start()
	h.run(2 * time.Minute)
	if after := h.running("telemetry"); !slices.Equal(before, after) || h.ready("telemetry") != 1 {
		t.Fatalf("telemetry %v then %v (ready %d): a restart replaces nothing", before, after, h.ready("telemetry"))
	}
	var found bool
	for _, r := range h.s.Status().Replicas {
		if r.ID == before[0] {
			found = true
			if r.Kind != string(api.KindTelemetry) || r.Pool != "data" || r.Old {
				t.Errorf("adopted %+v", r)
			}
		}
	}
	if !found {
		t.Errorf("the status lacks %s", before[0])
	}
}

// TestTelemetryCountsAgainstDataOne checks that the room the status shows on data-1 takes
// telemetry's request.
func TestTelemetryCountsAgainstDataOne(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, vm := range h.s.Status().VMs {
		if vm.Name != "data-1" {
			continue
		}
		// m7g.xlarge has 4000m and 16384Mi; the data pool keeps 1Gi back; postgres takes
		// 2000m and 8Gi, redis 1000m and 4Gi, telemetry 500m and 1536Mi.
		if want := (api.Resources{CPU: 500, Memory: 16384 - 1024 - 8192 - 4096 - 1536}); vm.Free != want {
			t.Errorf("data-1 free %+v, want %+v", vm.Free, want)
		}
		return
	}
	t.Fatal("no data-1 in the status")
}

func TestTheScrapeTargetsFileIsJSON(t *testing.T) {
	h := newHarness(t)
	h.settle()
	var v []map[string]any
	if err := json.Unmarshal([]byte(h.agents["i-data1"].assignment.Files[telemetry.ScrapeFile]), &v); err != nil || len(v) == 0 {
		t.Fatalf("%v %v", v, err)
	}
}
