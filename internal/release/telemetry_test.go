package release

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/spec"
)

// These tests cover the telemetry image in plans and releases (docs/telemetry.md): one per
// ProtoStrap version, like the edge's, which the deploy workflow builds and pushes.

func TestPlanNamesTheTelemetryImageOfItsVersion(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	p, err := MakePlan(specPath, "123.dkr.ecr.us-east-1.amazonaws.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Telemetry != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/telemetry:1.4.0" {
		t.Errorf("telemetry %q", p.Telemetry)
	}
	if p2, _ := MakePlan(specPath, "r", "1.4.0-dev.abc"); !strings.HasSuffix(p2.Telemetry, "/protostrap/telemetry:1.4.0-dev.abc") {
		t.Errorf("a version override names that version's image: %q", p2.Telemetry)
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), `"telemetry":"123.dkr.ecr.us-east-1.amazonaws.com/protostrap/telemetry:1.4.0"`) {
		t.Errorf("the workflow reads the plan's telemetry image as telemetry: %s", b)
	}
}

func TestAReleaseCarriesTelemetry(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	r, err := Make(specPath, "9f2c1e7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "123.dkr.ecr.us-east-1.amazonaws.com", "1.4.0-dev.x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Telemetry.Image != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/telemetry:1.4.0-dev.x" || len(r.Telemetry.Fingerprint) != 16 {
		t.Errorf("telemetry %+v", r.Telemetry)
	}
	b, _ := json.Marshal(r)
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Telemetry != r.Telemetry {
		t.Errorf("telemetry doesn't survive the round trip: %+v", back.Telemetry)
	}
}

// TestAnEarlierReleaseHasNoTelemetry checks that a release made before telemetry still
// parses, with none: rolling back to it stops the telemetry replica.
func TestAnEarlierReleaseHasNoTelemetry(t *testing.T) {
	old := `{"commit":"abc1234","app":"shop","protostrap":"1.3.0","spec":` + quote(testSpec) + `,"components":{` +
		`"api":{"image":"r/protostrap/api:c-1","fingerprint":"f1"},` +
		`"worker":{"image":"r/protostrap/worker:c-2","fingerprint":"f2"},` +
		`"postgres":{"image":"r/protostrap/postgres:17.2","fingerprint":"f3"},` +
		`"cache":{"image":"r/protostrap/cache:x","fingerprint":"f4"}},` +
		`"edge":{"image":"r/protostrap/edge:1.3.0","fingerprint":"f5"}}`
	r, err := Parse([]byte(old))
	if err != nil {
		t.Fatalf("an earlier release must still parse: %v", err)
	}
	if r.Telemetry != (Component{}) {
		t.Errorf("telemetry %+v, want none", r.Telemetry)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestTelemetryFingerprint checks what replaces the telemetry replica: its image, request
// and retention, which its programs read when they start. Its volume's size isn't one:
// Terraform grows the volume under it.
func TestTelemetryFingerprint(t *testing.T) {
	day := 24 * time.Hour
	base := spec.Telemetry{
		Request: spec.Resources{CPU: 500, Memory: 1536}, Volume: 20,
		Retention: spec.TelemetryRetention{Logs: spec.Duration{Duration: 30 * day}, Traces: spec.Duration{Duration: 30 * day}, Metrics: spec.Duration{Duration: 15 * day}},
	}
	fp := TelemetryFingerprint("r/protostrap/telemetry:1.4.0", base)
	if len(fp) != 16 {
		t.Fatalf("fingerprint %q", fp)
	}
	changed := func(name string, image string, edit func(*spec.Telemetry), want bool) {
		t.Helper()
		tel := base
		edit(&tel)
		if got := TelemetryFingerprint(image, tel) != fp; got != want {
			t.Errorf("%s: fingerprint changed = %v, want %v", name, got, want)
		}
	}
	changed("same", "r/protostrap/telemetry:1.4.0", func(*spec.Telemetry) {}, false)
	changed("image", "r/protostrap/telemetry:1.5.0", func(*spec.Telemetry) {}, true)
	changed("request", "r/protostrap/telemetry:1.4.0", func(t *spec.Telemetry) { t.Request.Memory = 2048 }, true)
	changed("logs retention", "r/protostrap/telemetry:1.4.0", func(t *spec.Telemetry) { t.Retention.Logs.Duration = 7 * day }, true)
	changed("traces retention", "r/protostrap/telemetry:1.4.0", func(t *spec.Telemetry) { t.Retention.Traces.Duration = 7 * day }, true)
	changed("metrics retention", "r/protostrap/telemetry:1.4.0", func(t *spec.Telemetry) { t.Retention.Metrics.Duration = 7 * day }, true)
	changed("volume", "r/protostrap/telemetry:1.4.0", func(t *spec.Telemetry) { t.Volume = 40 }, false)
}
