package spec

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// docsExample is the spec in docs/protostrap.md, the reference every part of ProtoStrap
// reads.
const docsExample = `app: shop
protostrap: 1.4.0
hosts:
  shop: shop.example.com

pools:
  ingress: { vm: c8g.medium }
  general:
    vm: [m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]
    min: 1
    max: 10
    spare: 1
    target: 70%
    reserved: { cpu: 400m, memory: 1Gi }
  compute:
    vm: [c7g.xlarge, c6g.xlarge, c8g.xlarge, c7gd.xlarge]
    min: 0
    max: 20
    spare: 0
    target: 85%
    reserved: { cpu: 400m, memory: 1Gi }
  data: { vm: m7g.xlarge, count: 1 }

edge:
  request: { cpu: 500m, memory: 256Mi }

queues:
  orders: {}

components:
  api:
    kind: service
    build: services/api
    sources: [services/shared]
    pool: general
    request: { cpu: 500m, memory: 512Mi }
    port: 8080
    ready: grpc
    routes: [shop/api/]
    metrics: 9090/metrics
    depends_on: [postgres, redis]
    uses: [orders]
    migrate: [dotnet, Api.dll, migrate]
    stop: 30s
    scale:
      min: 2
      max: 10
      demand: [{ cpu: 70% }]

  orders-worker:
    kind: worker
    build: services/orders-worker
    pool: compute
    request: { cpu: 1000m, memory: 1Gi }
    depends_on: [postgres]
    stop: 120s
    scale:
      min: 0
      max: 20
      demand: [{ queue: orders, rate: 2/s, drain: 60s }]
      limits: [{ metric: llm_budget_cap }]

  postgres:
    kind: stateful
    image: postgres:17.2
    vm: data-1
    request: { cpu: 2500m, memory: 9Gi }
    port: 5432
    volume: { path: /var/lib/postgresql/data, size: 100Gi }
    backup: [pg_dumpall, -U, postgres]

  redis:
    kind: stateful
    image: redis:7.4
    vm: data-1
    request: { cpu: 1000m, memory: 4Gi }
    port: 6379
    volume: { path: /data, size: 20Gi }
    backup: [redis-cli, --rdb, "-"]
`

func mustParse(t *testing.T, src string) *Spec {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func TestParseDocsExample(t *testing.T) {
	s := mustParse(t, docsExample)
	if s.App != "shop" || s.ProtoStrap != "1.4.0" || len(s.Hosts) != 1 {
		t.Errorf("top level: %q %q %v", s.App, s.ProtoStrap, s.Hosts)
	}
	g := s.Pools.General
	if len(g.VM) != 4 || g.VM[0] != "m7g.xlarge" || g.Min != 1 || g.Max != 10 || g.Spare != 1 || g.Target != 0.70 {
		t.Errorf("general pool: %+v", g)
	}
	if g.Reserved != (Resources{CPU: 400, Memory: 1024}) {
		t.Errorf("general reserved: %+v", g.Reserved)
	}
	if c := s.Pools.Compute; c == nil || c.Target != 0.85 || c.Min != 0 || c.Max != 20 {
		t.Errorf("compute pool: %+v", c)
	}
	if s.Pools.Data.VM != "m7g.xlarge" || s.Pools.Data.Count != 1 {
		t.Errorf("data pool: %+v", s.Pools.Data)
	}
	if s.Edge.Request != (Resources{CPU: 500, Memory: 256}) {
		t.Errorf("edge: %+v", s.Edge)
	}

	api := s.Components["api"]
	if api.Name != "api" || api.Kind != "service" || api.Request != (Resources{CPU: 500, Memory: 512}) {
		t.Errorf("api: %+v", api)
	}
	if len(s.Hosts) != 1 || s.Hosts["shop"] != "shop.example.com" {
		t.Errorf("hosts = %v", s.Hosts)
	}
	if len(api.Routes) != 1 || api.Routes[0] != (Route{Host: "shop", Path: "/api/"}) {
		t.Errorf("api routes: %+v", api.Routes)
	}
	if api.Stop.Duration != 30*time.Second || api.Migrate == nil || strings.Join(api.Migrate.Command, " ") != "dotnet Api.dll migrate" {
		t.Errorf("api stop %v, migrate %+v", api.Stop, api.Migrate)
	}
	if api.Scale.Min != 2 || api.Scale.Max != 10 || len(api.Scale.Demand) != 1 || api.Scale.Demand[0].Kind() != SignalCPU || api.Scale.Demand[0].CPU != 0.70 {
		t.Errorf("api scale: %+v", api.Scale)
	}

	w := s.Components["orders-worker"]
	d := w.Scale.Demand[0]
	if d.Kind() != SignalQueue || d.Queue != "orders" || d.Rate != 2 || d.Drain.Duration != time.Minute {
		t.Errorf("worker demand: %+v", d)
	}
	if l := w.Scale.Limits[0]; l.Metric != "llm_budget_cap" || l.Replicas != nil {
		t.Errorf("worker limit: %+v", l)
	}

	pg := s.Components["postgres"]
	if pg.Volume.Path != "/var/lib/postgresql/data" || pg.Volume.Size != 100 || strings.Join(pg.Backup, " ") != "pg_dumpall -U postgres" {
		t.Errorf("postgres: %+v %+v", pg.Volume, pg.Backup)
	}
	if got := s.Components["redis"].Backup[2]; got != "-" {
		t.Errorf("redis backup's last argument %q", got)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("the docs example fails its own check:\n%v", Problems(problems))
	}
}

func TestTheSampleSpecChecks(t *testing.T) {
	src, err := os.ReadFile("../../sample/protostrap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := mustParse(t, string(src))
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("sample/protostrap.yaml fails its check:\n%v", Problems(problems))
	}
	kinds := map[api.Kind]bool{}
	for _, c := range s.Components {
		kinds[c.Kind] = true
	}
	for _, k := range []api.Kind{api.KindService, api.KindWorker, api.KindStateful, api.KindTask} {
		if !kinds[k] {
			t.Errorf("the sample has no %s", k)
		}
	}
}

func TestDefaults(t *testing.T) {
	s := mustParse(t, `app: tiny
protostrap: 0.1.0
pools:
  ingress: { vm: c8g.medium }
  general: { vm: [m7g.large], max: 3 }
  data: { vm: m7g.large }
queues:
  q: {}
buckets:
  b: {}
components:
  web:
    kind: service
    build: web
    pool: general
    request: { cpu: 100m, memory: 128Mi }
    port: 80
    ready: /healthz
  w:
    kind: worker
    build: w
    pool: general
    request: { cpu: 100m, memory: 128Mi }
    scale: { max: 4, demand: [{ queue: q, rate: 1/s, drain: 1m }] }
`)
	g := s.Pools.General
	if g.Min != 1 || g.Spare != 1 || g.Target != 0.70 || g.Reserved != (Resources{CPU: 400, Memory: 1024}) {
		t.Errorf("general defaults: %+v", g)
	}
	if s.Pools.Compute != nil {
		t.Errorf("no compute pool unless the spec has one, got %+v", s.Pools.Compute)
	}
	// Stateful components get cores of their own on the data VM, so its reserve is memory.
	if s.Pools.Data.Count != 1 || s.Pools.Data.Reserved != (Resources{CPU: 0, Memory: 1024}) {
		t.Errorf("data defaults: %+v", s.Pools.Data)
	}
	if s.Pools.Ingress.Reserved != (Resources{CPU: 200, Memory: 512}) {
		t.Errorf("ingress reserve: %+v", s.Pools.Ingress.Reserved)
	}
	if s.Edge.Request != (Resources{CPU: 500, Memory: 256}) {
		t.Errorf("edge default: %+v", s.Edge.Request)
	}
	web := s.Components["web"]
	if web.Stop.Duration != 10*time.Second {
		t.Errorf("default stop %v", web.Stop)
	}
	if web.Scale == nil || web.Scale.Min != 1 || web.Scale.Max != 1 {
		t.Errorf("a service without scale runs one replica, got %+v", web.Scale)
	}
	if w := s.Components["w"]; w.Scale.Min != 0 {
		t.Errorf("a worker's minimum defaults to 0, got %d", w.Scale.Min)
	}
	q := s.Queues["q"]
	if q.Visibility.Duration != 2*time.Minute || q.Retention.Duration != 4*24*time.Hour || q.MaxReceives != 5 {
		t.Errorf("queue defaults: %+v", q)
	}
	if b := s.Buckets["b"]; b == nil || *b != (Bucket{}) {
		t.Errorf("a bucket is private and unversioned unless it says otherwise, got %+v", b)
	}
	if web.Context != "web" {
		t.Errorf("the build context defaults to the build directory, got %q", web.Context)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("check: %v", Problems(problems))
	}
}

func TestExplicitZerosAreKept(t *testing.T) {
	s := mustParse(t, strings.Replace(docsExample, "    spare: 1\n", "    spare: 0\n", 1))
	if s.Pools.General.Spare != 0 {
		t.Errorf("spare: 0 must stay 0, got %d", s.Pools.General.Spare)
	}
}

func TestQuantities(t *testing.T) {
	cpu := map[string]int64{"500m": 500, "2": 2000, "1.5": 1500, "0.25": 250, "4000m": 4000}
	for in, want := range cpu {
		if got, err := parseCPU(in); err != nil || got != want {
			t.Errorf("cpu %q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "m", "-1", "500k", "1.2345", "abc", "0.0001"} {
		if _, err := parseCPU(bad); err == nil {
			t.Errorf("cpu %q accepted", bad)
		}
	}
	mem := map[string]int64{"512Mi": 512, "1Gi": 1024, "1.5Gi": 1536, "2048Ki": 2, "1Ti": 1 << 20, "10Gi": 10240}
	for in, want := range mem {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("memory %q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "512", "512M", "1G", "-1Gi", "Gi", "1.0001Mi"} {
		if _, err := parseMemory(bad); err == nil {
			t.Errorf("memory %q accepted", bad)
		}
	}
	size := map[string]int64{"100Gi": 100, "1Ti": 1024, "20Gi": 20}
	for in, want := range size {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("size %q = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"512Mi", "1.5Gi", "100", "0Gi"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("size %q accepted", bad)
		}
	}
	pct := map[string]float64{"70%": 0.70, "100%": 1, "5.5%": 0.055}
	for in, want := range pct {
		if got, err := parsePercent(in); err != nil || got != want {
			t.Errorf("percent %q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"70", "0.7", "%", "-5%"} {
		if _, err := parsePercent(bad); err == nil {
			t.Errorf("percent %q accepted", bad)
		}
	}
	rate := map[string]float64{"2/s": 2, "30/m": 0.5, "30/min": 0.5, "3600/h": 1, "0.5/s": 0.5}
	for in, want := range rate {
		if got, err := parseRate(in); err != nil || got != want {
			t.Errorf("rate %q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"2", "2/d", "/s", "-1/s", "x/s"} {
		if _, err := parseRate(bad); err == nil {
			t.Errorf("rate %q accepted", bad)
		}
	}
	dur := map[string]time.Duration{"30s": 30 * time.Second, "2m": 2 * time.Minute, "4d": 96 * time.Hour, "1h30m": 90 * time.Minute, "500ms": 500 * time.Millisecond}
	for in, want := range dur {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("duration %q = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"30", "-1s", "1x", "d"} {
		if _, err := parseDuration(bad); err == nil {
			t.Errorf("duration %q accepted", bad)
		}
	}
}

func TestParseErrorsNameTheLine(t *testing.T) {
	tests := []struct{ src, want string }{
		{strings.Replace(docsExample, "cpu: 500m, memory: 512Mi", "cpu: 500m, memory: 512", 1), "line 36, column 35: components.api.request.memory"},
		{strings.Replace(docsExample, "    port: 8080\n", "    port: eighty\n", 1), "components.api.port: want a whole number"},
		{strings.Replace(docsExample, "    kind: service\n", "    kind: service\n    colour: red\n", 1), "components.api.colour: unknown field"},
		{strings.Replace(docsExample, "{ cpu: 70% }", "{ cpu: 70%, requests: 5/s }", 1), "components.api.scale.demand[0]: a demand signal has one of cpu, requests, queue or metric"},
		{strings.Replace(docsExample, "{ cpu: 70% }", "{ rate: 2/s }", 1), "a demand signal has one of"},
		{strings.Replace(docsExample, "routes: [shop/api/]", "routes: [{ route: /api/, burst: 3 }]", 1), "unknown field"},
		{strings.Replace(docsExample, "hosts:\n  shop: shop.example.com\n", "hosts: [shop.example.com]\n", 1), "hosts names each host"},
		{strings.Replace(docsExample, "migrate: [dotnet, Api.dll, migrate]", "migrate: { task: x }", 1), "migrate: want the name of a task, or a command"},
		{withBuckets("  documents: { public: true }\n")(docsExample), "buckets.documents.public: unknown field"},
		{withBuckets("  documents: { versions: forever }\n")(docsExample), "buckets.documents.versions: want a duration"},
	}
	for _, tt := range tests {
		_, err := Parse([]byte(tt.src))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("error %v, want it to contain %q", err, tt.want)
		}
	}
}

func TestRoutes(t *testing.T) {
	tests := []struct {
		in   string
		want Route
	}{
		{"shop/api/", Route{Host: "shop", Path: "/api/"}},
		{"/api/", Route{Path: "/api/"}},
		{"shop", Route{Host: "shop", Path: "/"}},
		{"{ route: /py/, rate: 200/s }", Route{Path: "/py/", Rate: 200}},
	}
	for _, tt := range tests {
		s := mustParse(t, strings.Replace(docsExample, "routes: [shop/api/]", "routes: ["+tt.in+"]", 1))
		if got := s.Components["api"].Routes[0]; got != tt.want {
			t.Errorf("route %s = %+v, want %+v", tt.in, got, tt.want)
		}
	}
}

func TestMigrateTask(t *testing.T) {
	s := mustParse(t, strings.Replace(docsExample, "migrate: [dotnet, Api.dll, migrate]", "migrate: api-migrations", 1))
	if m := s.Components["api"].Migrate; m.Task != "api-migrations" || m.Command != nil {
		t.Errorf("migrate %+v", m)
	}
}

// check parses the docs example with edit applied to its text, and returns its problems.
func check(t *testing.T, edit func(string) string) []Problem {
	t.Helper()
	return mustParse(t, edit(docsExample)).Check()
}

func replace(old, new string) func(string) string {
	return func(s string) string {
		if !strings.Contains(s, old) {
			panic("the docs example has no " + old)
		}
		return strings.Replace(s, old, new, 1)
	}
}

func insertAfter(anchor, text string) func(string) string { return replace(anchor, anchor+text) }

// withBuckets adds a buckets section, whose entries are lines, after the docs example's queues.
func withBuckets(lines string) func(string) string {
	return insertAfter("queues:\n  orders: {}\n", "\nbuckets:\n"+lines)
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string // It is the problem's path.
		want string // It is a substring of the problem's message.
	}{
		// These cases cover the top level.
		{"no app", replace("app: shop\n", ""), "app", "required"},
		{"bad app name", replace("app: shop", "app: Shop_1"), "app", "lowercase"},
		{"bad version", replace("protostrap: 1.4.0", "protostrap: latest"), "protostrap", "version"},
		{"bad hostname", replace("  shop: shop.example.com\n", "  shop: shop..example.com\n"), "hosts.shop", "host name"},
		{"bad host name", replace("  shop: shop.example.com\n", "  Shop.Web: shop.example.com\n"), "hosts.Shop.Web", "lowercase letters, digits and dashes"},
		{"hostname twice", replace("  shop: shop.example.com\n", "  shop: shop.example.com\n  www: shop.example.com\n"), "hosts.www", "also hosts.shop"},
		{"empty hostname", replace("  shop: shop.example.com\n", "  shop: \"\"\n"), "hosts.shop", "host name"},

		// These cases cover the pools.
		{"no ingress vm", replace("  ingress: { vm: c8g.medium }\n", "  ingress: {}\n"), "pools.ingress.vm", "required"},
		{"x86", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[m7i.xlarge]"), "pools.general.vm[0]", "arm64"},
		{"burstable x86", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[t3.xlarge]"), "pools.general.vm[0]", "x86"},
		{"burstable graviton", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[t4g.xlarge]"), "", ""},
		{"graviton without encryption in transit", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[m6g.xlarge]"), "", ""},
		{"unknown type", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[z9g.xlarge]"), "pools.general.vm[0]", "unknown"},
		{"mixed shapes", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[m7g.xlarge, m7g.2xlarge]"), "pools.general.vm[1]", "shape"},
		{"general min 0", replace("    min: 1\n    max: 10\n", "    min: 0\n    max: 10\n"), "pools.general.min", "at least 1"},
		{"max below min", replace("    min: 1\n    max: 10\n", "    min: 3\n    max: 2\n"), "pools.general.max", "at least min"},
		{"negative spare", replace("    spare: 1\n", "    spare: -1\n"), "pools.general.spare", "negative"},
		{"target over 100%", replace("target: 70%", "target: 120%"), "pools.general.target", "between"},
		{"reserve bigger than the VM", replace("    reserved: { cpu: 400m, memory: 1Gi }\n  compute", "    reserved: { cpu: 5000m, memory: 1Gi }\n  compute"), "pools.general.reserved", "leaves no room"},
		{"data vm with instance store", replace("data: { vm: m7g.xlarge, count: 1 }", "data: { vm: m6gd.xlarge, count: 1 }"), "pools.data.vm", "instance store"},
		{"data count over 3", replace("count: 1 }", "count: 4 }"), "pools.data.count", "1 to 3"},
		{"no data pool", replace("  data: { vm: m7g.xlarge, count: 1 }\n", ""), "pools.data.vm", "required"},
		{"edge too big", replace("request: { cpu: 500m, memory: 256Mi }\n\nqueues", "request: { cpu: 2000m, memory: 256Mi }\n\nqueues"), "edge.request", "fit"},

		// These cases cover the queues.
		{"bad queue name", replace("  orders: {}", "  Orders: {}"), "queues.Orders", "lowercase"},
		{"long queue name", replace("  orders: {}", "  "+strings.Repeat("q", 71)+": {}"), "queues." + strings.Repeat("q", 71), "70"},
		{"short visibility", replace("  orders: {}", "  orders: { visibility: 0s }"), "queues.orders.visibility", "between"},
		{"long retention", replace("  orders: {}", "  orders: { retention: 15d }"), "queues.orders.retention", "between"},
		{"max receives", replace("  orders: {}", "  orders: { max_receives: 0 }"), "queues.orders.max_receives", "1 to 1000"},
		{"queue named files", replace("  orders: {}", "  orders: {}\n  files: {}"), "queues.files", "reserved"},
		{"queue named backups", replace("  orders: {}", "  orders: {}\n  backups: {}"), "queues.backups", "reserved"},

		// These cases cover the buckets.
		{"bucket", withBuckets("  documents: {}\n"), "", ""},
		{"bucket settings", withBuckets("  documents: { browser: true, versions: 7d, lock: true, expire: 30d }\n"), "", ""},
		{"bucket in uses", func(s string) string {
			return replace("uses: [orders]", "uses: [orders, documents]")(withBuckets("  documents: {}\n")(s))
		}, "", ""},
		{"bad bucket name", withBuckets("  Documents: {}\n"), "buckets.Documents", "lowercase"},
		{"long bucket name", withBuckets("  " + strings.Repeat("b", 25) + ": {}\n"), "buckets." + strings.Repeat("b", 25), "24"},
		{"longest bucket name", withBuckets("  " + strings.Repeat("b", 24) + ": {}\n"), "", ""},
		{"bucket name with --", withBuckets("  a--b: {}\n"), "buckets.a--b", "S3"},
		{"bucket name ending -s3alias", withBuckets("  x-s3alias: {}\n"), "buckets.x-s3alias", "S3"},
		{"bucket named files", withBuckets("  files: {}\n"), "buckets.files", "reserved"},
		{"bucket named backups", withBuckets("  backups: {}\n"), "buckets.backups", "reserved"},
		{"bucket named deploy", withBuckets("  deploy: {}\n"), "buckets.deploy", "reserved"},
		{"bucket named tfstate", withBuckets("  tfstate: {}\n"), "buckets.tfstate", "reserved"},
		{"bucket named like a queue", withBuckets("  orders: {}\n"), "buckets.orders", "queue orders"},
		{"lock without versions", withBuckets("  documents: { lock: true }\n"), "buckets.documents.lock", "needs versions"},
		{"versions in hours", withBuckets("  documents: { versions: 36h }\n"), "buckets.documents.versions", "whole number of days"},
		{"no versions kept", withBuckets("  documents: { versions: 0d }\n"), "buckets.documents.versions", "at least 1d"},
		{"expire in minutes", withBuckets("  documents: { expire: 90m }\n"), "buckets.documents.expire", "whole number of days"},
		{"expire at once", withBuckets("  documents: { expire: 0d }\n"), "buckets.documents.expire", "at least 1d"},

		// These cases cover the components.
		{"no components", replace("components:\n", "components_x:\n"), "", ""}, // It fails to parse: see the loop below.
		{"bad name", replace("  orders-worker:\n", "  Orders:\n"), "components.Orders", "lowercase"},
		{"reserved name", replace("  orders-worker:\n", "  edge:\n"), "components.edge", "reserved"},
		{"long name", replace("  orders-worker:\n", "  "+strings.Repeat("w", 41)+":\n"), "components." + strings.Repeat("w", 41), "40"},
		{"unknown kind", replace("    kind: service\n", "    kind: daemon\n"), "components.api.kind", "service, worker, stateful or task"},
		{"no kind", replace("    kind: service\n", ""), "components.api.kind", "required"},
		{"service without build", replace("    build: services/api\n", ""), "components.api.build", "required"},
		{"project build", replace("    build: services/api\n", "    project: services/api/Api.csproj\n"), "", ""},
		{"build and project", insertAfter("    build: services/api\n", "    project: services/api/Api.csproj\n"), "components.api.project", "one of build or project"},
		{"project isn't a project file", replace("    build: services/api\n", "    project: services/api\n"), "components.api.project", ".csproj"},
		{"absolute project", replace("    build: services/api\n", "    project: /src/api/Api.csproj\n"), "components.api.project", "inside the repository"},
		{"project with context", replace("    build: services/api\n", "    project: services/api/Api.csproj\n    context: services\n"), "components.api.context", "goes with build"},
		{"stateful with project", insertAfter("    image: postgres:17.2\n", "    project: pg/Pg.csproj\n"), "components.postgres.project", "upstream image"},
		{"pathspec sources", replace("sources: [services/shared]", `sources: [nuget.config, ":(glob)services/**/Protos/**", ":(exclude,glob)services/api/**/*.md"]`), "", ""},
		{"escaping pathspec", replace("sources: [services/shared]", `sources: [":(glob)../shared/**"]`), "components.api.sources[0]", "inside the repository"},
		{"service with image", insertAfter("    build: services/api\n", "    image: nginx:1.27\n"), "components.api.image", "built"},
		{"absolute build", replace("build: services/api", "build: /src/api"), "components.api.build", "relative"},
		{"escaping source", replace("sources: [services/shared]", "sources: [../shared]"), "components.api.sources[0]", "inside the repository"},
		{"stateful without image", replace("    image: postgres:17.2\n", ""), "components.postgres.image", "required"},
		{"stateful latest", replace("image: postgres:17.2", "image: postgres:latest"), "components.postgres.image", "pinned"},
		{"stateful untagged", replace("image: postgres:17.2", "image: postgres"), "components.postgres.image", "pinned"},
		{"stateful with build", insertAfter("    image: postgres:17.2\n", "    build: pg\n"), "components.postgres.build", "upstream image"},
		{"bad pool", replace("    pool: general\n    request: { cpu: 500m", "    pool: data\n    request: { cpu: 500m"), "components.api.pool", "general or compute"},
		{"no pool", replace("    pool: general\n    request: { cpu: 500m", "    request: { cpu: 500m"), "components.api.pool", "required"},
		{"compute pool missing", replace("  compute:\n    vm: [c7g.xlarge, c6g.xlarge, c8g.xlarge, c7gd.xlarge]\n    min: 0\n    max: 20\n    spare: 0\n    target: 85%\n    reserved: { cpu: 400m, memory: 1Gi }\n", ""), "components.orders-worker.pool", "no compute pool"},
		{"stateful with pool", insertAfter("    vm: data-1\n    request: { cpu: 2500m", ""), "", ""}, // It changes nothing: see "stateful pool" below.
		{"stateful pool", insertAfter("    image: postgres:17.2\n", "    pool: general\n"), "components.postgres.pool", "pinned to a data VM"},
		{"no vm", replace("    vm: data-1\n    request: { cpu: 2500m", "    request: { cpu: 2500m"), "components.postgres.vm", "required"},
		{"unknown vm", replace("    vm: data-1\n    request: { cpu: 2500m", "    vm: data-2\n    request: { cpu: 2500m"), "components.postgres.vm", "no data-2"},
		{"no request", replace("    request: { cpu: 500m, memory: 512Mi }\n", ""), "components.api.request", "required"},
		{"request too big for the pool", replace("request: { cpu: 500m, memory: 512Mi }", "request: { cpu: 4000m, memory: 512Mi }"), "components.api.request", "fit"},
		{"memory too big for the pool", replace("request: { cpu: 500m, memory: 512Mi }", "request: { cpu: 500m, memory: 16Gi }"), "components.api.request", "fit"},
		{"stateful too big together", replace("request: { cpu: 1000m, memory: 4Gi }", "request: { cpu: 1000m, memory: 7Gi }"), "components.redis.request", "data-1"},
		{"service without port", replace("    port: 8080\n", ""), "components.api.port", "required"},
		{"bad port", replace("    port: 8080\n", "    port: 70000\n"), "components.api.port", "1 to 65535"},
		{"worker with port", insertAfter("    build: services/orders-worker\n", "    port: 80\n"), "components.orders-worker.port", "takes no requests"},
		{"service without ready", replace("    ready: grpc\n", ""), "components.api.ready", "required"},
		{"bad ready", replace("ready: grpc", "ready: healthz"), "components.api.ready", "grpc, tcp or an HTTP path"},
		{"ready on a bad port", replace("ready: grpc", "ready: grpc:0"), "components.api.ready", "on a port"},
		{"ready on another port", replace("ready: grpc", "ready: 9000/health/ready"), "", ""},
		{"tcp ready", replace("ready: grpc", "ready: tcp"), "", ""},
		{"routed service ready by command", replace("ready: grpc", "ready: [test, -f, /tmp/ready]"), "components.api.ready", "can't run a command"},
		{"unrouted service ready by command", func(s string) string {
			s = replace("ready: grpc", "ready: [test, -f, /tmp/ready]")(s)
			return replace("    routes: [shop/api/]\n", "")(s)
		}, "", ""},
		{"empty ready command", replace("ready: grpc", "ready: []"), "components.api.ready", "required"},
		{"worker ready without a port", insertAfter("    build: services/orders-worker\n", "    ready: /healthz\n"), "components.orders-worker.ready", "names a port"},
		{"worker grpc ready without a port", insertAfter("    build: services/orders-worker\n", "    ready: grpc\n"), "components.orders-worker.ready", "names a port"},
		{"worker ready on a port", insertAfter("    build: services/orders-worker\n", "    ready: 8081/healthz\n"), "", ""},
		{"worker ready by command", insertAfter("    build: services/orders-worker\n", "    ready: [test, -f, /tmp/ready]\n"), "", ""},
		{"stateful ready by command", insertAfter("    image: postgres:17.2\n", "    ready: [pg_isready, -U, postgres]\n"), "", ""},
		{"stateful ready on another port", insertAfter("    image: postgres:17.2\n", "    ready: 9187/metrics\n"), "", ""},
		{"task with ready", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    ready: [true]\n"), "components.mig.ready", "runs to completion"},
		{"bad protocol", insertAfter("    port: 8080\n", "    protocol: h3\n"), "components.api.protocol", "http1, http2 or grpc"},
		{"http2 protocol", insertAfter("    port: 8080\n", "    protocol: http2\n"), "", ""},
		{"grpc check over HTTP/1.1", insertAfter("    port: 8080\n", "    protocol: http1\n"), "components.api.ready", "http2 or grpc"},
		// Envoy refuses a gRPC check on a cluster that doesn't speak HTTP/2, whatever its port.
		{"grpc check on another port over HTTP/1.1", func(s string) string {
			s = insertAfter("    port: 8080\n", "    protocol: http1\n")(s)
			return replace("ready: grpc", "ready: grpc:9001")(s)
		}, "components.api.ready", "http2 or grpc"},
		{"grpc check on another port by default", replace("ready: grpc", "ready: grpc:9001"), "components.api.ready", "http2 or grpc"},
		{"grpc check on another port over HTTP/2", func(s string) string {
			s = insertAfter("    port: 8080\n", "    protocol: http2\n")(s)
			return replace("ready: grpc", "ready: grpc:9001")(s)
		}, "", ""},
		{"unrouted grpc check over HTTP/1.1", func(s string) string {
			s = insertAfter("    port: 8080\n", "    protocol: http1\n")(s)
			return replace("    routes: [shop/api/]\n", "")(s)
		}, "", ""},
		{"worker with protocol", insertAfter("    build: services/orders-worker\n", "    protocol: grpc\n"), "components.orders-worker.protocol", "only services"},
		{"worker with routes", insertAfter("    build: services/orders-worker\n", "    routes: [/x/]\n"), "components.orders-worker.routes", "only services"},
		{"route to unknown host", replace("routes: [shop/api/]", "routes: [other/api/]"), "components.api.routes[0]", "no host other in hosts"},
		{"route to a hostname", replace("routes: [shop/api/]", "routes: [shop.example.com/api/]"), "components.api.routes[0]", "by its name in hosts, such as shop/api/"},
		{"env sets a host", replace("    stop: 30s\n    scale:\n      min: 2", "    env: { HOST_SHOP: x.example.com }\n    stop: 30s\n    scale:\n      min: 2"), "components.api.env.HOST_SHOP", "ProtoStrap sets HOST_SHOP"},
		{"env sets an origin", replace("    stop: 30s\n    scale:\n      min: 2", "    env: { ORIGIN_SHOP: https://x.example.com }\n    stop: 30s\n    scale:\n      min: 2"), "components.api.env.ORIGIN_SHOP", "ProtoStrap sets ORIGIN_SHOP"},
		{"duplicate route", insertAfter("    build: services/orders-worker\n", ""), "", ""},
		{"bad metrics endpoint", replace("metrics: 9090/metrics", "metrics: metrics"), "components.api.metrics", "port/path"},
		{"unknown dependency", replace("depends_on: [postgres, redis]", "depends_on: [postgres, mysql]"), "components.api.depends_on[1]", "no component mysql"},
		{"self dependency", replace("depends_on: [postgres, redis]", "depends_on: [api]"), "components.api.depends_on[0]", "itself"},
		{"dependency cycle", insertAfter("    image: postgres:17.2\n", "    depends_on: [api]\n"), "components.postgres.depends_on", "cycle"},
		{"unknown use", replace("uses: [orders]", "uses: [payments]"), "components.api.uses[0]", "no queue or bucket payments"},
		{"files and backups uses", replace("uses: [orders]", "uses: [orders, files, backups]"), "", ""},
		{"bad env name", insertAfter("    stop: 30s\n", "    env: { 1BAD: x }\n"), "components.api.env.1BAD", "name"},
		{"reserved env", insertAfter("    stop: 30s\n", "    env: { PROTOSTRAP_RELEASE: x }\n"), "components.api.env.PROTOSTRAP_RELEASE", "ProtoStrap sets"},
		{"reserved queue env", insertAfter("    stop: 30s\n", "    env: { QUEUE_ORDERS_URL: x }\n"), "components.api.env.QUEUE_ORDERS_URL", "ProtoStrap sets"},
		{"reserved bucket env", insertAfter("    stop: 30s\n", "    env: { BUCKET_DOCUMENTS: x }\n"), "components.api.env.BUCKET_DOCUMENTS", "ProtoStrap sets"},
		{"aws env", insertAfter("    stop: 30s\n", "    env: { AWS_ACCESS_KEY_ID: x }\n"), "components.api.env.AWS_ACCESS_KEY_ID", "ProtoStrap sets"},
		{"bad secret name", insertAfter("    stop: 30s\n", "    secrets: [has-dash]\n"), "components.api.secrets[0]", "name"},
		{"secret also in env", insertAfter("    stop: 30s\n", "    env: { DB: x }\n    secrets: [DB]\n"), "components.api.secrets[0]", "also in env"},
		{"secret prefix", insertAfter("    stop: 30s\n", "    secrets: [DB]\n    secret_prefix: Shop_\n"), "", ""},
		{"secret files and a hook", insertAfter("    backup: [pg_dumpall, -U, postgres]\n", "    secrets: [POSTGRES_PASSWORD]\n    secret_files: true\n    on_secret_change: [sh, -c, x]\n"), "", ""},
		{"secret files that reload", insertAfter("    backup: [pg_dumpall, -U, postgres]\n", "    secrets: [POSTGRES_PASSWORD]\n    secret_files: true\n    on_secret_change: reload\n"), "", ""},
		{"lowercase secret prefix", insertAfter("    stop: 30s\n", "    secret_prefix: api_\n"), "components.api.secret_prefix", "such as Api_"},
		{"secret prefix without _", insertAfter("    stop: 30s\n", "    secret_prefix: Api\n"), "components.api.secret_prefix", "such as Api_"},
		{"secret prefix with two _", insertAfter("    stop: 30s\n", "    secret_prefix: Api_V2_\n"), "components.api.secret_prefix", "such as Api_"},
		{"shared secret prefix", insertAfter("    stop: 30s\n", "    secret_prefix: Shared_\n"), "components.api.secret_prefix", "reserved"},
		{"edge secret prefix", insertAfter("    stop: 30s\n", "    secret_prefix: Edge_\n"), "components.api.secret_prefix", "reserved"},
		{"component named shared", replace("  orders-worker:\n", "  shared:\n"), "components.shared.secret_prefix", "reserved"},
		{"component named reporter", replace("  orders-worker:\n", "  reporter:\n"), "components.reporter.secret_prefix", "reserved"},
		{"component named shared with a prefix", replace("  orders-worker:\n", "  shared:\n    secret_prefix: Common_\n"), "", ""},
		{"name that makes no prefix", replace("  orders-worker:\n", "  9worker:\n"), "components.9worker.secret_prefix", "set secret_prefix"},
		{"secret files without secrets", insertAfter("    backup: [pg_dumpall, -U, postgres]\n", "    secret_files: true\n"), "components.postgres.secret_files", "secrets"},
		{"secret change without secrets", insertAfter("    stop: 30s\n", "    on_secret_change: restart\n"), "components.api.on_secret_change", "secrets"},
		{"unknown secret change", insertAfter("    stop: 30s\n", "    secrets: [DB]\n    on_secret_change: bounce\n"), "components.api.on_secret_change", "restart, reload or a command"},
		{"empty secret change command", insertAfter("    backup: [pg_dumpall, -U, postgres]\n", "    secrets: [P]\n    secret_files: true\n    on_secret_change: []\n"), "components.postgres.on_secret_change", "restart, reload or a command"},
		{"reload without secret files", insertAfter("    stop: 30s\n", "    secrets: [DB]\n    on_secret_change: reload\n"), "components.api.on_secret_change", "secret_files"},
		{"command without secret files", insertAfter("    stop: 30s\n", "    secrets: [DB]\n    on_secret_change: [x]\n"), "components.api.on_secret_change", "secret_files"},
		{"secret change on a task", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    secrets: [DB]\n    on_secret_change: restart\n"), "components.mig.on_secret_change", "task"},
		{"secret files for a task", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    secrets: [DB]\n    secret_files: true\n"), "", ""},
		{"secret prefix env", insertAfter("    stop: 30s\n", "    env: { SECRET_PREFIX: x }\n"), "components.api.env.SECRET_PREFIX", "ProtoStrap sets"},
		{"migrate unknown task", replace("migrate: [dotnet, Api.dll, migrate]", "migrate: nothing"), "components.api.migrate", "no task nothing"},
		{"migrate names a service", replace("migrate: [dotnet, Api.dll, migrate]", "migrate: orders-worker"), "components.api.migrate", "not a task"},
		{"migrate on stateful", insertAfter("    image: postgres:17.2\n", "    migrate: [x]\n"), "components.postgres.migrate", "services and workers"},
		{"negative stop", replace("stop: 30s", "stop: -1s"), "", ""}, // It fails to parse.
		{"long stop", replace("stop: 30s", "stop: 2h"), "components.api.stop", "at most 1h"},
		{"stateful without volume", replace("    volume: { path: /data, size: 20Gi }\n", ""), "components.redis.volume", "required"},
		{"relative volume path", replace("path: /data, size: 20Gi", "path: data, size: 20Gi"), "components.redis.volume.path", "absolute"},
		{"service with volume", insertAfter("    stop: 30s\n", "    volume: { path: /x, size: 1Gi }\n"), "components.api.volume", "stateful"},
		{"service with backup", insertAfter("    stop: 30s\n", "    backup: [x]\n"), "components.api.backup", "stateful"},
		{"stateful with scale", insertAfter("    image: postgres:17.2\n", "    scale: { max: 2 }\n"), "components.postgres.scale", "services and workers"},
		{"scale max 0", replace("      min: 2\n      max: 10\n", "      min: 0\n      max: 0\n"), "components.api.scale.max", "at least 1"},
		{"scale max below min", replace("      min: 2\n      max: 10\n", "      min: 5\n      max: 3\n"), "components.api.scale.max", "at least min"},
		{"cpu target over 100%", replace("{ cpu: 70% }", "{ cpu: 150% }"), "components.api.scale.demand[0].cpu", "between"},
		{"requests without routes", replace("demand: [{ queue: orders, rate: 2/s, drain: 60s }]", "demand: [{ requests: 5/s }]"), "components.orders-worker.scale.demand[0]", "routes"},
		{"queue signal unknown", replace("queue: orders, rate", "queue: nope, rate"), "components.orders-worker.scale.demand[0].queue", "no queue nope"},
		{"queue signal without rate", replace("{ queue: orders, rate: 2/s, drain: 60s }", "{ queue: orders, drain: 60s }"), "components.orders-worker.scale.demand[0].rate", "required"},
		{"queue signal without drain", replace("{ queue: orders, rate: 2/s, drain: 60s }", "{ queue: orders, rate: 2/s }"), "components.orders-worker.scale.demand[0].drain", "required"},
		{"metric signal without endpoint", replace("demand: [{ queue: orders, rate: 2/s, drain: 60s }]", "demand: [{ metric: lag, target: 1 }]"), "components.orders-worker.scale.demand[0]", "metrics endpoint"},
		{"metric signal without target", replace("demand: [{ cpu: 70% }]", "demand: [{ metric: lag }]"), "components.api.scale.demand[0].target", "above 0"},
		{"bad metric name", replace("demand: [{ cpu: 70% }]", "demand: [{ metric: 9lag, target: 1 }]"), "components.api.scale.demand[0].metric", "metric name"},
		{"limit without any metrics", replace("    metrics: 9090/metrics\n", ""), "components.orders-worker.scale.limits[0]", "no component exports metrics"},
		{"negative replica limit", replace("limits: [{ metric: llm_budget_cap }]", "limits: [{ replicas: -1 }]"), "components.orders-worker.scale.limits[0].replicas", "negative"},
		{"limit with both", replace("limits: [{ metric: llm_budget_cap }]", "limits: [{ metric: x, replicas: 1 }]"), "components.orders-worker.scale.limits[0]", "one of"},
		{"task with routes", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    routes: [/m/]\n"), "components.mig.routes", "only services"},
		{"task with scale", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    scale: { max: 2 }\n"), "components.mig.scale", "services and workers"},
		{"depends on a task", insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n"), "", ""},
		{"depending on a task", func(s string) string {
			s = insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n")(s)
			return replace("depends_on: [postgres, redis]", "depends_on: [postgres, mig]")(s)
		}, "components.api.depends_on[1]", "migrate"},
		{"migration cycle", func(s string) string {
			s = insertAfter("components:\n", "  mig:\n    kind: task\n    build: mig\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    depends_on: [api]\n")(s)
			return replace("migrate: [dotnet, Api.dll, migrate]", "migrate: mig")(s)
		}, "components.api.migrate", "cycle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.edit(docsExample)
			s, err := Parse([]byte(src))
			if tt.path == "" && tt.want == "" {
				// The edit is either valid or refused when parsing: the case documents that it
				// isn't a check problem.
				if err == nil {
					if problems := s.Check(); len(problems) > 0 && tt.name != "no components" {
						t.Errorf("unexpected problems: %v", Problems(problems))
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			problems := s.Check()
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("no problem at %s containing %q; got:\n%v", tt.path, tt.want, Problems(problems))
		})
	}
}

func TestDuplicateRoutes(t *testing.T) {
	s := mustParse(t, insertAfter("    image: postgres:17.2\n", "")(docsExample))
	s.Components["web"] = &Component{
		Name: "web", Kind: api.KindService, Build: "web", Context: "web", Pool: "general",
		Request: Resources{CPU: 100, Memory: 128}, Port: 80, Ready: "/healthz",
		Routes: []Route{{Host: "shop", Path: "/api/"}},
		Scale:  &Scale{Min: 1, Max: 1}, Stop: Duration{10 * time.Second},
	}
	var found bool
	for _, p := range s.Check() {
		if strings.Contains(p.Msg, "also routed to") {
			found = true
		}
	}
	if !found {
		t.Errorf("two components on shop/api/ passed the check")
	}
}

func TestProblemsNameTheLine(t *testing.T) {
	s := mustParse(t, replace("depends_on: [postgres, redis]", "depends_on: [postgres, mysql]")(docsExample))
	problems := s.Check()
	if len(problems) != 1 {
		t.Fatalf("want one problem, got %v", Problems(problems))
	}
	if got := problems[0].String(); got != "line 41: components.api.depends_on[1]: no component mysql" {
		t.Errorf("problem %q", got)
	}
}

func TestFingerprint(t *testing.T) {
	base := mustParse(t, docsExample).Components["api"]
	fp := base.Fingerprint("img:c-1", "")
	if len(fp) != 16 {
		t.Errorf("fingerprint %q isn't 16 hex digits", fp)
	}
	same := func(name string, change func(c *Component), image string, want bool) {
		t.Helper()
		c := *base
		change(&c)
		if got := c.Fingerprint(image, "") == fp; got != want {
			t.Errorf("%s: fingerprint unchanged = %v, want %v", name, got, want)
		}
	}
	same("nothing", func(*Component) {}, "img:c-1", true)
	same("image", func(*Component) {}, "img:c-2", false)
	same("request", func(c *Component) { c.Request.CPU++ }, "img:c-1", false)
	same("port", func(c *Component) { c.Port++ }, "img:c-1", false)
	same("ready", func(c *Component) { c.Ready = "/x" }, "img:c-1", false)
	same("ready command", func(c *Component) { c.Ready, c.ReadyCommand = "", []string{"true"} }, "img:c-1", false)
	same("protocol", func(c *Component) { c.Protocol = "http2" }, "img:c-1", true)
	same("stop", func(c *Component) { c.Stop.Duration += time.Second }, "img:c-1", false)
	same("env", func(c *Component) { c.Env = map[string]string{"A": "1"} }, "img:c-1", false)
	same("secrets", func(c *Component) { c.Secrets = []string{"K"} }, "img:c-1", false)
	same("metrics", func(c *Component) { c.Metrics = "9091/metrics" }, "img:c-1", false)
	same("command", func(c *Component) { c.Command = []string{"x"} }, "img:c-1", false)
	same("secret prefix", func(c *Component) { c.SecretPrefix = "Shop_" }, "img:c-1", false)
	same("secret files", func(c *Component) { c.SecretFiles = true }, "img:c-1", false)
	same("reload", func(c *Component) { c.OnSecretChange = &Change{Mode: "reload"} }, "img:c-1", false)
	same("secret command", func(c *Component) { c.OnSecretChange = &Change{Command: []string{"x"}} }, "img:c-1", false)
	same("restart, said", func(c *Component) { c.OnSecretChange = &Change{Mode: "restart"} }, "img:c-1", true)
	same("uses", func(c *Component) { c.Uses = append(c.Uses, "files") }, "img:c-1", true)
	same("scale", func(c *Component) { c.Scale = &Scale{Min: 1, Max: 99} }, "img:c-1", true)
	same("routes", func(c *Component) { c.Routes = nil }, "img:c-1", true)
	same("depends_on", func(c *Component) { c.DependsOn = nil }, "img:c-1", true)

	a := *base
	a.Env = map[string]string{"A": "1", "B": "2"}
	b := *base
	b.Env = map[string]string{"B": "2", "A": "1"}
	if a.Fingerprint("i", "") != b.Fingerprint("i", "") {
		t.Errorf("env order changed the fingerprint")
	}
}

// TestAnUpgradeKeepsEveryFingerprint pins the docs example's fingerprints as ProtoStrap
// computed them before secret files: a field that changed them would replace every replica
// when ProtoStrap is upgraded. PostgreSQL's request is the one the example had then, before
// it made room for telemetry on data-1.
func TestAnUpgradeKeepsEveryFingerprint(t *testing.T) {
	s := mustParse(t, replace("request: { cpu: 2500m, memory: 9Gi }", "request: { cpu: 3000m, memory: 10Gi }")(docsExample))
	for name, want := range map[string]string{
		"api":           "4295d63b9c2d90c0",
		"orders-worker": "a47c8306785f021e",
		"postgres":      "1994510cadd692f5",
		"redis":         "0e64fc68a7d5c471",
	} {
		if got := s.Components[name].Fingerprint("img:c-1", ""); got != want {
			t.Errorf("%s: fingerprint %s, want %s", name, got, want)
		}
	}
}

func TestSecretPrefixDefaultsToTheComponentsName(t *testing.T) {
	for name, want := range map[string]string{
		"api":             "Api_",
		"orders-worker":   "OrdersWorker_",
		"document-import": "DocumentImport_",
		"s3-sync":         "S3Sync_",
	} {
		if got := (&Component{Name: name}).Prefix(); got != want {
			t.Errorf("%s: prefix %q, want %q", name, got, want)
		}
	}
	if got := (&Component{Name: "api-migrations", SecretPrefix: "Api_"}).Prefix(); got != "Api_" {
		t.Errorf("secret_prefix: prefix %q, want Api_", got)
	}
}

func TestOnSecretChangeParses(t *testing.T) {
	at := "    backup: [pg_dumpall, -U, postgres]\n"
	pg := func(t *testing.T, value string) *Component {
		t.Helper()
		return mustParse(t, insertAfter(at, "    on_secret_change: "+value+"\n")(docsExample)).Components["postgres"]
	}
	if c := mustParse(t, docsExample).Components["postgres"]; c.OnSecretChange != nil || c.Follows() {
		t.Errorf("left out: %+v, follows %v", c.OnSecretChange, c.Follows())
	}
	for _, tt := range []struct {
		value   string
		want    Change
		follows bool
	}{
		{"restart", Change{Mode: "restart"}, false},
		{"reload", Change{Mode: "reload"}, true},
		{"[sh, -c, x]", Change{Command: []string{"sh", "-c", "x"}}, true},
	} {
		c := pg(t, tt.value)
		if c.OnSecretChange == nil || c.OnSecretChange.Mode != tt.want.Mode || !slices.Equal(c.OnSecretChange.Command, tt.want.Command) {
			t.Errorf("%s: %+v, want %+v", tt.value, c.OnSecretChange, tt.want)
		}
		if c.Follows() != tt.follows {
			t.Errorf("%s: follows %v, want %v", tt.value, c.Follows(), tt.follows)
		}
	}
	if _, err := Parse([]byte(insertAfter(at, "    on_secret_change: { run: x }\n")(docsExample))); err == nil {
		t.Errorf("a map parsed")
	}
}

func TestInfraHash(t *testing.T) {
	base := mustParse(t, docsExample).InfraHash()
	tests := []struct {
		name    string
		edit    func(string) string
		changes bool
	}{
		{"env", insertAfter("    stop: 30s\n", "    env: { A: b }\n"), false},
		{"target", replace("target: 70%", "target: 60%"), false},
		{"spare", replace("    spare: 1\n", "    spare: 2\n"), false},
		{"scale", replace("      min: 2\n      max: 10\n", "      min: 3\n      max: 10\n"), false},
		{"request", replace("request: { cpu: 500m, memory: 512Mi }", "request: { cpu: 600m, memory: 512Mi }"), false},
		{"routes", replace("routes: [shop/api/]", "routes: [shop/v2/]"), false},
		{"version", replace("protostrap: 1.4.0", "protostrap: 1.5.0"), false},
		{"uses", replace("uses: [orders]", "uses: [orders, files]"), true},
		{"pool max", replace("    max: 10\n    spare", "    max: 11\n    spare"), true},
		{"pool vm types", replace("[m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]", "[m7g.xlarge, m6g.xlarge]"), true},
		{"data count", replace("count: 1 }", "count: 2 }"), true},
		{"queue", replace("  orders: {}", "  orders: {}\n  more: {}"), true},
		{"queue setting", replace("  orders: {}", "  orders: { fifo: true }"), true},
		{"bucket", withBuckets("  documents: {}\n"), true},
		{"bucket in uses", func(s string) string {
			return replace("uses: [orders]", "uses: [orders, documents]")(withBuckets("  documents: {}\n")(s))
		}, true},
		{"hosts", replace("  shop: shop.example.com\n", "  shop: shop.example.com\n  www: www.example.com\n"), true},
		{"hostname", replace("  shop: shop.example.com\n", "  shop: shop.example.org\n"), true},
		{"volume size", replace("size: 20Gi", "size: 30Gi"), true},
		{"stateful vm", replace("count: 1 }", "count: 2 }"), true},
		{"component added", insertAfter("components:\n", "  w2:\n    kind: worker\n    build: w2\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n"), true},
		{"queue signal", replace("demand: [{ queue: orders, rate: 2/s, drain: 60s }]", "demand: [{ cpu: 50% }]"), true},
	}
	for _, tt := range tests {
		got := mustParse(t, tt.edit(docsExample)).InfraHash()
		if (got != base) != tt.changes {
			t.Errorf("%s: infra hash changed = %v, want %v", tt.name, got != base, tt.changes)
		}
	}
}

// TestBucketSettingsChangeTheInfraHash checks that each of a bucket's settings is
// infrastructure: Terraform makes every one of them.
func TestBucketSettingsChangeTheInfraHash(t *testing.T) {
	base := mustParse(t, withBuckets("  documents: {}\n")(docsExample)).InfraHash()
	for _, settings := range []string{"{ browser: true }", "{ versions: 7d }", "{ versions: 7d, lock: true }", "{ expire: 30d }"} {
		got := mustParse(t, withBuckets("  documents: "+settings+"\n")(docsExample)).InfraHash()
		if got == base {
			t.Errorf("%s left the infra hash unchanged", settings)
		}
	}
}

func TestBuildInputs(t *testing.T) {
	s := mustParse(t, docsExample)
	api := s.Components["api"]
	if got := strings.Join(api.TagSources(), ","); got != "services/api,services/shared" {
		t.Errorf("api's sources %q: the build directory and its sources", got)
	}
	w := s.Components["orders-worker"]
	if got := strings.Join(w.TagSources(), ","); got != "services/orders-worker" {
		t.Errorf("worker's sources %q", got)
	}
	if api.Recipe() == w.Recipe() {
		t.Errorf("two builds share a recipe")
	}
	if tag := Tag("abc", api.Recipe()); !strings.HasPrefix(tag, "c-") || len(tag) != 18 {
		t.Errorf("tag %q isn't c- and 16 hex digits", tag)
	}
	if Tag("abc", api.Recipe()) == Tag("abd", api.Recipe()) {
		t.Errorf("sources don't change the tag")
	}
}

func TestProjectBuildInputs(t *testing.T) {
	s := mustParse(t, replace("    build: services/api\n", "    project: services/api/Api.csproj\n")(docsExample))
	api := s.Components["api"]
	if got := strings.Join(api.TagSources(), ","); got != "services/api,services/shared" {
		t.Errorf("a project's sources %q: its directory and its sources", got)
	}
	built := mustParse(t, docsExample).Components["api"]
	if api.Recipe() == built.Recipe() {
		t.Errorf("a project and a Dockerfile in the same directory share a recipe")
	}
	if !strings.Contains(api.Recipe(), `"builder":"dotnet"`) || !strings.Contains(api.Recipe(), "services/api/Api.csproj") {
		t.Errorf("recipe %s: the .NET SDK's container publish of the project", api.Recipe())
	}
}

// Every image is built for the VMs, which are Graviton. An image built for another
// platform has another tag, so moving platform rebuilds every image.
func TestImagesAreBuiltForArm64(t *testing.T) {
	built := mustParse(t, docsExample).Components["api"]
	project := mustParse(t, replace("    build: services/api\n", "    project: services/api/Api.csproj\n")(docsExample)).Components["api"]
	for _, c := range []*Component{built, project} {
		if !strings.Contains(c.Recipe(), `"platform":"linux/arm64"`) {
			t.Errorf("recipe %s isn't for linux/arm64", c.Recipe())
		}
	}
	if Platform != "linux/arm64" {
		t.Errorf("Platform is %s", Platform)
	}
}

// TestAGeneratedSpecChecks reads the spec ProtoStrap.Hosting generates from the AppHost in
// docs/protostrap-aspire.md (the golden file of its tests in hosting/tests), so a change to
// either the writer or the reader that breaks the other fails here.
func TestAGeneratedSpecChecks(t *testing.T) {
	path := "../../hosting/tests/ProtoStrap.Hosting.Tests/Golden/protofast.yaml"
	s, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Fatalf("%s:\n%v", path, Problems(problems))
	}
	api := s.Components["api"]
	if api.Project != "services/api/src/ProtoFast.Api/ProtoFast.Api.csproj" || len(api.Sources) != 15 {
		t.Errorf("api builds from its project and its reference closure: %+v", api)
	}
	if api.Sources[0] != "global.json" {
		t.Errorf("api's sources start with the global.json that pins its SDK: %v", api.Sources)
	}
	if api.Migrate == nil || api.Migrate.Task != "protofast-db-migrations" || strings.Join(api.Secrets, ",") != "ConnectionStrings__protofast,ConnectionStrings__redis" {
		t.Errorf("api: %+v", api)
	}
	if !s.Queues["protofast-workflow-outcomes"].FIFO || s.Queues["protofast-document-upload"].FIFO {
		t.Errorf("queues: %+v", s.Queues)
	}
	// ProtoFast's documents are versioned and locked, as its own infra has them.
	if b := s.Buckets["documents"]; b == nil || !b.Browser || !b.Lock || b.Versions.Duration != 7*24*time.Hour {
		t.Errorf("buckets: %+v", s.Buckets)
	}
	for _, name := range []string{"api", "conversion", "document-import"} {
		if !slices.Contains(s.Components[name].Uses, "documents") {
			t.Errorf("%s doesn't use the documents bucket: %v", name, s.Components[name].Uses)
		}
	}
	conversion := s.Components["conversion"]
	if conversion.Build != "services/conversion" || !strings.HasPrefix(conversion.Sources[0], ":(exclude,glob)") {
		t.Errorf("conversion builds from its Dockerfile, without what .dockerignore leaves out: %+v", conversion)
	}
	if got := s.Components["document-import"].Env["DocumentImport_Conversion__BaseUrl"]; got != "http://conversion:8090" {
		t.Errorf("an endpoint renders as the component's name and port: %q", got)
	}

	// The commands that read PostgreSQL's and Redis's passwords from their files are shell,
	// quoted by the generator and read back here: each must arrive exactly as it was meant.
	pg, redis := s.Components["postgres"], s.Components["redis"]
	if !pg.SecretFiles || !slices.Equal(pg.Secrets, []string{"POSTGRES_PASSWORD"}) || pg.Env["POSTGRES_PASSWORD_FILE"] != "/run/secrets/POSTGRES_PASSWORD" {
		t.Errorf("postgres: %+v", pg)
	}
	for what, tt := range map[string]struct{ got, want []string }{
		"postgres's hook": {pg.OnSecretChange.Command, []string{"sh", "-c",
			`printf '%s\n' '\set pw ` + "`cat /run/secrets/POSTGRES_PASSWORD`" + `' "ALTER ROLE postgres PASSWORD :'pw';" | psql -X -q -v ON_ERROR_STOP=1 -U postgres -d postgres`}},
		"redis's command": {redis.Command, []string{"sh", "-c",
			`exec docker-entrypoint.sh redis-server --requirepass "$(cat /run/secrets/REDIS_PASSWORD)"`}},
		"redis's hook": {redis.OnSecretChange.Command, []string{"sh", "-c",
			`for f in /run/secrets/REDIS_PASSWORD /run/secrets/previous/REDIS_PASSWORD; do [ -f "$f" ] && REDISCLI_AUTH="$(cat "$f")" redis-cli --no-auth-warning -x CONFIG SET requirepass </run/secrets/REDIS_PASSWORD | grep -qx OK && exit 0; done; exit 1`}},
		"redis's backup": {redis.Backup, []string{"sh", "-c",
			`REDISCLI_AUTH="$(cat /run/secrets/REDIS_PASSWORD)" exec redis-cli --no-auth-warning --rdb -`}},
	} {
		if !slices.Equal(tt.got, tt.want) {
			t.Errorf("%s reads back as\n%q\nwant\n%q", what, tt.got, tt.want)
		}
	}
}

func TestBucketEnv(t *testing.T) {
	if got := BucketEnv("documents"); got != "BUCKET_DOCUMENTS" {
		t.Errorf("BucketEnv(documents) = %q", got)
	}
	if got := BucketEnv("export-files"); got != "BUCKET_EXPORT_FILES" {
		t.Errorf("BucketEnv(export-files) = %q", got)
	}
}

func TestBucketNamesAreSorted(t *testing.T) {
	s := mustParse(t, withBuckets("  reports: {}\n  documents: {}\n")(docsExample))
	if got := strings.Join(s.BucketNames(), ","); got != "documents,reports" {
		t.Errorf("BucketNames() = %s", got)
	}
}

func TestQueueEnv(t *testing.T) {
	if got := QueueEnv("orders"); got != "QUEUE_ORDERS_URL" {
		t.Errorf("QueueEnv(orders) = %q", got)
	}
	if got := QueueEnv("document-upload"); got != "QUEUE_DOCUMENT_UPLOAD_URL" {
		t.Errorf("QueueEnv(document-upload) = %q", got)
	}
}

func TestReadyIsACheckOrACommand(t *testing.T) {
	s := mustParse(t, replace("ready: grpc", "ready: [test, -f, /tmp/ready]")(docsExample))
	api := s.Components["api"]
	if api.Ready != "" || strings.Join(api.ReadyCommand, " ") != "test -f /tmp/ready" {
		t.Errorf("ready %q, command %q", api.Ready, api.ReadyCommand)
	}
	s = mustParse(t, docsExample)
	if api := s.Components["api"]; api.Ready != "grpc" || api.ReadyCommand != nil {
		t.Errorf("ready %q, command %q", api.Ready, api.ReadyCommand)
	}
	if _, err := Parse([]byte(replace("ready: grpc", "ready: { path: /x }")(docsExample))); err == nil {
		t.Error("a mapping is neither a check nor a command")
	}
}

// What the gateway speaks to a service defaults from its check, so specs written before
// protocol existed keep their behaviour.
func TestUpstreamProtocol(t *testing.T) {
	for _, tt := range []struct{ ready, protocol, want string }{
		{"grpc", "", "grpc"},
		{"grpc:9001", "", "http1"},
		{"/healthz", "", "http1"},
		{"/healthz", "http2", "http2"},
		{"grpc", "http2", "http2"},
		{"tcp", "grpc", "grpc"},
	} {
		c := &Component{Kind: api.KindService, Ready: tt.ready, Protocol: tt.protocol}
		if got := c.UpstreamProtocol(); got != tt.want {
			t.Errorf("ready %q, protocol %q: %q, want %q", tt.ready, tt.protocol, got, tt.want)
		}
	}
}

// Every replica finds each of the application's hostnames in HOST_<NAME>, and its origin, the
// scheme and host as browsers address it, in ORIGIN_<NAME>.
func TestHostVariables(t *testing.T) {
	if got := HostEnv("admin-web"); got != "HOST_ADMIN_WEB" {
		t.Errorf("HostEnv = %s", got)
	}
	if got := OriginEnv("admin-web"); got != "ORIGIN_ADMIN_WEB" {
		t.Errorf("OriginEnv = %s", got)
	}
}
