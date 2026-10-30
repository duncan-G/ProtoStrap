package scheduler

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/metrics"
	"github.com/duncan-G/protostrap/internal/spec"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// The telemetry replica (docs/telemetry.md) is ProtoStrap's own, like the edge: one on
// data-1, with its own volume, so like a stateful component it stops before its
// replacement starts. A release made before telemetry has no telemetry image, and runs
// without it.

// telemetryStop is how many seconds the telemetry replica gets to stop: Loki and Tempo
// flush what they hold.
const telemetryStop = 60

// telemetryVM is the data VM telemetry runs on, beside the scheduler that serves Grafana.
const telemetryVM = "data-1"

func hasTelemetry(tgt *target) bool { return tgt != nil && tgt.rel.Telemetry.Image != "" }

// telemetrySpec is a replica of telemetry, the release's ProtoStrap version of it.
func (s *Scheduler) telemetrySpec(tgt *target, id string) api.ReplicaSpec {
	t := tgt.spec.Telemetry
	days := func(d spec.Duration) string { return strconv.Itoa(int(d.Hours() / 24)) }
	return api.ReplicaSpec{
		ID: id, Component: telemetry.Name, Kind: api.KindTelemetry, Release: tgt.rel.Commit,
		Fingerprint: tgt.rel.Telemetry.Fingerprint, Image: tgt.rel.Telemetry.Image,
		Request: t.Request.API(), Port: telemetry.StatusPort, Ready: "/ready", Stop: telemetryStop,
		Volume: telemetry.VolumePath, Role: s.roleARN(telemetry.Name),
		Env: map[string]string{
			telemetry.EnvBucket:      s.bucketName(telemetry.Name),
			telemetry.EnvLogsDays:    days(t.Retention.Logs),
			telemetry.EnvTracesDays:  days(t.Retention.Traces),
			telemetry.EnvMetricsDays: days(t.Retention.Metrics),
		},
	}
}

func (s *Scheduler) newTelemetry(tgt *target, now time.Time) *Replica {
	id := s.newID(telemetry.Name)
	r := &Replica{ID: id, Component: telemetry.Name, Kind: api.KindTelemetry, Release: tgt.rel.Commit,
		Fingerprint: tgt.rel.Telemetry.Fingerprint, Pool: "data", Pin: telemetryVM, State: StatePending,
		Request: tgt.spec.Telemetry.Request.API(), Spec: s.telemetrySpec(tgt, id), Created: now}
	s.st.replicas[id] = r
	return r
}

// reconcileTelemetry runs one telemetry replica on data-1, stopping an old one before its
// replacement starts, and none for a release without telemetry.
func (s *Scheduler) reconcileTelemetry(tgt *target, now time.Time) {
	var cur []*Replica
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.Kind != api.KindTelemetry || !r.live() {
			continue
		}
		switch {
		case !hasTelemetry(tgt):
			s.stop(r, "the release has no telemetry")
		case r.Fingerprint != tgt.rel.Telemetry.Fingerprint:
			s.stop(r, "replaced: telemetry stops before its replacement starts")
		default:
			cur = append(cur, r)
		}
	}
	if hasTelemetry(tgt) && len(cur) == 0 && !s.backingOff(telemetry.Name, now) {
		s.newTelemetry(tgt, now)
	}
	for _, r := range cur[min(1, len(cur)):] {
		s.stop(r, "one telemetry replica owns its volume")
	}
}

// scrapeTargets are the metrics endpoints Prometheus scrapes: every component's, and the
// edge's request counts.
func (s *Scheduler) scrapeTargets(tgt *target) []telemetry.Target {
	var out []telemetry.Target
	for _, c := range tgt.spec.Sorted() {
		if c.Metrics == "" || c.Kind == api.KindTask {
			continue
		}
		port, path, err := metrics.ParseEndpoint(c.Metrics)
		if err != nil {
			continue // The spec's check refuses it.
		}
		out = append(out, telemetry.Target{Component: c.Name, Port: port, Path: path})
	}
	return append(out, telemetry.Target{Component: "edge", Port: EdgeStatusPort, Path: "/metrics"})
}

// otelEnv sets the OTEL_ variables a component's replica gets, which OpenTelemetry's SDKs
// read (docs/telemetry.md#traces-and-metrics). The spec's env wins for each, and the
// endpoint and its protocol go together: a spec that sets either gets neither of these.
func (s *Scheduler) otelEnv(env map[string]string, c *spec.Component, id, release string) {
	set := func(k, v string) {
		if _, ok := c.Env[k]; !ok {
			env[k] = v
		}
	}
	_, endpoint := c.Env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	_, protocol := c.Env["OTEL_EXPORTER_OTLP_PROTOCOL"]
	if !endpoint && !protocol {
		env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://" + telemetry.Name + ":" + strconv.Itoa(telemetry.OTLPHTTPPort)
		env["OTEL_EXPORTER_OTLP_PROTOCOL"] = "http/protobuf"
	}
	set("OTEL_SERVICE_NAME", c.Name)
	attrs := []string{"service.instance.id=" + id, "service.version=" + release}
	if s.cfg.Environment != "" {
		attrs = append(attrs, "deployment.environment.name="+s.cfg.Environment)
	}
	set("OTEL_RESOURCE_ATTRIBUTES", strings.Join(attrs, ","))
}
