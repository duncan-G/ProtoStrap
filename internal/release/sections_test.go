package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/spec"
)

// These tests cover the tunnel, gateway and telemetry in plans and releases: each of their
// images is built and carried only when the spec runs it.

func TestAPlanBuildsOnlyWhatTheSpecRuns(t *testing.T) {
	without := func(section string) string {
		switch section {
		case "gateway":
			return strings.Replace(testSpec, "tunnel: {}\ngateway:\n  upstreams:\n    api:\n      routes: [/api/]\n", "", 1)
		case "tunnel":
			return strings.Replace(testSpec, "tunnel: {}\n", "", 1)
		default:
			return strings.Replace(testSpec, "telemetry: {}\n", "", 1)
		}
	}
	tests := []struct {
		name            string
		spec            string
		edge, telemetry bool
	}{
		{"all three", testSpec, true, true},
		{"no tunnel", without("tunnel"), true, true},
		{"no gateway", without("gateway"), false, true},
		{"no telemetry", without("telemetry"), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := repoWith(t, tt.spec)
			specPath := filepath.Join(root, "app", "protostrap.yaml")
			p, err := MakePlan(specPath, "r", "")
			if err != nil {
				t.Fatal(err)
			}
			if (p.Edge != "") != tt.edge || (p.Telemetry != "") != tt.telemetry {
				t.Errorf("plan edge %q, telemetry %q", p.Edge, p.Telemetry)
			}
			r, err := Make(specPath, "9f2c1e7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "r", "1.4.0")
			if err != nil {
				t.Fatal(err)
			}
			if (r.Edge.Image != "") != tt.edge || (r.Telemetry.Image != "") != tt.telemetry {
				t.Errorf("release edge %+v, telemetry %+v", r.Edge, r.Telemetry)
			}
			b, _ := json.Marshal(r)
			if !tt.edge && strings.Contains(string(b), `"edge"`) {
				t.Errorf("a release without a gateway names an edge: %s", b)
			}
			if _, err := Parse(b); err != nil {
				t.Errorf("the release doesn't parse: %v", err)
			}
		})
	}
}

// TestAReleaseWithAGatewayNeedsTheEdgesImage checks that the scheduler never takes a release
// whose gateway has nothing to run.
func TestAReleaseWithAGatewayNeedsTheEdgesImage(t *testing.T) {
	r := `{"commit":"abc1234","app":"shop","protostrap":"1.4.0","spec":` + quote(testSpec) + `,"components":{` +
		`"api":{"image":"r/protostrap/api:c-1","fingerprint":"f1"},` +
		`"worker":{"image":"r/protostrap/worker:c-2","fingerprint":"f2"},` +
		`"postgres":{"image":"r/protostrap/postgres:17.2","fingerprint":"f3"},` +
		`"cache":{"image":"r/protostrap/cache:x","fingerprint":"f4"}}}`
	if _, err := Parse([]byte(r)); err == nil || !strings.Contains(err.Error(), "no edge image") {
		t.Errorf("err %v, want no edge image", err)
	}
}

// TestEdgeFingerprint checks what replaces the edge: its image, its request, and whether it
// runs the tunnel. An edge with a tunnel keeps the fingerprint it had before the tunnel was
// optional, so upgrading ProtoStrap with the same image changes nothing.
func TestEdgeFingerprint(t *testing.T) {
	request := spec.Resources{CPU: 500, Memory: 256}
	b, _ := json.Marshal(struct {
		Image   string         `json:"image"`
		Request spec.Resources `json:"request"`
	}{"r/protostrap/edge:1.4.0", request})
	sum := sha256.Sum256(b)
	before := hex.EncodeToString(sum[:8])

	if got := EdgeFingerprint("r/protostrap/edge:1.4.0", request, true); got != before {
		t.Errorf("with a tunnel %s, want %s as before", got, before)
	}
	if got := EdgeFingerprint("r/protostrap/edge:1.4.0", request, false); got == before {
		t.Errorf("without a tunnel, the fingerprint is the same")
	}
	if got := EdgeFingerprint("r/protostrap/edge:1.4.0", spec.Resources{CPU: 600, Memory: 256}, true); got == before {
		t.Errorf("another request, the same fingerprint")
	}
}
