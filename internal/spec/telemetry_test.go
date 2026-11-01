package spec

import (
	"strings"
	"testing"
	"time"
)

// These tests cover the spec's telemetry section (docs/telemetry.md#settings): with one,
// ProtoStrap adds the telemetry replica on data-1, and the section sets its size and retention.

// withTelemetry gives the docs example's telemetry section the settings in lines.
func withTelemetry(lines string) func(string) string {
	return replace("telemetry: {}\n", "telemetry:\n"+lines)
}

func TestTelemetryDefaults(t *testing.T) {
	s := mustParse(t, docsExample)
	tel := s.Telemetry
	if tel.Request != (Resources{CPU: 500, Memory: 1536}) {
		t.Errorf("request %+v, want 500m and 1536Mi", tel.Request)
	}
	if tel.Volume != 20 {
		t.Errorf("volume %dGi, want 20Gi", tel.Volume)
	}
	day := 24 * time.Hour
	if r := tel.Retention; r.Logs.Duration != 30*day || r.Traces.Duration != 30*day || r.Metrics.Duration != 15*day {
		t.Errorf("retention %v %v %v, want 30d, 30d and 15d", r.Logs, r.Traces, r.Metrics)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("the docs example, with telemetry's defaults, must fit data-1: %v", Problems(problems))
	}
}

func TestTelemetrySettings(t *testing.T) {
	src := withTelemetry("  request: { cpu: 250m, memory: 1Gi }\n  volume: 50Gi\n  retention: { logs: 7d, traces: 3d, metrics: 30d }\n")(docsExample)
	s := mustParse(t, src)
	tel := s.Telemetry
	if tel.Request != (Resources{CPU: 250, Memory: 1024}) || tel.Volume != 50 {
		t.Errorf("request %+v volume %d", tel.Request, tel.Volume)
	}
	day := 24 * time.Hour
	if r := tel.Retention; r.Logs.Duration != 7*day || r.Traces.Duration != 3*day || r.Metrics.Duration != 30*day {
		t.Errorf("retention %+v", r)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("problems: %v", Problems(problems))
	}
}

func TestTelemetryKeepsTheDefaultsItDoesntSet(t *testing.T) {
	s := mustParse(t, withTelemetry("  retention: { logs: 7d }\n")(docsExample))
	tel := s.Telemetry
	day := 24 * time.Hour
	if tel.Retention.Logs.Duration != 7*day || tel.Retention.Traces.Duration != 30*day || tel.Retention.Metrics.Duration != 15*day {
		t.Errorf("retention %+v: an unset field keeps its default", tel.Retention)
	}
	if tel.Request != (Resources{CPU: 500, Memory: 1536}) || tel.Volume != 20 {
		t.Errorf("request %+v volume %d", tel.Request, tel.Volume)
	}
}

func TestTelemetryCheck(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"fits beside the stateful components", withTelemetry("  request: { cpu: 500m, memory: 2Gi }\n"), "", ""},
		{"too much CPU for data-1", withTelemetry("  request: { cpu: 1000m, memory: 1Gi }\n"), "telemetry.request", "data-1"},
		{"too much memory for data-1", withTelemetry("  request: { cpu: 500m, memory: 3Gi }\n"), "telemetry.request", "data-1"},
		{"no CPU", withTelemetry("  request: { cpu: 0m, memory: 1Gi }\n"), "telemetry.request", "must reserve CPU and memory"},
		{"no memory", withTelemetry("  request: { cpu: 500m, memory: 0Mi }\n"), "telemetry.request", "must reserve CPU and memory"},
		{"less memory than its programs need", withTelemetry("  request: { cpu: 500m, memory: 512Mi }\n"), "telemetry.request", "at least 1Gi"},
		{"small volume", withTelemetry("  volume: 2Gi\n"), "telemetry.volume", "at least 5Gi"},
		{"logs in hours", withTelemetry("  retention: { logs: 36h }\n"), "telemetry.retention.logs", "whole number of days"},
		{"no traces kept", withTelemetry("  retention: { traces: 0d }\n"), "telemetry.retention.traces", "at least 1d"},
		{"metrics in minutes", withTelemetry("  retention: { metrics: 90m }\n"), "telemetry.retention.metrics", "whole number of days"},
		{"a component named telemetry", replace("  orders-worker:\n", "  telemetry:\n"), "components.telemetry", "reserved"},
		{"a bucket named telemetry", withBuckets("  telemetry: {}\n"), "buckets.telemetry", "reserved"},
		// Without telemetry, data-1 is the stateful components' alone.
		{"stateful fill data-1 without telemetry", func(s string) string {
			s = replace("telemetry: {}\n\n", "")(s)
			return replace("request: { cpu: 1000m, memory: 4Gi }", "request: { cpu: 1500m, memory: 5Gi }")(s)
		}, "", ""},
		{"stateful fill data-1 beside telemetry", replace("request: { cpu: 1000m, memory: 4Gi }", "request: { cpu: 1500m, memory: 5Gi }"), "telemetry.request", "data-1"},
		// The stateful components are blamed when they don't fit by themselves, not
		// telemetry.
		{"stateful too big without telemetry", replace("request: { cpu: 1000m, memory: 4Gi }", "request: { cpu: 2000m, memory: 4Gi }"), "components.redis.request", "data-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse([]byte(tt.edit(docsExample)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			problems := s.Check()
			if tt.path == "" {
				if len(problems) > 0 {
					t.Fatalf("unexpected problems: %v", Problems(problems))
				}
				return
			}
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("no problem at %s containing %q; got:\n%v", tt.path, tt.want, Problems(problems))
		})
	}
}

// TestTelemetryOnlyBlamesItselfOnce checks that a stateful overflow isn't reported twice,
// once for the component and again for telemetry.
func TestTelemetryOnlyBlamesItselfOnce(t *testing.T) {
	s := mustParse(t, replace("request: { cpu: 1000m, memory: 4Gi }", "request: { cpu: 2000m, memory: 4Gi }")(docsExample))
	for _, p := range s.Check() {
		if p.Path == "telemetry.request" {
			t.Fatalf("telemetry blamed for stateful components that don't fit by themselves: %v", p)
		}
	}
}

func TestTelemetryChangesTheInfraHashOnlyWhereTerraformReadsIt(t *testing.T) {
	base := mustParse(t, docsExample).InfraHash()
	tests := []struct {
		name    string
		edit    func(string) string
		changes bool
	}{
		// Terraform makes the volume, and the bucket's expiry from the longer of logs and
		// traces.
		{"volume", withTelemetry("  volume: 40Gi\n"), true},
		{"logs retention", withTelemetry("  retention: { logs: 60d }\n"), true},
		{"traces retention", withTelemetry("  retention: { traces: 7d }\n"), true},
		// The replica's request and Prometheus's retention are the scheduler's alone.
		{"request", withTelemetry("  request: { cpu: 250m, memory: 1Gi }\n"), false},
		{"metrics retention", withTelemetry("  retention: { metrics: 30d }\n"), false},
		{"the defaults, written out", withTelemetry("  request: { cpu: 500m, memory: 1536Mi }\n  volume: 20Gi\n  retention: { logs: 30d, traces: 30d, metrics: 15d }\n"), false},
	}
	for _, tt := range tests {
		got := mustParse(t, tt.edit(docsExample)).InfraHash()
		if (got != base) != tt.changes {
			t.Errorf("%s: infra hash changed = %v, want %v", tt.name, got != base, tt.changes)
		}
	}
}
