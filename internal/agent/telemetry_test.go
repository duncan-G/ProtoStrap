package agent

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// These tests cover the agent's part in telemetry (docs/telemetry.md): every replica's
// output goes to the collector on data-1, and data-1's agent runs the telemetry replica
// with its volume, its files, Grafana's socket directory and the port Docker sends to.

// wantFluentd is how every replica but telemetry's own logs: Docker's fluentd driver, to
// the collector at data-1's fixed address, holding up to 10,000 lines while it can't send.
var wantFluentd = map[string]string{
	"fluentd-address":              "tcp://10.20.1.10:24224",
	"fluentd-async":                "true",
	"fluentd-buffer-limit":         "10000",
	"fluentd-sub-second-precision": "true",
	"tag":                          "{{.Name}}",
	"labels":                       "protostrap.component,protostrap.replica,protostrap.release",
	"mode":                         "non-blocking",
	"max-buffer-size":              "4m",
}

func TestEveryReplicaSendsItsOutputToTelemetry(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.sched.assign(service("api-1"), worker("w-1"), task("m-1", "migrate"), edge("edge-1"), stateful("postgres-1", "postgres", 1000, "/data"))
	h.mustTick()
	for _, name := range []string{"api-1", "w-1", "m-1", "edge-1", "postgres-1"} {
		s := h.rt.specOf(name)
		if s.LogDriver != "fluentd" || !maps.Equal(s.LogOptions, wantFluentd) {
			t.Errorf("%s: log driver %q options\n got %v\nwant %v", name, s.LogDriver, s.LogOptions, wantFluentd)
		}
	}
}

// TestWithoutTelemetryOutputGoesToCloudWatch checks that a replica whose spec names a log
// group, as the scheduler does when the application runs no telemetry, logs there with
// Docker's awslogs driver, never blocking on CloudWatch.
func TestWithoutTelemetryOutputGoesToCloudWatch(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	specs := []api.ReplicaSpec{service("api-1"), worker("w-1"), task("m-1", "migrate"), edge("edge-1"), stateful("postgres-1", "postgres", 1000, "/data")}
	for i := range specs {
		specs[i].LogGroup = "/protostrap/shop/" + specs[i].Component
	}
	h.sched.assign(specs...)
	h.mustTick()
	for _, r := range specs {
		s := h.rt.specOf(r.ID)
		want := map[string]string{
			"awslogs-region":       "eu-west-1",
			"awslogs-group":        "/protostrap/shop/" + r.Component,
			"awslogs-create-group": "false",
			"tag":                  "{{.Name}}",
			"mode":                 "non-blocking",
			"max-buffer-size":      "4m",
		}
		if s.LogDriver != "awslogs" || !maps.Equal(s.LogOptions, want) {
			t.Errorf("%s: log driver %q options\n got %v\nwant %v", r.ID, s.LogDriver, s.LogOptions, want)
		}
	}
}

func TestTelemetryReplicaSettings(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "telemetry")] = true
	h.sched.assignment.Files = map[string]string{"telemetry/scrape.json": `[{"component":"api","port":9090,"path":"/metrics"}]`}
	h.sched.assign(telemetryReplica("telemetry-1"))
	h.mustTick()
	s := h.rt.specOf("telemetry-1")

	wantMounts := []container.Mount{
		{Source: filepath.Join(h.cfg.DataDir, "telemetry"), Target: "/var/lib/telemetry"},
		{Source: filepath.Join(h.cfg.FilesDir, "telemetry"), Target: "/etc/protostrap", ReadOnly: true},
		{Source: h.cfg.TelemetrySocketDir, Target: "/run/protostrap/telemetry"},
	}
	if !slices.Equal(s.Mounts, wantMounts) {
		t.Errorf("mounts\n got %+v\nwant %+v", s.Mounts, wantMounts)
	}
	// Docker on every VM sends replicas' output to data-1's address, port 24224, and only
	// there: never on every address, where a container could reach it.
	wantPublish := []container.Port{{HostIP: "10.20.16.5", HostPort: 24224, ContainerPort: 24224, Protocol: "tcp"}}
	if !slices.Equal(s.Publish, wantPublish) {
		t.Errorf("publish %+v, want %+v", s.Publish, wantPublish)
	}
	// Its own output stays on data-1: sent to itself, a fault would feed on itself.
	if s.LogDriver != "local" {
		t.Errorf("log driver %q, want local", s.LogDriver)
	}
	if s.Network != "protostrap" || !slices.Equal(s.Aliases, []string{"telemetry"}) {
		t.Errorf("network %q aliases %v: services send OTLP to telemetry:4318", s.Network, s.Aliases)
	}
	if s.CPUSet != "" || s.CPUShares != 512 || s.MemoryBytes != 1536<<20 {
		t.Errorf("cpuset %q shares %d memory %d: a share and a hard memory limit, no cores of its own", s.CPUSet, s.CPUShares, s.MemoryBytes)
	}
	if s.Restart != container.RestartUnlessStopped {
		t.Errorf("restart %q", s.Restart)
	}
	if s.Labels[container.LabelKind] != "telemetry" || s.Labels[container.LabelReady] != "/ready" || s.Labels[container.LabelPort] != "13133" {
		t.Errorf("labels %v", s.Labels)
	}
	env := envMap(s.Env)
	if env["TELEMETRY_BUCKET"] != "protostrap-123456789012-eu-west-1-telemetry" || env["AWS_REGION"] != "eu-west-1" ||
		!strings.HasPrefix(env["AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"], "/v1/credentials/") {
		t.Errorf("env %v", env)
	}
	if b, err := os.ReadFile(filepath.Join(h.cfg.FilesDir, "telemetry", "scrape.json")); err != nil || !strings.Contains(string(b), `"api"`) {
		t.Errorf("the scrape targets must be written before telemetry starts: %q %v", b, err)
	}
	if info, err := os.Stat(h.cfg.TelemetrySocketDir); err != nil || !info.IsDir() {
		t.Errorf("Grafana's socket directory must exist before telemetry starts: %v", err)
	}
}

func TestTelemetryNeverStartsWithoutItsVolume(t *testing.T) {
	// Without its volume, Prometheus would fill data-1's root disk.
	h := newHarness(t, dataVM)
	h.sched.assign(telemetryReplica("telemetry-1"))
	h.mustTick()
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatal("started without its volume")
	}
	h.mustTick()
	r := h.replica("telemetry-1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "not mounted") || r.Component != "telemetry" {
		t.Fatalf("got %+v", r)
	}
}

func TestTelemetryStopsBeforeItsReplacementStarts(t *testing.T) {
	// Only one can own the volume, and the port.
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "telemetry")] = true
	h.running(telemetryReplica("telemetry-1"))
	gate := make(chan struct{})
	h.rt.mu.Lock()
	h.rt.stopGate = gate
	h.rt.mu.Unlock()
	h.rt.resetCalls()
	h.sched.assign(telemetryReplica("telemetry-2"))
	h.a.Tick(t.Context())
	h.a.Tick(t.Context())
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Fatalf("the new replica started while the old one still held the volume: %v", creates)
	}
	close(gate)
	h.a.Wait()
	h.mustTick()
	calls := h.rt.calls()
	if slices.Index(calls, "remove telemetry-1") < 0 || slices.Index(calls, "create telemetry-2") < slices.Index(calls, "remove telemetry-1") {
		t.Fatalf("order %v", calls)
	}
}

func TestTelemetryIsReadyWhenItsStatusAnswersForItsName(t *testing.T) {
	h := newHarness(t, dataVM)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "telemetry")] = true
	h.sched.assign(telemetryReplica("telemetry-1"))
	h.mustTick()
	h.mustTick()
	if got := h.state("telemetry-1"); got != api.StateStarting {
		t.Fatalf("state %s before its check passes", got)
	}
	h.prober.setReady("telemetry", true)
	h.mustTick()
	if got := h.state("telemetry-1"); got != api.StateReady {
		t.Fatalf("state %s", got)
	}
}

func TestTheAgentNeedsTheTelemetryAddress(t *testing.T) {
	_, err := New(Config{App: "shop", InstanceID: "i-1", Pool: "general", Region: "eu-west-1", Version: "1.4.0", PrivateIP: "10.20.16.5"}, Deps{})
	if err == nil || !strings.Contains(err.Error(), "telemetry") {
		t.Fatalf("err %v: without data-1's address, replicas' output would go nowhere", err)
	}
}
