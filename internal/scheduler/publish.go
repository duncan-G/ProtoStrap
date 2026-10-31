package scheduler

import (
	"maps"
	"slices"
	"strings"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/edge"
	"github.com/duncan-G/protostrap/internal/secret"
	"github.com/duncan-G/protostrap/internal/spec"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// EdgeStatusPort is the port of the edge's own HTTP server, `protostrap edge`, where it
// answers its readiness check and serves Envoy's request counts. edgeStop is how many
// seconds the edge gets to stop.
const (
	EdgeStatusPort = 9902
	edgeStop       = 30
)

// assignment is everything a VM should run (docs/protostrap.md#sync). It is complete:
// whatever runs on the VM and isn't in it is stopped.
func (s *Scheduler) assignment(vm *VM) api.Assignment {
	a := api.Assignment{Version: s.version(), Replicas: []api.ReplicaSpec{}}
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.VM != vm.ID || r.Stop || !(r.State == StateAssigned || r.State == api.StateStarting || r.State == api.StateReady) {
			continue
		}
		a.Replicas = append(a.Replicas, r.Spec)
	}
	if vm.Pool == "ingress" && s.st.target != nil {
		a.Files = edge.Files(edge.Routing{Services: s.edgeServices(s.st.target), Hosts: s.hosts(s.st.target)})
	}
	if vm.Pool == "data" && vm.Name == telemetryVM && hasTelemetry(s.st.target) {
		a.Files = map[string]string{telemetry.ScrapeFile: telemetry.TargetsFile(s.scrapeTargets(s.st.target))}
	}
	a.Secret = s.secretName()
	for _, r := range a.Replicas {
		if cs := s.st.secrets[r.Component]; r.SecretFollows && cs != nil {
			if a.SecretVersions == nil {
				a.SecretVersions = map[string]string{}
			}
			a.SecretVersions[r.Component] = cs.Read
		}
		if rep := s.st.replicas[r.ID]; r.ConfigFollows && rep != nil && rep.ConfigWant != "" {
			if a.ConfigDigests == nil {
				a.ConfigDigests = map[string]string{}
			}
			a.ConfigDigests[r.ID] = rep.ConfigWant
		}
	}
	return a
}

// edgeServices are the services the edge sends requests to, and those it asks whether to.
func (s *Scheduler) edgeServices(tgt *target) []edge.Service {
	return EdgeServices(tgt.spec)
}

// EdgeServices are a spec's services as the edge sees them: those with routes, and those
// that authorize their requests.
func EdgeServices(sp *spec.Spec) []edge.Service {
	authorizers := map[string]bool{}
	for _, c := range sp.Components {
		if c.Authorize != nil && len(c.Routes) > 0 {
			authorizers[c.Authorize.By] = true
		}
	}
	var out []edge.Service
	for _, c := range sp.Sorted() {
		if c.Kind != api.KindService || (len(c.Routes) == 0 && !authorizers[c.Name]) {
			continue
		}
		svc := edge.Service{Name: c.Name, Port: c.Port, Ready: c.Ready, Protocol: c.UpstreamProtocol()}
		for _, r := range c.Routes {
			svc.Routes = append(svc.Routes, edge.Route{Host: r.Host, Path: r.Path, Rate: float64(r.Rate)})
		}
		if a := c.Authorize; a != nil {
			svc.Authorize = &edge.Authorize{By: a.By, Path: a.CheckPath(), Headers: a.Headers, Forward: a.Forward, Respond: a.Respond}
		}
		out = append(out, svc)
	}
	return out
}

func partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	}
	return "aws"
}

// roleARN is a component's role, which the agent takes on for its replicas.
func (s *Scheduler) roleARN(component string) string {
	return "arn:" + partition(s.cfg.Region) + ":iam::" + s.cfg.Account + ":role/protostrap/protostrap-c-" + component
}

func (s *Scheduler) queueURL(queue string, fifo bool) string {
	name := "protostrap-" + queue
	if fifo {
		name += ".fifo"
	}
	return "https://sqs." + s.cfg.Region + ".amazonaws.com/" + s.cfg.Account + "/" + name
}

// bucketName is one of the spec's buckets in S3, named as infra/platform/buckets.tf names it.
func (s *Scheduler) bucketName(bucket string) string {
	return "protostrap-" + s.cfg.Account + "-" + s.cfg.Region + "-" + bucket
}

// buildSpec is a replica of a component at a release (docs/protostrap.md#agent, "How it
// runs a replica"). The agent adds ProtoStrap's own variables for the replica.
func (s *Scheduler) buildSpec(tgt *target, c *spec.Component, id string) api.ReplicaSpec {
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = v
	}
	for _, q := range tgt.spec.QueueNames() {
		env[spec.QueueEnv(q)] = s.queueURL(q, tgt.spec.Queues[q].FIFO)
	}
	for _, b := range tgt.spec.BucketNames() {
		env[spec.BucketEnv(b)] = s.bucketName(b)
	}
	if s.cfg.FilesBucket != "" {
		env["FILES_BUCKET"] = s.cfg.FilesBucket
	}
	if hasTelemetry(tgt) {
		s.otelEnv(env, c, id, tgt.rel.Commit)
	}
	hostsEnv(env, s.hosts(tgt))
	rs := api.ReplicaSpec{
		ID: id, Component: c.Name, Kind: c.Kind, Release: tgt.rel.Commit,
		Fingerprint: s.fingerprint(tgt, c.Name), Image: tgt.rel.Components[c.Name].Image,
		Command: c.Command, Request: c.Request.API(), Port: c.Port, Ready: c.Ready, ReadyCommand: c.ReadyCommand,
		Stop: int(c.Stop.Seconds()), Env: env, Role: s.roleARN(c.Name),
	}
	if c.Kind == api.KindService && c.UpstreamProtocol() != spec.ProtocolHTTP1 {
		rs.H2C = true
	}
	if c.Metrics != "" {
		if names := s.metricNames(tgt, c); len(names) > 0 {
			rs.Metrics = &api.MetricsSpec{Endpoint: c.Metrics, Names: names}
		}
	}
	if c.Kind == api.KindStateful && c.Volume != nil {
		rs.Volume = c.Volume.Path
		rs.Backup = c.Backup
	}
	s.secretSpec(&rs, c.Name, c.Prefix())
	if c.SecretFiles {
		rs.SecretFiles = c.Secrets
	}
	if c.Follows() && c.Kind != api.KindTask {
		rs.SecretFollows, rs.SecretHook = true, c.OnSecretChange.Command
	}
	if c.Config != nil {
		rs.ConfigPath, rs.ConfigDigest = c.Config.Path, tgt.rel.Components[c.Name].Config
		if c.ConfigFollows() {
			rs.ConfigFollows, rs.ConfigHook = true, c.OnConfigChange.Command
		}
	}
	return rs
}

// metricNames are the metrics the scheduler asks a component's replicas for: its own
// metric signals, and every limit in the spec, since any replica of any component may
// export one.
func (s *Scheduler) metricNames(tgt *target, c *spec.Component) []string {
	set := map[string]bool{}
	if c.Scale != nil {
		for _, d := range c.Scale.Demand {
			if d.Kind() == spec.SignalMetric {
				set[d.Metric] = true
			}
		}
	}
	for _, other := range tgt.spec.Components {
		if other.Scale == nil {
			continue
		}
		for _, l := range other.Scale.Limits {
			if l.Metric != "" {
				set[l.Metric] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// edgeSpec is a replica of the edge, the release's ProtoStrap version of it.
func (s *Scheduler) edgeSpec(tgt *target, id string) api.ReplicaSpec {
	var names []string
	for _, svc := range s.edgeServices(tgt) {
		names = append(names, edge.RequestsMetric(svc.Name))
	}
	rs := api.ReplicaSpec{
		ID: id, Component: "edge", Kind: api.KindEdge, Release: tgt.rel.Commit,
		Fingerprint: tgt.rel.Edge.Fingerprint, Image: tgt.rel.Edge.Image,
		Request: tgt.spec.Edge.Request.API(), Port: EdgeStatusPort, Ready: "/ready", Stop: edgeStop,
		Role: s.roleARN("edge"),
	}
	if len(names) > 0 {
		rs.Metrics = &api.MetricsSpec{Endpoint: "9902/metrics", Names: names}
	}
	s.secretSpec(&rs, "edge", secret.Edge)
	return rs
}
