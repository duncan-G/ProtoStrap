// Package spec reads protostrap.yaml, the one file that describes an application: its
// pools, its tunnel, gateway and telemetry, its queues, its buckets and its components
// (docs/protostrap.md#spec). Terraform reads the
// same file for the resources it creates; the scheduler reads it, inside a release, for the
// replicas it runs.
//
// Parse decodes and fills in defaults; Check applies the rules `protostrap check` enforces.
// Units follow the agent's: CPU in millicores, memory in MiB, volume sizes in GiB.
package spec

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/probe"
	"github.com/duncan-G/protostrap/internal/yaml"
)

// Spec is protostrap.yaml.
type Spec struct {
	App        string `yaml:"app"`
	ProtoStrap string `yaml:"protostrap"`
	Pools      Pools  `yaml:"pools"`
	// Tunnel, Gateway and Telemetry are ProtoStrap's own components. Each is nil when the
	// spec leaves it out, and then doesn't run.
	Tunnel     *Tunnel               `yaml:"tunnel"`
	Gateway    *Gateway              `yaml:"gateway"`
	Telemetry  *Telemetry            `yaml:"telemetry"`
	Queues     map[string]*Queue     `yaml:"queues"`
	Buckets    map[string]*Bucket    `yaml:"buckets"`
	Email      *Email                `yaml:"email"`
	Components map[string]*Component `yaml:"components"`

	root *yaml.Node
	// moved are the fields of a spec written for ProtoStrap 0.1, which Parse read in their
	// new places, and Check reports.
	moved []movedField
}

// Pools are the four VM pools. Compute is nil when the application has no compute pool.
type Pools struct {
	Ingress IngressPool  `yaml:"ingress"`
	General ScalingPool  `yaml:"general"`
	Compute *ScalingPool `yaml:"compute"`
	Data    DataPool     `yaml:"data"`
}

// IngressPool is one VM per zone, running the zone's NAT, and the gateway and tunnel when
// the spec has them.
type IngressPool struct {
	VM       string    `yaml:"vm"`
	Reserved Resources `yaml:"reserved"`
}

// ScalingPool is general or compute: VM types of one shape, sized by the scheduler.
type ScalingPool struct {
	VM       VMTypes   `yaml:"vm"`
	Min      int       `yaml:"min"`
	Max      int       `yaml:"max"`
	Spare    int       `yaml:"spare"`
	Target   Percent   `yaml:"target"`
	Reserved Resources `yaml:"reserved"`
}

// DataPool is a fixed set of VMs, data-1 to data-<count>.
type DataPool struct {
	VM       string    `yaml:"vm"`
	Count    int       `yaml:"count"`
	Reserved Resources `yaml:"reserved"`
}

// VMTypes is one instance type, or a list of them.
type VMTypes []string

func (v *VMTypes) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*v = VMTypes{n.Value}
		return nil
	}
	var list []string
	if err := yaml.Decode(n, &list); err != nil {
		return err
	}
	*v = list
	return nil
}

// Resources are what one replica reserves, or a VM keeps back.
type Resources struct {
	CPU    CPU    `yaml:"cpu"`
	Memory Memory `yaml:"memory"`
}

// API converts to the agent's units.
func (r Resources) API() api.Resources {
	return api.Resources{CPU: int64(r.CPU), Memory: int64(r.Memory)}
}

// Tunnel is cloudflared, which brings the requests for the gateway's hosts in from
// Cloudflare (docs/protostrap.md#tunnel). It runs in the gateway's container on each ingress
// VM, so it needs the gateway, and the gateway's request covers it. It has no settings:
// tunnel: {} runs it.
type Tunnel struct{}

// Gateway is Envoy on each ingress VM, which sends requests for the hosts to the upstreams
// (docs/protostrap.md#gateway).
type Gateway struct {
	// Request is what the gateway's container reserves, the tunnel's cloudflared included.
	Request Resources `yaml:"request"`
	// Hosts are the hosts the application answers on.
	Hosts Hosts `yaml:"hosts"`
	// Upstreams are the services the gateway sends requests to, by component name.
	Upstreams map[string]*Upstream `yaml:"upstreams"`
}

// Upstream is a service the gateway sends requests to: on which routes, how it speaks to
// it, and who authorizes the requests first.
type Upstream struct {
	Name string `yaml:"-"`
	// Protocol is what the gateway speaks to the service: http1, http2 or grpc. Empty takes
	// the default: see Spec.UpstreamProtocol.
	Protocol string  `yaml:"protocol"`
	Routes   []Route `yaml:"routes"`
	// Authorize has the gateway ask another service about each request to the routes
	// before sending it on.
	Authorize *Authorize `yaml:"authorize"`
}

// Telemetry is ProtoStrap's own replica on data-1, which keeps the application's logs,
// traces and metrics (docs/telemetry.md). Only its size and retention can be set.
type Telemetry struct {
	Request Resources `yaml:"request"`
	// Volume is its EBS volume's size in GiB.
	Volume    Size               `yaml:"volume"`
	Retention TelemetryRetention `yaml:"retention"`
}

// TelemetryRetention is how long telemetry keeps each kind of record, in whole days.
type TelemetryRetention struct {
	Logs    Duration `yaml:"logs"`
	Traces  Duration `yaml:"traces"`
	Metrics Duration `yaml:"metrics"`
}

// Queue is an SQS queue with its dead-letter queue.
type Queue struct {
	FIFO bool `yaml:"fifo"`
	// Visibility is how long a received job stays hidden.
	Visibility Duration `yaml:"visibility"`
	// Retention is how long an unreceived job is kept.
	Retention Duration `yaml:"retention"`
	// MaxReceives is how many times a job is received before it goes to the dead-letter queue.
	MaxReceives int `yaml:"max_receives"`
}

// Bucket is an S3 bucket of the application's own, besides the files bucket. With no
// settings it is private, encrypted and reached only over TLS.
type Bucket struct {
	// Browser lets browsers on the application's hosts use signed URLs on it.
	Browser bool `yaml:"browser"`
	// Versions turns versioning on: a replaced or deleted object's earlier version is kept
	// this long. Zero leaves versioning off.
	Versions Duration `yaml:"versions"`
	// Lock turns object lock on, so replicas may write objects under GOVERNANCE retention.
	// It needs Versions, and can't be turned off.
	Lock bool `yaml:"lock"`
	// Expire is how long after it is written an object is deleted. Zero keeps objects.
	Expire Duration `yaml:"expire"`
}

// Component is one entry under components.
type Component struct {
	Name string `yaml:"-"`

	Kind api.Kind `yaml:"kind"`
	// Build is the directory with the Dockerfile.
	Build string `yaml:"build"`
	// Project is a .csproj, .fsproj or .vbproj to build instead of Build, with the .NET SDK's
	// container publish.
	Project string `yaml:"project"`
	// Context is the build context. It defaults to Build.
	Context string `yaml:"context"`
	// Sources are more inputs to the image tag: files, directories or git pathspecs.
	Sources []string `yaml:"sources"`
	// Image is a stateful component's upstream image.
	Image   string    `yaml:"image"`
	Command []string  `yaml:"command"`
	Pool    string    `yaml:"pool"`
	VM      string    `yaml:"vm"`
	Request Resources `yaml:"request"`
	Port    int       `yaml:"port"`
	// Ready is the readiness check, as a spec writes it, such as "grpc" or "9000/healthz",
	// and ReadyCommand is a command run in the replica instead. Parse sets one of them from
	// the spec's ready, which ReadyAs holds as written.
	Ready        string            `yaml:"-"`
	ReadyCommand []string          `yaml:"-"`
	ReadyAs      readyAs           `yaml:"ready"`
	Metrics      string            `yaml:"metrics"`
	DependsOn    []string          `yaml:"depends_on"`
	Uses         []string          `yaml:"uses"`
	Env          map[string]string `yaml:"env"`
	// Secrets are the keys the component reads from the environment's secret, without their
	// prefix.
	Secrets []string `yaml:"secrets"`
	// SecretPrefix is the prefix of the component's own keys in the environment's secret. It
	// defaults to the component's name: see Prefix.
	SecretPrefix string `yaml:"secret_prefix"`
	// SecretFiles has the agent write each of Secrets to a file in /run/secrets, for images
	// that can't read the secret themselves.
	SecretFiles bool `yaml:"secret_files"`
	// OnSecretChange is what a change to the component's keys does to its replicas. Nil
	// replaces them.
	OnSecretChange *Change `yaml:"on_secret_change"`
	// Config is a directory of the repository whose files the agent mounts, read-only, in
	// the component's replicas (docs/config.md).
	Config *Config `yaml:"config"`
	// OnConfigChange is what new config files do to the component's replicas. Nil replaces
	// them.
	OnConfigChange *Change `yaml:"on_config_change"`
	// Configure names a task that configures the component once it is ready, such as
	// through its API, and runs again whenever the task, its keys or the component change.
	Configure string   `yaml:"configure"`
	Migrate   *Migrate `yaml:"migrate"`
	Stop      Duration `yaml:"stop"`
	Volume    *Volume  `yaml:"volume"`
	Backup    []string `yaml:"backup"`
	Scale     *Scale   `yaml:"scale"`
}

// Authorize is a service that answers the gateway for an upstream's requests
// (docs/protostrap.md, "Authorizing requests"). The gateway sends it each request's method, path and headers,
// without the body, to its port at Path followed by the request's own path. A 200 lets the
// request through, with the authorizer's Headers added to it and its Respond headers added
// to the service's response; anything else is the response.
type Authorize struct {
	// By is the service that answers.
	By string `yaml:"by"`
	// Path is the prefix of the path the edge asks for, such as /auth/check. Empty is /.
	Path string `yaml:"path"`
	// Headers are the authorizer's response headers the edge adds to the request it lets
	// through. The edge first removes them from the request as the client sent it, so the
	// service can trust them.
	Headers []string `yaml:"headers"`
	// Forward are the request's headers the edge sends the authorizer, besides
	// Authorization, such as cookie.
	Forward []string `yaml:"forward"`
	// Respond are the response headers of the authorizer's 200 the edge adds to the
	// service's response, such as set-cookie for a session it renewed. Each value is a
	// header of its own, after the service's.
	Respond []string `yaml:"respond"`
	// Open has the edge send a request the authorizer can't answer for on, without Headers,
	// rather than answer 503: no ready replica, no connection, no answer in time, or a 5xx.
	Open bool `yaml:"open"`
	// Timeout is how long the edge waits for the authorizer. Zero is 2s.
	Timeout Duration `yaml:"timeout"`
	// Skip are the requests the edge sends on without asking: path prefixes under the
	// service's routes, such as /assets/, and file extensions, such as *.js. Headers are
	// still removed from them.
	Skip []string `yaml:"skip"`
}

// CheckPath is the prefix of the path the edge asks the authorizer for.
func (a *Authorize) CheckPath() string {
	if a.Path == "" {
		return "/"
	}
	return a.Path
}

// Hosts names each host the application answers on, with its hostname, such as
// { admin: admin.example.com }. Routes refer to a host by its name, so they mean the same in
// every account, and an account's Terraform config may give the names other hostnames
// (docs/protostrap.md, "Hosts").
type Hosts map[string]string

func (h *Hosts) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MapNode {
		return fmt.Errorf("hosts names each host, so routes can refer to it: { app: example.com, admin: admin.example.com }")
	}
	m := map[string]string{}
	if err := yaml.Decode(n, &m); err != nil {
		return err
	}
	*h = m
	return nil
}

// Names are the hosts' names, sorted.
func (h Hosts) Names() []string { return sortedKeys(h) }

// HostEnv is the variable that holds a host's hostname in every replica: HOST_<NAME>.
func HostEnv(name string) string {
	return "HOST_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// OriginEnv is the variable that holds a host's origin in every replica, the scheme and
// host browsers address it by, such as https://admin.example.com: ORIGIN_<NAME>.
func OriginEnv(name string) string {
	return "ORIGIN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// Route is a host's name and a path prefix the gateway sends to a service. An empty host
// matches every host.
type Route struct {
	Host string
	Path string
	// Rate is the requests per second allowed, or 0 for no limit.
	Rate Rate
}

func (r *Route) UnmarshalYAML(n *yaml.Node) error {
	var match string
	switch n.Kind {
	case yaml.ScalarNode:
		match = n.Value
	case yaml.MapNode:
		var m struct {
			Route string `yaml:"route"`
			Rate  Rate   `yaml:"rate"`
		}
		if err := yaml.Decode(n, &m); err != nil {
			return err
		}
		match, r.Rate = m.Route, m.Rate
	default:
		return fmt.Errorf(`want a route like "app/api/" or "/api/", or { route: ..., rate: 100/s }`)
	}
	if match == "" {
		return fmt.Errorf("empty route")
	}
	if strings.HasPrefix(match, "/") {
		r.Path = match
		return nil
	}
	host, path, found := strings.Cut(match, "/")
	r.Host, r.Path = host, "/"+path
	if !found {
		r.Path = "/"
	}
	return nil
}

// String writes a route the way the spec does.
func (r Route) String() string { return r.Host + r.Path }

// readyAs is a component's ready as written: a check, or a command as a list.
type readyAs struct {
	check   string
	command []string
	set     bool
}

func (r *readyAs) UnmarshalYAML(n *yaml.Node) error {
	r.set = true
	switch n.Kind {
	case yaml.ScalarNode:
		r.check = n.Value
		return nil
	case yaml.SeqNode:
		if err := yaml.Decode(n, &r.command); err != nil {
			return err
		}
		if r.command == nil {
			r.command = []string{}
		}
		return nil
	}
	return fmt.Errorf("want a check, such as /healthz or grpc, or a command as a list")
}

// These are what the gateway speaks to a service.
const (
	ProtocolHTTP1 = "http1"
	// ProtocolHTTP2 is cleartext HTTP/2.
	ProtocolHTTP2 = "http2"
	// ProtocolGRPC is cleartext HTTP/2, with browsers' gRPC-Web translated on its routes.
	ProtocolGRPC = "grpc"
)

// UpstreamProtocol is what the gateway speaks to a service: its upstream's protocol, or
// grpc when its check is gRPC on its own port, otherwise http1. A service off the gateway
// speaks what its check implies, which the agent's probe uses.
func (s *Spec) UpstreamProtocol(c *Component) string {
	switch u := s.Upstream(c.Name); {
	case u != nil && u.Protocol != "":
		return u.Protocol
	case c.Ready == probe.CheckGRPC:
		return ProtocolGRPC
	}
	return ProtocolHTTP1
}

// Hosts are the gateway's hosts, or nil without a gateway.
func (s *Spec) Hosts() Hosts {
	if s.Gateway == nil {
		return nil
	}
	return s.Gateway.Hosts
}

// Upstream is the component's entry in the gateway's upstreams, or nil if it has none.
func (s *Spec) Upstream(name string) *Upstream {
	if s.Gateway == nil {
		return nil
	}
	return s.Gateway.Upstreams[name]
}

// Upstreams are the gateway's upstreams in name order, or nil without any.
func (s *Spec) Upstreams() []*Upstream {
	if s.Gateway == nil || len(s.Gateway.Upstreams) == 0 {
		return nil
	}
	out := make([]*Upstream, 0, len(s.Gateway.Upstreams))
	for _, name := range sortedKeys(s.Gateway.Upstreams) {
		out = append(out, s.Gateway.Upstreams[name])
	}
	return out
}

// Migrate is what runs once per release before a component's new replicas: a task, by
// name, or a command in the component's own image.
type Migrate struct {
	Task    string
	Command []string
}

func (m *Migrate) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		m.Task = n.Value
		return nil
	case yaml.SeqNode:
		return yaml.Decode(n, &m.Command)
	}
	return fmt.Errorf("want the name of a task, or a command as a list")
}

// These are the ways a component takes a change to its keys in the environment's secret, or
// to its config files.
const (
	// SecretRestart replaces its replicas, which read the new values when they start.
	SecretRestart = "restart"
	// SecretReload rewrites its replicas' files, which they read again themselves.
	SecretReload = "reload"
)

// Change is what a change to a component's keys, or to its config files, does to its
// replicas: Mode is restart or reload, or Command is run in each replica once its files are
// rewritten.
type Change struct {
	Mode    string
	Command []string
}

// follows reports whether replicas take the change without being replaced.
func (c *Change) follows() bool { return c != nil && (c.Mode == SecretReload || len(c.Command) > 0) }

func (c *Change) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		c.Mode = n.Value
		return nil
	case yaml.SeqNode:
		return yaml.Decode(n, &c.Command)
	}
	return fmt.Errorf("want restart, reload, or a command as a list")
}

// Prefix is the prefix of the component's own keys in the environment's secret: its
// secret_prefix, or its name in PascalCase and _, such as DocumentImport_.
func (c *Component) Prefix() string {
	if c.SecretPrefix != "" {
		return c.SecretPrefix
	}
	var b strings.Builder
	for _, part := range strings.Split(c.Name, "-") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String() + "_"
}

// Follows reports whether the component's replicas take a change to its keys without being
// replaced: the agent rewrites their secret files, and runs their command if they have one.
func (c *Component) Follows() bool { return c.OnSecretChange.follows() }

// Config is a directory of the repository, From, whose files the agent mounts at Path in the
// component's replicas.
type Config struct {
	From string `yaml:"from"`
	Path string `yaml:"path"`
}

// ConfigFollows reports whether the component's replicas take new config files without
// being replaced: the agent swaps the files in, and runs their command if they have one.
func (c *Component) ConfigFollows() bool {
	return c.Config != nil && c.Kind != api.KindTask && c.OnConfigChange.follows()
}

// ConfiguredBy returns the component that names task as its configure task, or nil.
func (s *Spec) ConfiguredBy(task string) *Component {
	for _, c := range s.Sorted() {
		if c.Configure == task && task != "" {
			return c
		}
	}
	return nil
}

// Volume is a stateful component's own EBS volume.
type Volume struct {
	// Path is where the data lives in the container.
	Path string `yaml:"path"`
	// Size is the volume's size in GiB.
	Size Size `yaml:"size"`
	// IOPS is the gp3 volume's IOPS. Terraform defaults it to 3000.
	IOPS int `yaml:"iops"`
	// Throughput is the gp3 volume's throughput in MiB/s. Terraform defaults it to 125.
	Throughput int `yaml:"throughput"`
}

// Scale is how many replicas a service or worker runs.
type Scale struct {
	Min    int      `yaml:"min"`
	Max    int      `yaml:"max"`
	Demand []Signal `yaml:"demand"`
	Limits []Limit  `yaml:"limits"`
}

// These are the kinds of demand signal, each named after the key that sets it.
const (
	SignalCPU      = "cpu"
	SignalRequests = "requests"
	SignalQueue    = "queue"
	SignalMetric   = "metric"
)

// Signal is one demand signal (docs/protostrap.md, "Demand signals").
type Signal struct {
	// CPU is the target usage, as a share of the request.
	CPU Percent `yaml:"cpu"`
	// Requests is the requests per second one replica handles.
	Requests Rate   `yaml:"requests"`
	Queue    string `yaml:"queue"`
	// Rate is the jobs per second one replica handles.
	Rate Rate `yaml:"rate"`
	// Drain is how soon the queue should be cleared.
	Drain  Duration `yaml:"drain"`
	Metric string   `yaml:"metric"`
	// Target is the metric's value one replica should carry.
	Target float64 `yaml:"target"`

	kind string
}

// Kind is which signal this is.
func (s Signal) Kind() string { return s.kind }

func (s *Signal) UnmarshalYAML(n *yaml.Node) error {
	type plain Signal
	var p plain
	if err := yaml.Decode(n, &p); err != nil {
		return err
	}
	*s = Signal(p)
	var kinds []string
	for _, k := range []string{SignalCPU, SignalRequests, SignalQueue, SignalMetric} {
		if n.Get(k) != nil {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) != 1 {
		return fmt.Errorf("a demand signal has one of cpu, requests, queue or metric")
	}
	s.kind = kinds[0]
	for key, owner := range map[string]string{"rate": SignalQueue, "drain": SignalQueue, "target": SignalMetric} {
		if n.Get(key) != nil && s.kind != owner {
			return fmt.Errorf("%s goes with a %s signal", key, owner)
		}
	}
	return nil
}

// Limit caps replicas: a fixed number, or a gauge some replica exports.
type Limit struct {
	Replicas *int   `yaml:"replicas"`
	Metric   string `yaml:"metric"`
}

// Parse fills in these defaults, among others, when the spec leaves a value out.
var (
	defaultIngressReserve = Resources{CPU: 200, Memory: 512}
	defaultPoolReserve    = Resources{CPU: 400, Memory: 1024}
	// defaultDataReserve keeps back no CPU: stateful components get cores of their own, and
	// the agent and scheduler share those cores.
	defaultDataReserve = Resources{CPU: 0, Memory: 1024}
	defaultGateway     = Resources{CPU: 500, Memory: 256}
	defaultStop        = 10 * time.Second
	// The telemetry replica's defaults are in docs/telemetry.md#settings.
	defaultTelemetry          = Resources{CPU: 500, Memory: 1536}
	defaultTelemetryVolume    = Size(20)
	defaultTelemetryRetention = TelemetryRetention{Logs: days(30), Traces: days(30), Metrics: days(15)}
)

func days(n int) Duration { return Duration{time.Duration(n) * 24 * time.Hour} }

// ReadFile parses a spec file.
func ReadFile(path string) (*Spec, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse decodes a spec and fills in its defaults. It fails only on what can't be decoded;
// Check finds everything else.
func Parse(src []byte) (*Spec, error) {
	root, err := yaml.Parse(src)
	if err != nil {
		return nil, err
	}
	moved, err := upgrade(root)
	if err != nil {
		return nil, err
	}
	s := &Spec{root: root, moved: moved}
	if err := yaml.Decode(root, s); err != nil {
		return nil, err
	}
	s.defaults()
	return s, nil
}

// movedField is a field of a spec written for ProtoStrap 0.1, where it was, and where it
// went.
type movedField struct {
	Path string
	Line int
	Msg  string
}

// upgrade reads a spec written for ProtoStrap 0.1 as this one: its hosts and the edge's
// request go to the gateway, a service's routes, protocol and authorizer to its upstream,
// and the tunnel and telemetry, which always ran, are on. A scheduler reads the releases it
// finds running, so their specs must still load; Check reports each field, so a spec isn't
// written that way again. A spec with both the old places and the new is neither, and isn't
// read.
func upgrade(root *yaml.Node) ([]movedField, error) {
	if root == nil || root.Kind != yaml.MapNode {
		return nil, nil
	}
	var moved []movedField
	add := func(k *yaml.Node, path, msg string) {
		moved = append(moved, movedField{Path: path, Line: k.Line, Msg: msg})
	}
	if k := root.KeyNode("hosts"); k != nil {
		add(k, "hosts", "moved to gateway.hosts")
	}
	if k := root.KeyNode("edge"); k != nil {
		add(k, "edge", "is now the tunnel and the gateway: its request is gateway.request")
	}
	type upstream struct {
		name string
		at   int
		node *yaml.Node
	}
	var upstreams []upstream
	if components := root.Get("components"); components != nil && components.Kind == yaml.MapNode {
		for _, c := range components.Pairs {
			if c.Value == nil || c.Value.Kind != yaml.MapNode {
				continue
			}
			u := &yaml.Node{Kind: yaml.MapNode, Line: c.Key.Line, Column: c.Key.Column}
			kept := c.Value.Pairs[:0:0]
			for _, p := range c.Value.Pairs {
				switch field := p.Key.Value; field {
				case "routes", "protocol", "authorize":
					name := c.Key.Value
					add(p.Key, "components."+name+"."+field, fmt.Sprintf("moved to gateway.upstreams.%s.%s", name, field))
					u.Pairs = append(u.Pairs, p)
				default:
					kept = append(kept, p)
				}
			}
			if len(u.Pairs) > 0 {
				c.Value.Pairs = kept
				upstreams = append(upstreams, upstream{c.Key.Value, c.Key.Line, u})
			}
		}
	}
	if len(moved) == 0 {
		return nil, nil
	}
	for _, section := range []string{"tunnel", "gateway"} {
		if k := root.KeyNode(section); k != nil {
			m := moved[0]
			return nil, &yaml.Error{Line: m.Line, Msg: fmt.Sprintf("%s: %s, and the spec has %s too: write each field in its new place", m.Path, m.Msg, section)}
		}
	}

	at := func(n *yaml.Node, key string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: key, Line: n.Line, Column: n.Column}
	}
	gateway := &yaml.Node{Kind: yaml.MapNode, Line: moved[0].Line}
	if edge := root.Get("edge"); edge != nil {
		if r := edge.Get("request"); r != nil {
			gateway.Pairs = append(gateway.Pairs, yaml.Pair{Key: at(edge.KeyNode("request"), "request"), Value: r})
		}
	}
	if hosts := root.Get("hosts"); hosts != nil {
		gateway.Pairs = append(gateway.Pairs, yaml.Pair{Key: at(root.KeyNode("hosts"), "hosts"), Value: hosts})
	}
	if len(upstreams) > 0 {
		m := &yaml.Node{Kind: yaml.MapNode, Line: upstreams[0].at}
		for _, u := range upstreams {
			m.Pairs = append(m.Pairs, yaml.Pair{Key: at(u.node, u.name), Value: u.node})
		}
		gateway.Pairs = append(gateway.Pairs, yaml.Pair{Key: at(m, "upstreams"), Value: m})
	}
	pairs := root.Pairs[:0:0]
	for _, p := range root.Pairs {
		if p.Key.Value != "hosts" && p.Key.Value != "edge" {
			pairs = append(pairs, p)
		}
	}
	empty := func() *yaml.Node { return &yaml.Node{Kind: yaml.MapNode, Line: gateway.Line} }
	pairs = append(pairs,
		yaml.Pair{Key: at(gateway, "tunnel"), Value: empty()},
		yaml.Pair{Key: at(gateway, "gateway"), Value: gateway})
	if root.KeyNode("telemetry") == nil {
		pairs = append(pairs, yaml.Pair{Key: at(gateway, "telemetry"), Value: empty()})
	}
	root.Pairs = pairs
	return moved, nil
}

// has reports whether the spec sets the value at path, such as "pools.general.spare".
func (s *Spec) has(path string) bool { return s.node(path) != nil }

func (s *Spec) defaults() {
	if !s.has("pools.ingress.reserved") {
		s.Pools.Ingress.Reserved = defaultIngressReserve
	}
	pool := func(p *ScalingPool, name string, min, spare int, target Percent) {
		for field, set := range map[string]func(){
			"min":      func() { p.Min = min },
			"spare":    func() { p.Spare = spare },
			"target":   func() { p.Target = target },
			"reserved": func() { p.Reserved = defaultPoolReserve },
		} {
			if !s.has("pools." + name + "." + field) {
				set()
			}
		}
	}
	pool(&s.Pools.General, "general", 1, 1, 0.70)
	if s.Pools.Compute != nil {
		pool(s.Pools.Compute, "compute", 0, 0, 0.85)
	}
	if !s.has("pools.data.count") {
		s.Pools.Data.Count = 1
	}
	if !s.has("pools.data.reserved") {
		s.Pools.Data.Reserved = defaultDataReserve
	}
	if g := s.Gateway; g != nil {
		if !s.has("gateway.request") {
			g.Request = defaultGateway
		}
		for name, u := range g.Upstreams {
			if u == nil {
				u = &Upstream{}
				g.Upstreams[name] = u
			}
			u.Name = name
		}
	}
	if t := s.Telemetry; t != nil {
		if !s.has("telemetry.request") {
			t.Request = defaultTelemetry
		}
		if !s.has("telemetry.volume") {
			t.Volume = defaultTelemetryVolume
		}
		for field, set := range map[string]func(){
			"logs":    func() { t.Retention.Logs = defaultTelemetryRetention.Logs },
			"traces":  func() { t.Retention.Traces = defaultTelemetryRetention.Traces },
			"metrics": func() { t.Retention.Metrics = defaultTelemetryRetention.Metrics },
		} {
			if !s.has("telemetry.retention." + field) {
				set()
			}
		}
	}
	for name, q := range s.Queues {
		if q == nil {
			q = &Queue{}
			s.Queues[name] = q
		}
		at := "queues." + name + "."
		if !s.has(at + "visibility") {
			q.Visibility.Duration = 2 * time.Minute
		}
		if !s.has(at + "retention") {
			q.Retention.Duration = 4 * 24 * time.Hour
		}
		if !s.has(at + "max_receives") {
			q.MaxReceives = 5
		}
	}
	for name, b := range s.Buckets {
		if b == nil {
			s.Buckets[name] = &Bucket{}
		}
	}
	for name, c := range s.Components {
		if c == nil {
			c = &Component{}
			s.Components[name] = c
		}
		c.Name = name
		if c.ReadyAs.set {
			c.Ready, c.ReadyCommand = c.ReadyAs.check, c.ReadyAs.command
		}
		at := "components." + name + "."
		if c.Context == "" {
			c.Context = c.Build
		}
		if !s.has(at + "stop") {
			c.Stop.Duration = defaultStop
		}
		if c.Kind != api.KindService && c.Kind != api.KindWorker {
			continue
		}
		min := 1
		if c.Kind == api.KindWorker {
			min = 0
		}
		switch {
		case c.Scale == nil:
			c.Scale = &Scale{Min: 1, Max: 1}
		case !s.has(at + "scale.min"):
			c.Scale.Min = min
		}
	}
}

// Sorted returns the components in name order.
func (s *Spec) Sorted() []*Component {
	out := make([]*Component, 0, len(s.Components))
	for _, name := range sortedKeys(s.Components) {
		out = append(out, s.Components[name])
	}
	return out
}

// QueueNames returns the queues in name order.
func (s *Spec) QueueNames() []string { return sortedKeys(s.Queues) }

// BucketNames returns the buckets in name order.
func (s *Spec) BucketNames() []string { return sortedKeys(s.Buckets) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// DataVMs are data-1 to data-<count>.
func (s *Spec) DataVMs() []string {
	var names []string
	for i := 1; i <= s.Pools.Data.Count; i++ {
		names = append(names, fmt.Sprintf("data-%d", i))
	}
	return names
}

// Pool returns a scaling pool by name, or nil.
func (s *Spec) Pool(name string) *ScalingPool {
	switch name {
	case "general":
		return &s.Pools.General
	case "compute":
		return s.Pools.Compute
	}
	return nil
}

// QueueEnv is the variable that holds a queue's URL in every replica: QUEUE_<NAME>_URL.
func QueueEnv(queue string) string {
	return "QUEUE_" + strings.ToUpper(strings.ReplaceAll(queue, "-", "_")) + "_URL"
}

// BucketEnv is the variable that holds a bucket's name in every replica: BUCKET_<NAME>.
func BucketEnv(bucket string) string {
	return "BUCKET_" + strings.ToUpper(strings.ReplaceAll(bucket, "-", "_"))
}

// QueueSignals are the queues a component scales on.
func (c *Component) QueueSignals() []string {
	var qs []string
	if c.Scale == nil {
		return nil
	}
	for _, d := range c.Scale.Demand {
		if d.Kind() == SignalQueue {
			qs = append(qs, d.Queue)
		}
	}
	return qs
}

// node finds the node at a path such as "components.api.depends_on[1]": the item for an
// index, and for a mapping key, its value. It returns nil if the spec doesn't set it.
func (s *Spec) node(path string) *yaml.Node {
	n, _ := s.locate(path)
	return n
}

// locate returns the node at path, and the line to report a problem there on: a key's
// line for a mapping entry, an item's for a list index, and when the path doesn't exist,
// the line of the deepest part that does.
func (s *Spec) locate(path string) (*yaml.Node, int) {
	n := s.root
	line := 0
	if path == "" || n == nil {
		return n, 1
	}
	for _, part := range strings.Split(path, ".") {
		key, rest, _ := strings.Cut(part, "[")
		if key != "" {
			k := n.KeyNode(key)
			if k == nil {
				return nil, line
			}
			n, line = n.Get(key), k.Line
		}
		for rest != "" {
			idx, after, _ := strings.Cut(rest, "]")
			rest = strings.TrimPrefix(after, "[")
			var i int
			if _, err := fmt.Sscanf(idx, "%d", &i); err != nil || n == nil || n.Kind != yaml.SeqNode || i >= len(n.Items) {
				return nil, line
			}
			n = n.Items[i]
			line = n.Line
		}
	}
	return n, line
}
