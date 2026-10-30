package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `protostrap edge render` writes the local edge's files from the routing ProtoStrap.Hosting
// writes under `aspire run`.
func TestEdgeRender(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(routing, []byte(`{
	  "listen": "0.0.0.0:10000",
	  "hosts": { "app": "app.localhost" },
	  "services": [ { "name": "web", "port": 4000, "ready": "/healthz", "address": "host.docker.internal:51234", "routes": [ { "host": "app", "path": "/" } ] } ]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "edge")
	var buf bytes.Buffer
	if code := run([]string{"protostrap", "edge", "render", "--routing", routing, "--out", out}, &buf); code != 0 {
		t.Fatalf("exit %d: %s", code, buf.String())
	}
	lds, err := os.ReadFile(filepath.Join(out, "lds.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lds), `"app.localhost"`) || !strings.Contains(string(lds), `"port_value": 10000`) {
		t.Errorf("listener:\n%s", lds)
	}
	for _, name := range []string{"envoy.json", "cds.json"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Error(err)
		}
	}
}

func TestEdgeRenderRefusesBadRouting(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.json")
	if err := os.WriteFile(routing, []byte(`{"services": [ { "name": 3 } ]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := run([]string{"protostrap", "edge", "render", "--routing", routing, "--out", filepath.Join(dir, "edge")}, &buf); code != 1 || !strings.Contains(buf.String(), "routing.json") {
		t.Errorf("exit %d: %s", code, buf.String())
	}
	buf.Reset()
	if code := run([]string{"protostrap", "edge", "render"}, &buf); code != 2 {
		t.Errorf("exit %d without flags: %s", code, buf.String())
	}
}
