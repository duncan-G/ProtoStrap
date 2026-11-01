package spec

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/metrics"
	"github.com/duncan-G/protostrap/internal/probe"
	"github.com/duncan-G/protostrap/internal/secret"
)

// Problem is one rule a spec breaks.
type Problem struct {
	// Path is where the problem is, such as components.api.request.
	Path string
	// Line is the line the problem is on, or 0 if it is unknown.
	Line int
	Msg  string
}

func (p Problem) String() string {
	at := p.Path
	if p.Line > 0 {
		at = fmt.Sprintf("line %d: %s", p.Line, p.Path)
	}
	if p.Path == "" && p.Line == 0 {
		return p.Msg
	}
	return at + ": " + p.Msg
}

// Problems is a list of problems, usable as an error.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

var (
	dnsLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	hostName   = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	semver     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)
	dataVM     = regexp.MustCompile(`^data-([1-9][0-9]*)$`)
)

// reservedComponents are names ProtoStrap uses itself, bucketUses are ProtoStrap's buckets
// a component can name in uses, and reservedBuckets are the names its own buckets end in.
var (
	reservedComponents = map[string]bool{"edge": true, "telemetry": true, "protostrap": true, "scheduler": true, "agent": true}
	bucketUses         = map[string]bool{"files": true, "backups": true}
	reservedBuckets    = map[string]bool{"files": true, "backups": true, "deploy": true, "tfstate": true, "telemetry": true}
)

const (
	// minTelemetryMemory is the least the telemetry replica's five programs can share.
	minTelemetryMemory = 1024
	// minTelemetryVolume holds Prometheus's metrics and Loki's and Tempo's write-ahead logs.
	minTelemetryVolume = 5
)

const (
	maxComponentName = 40 // It keeps protostrap-c-<name> within IAM's 64 characters.
	maxQueueName     = 70 // It keeps protostrap-<name>-dlq.fifo within SQS's 80 characters.
	// maxBucketName keeps protostrap-<account>-<region>-<name> within S3's 63 characters in
	// every region, the longest of which have 14.
	maxBucketName = 24
	maxStop       = time.Hour
	day           = 24 * time.Hour
)

type checker struct {
	s        *Spec
	problems []Problem
}

func (c *checker) add(path, format string, args ...any) {
	_, line := c.s.locate(path)
	c.problems = append(c.problems, Problem{Path: path, Line: line, Msg: fmt.Sprintf(format, args...)})
}

// Check returns every rule the spec breaks, in the order of the file's sections.
func (s *Spec) Check() []Problem {
	c := &checker{s: s}
	c.top()
	c.pools()
	c.cdn()
	c.tunnel()
	c.gateway()
	c.telemetry()
	c.queues()
	c.buckets()
	c.email()
	c.components()
	return c.problems
}

func (c *checker) top() {
	s := c.s
	switch {
	case s.App == "":
		c.add("app", "required")
	case !dnsLabel.MatchString(s.App) || len(s.App) > 30:
		c.add("app", "%q must be up to 30 lowercase letters, digits and dashes", s.App)
	}
	if !semver.MatchString(s.ProtoStrap) {
		c.add("protostrap", "%q isn't a ProtoStrap version, like 1.4.0", s.ProtoStrap)
	}
	for _, m := range s.moved {
		c.problems = append(c.problems, Problem{Path: m.Path, Line: m.Line, Msg: m.Msg})
	}
}

// present reports whether the spec has a section, and that one written with nothing after it,
// which YAML reads as null and so leaves off, is a mistake.
func (c *checker) present(section string) bool {
	n := c.s.node(section)
	if n != nil && n.IsNull() {
		c.add(section, "empty, so it doesn't run: write %s: {} to run it with ProtoStrap's defaults, or leave it out", section)
	}
	return n != nil && !n.IsNull()
}

// These are what a CDN rule may name: Cloudflare's rate limit periods, its WAF rules'
// actions, and the shapes of a rule's values.
var (
	rateLimitPeriods = []time.Duration{10 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, time.Hour}
	wafActions       = []string{"block", "managed_challenge", "js_challenge", "challenge", "skip"}
	httpMethods      = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	countryCode      = regexp.MustCompile(`^[A-Z][A-Z0-9]$`)
	fileExtension    = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
)

// cdn checks the CDN: its rate limit, and each of its WAF and cache rules.
func (c *checker) cdn() {
	if !c.present("cdn") {
		return
	}
	cdn := c.s.CDN
	if c.s.Tunnel == nil {
		c.add("cdn", "the CDN is Cloudflare in front of the tunnel: add tunnel: {}")
	}
	if r := cdn.RateLimit; r != nil {
		if r.Requests < 1 {
			c.add("cdn.rate_limit.requests", "must be at least 1")
		}
		if !slices.Contains(rateLimitPeriods, r.Period.Duration) {
			c.add("cdn.rate_limit.period", "must be 10s, 1m, 2m, 5m, 10m or 1h, Cloudflare's periods")
		}
	}
	for _, list := range []struct {
		name  string
		rules []CDNRule
	}{{"waf", cdn.WAF}, {"cache", cdn.Cache}} {
		named := map[string]int{}
		for i, r := range list.rules {
			at := fmt.Sprintf("cdn.%s[%d]", list.name, i)
			switch first, ok := named[r.Name]; {
			case r.Name == "":
				c.add(at+".name", "required: a name for the rule, such as block-admin")
			case !dnsLabel.MatchString(r.Name):
				c.add(at+".name", "%q must be lowercase letters, digits and dashes", r.Name)
			case ok:
				c.add(at+".name", "%s is also cdn.%s[%d]", r.Name, list.name, first)
			default:
				named[r.Name] = i
			}
			c.cdnRule(at, list.name, r)
		}
	}
}

// cdnRule checks what a CDN rule matches, and what it does.
func (c *checker) cdnRule(at, list string, r CDNRule) {
	hosts := c.s.Hosts()
	for i, h := range r.Hosts {
		ha := fmt.Sprintf("%s.hosts[%d]", at, i)
		if _, ok := hosts[h]; ok {
			continue
		}
		if strings.Contains(h, ".") {
			c.add(ha, "a rule names a host by its name in gateway.hosts, such as %s, not its hostname", example(hosts))
		} else {
			c.add(ha, "no host %s in gateway.hosts", h)
		}
	}
	for i, p := range r.Paths {
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\"\\ \t") {
			c.add(fmt.Sprintf("%s.paths[%d]", at, i), "%q isn't a path prefix that starts with /, such as /admin/", p)
		}
	}
	for i, e := range r.Extensions {
		if !fileExtension.MatchString(e) {
			c.add(fmt.Sprintf("%s.extensions[%d]", at, i), "%q isn't a file extension: up to 10 lower-case letters and digits, such as js", e)
		}
	}
	for i, m := range r.Methods {
		if !slices.Contains(httpMethods, m) {
			c.add(fmt.Sprintf("%s.methods[%d]", at, i), "%q isn't an HTTP method in upper case, such as POST", m)
		}
	}
	for field, codes := range map[string][]string{"countries": r.Countries, "not_countries": r.NotCountries} {
		for i, code := range codes {
			if !countryCode.MatchString(code) {
				c.add(fmt.Sprintf("%s.%s[%d]", at, field, i), "%q isn't a two-letter country code, such as US", code)
			}
		}
	}
	for field, ranges := range map[string][]string{"ips": r.IPs, "not_ips": r.NotIPs} {
		for i, ip := range ranges {
			if _, err := netip.ParsePrefix(ip); err == nil {
				continue
			}
			if _, err := netip.ParseAddr(ip); err != nil {
				c.add(fmt.Sprintf("%s.%s[%d]", at, field, i), "%q isn't an IP address or range, such as 203.0.113.0/24", ip)
			}
		}
	}
	if c.s.has(at+".expression") && strings.TrimSpace(r.Expression) == "" {
		c.add(at+".expression", "empty: leave it out, or write Cloudflare's rules language, such as http.user_agent contains \"bot\"")
	}

	cacheFields := []string{"cache", "edge_ttl", "browser_ttl"}
	if list == "waf" {
		switch {
		case r.Action == "":
			c.add(at+".action", "required: block, managed_challenge, js_challenge, challenge or skip")
		case !slices.Contains(wafActions, r.Action):
			c.add(at+".action", "%q must be block, managed_challenge, js_challenge, challenge or skip", r.Action)
		}
		for _, f := range cacheFields {
			if c.s.has(at + "." + f) {
				c.add(at+"."+f, "goes in a cache rule, in cdn.cache")
			}
		}
		return
	}
	if c.s.has(at + ".action") {
		c.add(at+".action", "goes in a WAF rule, in cdn.waf")
	}
	bypass := r.Cache != nil && !*r.Cache
	for _, ttl := range []struct {
		field string
		d     time.Duration
	}{{"edge_ttl", r.EdgeTTL.Duration}, {"browser_ttl", r.BrowserTTL.Duration}} {
		switch {
		case !c.s.has(at + "." + ttl.field):
		case bypass:
			c.add(at+"."+ttl.field, "a rule that bypasses the cache has no TTLs")
		case ttl.d < time.Second:
			c.add(at+"."+ttl.field, "must be at least 1s")
		case ttl.d%time.Second != 0:
			c.add(at+"."+ttl.field, "must be a whole number of seconds")
		}
	}
}

// tunnel checks the tunnel, which brings requests in for the gateway.
func (c *checker) tunnel() {
	if c.present("tunnel") && c.s.Gateway == nil {
		c.add("tunnel", "the tunnel sends every request to the gateway: add a gateway section")
	}
}

// gateway checks the gateway: its request, its hosts and its upstreams.
func (c *checker) gateway() {
	if !c.present("gateway") {
		return
	}
	g := c.s.Gateway
	if r := g.Request; r.CPU <= 0 || r.Memory <= 0 {
		c.add("gateway.request", "must reserve CPU and memory")
	} else if ingress, err := LookupVM(c.s.Pools.Ingress.VM); err == nil && !fits(r, room(ingress, c.s.Pools.Ingress.Reserved)) {
		c.add("gateway.request", "%s and %s doesn't fit the ingress VM, %s, after its reserve", FormatCPU(int64(r.CPU)), FormatMemory(int64(r.Memory)), c.s.Pools.Ingress.VM)
	}
	named := map[string]string{} // It maps each hostname to the first host's name it has.
	for _, name := range g.Hosts.Names() {
		h, at := g.Hosts[name], "gateway.hosts."+name
		if !dnsLabel.MatchString(name) {
			c.add(at, "%q isn't a host's name: lowercase letters, digits and dashes, which routes refer to as %s/", name, name)
		}
		if !hostName.MatchString(h) {
			c.add(at, "%q isn't a host name", h)
		} else if other, ok := named[h]; ok {
			c.add(at, "%s is also gateway.hosts.%s: each host has a hostname of its own", h, other)
		} else {
			named[h] = name
		}
	}
	routed := map[string]string{} // It maps each host and path to the upstream routed there.
	for _, u := range c.s.Upstreams() {
		c.upstream(u, routed)
	}
}

// upstream checks one of the gateway's upstreams: a service, with routes no other has.
func (c *checker) upstream(u *Upstream, routed map[string]string) {
	at := "gateway.upstreams." + u.Name
	switch comp, ok := c.s.Components[u.Name]; {
	case !ok:
		c.add(at, "no component %s", u.Name)
	case comp.Kind != api.KindService:
		kind := string(comp.Kind)
		if comp.Kind == api.KindStateful {
			kind = "stateful component"
		}
		c.add(at, "%s is a %s: only services get requests from the gateway", u.Name, kind)
	}
	if len(u.Routes) == 0 {
		c.add(at+".routes", "required: the routes the gateway sends to %s; a service that only authorizes requests needs no upstream", u.Name)
	}
	hosts := c.s.Gateway.Hosts
	for i, route := range u.Routes {
		ra := fmt.Sprintf("%s.routes[%d]", at, i)
		_, known := hosts[route.Host]
		switch {
		case route.Host == "" || known:
		case strings.Contains(route.Host, "."):
			c.add(ra, "a route names its host by its name in gateway.hosts, such as %s%s, not %s", example(hosts), route.Path, route.Host)
		default:
			c.add(ra, "no host %s in gateway.hosts", route.Host)
		}
		if route.Rate < 0 {
			c.add(ra, "negative rate")
		}
		key := route.Host + route.Path
		if other, ok := routed[key]; ok {
			c.add(ra, "%s is also routed to %s", route, other)
		} else {
			routed[key] = u.Name
		}
	}
	switch u.Protocol {
	case "", ProtocolHTTP1, ProtocolHTTP2, ProtocolGRPC:
	default:
		c.add(at+".protocol", "%q must be http1, http2 or grpc", u.Protocol)
	}
	c.authorize(u)
}

// vm looks up a pool's VM type, reporting what's wrong with it.
func (c *checker) vm(at, name string, data bool) (VMType, bool) {
	if name == "" {
		c.add(at, "required")
		return VMType{}, false
	}
	t, err := LookupVM(name)
	if err != nil {
		c.add(at, "%v", err)
		return VMType{}, false
	}
	if data && t.InstanceStore {
		c.add(at, "%s has an instance store, which loses data on any stop: the data VM keeps its state on EBS", name)
	}
	return t, true
}

// room is what a VM of type t has for replicas once the reserve is kept back.
func room(t VMType, reserved Resources) Resources {
	return Resources{CPU: CPU(t.CPU) - reserved.CPU, Memory: Memory(t.Memory) - reserved.Memory}
}

func fits(r, room Resources) bool { return r.CPU <= room.CPU && r.Memory <= room.Memory }

func (c *checker) reserve(at string, r Resources, t VMType, ok bool) {
	if r.CPU < 0 || r.Memory < 0 {
		c.add(at, "negative")
	} else if ok && (room(t, r).CPU <= 0 || room(t, r).Memory <= 0) {
		c.add(at, "leaves no room on the VM for replicas")
	}
}

func (c *checker) pools() {
	p := c.s.Pools
	ingress, ok := c.vm("pools.ingress.vm", p.Ingress.VM, false)
	c.reserve("pools.ingress.reserved", p.Ingress.Reserved, ingress, ok)

	c.scalingPool("general", &p.General, 1)
	if p.Compute != nil {
		c.scalingPool("compute", p.Compute, 0)
	}

	data, ok := c.vm("pools.data.vm", p.Data.VM, true)
	if p.Data.Count < 1 || p.Data.Count > 3 {
		c.add("pools.data.count", "must be 1 to 3: data VMs have the fixed addresses 10.20.1.10 to 10.20.1.12")
	}
	c.reserve("pools.data.reserved", p.Data.Reserved, data, ok)
}

// telemetry checks the telemetry section; whether it fits data-1 is checked with the
// stateful components there, in components.
func (c *checker) telemetry() {
	if !c.present("telemetry") {
		return
	}
	t := c.s.Telemetry
	switch r := t.Request; {
	case r.CPU <= 0 || r.Memory <= 0:
		c.add("telemetry.request", "must reserve CPU and memory")
	case r.Memory < minTelemetryMemory:
		c.add("telemetry.request", "%s is too little memory for its five programs: at least 1Gi", FormatMemory(int64(r.Memory)))
	}
	if t.Volume < minTelemetryVolume {
		c.add("telemetry.volume", "must be at least 5Gi")
	}
	// Loki, Tempo and Prometheus keep whole days, as the bucket's lifecycle rule counts them.
	for _, r := range []struct {
		field string
		d     time.Duration
	}{{"logs", t.Retention.Logs.Duration}, {"traces", t.Retention.Traces.Duration}, {"metrics", t.Retention.Metrics.Duration}} {
		if r.d < day || r.d%day != 0 {
			c.add("telemetry.retention."+r.field, "must be a whole number of days, at least 1d")
		}
	}
}

func (c *checker) scalingPool(name string, p *ScalingPool, minMin int) {
	at := "pools." + name
	if len(p.VM) == 0 {
		c.add(at+".vm", "required: one or more instance types of one shape")
	}
	var first VMType
	var firstOK bool
	for i, vm := range p.VM {
		t, ok := c.vm(fmt.Sprintf("%s.vm[%d]", at, i), vm, false)
		if !ok {
			continue
		}
		if !firstOK && i == 0 {
			first, firstOK = t, true
		} else if firstOK && (t.CPU != first.CPU || t.Memory != first.Memory) {
			c.add(fmt.Sprintf("%s.vm[%d]", at, i), "%s isn't the same shape as %s: sizing assumes every type in a pool has the first one's CPU and memory", vm, p.VM[0])
		}
	}
	if p.Min < minMin {
		c.add(at+".min", "must be at least %d", minMin)
	}
	if p.Max < 1 {
		c.add(at+".max", "must be at least 1")
	} else if p.Max < p.Min {
		c.add(at+".max", "must be at least min, %d", p.Min)
	}
	if p.Spare < 0 {
		c.add(at+".spare", "negative")
	}
	if p.Target <= 0 || p.Target > 1 {
		c.add(at+".target", "must be between 1%% and 100%%")
	}
	c.reserve(at+".reserved", p.Reserved, first, firstOK)
}

func (c *checker) queues() {
	for _, name := range c.s.QueueNames() {
		q := c.s.Queues[name]
		at := "queues." + name
		switch {
		case !dnsLabel.MatchString(name):
			c.add(at, "%q must be lowercase letters, digits and dashes", name)
		case len(name) > maxQueueName:
			c.add(at, "must be at most %d characters", maxQueueName)
		case bucketUses[name]:
			c.add(at, "%s is reserved: in uses it names ProtoStrap's %s bucket", name, name)
		case name == emailUse:
			c.add(at, "email is reserved: in uses it names the email section")
		}
		if v := q.Visibility.Duration; v < time.Second || v > 12*time.Hour {
			c.add(at+".visibility", "must be between 1s and 12h")
		}
		if r := q.Retention.Duration; r < time.Minute || r > 14*24*time.Hour {
			c.add(at+".retention", "must be between 1m and 14d")
		}
		if q.MaxReceives < 1 || q.MaxReceives > 1000 {
			c.add(at+".max_receives", "must be 1 to 1000")
		}
	}
}

func (c *checker) buckets() {
	for _, name := range c.s.BucketNames() {
		b := c.s.Buckets[name]
		at := "buckets." + name
		_, queue := c.s.Queues[name]
		switch {
		case !dnsLabel.MatchString(name):
			c.add(at, "%q must be lowercase letters, digits and dashes", name)
		case len(name) > maxBucketName:
			c.add(at, "must be at most %d characters", maxBucketName)
		case strings.Contains(name, "--") || strings.HasSuffix(name, "-s3alias"):
			c.add(at, "%q can't contain -- or end in -s3alias: S3 refuses such names", name)
		case reservedBuckets[name]:
			c.add(at, "%s is reserved: ProtoStrap's own buckets end in files, backups, deploy, tfstate and telemetry", name)
		case name == emailUse:
			c.add(at, "email is reserved: in uses it names the email section")
		case queue:
			c.add(at, "uses names buckets and queues alike, so a bucket can't share its name with queue %s", name)
		}
		// S3's lifecycle rules count in days.
		days := func(field string, d time.Duration) {
			if c.s.has(at+"."+field) && (d < day || d%day != 0) {
				c.add(at+"."+field, "must be a whole number of days, at least 1d")
			}
		}
		days("versions", b.Versions.Duration)
		days("expire", b.Expire.Duration)
		if b.Lock && b.Versions.Duration == 0 {
			c.add(at+".lock", "needs versions: S3 locks only a versioned bucket")
		}
		if b.Browser && len(c.s.Hosts()) == 0 {
			c.add(at+".browser", "browsers reach the application on the gateway's hosts, and its CORS rule allows their origins: add gateway.hosts")
		}
	}
}

// insideRepo reports whether a path, or a git pathspec such as ":(glob)services/**/Protos/**",
// is relative and stays inside the repository.
func insideRepo(p string) bool {
	switch {
	case strings.HasPrefix(p, ":("):
		if end := strings.Index(p, ")"); end > 0 {
			p = p[end+1:]
		}
	case strings.HasPrefix(p, ":!"), strings.HasPrefix(p, ":^"):
		p = p[2:]
	}
	return p != "" && !path.IsAbs(p) && !strings.HasPrefix(path.Clean(p), "..")
}

// projectFile reports whether a path names a .NET project, which the .NET SDK can publish as an
// image.
func projectFile(p string) bool {
	switch path.Ext(p) {
	case ".csproj", ".fsproj", ".vbproj":
		return true
	}
	return false
}

func (c *checker) components() {
	s := c.s
	if len(s.Components) == 0 {
		c.add("components", "required: an application has at least one component")
	}
	exportsMetrics := false
	for _, comp := range s.Components {
		if comp.Metrics != "" {
			exportsMetrics = true
		}
	}
	stateful := map[string][]*Component{}
	for _, comp := range s.Sorted() {
		c.component(comp, exportsMetrics)
		if comp.Kind == api.KindStateful && comp.VM != "" {
			stateful[comp.VM] = append(stateful[comp.VM], comp)
		}
	}
	// The stateful components pinned to a data VM must fit it together, and on data-1 the
	// telemetry replica must fit beside them. Telemetry is blamed only when they fit alone.
	if data, err := LookupVM(s.Pools.Data.VM); err == nil {
		r := room(data, s.Pools.Data.Reserved)
		vms := sortedKeys(stateful)
		if !slices.Contains(vms, "data-1") {
			vms = append([]string{"data-1"}, vms...)
		}
		for _, vm := range vms {
			var total Resources
			fit := true
			for _, comp := range stateful[vm] {
				total.CPU += comp.Request.CPU
				total.Memory += comp.Request.Memory
				if !fits(total, r) {
					c.add("components."+comp.Name+".request", "the stateful components on %s don't fit it together: they request %s and %s, and %s has %s and %s after its reserve",
						vm, FormatCPU(int64(total.CPU)), FormatMemory(int64(total.Memory)), s.Pools.Data.VM, FormatCPU(int64(r.CPU)), FormatMemory(int64(r.Memory)))
					fit = false
					break
				}
			}
			if s.Telemetry == nil {
				continue
			}
			if t := s.Telemetry.Request; vm == "data-1" && fit && t.CPU > 0 && t.Memory > 0 {
				total.CPU += t.CPU
				total.Memory += t.Memory
				if !fits(total, r) {
					c.add("telemetry.request", "telemetry doesn't fit data-1 beside the stateful components there: together they request %s and %s, and %s has %s and %s after its reserve",
						FormatCPU(int64(total.CPU)), FormatMemory(int64(total.Memory)), s.Pools.Data.VM, FormatCPU(int64(r.CPU)), FormatMemory(int64(r.Memory)))
				}
			}
		}
	}
	c.cycles()
}

func (c *checker) component(comp *Component, exportsMetrics bool) {
	s := c.s
	at := "components." + comp.Name
	f := func(field string) string { return at + "." + field }
	switch {
	case !dnsLabel.MatchString(comp.Name):
		c.add(at, "%q must be lowercase letters, digits and dashes", comp.Name)
	case len(comp.Name) > maxComponentName:
		c.add(at, "must be at most %d characters", maxComponentName)
	case reservedComponents[comp.Name]:
		c.add(at, "%q is reserved for ProtoStrap", comp.Name)
	}

	kind := comp.Kind
	switch kind {
	case api.KindService, api.KindWorker, api.KindStateful, api.KindTask:
	case "":
		c.add(f("kind"), "required: service, worker, stateful or task")
		return
	default:
		c.add(f("kind"), "%q must be service, worker, stateful or task", kind)
		return
	}
	built := kind != api.KindStateful
	scaled := kind == api.KindService || kind == api.KindWorker

	// Check what the component runs.
	if built {
		switch {
		case comp.Build == "" && comp.Project == "":
			c.add(f("build"), "required: the directory with the component's Dockerfile, or project, the .NET project it is published from")
		case comp.Build != "" && comp.Project != "":
			c.add(f("project"), "a component is built one way: set one of build or project")
		case comp.Project != "" && (!insideRepo(comp.Project) || !projectFile(comp.Project)):
			c.add(f("project"), "%q must be a .csproj, .fsproj or .vbproj inside the repository", comp.Project)
		case comp.Build != "" && !insideRepo(comp.Build):
			c.add(f("build"), "must be a relative path inside the repository")
		}
		if comp.Project != "" && comp.Context != comp.Build {
			c.add(f("context"), "context goes with build: a project is published from its own directory")
		} else if comp.Context != comp.Build && !insideRepo(comp.Context) {
			c.add(f("context"), "must be a relative path inside the repository")
		}
		for i, src := range comp.Sources {
			if !insideRepo(src) {
				c.add(fmt.Sprintf("%s.sources[%d]", at, i), "%q must be a relative path inside the repository", src)
			}
		}
		if comp.Image != "" {
			c.add(f("image"), "services, workers and tasks are built from their build directory or project, not run from an upstream image")
		}
	} else {
		if comp.Build != "" {
			c.add(f("build"), "a stateful component runs an upstream image, pinned to a tag")
		}
		if comp.Project != "" {
			c.add(f("project"), "a stateful component runs an upstream image, pinned to a tag")
		}
		switch {
		case comp.Image == "":
			c.add(f("image"), "required: the upstream image, pinned to a tag")
		case !pinned(comp.Image):
			c.add(f("image"), "%q must be pinned to a tag other than latest, or a digest", comp.Image)
		}
	}

	// Check where the component runs.
	if built {
		switch comp.Pool {
		case "":
			c.add(f("pool"), "required: general or compute")
		case "general":
		case "compute":
			if s.Pools.Compute == nil {
				c.add(f("pool"), "the spec has no compute pool")
			}
		default:
			c.add(f("pool"), "%q must be general or compute", comp.Pool)
		}
		if comp.VM != "" {
			c.add(f("vm"), "only stateful components are pinned to a data VM")
		}
	} else {
		if comp.Pool != "" {
			c.add(f("pool"), "a stateful component is pinned to a data VM with vm, not placed in a pool")
		}
		if comp.VM == "" {
			c.add(f("vm"), "required: the data VM it is pinned to, such as data-1")
		} else if m := dataVM.FindStringSubmatch(comp.VM); m == nil || !slices.Contains(s.DataVMs(), comp.VM) {
			c.add(f("vm"), "there is no %s: the data pool has data-1 to data-%d", comp.VM, s.Pools.Data.Count)
		}
	}

	// Check what the component reserves.
	r := comp.Request
	if !s.has(f("request")) || r.CPU <= 0 || r.Memory <= 0 {
		c.add(f("request"), "required: the CPU and memory one replica reserves")
	} else if pool := s.Pool(comp.Pool); built && pool != nil && len(pool.VM) > 0 {
		if t, err := LookupVM(pool.VM[0]); err == nil && !fits(r, room(t, pool.Reserved)) {
			free := room(t, pool.Reserved)
			c.add(f("request"), "%s and %s doesn't fit one %s VM, which has %s and %s after its reserve",
				FormatCPU(int64(r.CPU)), FormatMemory(int64(r.Memory)), pool.VM[0], FormatCPU(int64(free.CPU)), FormatMemory(int64(free.Memory)))
		}
	}

	// Check how the component is reached.
	takesRequests := kind == api.KindService || kind == api.KindStateful
	switch {
	case takesRequests && comp.Port == 0:
		c.add(f("port"), "required: the port it listens on")
	case !takesRequests && comp.Port != 0:
		c.add(f("port"), "a %s takes no requests", kind)
	case comp.Port < 0 || comp.Port > 65535:
		c.add(f("port"), "must be 1 to 65535")
	}
	c.readiness(comp)
	if comp.Metrics != "" {
		if _, _, err := metrics.ParseEndpoint(comp.Metrics); err != nil {
			c.add(f("metrics"), "%q must be port/path, such as 9090/metrics", comp.Metrics)
		}
	}

	// Check what the component needs.
	for i, dep := range comp.DependsOn {
		da := fmt.Sprintf("%s.depends_on[%d]", at, i)
		other, ok := s.Components[dep]
		switch {
		case dep == comp.Name:
			c.add(da, "a component can't depend on itself")
		case !ok:
			c.add(da, "no component %s", dep)
		case other.Kind == api.KindTask:
			c.add(da, "%s is a task, which never becomes ready: run it with migrate instead", dep)
		}
	}
	for i, use := range comp.Uses {
		_, queue := s.Queues[use]
		_, bucket := s.Buckets[use]
		if use == emailUse {
			if s.Email == nil {
				c.add(fmt.Sprintf("%s.uses[%d]", at, i), "the spec has no email section: add one with the domains to send mail from")
			}
			continue
		}
		if !queue && !bucket && !bucketUses[use] {
			c.add(fmt.Sprintf("%s.uses[%d]", at, i), "no queue or bucket %s: uses names queues, buckets, files, backups or email", use)
		}
	}
	for _, key := range sortedKeys(comp.Env) {
		ea := f("env." + key)
		switch {
		case !envName.MatchString(key):
			c.add(ea, "%q isn't a variable name", key)
		case setByProtoStrap(key), s.hostVariable(key):
			c.add(ea, "ProtoStrap sets %s in every replica", key)
		}
	}
	for i, key := range comp.Secrets {
		sa := fmt.Sprintf("%s.secrets[%d]", at, i)
		if !envName.MatchString(key) {
			c.add(sa, "%q isn't a variable name", key)
		}
		if _, ok := comp.Env[key]; ok {
			c.add(sa, "%s is also in env: secrets never go there", key)
		}
	}
	c.secrets(comp)
	c.config(comp)
	if m := comp.Migrate; m != nil {
		task, ok := s.Components[m.Task]
		switch {
		case kind != api.KindService && kind != api.KindWorker:
			c.add(f("migrate"), "only services and workers run migrations")
		case m.Task == "" && len(m.Command) == 0:
			c.add(f("migrate"), "want a task's name or a command")
		case m.Task != "" && !ok:
			c.add(f("migrate"), "no task %s", m.Task)
		case m.Task != "" && task.Kind != api.KindTask:
			c.add(f("migrate"), "%s is a %s, not a task", m.Task, task.Kind)
		}
	}
	if comp.Stop.Duration > maxStop {
		c.add(f("stop"), "must be at most 1h")
	}

	// Check the component's state: its volume and backups.
	if kind == api.KindStateful {
		switch v := comp.Volume; {
		case v == nil:
			c.add(f("volume"), "required: the path its data lives at and its size")
		case !path.IsAbs(v.Path):
			c.add(f("volume.path"), "must be an absolute path in the container")
		case v.Size < 1:
			c.add(f("volume.size"), "required")
		}
	} else {
		if comp.Volume != nil {
			c.add(f("volume"), "only stateful components have a volume")
		}
		if len(comp.Backup) > 0 {
			c.add(f("backup"), "only stateful components have backups")
		}
	}

	// Check how the component scales.
	if !scaled {
		if s.has(f("scale")) {
			c.add(f("scale"), "only services and workers scale")
		}
		return
	}
	c.scale(comp, exportsMetrics)
}

// config checks the component's config files, how it takes new ones, and its configure
// task.
func (c *checker) config(comp *Component) {
	s := c.s
	f := func(field string) string { return "components." + comp.Name + "." + field }
	if cf := comp.Config; cf != nil {
		switch {
		case cf.From == "":
			c.add(f("config.from"), "required: the directory in the repository the files come from")
		case strings.HasPrefix(cf.From, ":") || path.Clean(cf.From) == ".":
			c.add(f("config.from"), "%q must be a directory of the repository, not a pathspec or the whole repository", cf.From)
		case !insideRepo(cf.From):
			c.add(f("config.from"), "%q must be a relative path inside the repository", cf.From)
		}
		switch p := cf.Path; {
		case p == "":
			c.add(f("config.path"), "required: the directory in the container the files are mounted at")
		case !path.IsAbs(p):
			c.add(f("config.path"), "%q must be an absolute path in the container", p)
		case path.Clean(p) == "/":
			c.add(f("config.path"), "can't be the root of the container")
		case path.Clean(p) != p:
			c.add(f("config.path"), "write it as %s", path.Clean(p))
		case within(p, secretsMount) || within(secretsMount, p):
			c.add(f("config.path"), "%s would cover or sit in the secret files at %s", p, secretsMount)
		case comp.Volume != nil && path.IsAbs(comp.Volume.Path) && (within(p, comp.Volume.Path) || within(comp.Volume.Path, p)):
			c.add(f("config.path"), "%s would cover or sit in the volume at %s", p, comp.Volume.Path)
		}
	}
	if cc := comp.OnConfigChange; cc != nil {
		switch {
		case comp.Config == nil:
			c.add(f("on_config_change"), "a change needs config files to change: set config")
		case comp.Kind == api.KindTask:
			c.add(f("on_config_change"), "a task reads its config each time it runs: there is nothing to change")
		case len(cc.Command) == 0 && cc.Mode != SecretRestart && cc.Mode != SecretReload:
			c.add(f("on_config_change"), "want restart, reload or a command")
		}
	}
	if comp.Configure == "" {
		return
	}
	task, ok := s.Components[comp.Configure]
	switch {
	case comp.Kind != api.KindService && comp.Kind != api.KindWorker && comp.Kind != api.KindStateful:
		c.add(f("configure"), "only services, workers and stateful components are configured")
	case !ok:
		c.add(f("configure"), "no task %s", comp.Configure)
	case task.Kind != api.KindTask:
		c.add(f("configure"), "%s is a %s, not a task", comp.Configure, task.Kind)
	case s.ConfiguredBy(comp.Configure) != comp:
		c.add(f("configure"), "%s is already %s's configure task: each component has its own", comp.Configure, s.ConfiguredBy(comp.Configure).Name)
	}
	for _, other := range s.Sorted() {
		if m := other.Migrate; m != nil && m.Task == comp.Configure {
			c.add("components."+other.Name+".migrate", "%s is %s's configure task, which runs after %s is ready, never before another component starts", m.Task, comp.Name, comp.Name)
		}
	}
}

// headerName is an HTTP header's name as the edge passes it, in lower case.
var headerName = regexp.MustCompile(`^[a-z0-9!#$%&'*+.^_|~-]+$`)

// edgeHeaders are the request headers the edge sets itself, which an authorizer can't.
var edgeHeaders = map[string]bool{"host": true, "content-length": true, "transfer-encoding": true, "connection": true,
	"te": true, "upgrade": true, "x-forwarded-for": true, "x-forwarded-proto": true, "x-request-id": true, "x-envoy-original-path": true}

// edgeResponseHeaders are the response headers the edge sets itself on each connection to
// the client, which an authorizer can't add to the service's response: its content-length is
// its own body's, and HTTP/2 refuses a message with any of the rest (RFC 9113, 8.2.2).
var edgeResponseHeaders = map[string]bool{"content-length": true, "transfer-encoding": true, "connection": true,
	"keep-alive": true, "proxy-connection": true, "te": true, "upgrade": true}

// skipExtension is a file extension whose requests the edge sends on without asking the
// authorizer, such as *.js.
var skipExtension = regexp.MustCompile(`^\*\.[a-z0-9]{1,10}$`)

const (
	// maxAuthorizeTimeout keeps the edge's own 504 ahead of Cloudflare's: a route's 120s
	// starts once the authorizer answers, and Cloudflare gives up at 125s.
	maxAuthorizeTimeout = 5 * time.Second
	// maxSkipExtensions keeps the pattern of a route's skipped extensions, about 300 in RE2's
	// program size at 30 of 10 characters, within the 1000 the edge's Envoy compiles.
	maxSkipExtensions = 30
)

// authorize checks the service that authorizes an upstream's requests at the gateway.
func (c *checker) authorize(u *Upstream) {
	a := u.Authorize
	if a == nil {
		return
	}
	f := func(field string) string { return "gateway.upstreams." + u.Name + ".authorize" + field }
	switch by, ok := c.s.Components[a.By]; {
	case a.By == "":
		c.add(f(".by"), "required: the service the edge asks")
	case a.By == u.Name:
		c.add(f(".by"), "a service can't authorize its own requests: the edge would ask it about every request it asks")
	case !ok:
		c.add(f(".by"), "no component %s", a.By)
	case by.Kind != api.KindService:
		kind := string(by.Kind)
		if by.Kind == api.KindStateful {
			kind = "stateful component"
		}
		c.add(f(".by"), "%s is a %s: only a service answers the edge", a.By, kind)
	}
	if a.Path != "" && !strings.HasPrefix(a.Path, "/") {
		c.add(f(".path"), "%q must start with /", a.Path)
	}
	for _, list := range []struct {
		field string
		names []string
		edge  map[string]bool
	}{{"headers", a.Headers, edgeHeaders}, {"forward", a.Forward, edgeHeaders}, {"respond", a.Respond, edgeResponseHeaders}} {
		field := list.field
		for i, name := range list.names {
			at := f(fmt.Sprintf(".%s[%d]", field, i))
			switch {
			case !headerName.MatchString(name):
				c.add(at, "%q isn't a header name: lower case, such as x-user", name)
			case list.edge[name]:
				c.add(at, "the edge sets %s itself", name)
			}
		}
	}
	switch t := a.Timeout.Duration; {
	case c.s.has(f(".timeout")) && t <= 0:
		c.add(f(".timeout"), "must be above 0")
	case t > maxAuthorizeTimeout:
		c.add(f(".timeout"), "must be at most 5s: the edge gives the service 120s once the authorizer answers, and Cloudflare gives up at 125s")
	}
	extensions := 0
	for i, skip := range a.Skip {
		at := f(fmt.Sprintf(".skip[%d]", i))
		if strings.HasPrefix(skip, "*.") {
			if !skipExtension.MatchString(skip) {
				c.add(at, "%q isn't a file extension: up to 10 lower-case letters and digits after *., such as *.js", skip)
			}
			extensions++
			continue
		}
		switch clean := strings.TrimSuffix(path.Clean(skip), "/") + "/"; {
		case !strings.HasPrefix(skip, "/") || !strings.HasSuffix(skip, "/"):
			c.add(at, "%q isn't a path prefix that starts and ends with /, such as /assets/, or a file extension, such as *.js", skip)
		case clean != skip:
			c.add(at, "write it as %s", clean)
		case len(u.Routes) > 0 && !slices.ContainsFunc(u.Routes, func(r Route) bool { return within(skip, r.Path) }):
			c.add(at, "%s isn't under any of the service's routes", skip)
		}
	}
	if extensions > maxSkipExtensions {
		c.add(f(".skip"), "at most %d file extensions", maxSkipExtensions)
	}
}

// secretsMount is where a replica's secret files are mounted.
const secretsMount = "/run/secrets"

// within reports whether p is dir or inside it.
// readiness checks a component's ready and protocol (docs/protostrap.md#readiness-checks).
func (c *checker) readiness(comp *Component) {
	f := func(field string) string { return "components." + comp.Name + "." + field }
	isCommand := comp.ReadyCommand != nil
	switch {
	case comp.Kind == api.KindTask:
		if comp.Ready != "" || isCommand {
			c.add(f("ready"), "a task runs to completion: it has no readiness check")
		}
	case comp.Kind == api.KindService && comp.Ready == "" && len(comp.ReadyCommand) == 0:
		c.add(f("ready"), "required: grpc, tcp, an HTTP path such as /healthz, or a command")
	case isCommand && len(comp.ReadyCommand) == 0:
		c.add(f("ready"), "required: the command to run, as a list")
	case isCommand && comp.Kind == api.KindService && c.s.Upstream(comp.Name) != nil:
		c.add(f("ready"), "the gateway checks its upstreams too, and can't run a command: use grpc, tcp or an HTTP path")
	case comp.Ready != "":
		check, err := probe.ParseCheck(comp.Ready)
		switch {
		case err != nil:
			c.add(f("ready"), "%v", err)
		case check.Port == 0 && comp.Port == 0:
			example := probe.Check{Kind: check.Kind, Port: 8081, Path: check.Path}
			c.add(f("ready"), "a %s has no port, so its check names a port, such as %s", comp.Kind, example)
		case comp.Kind == api.KindService && c.s.Upstream(comp.Name) != nil && check.Kind == probe.CheckGRPC &&
			c.s.UpstreamProtocol(comp) == ProtocolHTTP1:
			c.add(f("ready"), "the gateway checks gRPC over its HTTP/2 connections to the service: set gateway.upstreams.%s.protocol to http2 or grpc", comp.Name)
		}
	}
}

func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// secrets checks the component's prefix in the environment's secret, and how it takes a
// change to its keys.
func (c *checker) secrets(comp *Component) {
	f := func(field string) string { return "components." + comp.Name + "." + field }
	switch prefix := comp.Prefix(); {
	case comp.SecretPrefix != "" && !secret.ValidPrefix(prefix):
		c.add(f("secret_prefix"), "%q must be a capital letter, then letters and digits, then _, such as Api_", prefix)
	case !secret.ValidPrefix(prefix):
		c.add(f("secret_prefix"), "the name gives no prefix for its keys: set secret_prefix, such as Api_")
	case secret.Reserved(prefix) && comp.SecretPrefix != "":
		c.add(f("secret_prefix"), "%s is reserved: Shared_ keys are every component's, and Edge_ and Reporter_ are ProtoStrap's", prefix)
	case secret.Reserved(prefix):
		c.add(f("secret_prefix"), "%s, from the name, is reserved: set secret_prefix", prefix)
	}
	if comp.SecretFiles && len(comp.Secrets) == 0 {
		c.add(f("secret_files"), "the agent writes the keys in secrets, and there are none")
	}
	sc := comp.OnSecretChange
	if sc == nil {
		return
	}
	switch {
	case len(comp.Secrets) == 0:
		c.add(f("on_secret_change"), "a change needs keys to change: list them in secrets")
	case comp.Kind == api.KindTask:
		c.add(f("on_secret_change"), "a task reads its keys each time it runs: there is nothing to change")
	case len(sc.Command) == 0 && sc.Mode != SecretRestart && sc.Mode != SecretReload:
		c.add(f("on_secret_change"), "want restart, reload or a command")
	case comp.Follows() && !comp.SecretFiles:
		what := "a command"
		if sc.Mode == SecretReload {
			what = "reload"
		}
		c.add(f("on_secret_change"), "%s acts on the component's secret files: set secret_files", what)
	}
}

func (c *checker) scale(comp *Component, exportsMetrics bool) {
	sc := comp.Scale
	at := "components." + comp.Name + ".scale"
	if sc.Min < 0 {
		c.add(at+".min", "negative")
	}
	if sc.Max < 1 {
		c.add(at+".max", "must be at least 1")
	} else if sc.Max < sc.Min {
		c.add(at+".max", "must be at least min, %d", sc.Min)
	}
	for i, d := range sc.Demand {
		da := fmt.Sprintf("%s.demand[%d]", at, i)
		switch d.Kind() {
		case SignalCPU:
			if d.CPU <= 0 || d.CPU > 1 {
				c.add(da+".cpu", "must be between 1%% and 100%% of the request")
			}
		case SignalRequests:
			if d.Requests <= 0 {
				c.add(da+".requests", "must be above 0")
			}
			if c.s.Upstream(comp.Name) == nil {
				c.add(da, "requests are counted by the gateway, so a requests signal is for one of gateway.upstreams")
			}
		case SignalQueue:
			if _, ok := c.s.Queues[d.Queue]; !ok {
				c.add(da+".queue", "no queue %s", d.Queue)
			}
			if d.Rate <= 0 {
				c.add(da+".rate", "required: the jobs per second one replica handles")
			}
			if d.Drain.Duration <= 0 {
				c.add(da+".drain", "required: how soon the queue should be cleared")
			}
		case SignalMetric:
			if !metricName.MatchString(d.Metric) {
				c.add(da+".metric", "%q isn't a metric name", d.Metric)
			}
			if d.Target <= 0 {
				c.add(da+".target", "must be above 0: the value one replica should carry")
			}
			if comp.Metrics == "" {
				c.add(da, "a metric signal needs the component's metrics endpoint")
			}
		}
	}
	for i, l := range sc.Limits {
		la := fmt.Sprintf("%s.limits[%d]", at, i)
		switch {
		case (l.Replicas != nil) == (l.Metric != ""):
			c.add(la, "a limit has one of replicas or metric")
		case l.Replicas != nil && *l.Replicas < 0:
			c.add(la+".replicas", "negative")
		case l.Metric != "" && !metricName.MatchString(l.Metric):
			c.add(la+".metric", "%q isn't a metric name", l.Metric)
		case l.Metric != "" && !exportsMetrics:
			c.add(la, "no component exports metrics, so nothing can export %s", l.Metric)
		}
	}
}

// pinned reports whether an image names a tag other than latest, or a digest.
func pinned(image string) bool {
	if strings.Contains(image, "@sha256:") {
		return true
	}
	name := image[strings.LastIndex(image, "/")+1:]
	_, tag, ok := strings.Cut(name, ":")
	return ok && tag != "" && tag != "latest"
}

// setByProtoStrap reports whether every replica gets the variable from ProtoStrap
// (docs/protostrap.md#application-contract).
func setByProtoStrap(key string) bool {
	switch {
	case strings.HasPrefix(key, "PROTOSTRAP_"), strings.HasPrefix(key, "AWS_"):
		return true
	case key == "SECRET_ID", key == "SECRET_PREFIX", key == "FILES_BUCKET", key == "BACKUPS_BUCKET":
		return true
	case strings.HasPrefix(key, "QUEUE_") && strings.HasSuffix(key, "_URL"):
		return true
	case strings.HasPrefix(key, "BUCKET_"):
		return true
	}
	return false
}

// hostVariable reports whether key is the HOST_ or ORIGIN_ variable of one of the hosts.
func (s *Spec) hostVariable(key string) bool {
	for name := range s.Hosts() {
		if key == HostEnv(name) || key == OriginEnv(name) {
			return true
		}
	}
	return false
}

// example is the name of a host, for a message: the first, or "app" without one.
func example(h Hosts) string {
	if names := h.Names(); len(names) > 0 {
		return names[0]
	}
	return "app"
}

// cycles reports components whose depends_on and migrate form a cycle, which would leave
// every component in it waiting for another.
func (c *checker) cycles() {
	s := c.s
	type edge struct{ to, field string }
	edges := map[string][]edge{}
	for _, comp := range s.Sorted() {
		for _, dep := range comp.DependsOn {
			if _, ok := s.Components[dep]; ok && dep != comp.Name {
				edges[comp.Name] = append(edges[comp.Name], edge{dep, "depends_on"})
			}
		}
		if m := comp.Migrate; m != nil && m.Task != "" && m.Task != comp.Name {
			if _, ok := s.Components[m.Task]; ok {
				edges[comp.Name] = append(edges[comp.Name], edge{m.Task, "migrate"})
			}
		}
		// A component waits for what it depends on to be configured, so for that one's
		// configure task.
		for _, dep := range comp.DependsOn {
			if d, ok := s.Components[dep]; ok && d.Configure != "" && d.Configure != comp.Name {
				if _, ok := s.Components[d.Configure]; ok {
					edges[comp.Name] = append(edges[comp.Name], edge{d.Configure, "depends_on"})
				}
			}
		}
	}
	// Tarjan's algorithm finds the strongly connected components.
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var sccs [][]string
	var visit func(v string)
	visit = func(v string) {
		index[v] = len(index)
		low[v] = index[v]
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range edges[v] {
			if _, seen := index[e.to]; !seen {
				visit(e.to)
				low[v] = min(low[v], low[e.to])
			} else if onStack[e.to] {
				low[v] = min(low[v], index[e.to])
			}
		}
		if low[v] == index[v] {
			var scc []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				scc = append(scc, w)
				if w == v {
					break
				}
			}
			sccs = append(sccs, scc)
		}
	}
	for _, name := range sortedKeys(s.Components) {
		if _, seen := index[name]; !seen {
			visit(name)
		}
	}
	for _, scc := range sccs {
		if len(scc) < 2 {
			continue
		}
		slices.Sort(scc)
		members := strings.Join(scc, ", ")
		for _, name := range scc {
			reported := map[string]bool{}
			for _, e := range edges[name] {
				if slices.Contains(scc, e.to) && !reported[e.field] {
					reported[e.field] = true
					c.add("components."+name+"."+e.field, "forms a cycle between %s: each would wait for another forever", members)
				}
			}
		}
	}
}
