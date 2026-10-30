package main

import (
	"strings"
	"testing"
)

// These tests cover Grafana's listener on data-1 (docs/telemetry.md#reading-telemetry): a
// loopback port of its own, which the protostrap-telemetry session document forwards, apart
// from the operator UI's, since Developers may open it and not the UI.

func TestGrafanaListensOnItsOwnLoopbackPort(t *testing.T) {
	c, err := loadWithUI(t, "")
	if err != nil || c.TelemetryListen != "127.0.0.1:7482" {
		t.Errorf("telemetry_listen %q, %v; want 127.0.0.1:7482", c.TelemetryListen, err)
	}
	c, err = loadWithUI(t, `,"telemetry_listen":""`)
	if err != nil || c.TelemetryListen != "" {
		t.Errorf("telemetry_listen %q, %v; want it off", c.TelemetryListen, err)
	}
	for _, addr := range []string{":7482", "0.0.0.0:7482", "10.20.1.10:7482"} {
		if _, err := loadWithUI(t, `,"telemetry_listen":"`+addr+`"`); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%q: %v, want it refused as not loopback", addr, err)
		}
	}
}

func TestGrafanaNeverSharesTheOperatorUIsPort(t *testing.T) {
	// The port is the role: Developers' document forwards Grafana's port, and must never
	// reach the infra log.
	for _, extra := range []string{`,"telemetry_listen":"127.0.0.1:7480"`, `,"telemetry_listen":"127.0.0.1:7481"`} {
		if _, err := loadWithUI(t, extra); err == nil || !strings.Contains(err.Error(), "telemetry_listen") {
			t.Errorf("%s: %v, want it refused", extra, err)
		}
	}
}
