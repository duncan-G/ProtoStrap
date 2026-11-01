package scheduler

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// These tests cover a spec without the tunnel, the gateway or telemetry: what it leaves out
// doesn't run, and what it removes stops (docs/protostrap.md, "Tunnel", "Gateway" and
// "Telemetry").

// edited is spec with each pair of edits applied, old then new; it panics on an old the spec
// doesn't have, so a test never quietly runs the spec it started with.
func edited(spec string, edits ...string) string {
	for i := 0; i < len(edits); i += 2 {
		if !strings.Contains(spec, edits[i]) {
			panic("the spec has no " + edits[i])
		}
		spec = strings.Replace(spec, edits[i], edits[i+1], 1)
	}
	return spec
}

var (
	// noGatewaySpec is testSpec without its tunnel and gateway. web scales on CPU, since
	// requests are the gateway's to count.
	noGatewaySpec = edited(testSpec,
		"tunnel: {}\ngateway:\n  request: { cpu: 500m, memory: 256Mi }\n  upstreams:\n    api:\n      routes: [/api/]\n    web:\n      routes: [/web/]\n", "",
		"      demand: [{ requests: 20/s }]", "      demand: [{ cpu: 70% }]")
	noTunnelSpec    = edited(testSpec, "tunnel: {}\n", "")
	noTelemetrySpec = edited(testSpec, "telemetry: {}\n", "")
)

func TestWithoutAGatewayNoEdgeRuns(t *testing.T) {
	h := newHarness(t)
	h.deploy("c1", noGatewaySpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	if ids := h.running("edge"); len(ids) > 0 {
		t.Errorf("edges run without a gateway: %v", ids)
	}
	for _, a := range h.vms("ingress") {
		if len(a.assignment.Replicas) > 0 || len(a.assignment.Files) > 0 {
			t.Errorf("ingress VM %s was assigned %+v", a.id, a.assignment)
		}
	}
	targets, _ := telemetry.ParseTargets([]byte(h.agents["i-data1"].assignment.Files[telemetry.ScrapeFile]))
	if slices.ContainsFunc(targets, func(t telemetry.Target) bool { return t.Component == "edge" }) {
		t.Errorf("Prometheus scrapes an edge that doesn't run: %+v", targets)
	}
}

func TestRemovingTheGatewayStopsTheEdge(t *testing.T) {
	h := newHarness(t)
	h.settle()
	if h.ready("edge") != 1 {
		t.Fatalf("no edge before: %+v", h.replicas("edge"))
	}
	h.deploy("c2", noGatewaySpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.until("the edge stops", time.Minute, func() bool { return len(h.running("edge")) == 0 })
	if files := h.vms("ingress")[0].assignment.Files; len(files) > 0 {
		t.Errorf("the ingress VM still gets the edge's files: %v", slices.Sorted(maps.Keys(files)))
	}

	// Adding it back starts an edge again.
	h.deploy("c3", testSpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c3") == RolloutDone })
	if h.ready("edge") != 1 {
		t.Errorf("edges %+v, want one ready", h.replicas("edge"))
	}
}

// TestAGatewayWithoutATunnelRunsEnvoyAlone checks that the edge is told to run without
// cloudflared, and so reads no keys: a new tunnel token doesn't replace it.
func TestAGatewayWithoutATunnelRunsEnvoyAlone(t *testing.T) {
	h := newHarness(t)
	h.writeSecret(secretV1)
	h.run(time.Minute)
	h.deploy("c1", noTunnelSpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	ids := h.running("edge")
	if len(ids) != 1 {
		t.Fatalf("edges %v, want one", ids)
	}
	s := h.specOf(ids[0])
	if s.Env["EDGE_TUNNEL"] != "off" {
		t.Errorf("the edge's env %v lacks EDGE_TUNNEL=off", s.Env)
	}
	if s.SecretPrefix != "" || s.SecretDigest != "" {
		t.Errorf("the edge reads keys without a tunnel: %+v", s)
	}
	h.writeSecret(map[string]string{"Edge_TUNNEL_TOKEN": "tunnel-token-two"})
	h.run(3 * time.Minute)
	if after := h.running("edge"); !slices.Equal(after, ids) {
		t.Errorf("a tunnel token replaced an edge without a tunnel: %v then %v", ids, after)
	}

	// With the tunnel, the edge runs it.
	h.deploy("c2", testSpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.until("the old edge stops", time.Minute, func() bool { return len(h.running("edge")) == 1 })
	ids = h.running("edge")
	if len(ids) != 1 {
		t.Fatalf("edges %v, want one", ids)
	}
	if s := h.specOf(ids[0]); s.Env["EDGE_TUNNEL"] != "" || s.SecretPrefix != "Edge_" {
		t.Errorf("an edge with a tunnel: env %v, prefix %q", s.Env, s.SecretPrefix)
	}
}

// TestWithoutTelemetryLogsGoToCloudWatch checks that no telemetry replica runs, replicas
// get no OTEL_ variables, and Docker sends each one's output to its component's log group.
func TestWithoutTelemetryLogsGoToCloudWatch(t *testing.T) {
	h := newHarness(t)
	h.deploy("c1", noTelemetrySpec, nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	if ids := h.running("telemetry"); len(ids) > 0 {
		t.Errorf("telemetry runs without a telemetry section: %v", ids)
	}
	if _, ok := h.agents["i-data1"].assignment.Files[telemetry.ScrapeFile]; ok {
		t.Errorf("data-1 gets scrape targets without telemetry")
	}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge"} {
		ids := h.running(c)
		if len(ids) == 0 {
			t.Fatalf("no %s runs", c)
		}
		s := h.specOf(ids[0])
		if s.LogGroup != "/protostrap/shop/"+c {
			t.Errorf("%s logs to %q, want /protostrap/shop/%s", c, s.LogGroup, c)
		}
		for k := range s.Env {
			if strings.HasPrefix(k, "OTEL_") {
				t.Errorf("%s gets %s without telemetry", c, k)
			}
		}
	}
}

// TestWithTelemetryLogsGoToTheCollector checks that no replica names a log group while
// telemetry runs: Docker sends their output to the collector on data-1.
func TestWithTelemetryLogsGoToTheCollector(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, c := range []string{"api", "web", "postgres", "redis", "edge", "telemetry"} {
		for _, id := range h.running(c) {
			if s := h.specOf(id); s.LogGroup != "" {
				t.Errorf("%s logs to %q while telemetry runs", id, s.LogGroup)
			}
		}
	}
}

// TestRemovingTelemetryMovesTheLogsToCloudWatch checks that removing telemetry stops its
// replica and replaces every component's, so their output goes to CloudWatch, and that
// adding it back moves them again.
func TestRemovingTelemetryMovesTheLogsToCloudWatch(t *testing.T) {
	h := newHarness(t)
	h.settle()
	api1 := h.running("api")
	h.deploy("c2", noTelemetrySpec, nil)
	h.until("the release finishes", 15*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.until("telemetry stops", time.Minute, func() bool { return len(h.running("telemetry")) == 0 })
	h.until("the old api replicas stop", time.Minute, func() bool { return len(h.running("api")) == 2 })
	api2 := h.running("api")
	if slices.ContainsFunc(api2, func(id string) bool { return slices.Contains(api1, id) }) {
		t.Errorf("api's replicas weren't replaced: %v then %v", api1, api2)
	}
	for _, id := range api2 {
		if s := h.specOf(id); s.LogGroup != "/protostrap/shop/api" {
			t.Errorf("%s logs to %q", id, s.LogGroup)
		}
	}

	h.deploy("c3", testSpec, nil)
	h.until("the release finishes", 15*time.Minute, func() bool { return h.rollout("c3") == RolloutDone })
	h.until("the old api replicas stop", time.Minute, func() bool { return len(h.running("api")) == 2 })
	if h.ready("telemetry") != 1 {
		t.Errorf("telemetry %+v, want one ready", h.replicas("telemetry"))
	}
	for _, id := range h.running("api") {
		if s := h.specOf(id); s.LogGroup != "" || s.Env["OTEL_EXPORTER_OTLP_ENDPOINT"] == "" {
			t.Errorf("%s with telemetry back: log group %q, env %v", id, s.LogGroup, s.Env)
		}
	}
}

// TestAReleaseWithTheSameSectionsKeepsItsReplicas checks that folding telemetry's absence
// into the fingerprints replaces nothing while telemetry stays: the fingerprints a running
// environment has are the ones it keeps.
func TestAReleaseWithTheSameSectionsKeepsItsReplicas(t *testing.T) {
	h := newHarness(t)
	h.settle()
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge", "telemetry"} {
		before[c] = h.running(c)
	}
	h.deploy("c2", edited(testSpec, "env: { MODE: prod }", "env: { MODE: prod }"), nil)
	h.until("the release finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	for c, ids := range before {
		if after := h.running(c); !slices.Equal(after, ids) {
			t.Errorf("%s was replaced: %v then %v", c, ids, after)
		}
	}
	for _, a := range h.agents {
		for _, r := range a.assignment.Replicas {
			if r.Kind == api.KindEdge && r.LogGroup != "" {
				t.Errorf("the edge logs to %q with telemetry", r.LogGroup)
			}
		}
	}
}
