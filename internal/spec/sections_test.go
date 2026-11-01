package spec

import (
	"strings"
	"testing"
)

// These tests cover the tunnel, gateway and telemetry sections (docs/protostrap.md, "Tunnel",
// "Gateway" and "Telemetry"). Each is ProtoStrap's own, and runs only when the spec has it: the
// tunnel brings requests in from Cloudflare, the gateway routes them to the upstreams, and
// telemetry keeps the application's logs, traces and metrics.

// TestEachSectionRunsOnlyWhenTheSpecHasIt checks that leaving a section out leaves it off,
// and that every other section still checks without it.
func TestEachSectionRunsOnlyWhenTheSpecHasIt(t *testing.T) {
	tests := []struct {
		name                       string
		edit                       func(string) string
		tunnel, gateway, telemetry bool
	}{
		{"all three", func(s string) string { return s }, true, true, true},
		{"no tunnel", replace("tunnel: {}\n\n", ""), false, true, true},
		{"no telemetry", replace("telemetry: {}\n\n", ""), true, true, false},
		{"no gateway or tunnel", withoutGateway, false, false, true},
		{"none", func(s string) string { return replace("telemetry: {}\n\n", "")(withoutGateway(s)) }, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.edit(docsExample))
			if got := [3]bool{s.Tunnel != nil, s.Gateway != nil, s.Telemetry != nil}; got != [3]bool{tt.tunnel, tt.gateway, tt.telemetry} {
				t.Errorf("tunnel, gateway, telemetry = %v, want %v", got, [3]bool{tt.tunnel, tt.gateway, tt.telemetry})
			}
			if problems := s.Check(); len(problems) > 0 {
				t.Errorf("problems: %v", Problems(problems))
			}
		})
	}
}

// TestASectionKeepsItsDefaults checks that an empty section runs with ProtoStrap's defaults,
// and a missing one has none to keep.
func TestASectionKeepsItsDefaults(t *testing.T) {
	s := mustParse(t, replace("  request: { cpu: 500m, memory: 256Mi }\n  hosts:", "  hosts:")(docsExample))
	if s.Gateway.Request != (Resources{CPU: 500, Memory: 256}) {
		t.Errorf("gateway request %+v, want 500m and 256Mi", s.Gateway.Request)
	}
	if s.Telemetry.Request != (Resources{CPU: 500, Memory: 1536}) || s.Telemetry.Volume != 20 {
		t.Errorf("telemetry %+v", s.Telemetry)
	}
	s = mustParse(t, "app: x\nprotostrap: 1.0.0\ngateway: {}\n")
	if s.Gateway == nil || s.Gateway.Request != (Resources{CPU: 500, Memory: 256}) || s.Gateway.Hosts != nil || s.Gateway.Upstreams != nil {
		t.Errorf("an empty gateway: %+v", s.Gateway)
	}
}

// TestAnEmptySectionIsAMistake checks that a section written with nothing after it, which
// YAML reads as null, isn't quietly left off: the spec means { } to run it with defaults.
func TestAnEmptySectionIsAMistake(t *testing.T) {
	for _, section := range []string{"tunnel", "telemetry"} {
		s := mustParse(t, replace(section+": {}\n", section+":\n")(docsExample))
		found := false
		for _, p := range s.Check() {
			if p.Path == section && strings.Contains(p.Msg, "write "+section+": {}") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: with nothing, no problem: %v", section, Problems(s.Check()))
		}
	}
	s := mustParse(t, replace("gateway:\n  request: { cpu: 500m, memory: 256Mi }\n  hosts:\n    shop: shop.example.com\n  upstreams:\n    api:\n      routes: [shop/api/]\n", "gateway:\n")(docsExample))
	found := false
	for _, p := range s.Check() {
		if p.Path == "gateway" && strings.Contains(p.Msg, "write gateway: {}") {
			found = true
		}
	}
	if !found {
		t.Errorf("gateway: with nothing, no problem: %v", Problems(s.Check()))
	}
}

// oldExample is the docs example as ProtoStrap 0.1 wrote it, with hosts and edge at the top,
// routes, protocol and authorize on the components, and telemetry always on.
const oldExample = `app: shop
protostrap: 0.1.1
hosts:
  shop: shop.example.com

pools:
  ingress: { vm: c8g.medium }
  general: { vm: [m7g.xlarge], min: 1, max: 4 }
  data: { vm: m7g.xlarge }

edge:
  request: { cpu: 400m, memory: 256Mi }

components:
  auth:
    kind: service
    build: auth
    pool: general
    request: { cpu: 100m, memory: 128Mi }
    port: 8080
    ready: /healthz
  api:
    kind: service
    build: api
    pool: general
    request: { cpu: 100m, memory: 128Mi }
    port: 8080
    protocol: http2
    ready: /healthz
    routes: [shop/api/, { route: /x/, rate: 5/s }]
    authorize: { by: auth, headers: [x-user] }
`

// TestAnOldSpecReadsAsTheNewOne checks that a release made by an older ProtoStrap still loads:
// a new scheduler starts on the release it finds running, so Parse reads the old places as
// the new ones, with the tunnel and telemetry on, as they always were.
func TestAnOldSpecReadsAsTheNewOne(t *testing.T) {
	s := mustParse(t, oldExample)
	if s.Tunnel == nil || s.Gateway == nil || s.Telemetry == nil {
		t.Fatalf("tunnel %v, gateway %v, telemetry %v: an old spec ran all three", s.Tunnel, s.Gateway, s.Telemetry)
	}
	if s.Gateway.Request != (Resources{CPU: 400, Memory: 256}) {
		t.Errorf("gateway request %+v, want the edge's", s.Gateway.Request)
	}
	if hosts := s.Hosts(); len(hosts) != 1 || hosts["shop"] != "shop.example.com" {
		t.Errorf("hosts %v", hosts)
	}
	if s.Telemetry.Request != (Resources{CPU: 500, Memory: 1536}) || s.Telemetry.Volume != 20 {
		t.Errorf("telemetry %+v, want the defaults", s.Telemetry)
	}
	u := s.Upstream("api")
	if u == nil || u.Name != "api" || u.Protocol != "http2" || len(u.Routes) != 2 || u.Routes[1] != (Route{Path: "/x/", Rate: 5}) {
		t.Fatalf("api's upstream %+v", u)
	}
	if u.Authorize == nil || u.Authorize.By != "auth" || strings.Join(u.Authorize.Headers, ",") != "x-user" {
		t.Errorf("api's authorizer %+v", u.Authorize)
	}
	if s.Upstream("auth") != nil {
		t.Errorf("auth has no routes, so no upstream: %+v", s.Upstream("auth"))
	}
	// Without an edge section, the gateway keeps its default request.
	s = mustParse(t, strings.Replace(oldExample, "edge:\n  request: { cpu: 400m, memory: 256Mi }\n", "", 1))
	if s.Gateway == nil || s.Gateway.Request != (Resources{CPU: 500, Memory: 256}) {
		t.Errorf("gateway %+v", s.Gateway)
	}
}

// TestTheOldPlacesSayWhereTheyWent checks that protostrap check, which authors run, rejects a
// spec written for an older ProtoStrap, and says where each field moved.
func TestTheOldPlacesSayWhereTheyWent(t *testing.T) {
	want := map[string]string{
		"hosts":                    "moved to gateway.hosts",
		"edge":                     "is now the tunnel and the gateway: its request is gateway.request",
		"components.api.protocol":  "moved to gateway.upstreams.api.protocol",
		"components.api.routes":    "moved to gateway.upstreams.api.routes",
		"components.api.authorize": "moved to gateway.upstreams.api.authorize",
	}
	lines := map[string]int{"hosts": 3, "edge": 11, "components.api.protocol": 28, "components.api.routes": 30, "components.api.authorize": 31}
	got := map[string]string{}
	for _, p := range mustParse(t, oldExample).Check() {
		if _, ok := want[p.Path]; ok {
			got[p.Path] = p.Msg
			if p.Line != lines[p.Path] {
				t.Errorf("%s on line %d, want %d", p.Path, p.Line, lines[p.Path])
			}
		}
	}
	for path, msg := range want {
		if !strings.Contains(got[path], msg) {
			t.Errorf("%s: %q, want %q", path, got[path], msg)
		}
	}
	// A spec with both the old places and the new is neither, and isn't read.
	for _, edit := range []func(string) string{
		insertAfter("protostrap: 1.4.0\n", "hosts:\n  shop: shop.example.com\n"),
		insertAfter("    port: 8080\n", "    routes: [shop/api/]\n"),
	} {
		if _, err := Parse([]byte(edit(docsExample))); err == nil || !strings.Contains(err.Error(), "moved to gateway.") {
			t.Errorf("a spec with both: %v", err)
		}
	}
}

// TestTheTunnelNeedsTheGateway checks that the tunnel, which sends every request it brings
// in to the gateway, can't run without one.
func TestTheTunnelNeedsTheGateway(t *testing.T) {
	s := mustParse(t, func(s string) string {
		return insertAfter("  data: { vm: m7g.xlarge, count: 1 }\n", "\ntunnel: {}\n")(withoutGateway(s))
	}(docsExample))
	for _, p := range s.Check() {
		if p.Path == "tunnel" && strings.Contains(p.Msg, "sends every request to the gateway: add a gateway section") {
			return
		}
	}
	t.Errorf("a tunnel without a gateway passed: %v", Problems(s.Check()))
}

// TestCheckGateway covers the gateway's upstreams: each a service, each with routes.
func TestCheckGateway(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) string
		path string
		want string
	}{
		{"no hosts", replace("  hosts:\n    shop: shop.example.com\n", ""), "gateway.upstreams.api.routes[0]", "no host shop in gateway.hosts"},
		{"routes for every host", replace("routes: [shop/api/]", "routes: [/api/]"), "", ""},
		{"no upstreams", replace("  upstreams:\n    api:\n      routes: [shop/api/]\n", ""), "", ""},
		{"unknown upstream", upstreamNamed("nobody", "[shop/x/]"), "gateway.upstreams.nobody", "no component nobody"},
		{"worker upstream", upstreamNamed("orders-worker", "[shop/w/]"), "gateway.upstreams.orders-worker", "orders-worker is a worker: only services get requests from the gateway"},
		{"stateful upstream", upstreamNamed("postgres", "[shop/pg/]"), "gateway.upstreams.postgres", "postgres is a stateful component: only services get requests from the gateway"},
		{"upstream without routes", replace("      routes: [shop/api/]\n", "      protocol: grpc\n"), "gateway.upstreams.api.routes", "required: the routes the gateway sends to api"},
		{"upstream with no routes", replace("routes: [shop/api/]", "routes: []"), "gateway.upstreams.api.routes", "required"},
		{"empty upstream", replace("    api:\n      routes: [shop/api/]\n", "    api: {}\n"), "gateway.upstreams.api.routes", "required"},
		{"no request", replace("  request: { cpu: 500m, memory: 256Mi }\n  hosts:", "  request: { cpu: 0m, memory: 256Mi }\n  hosts:"), "gateway.request", "must reserve CPU and memory"},
		{"route twice", func(s string) string {
			s = insertAfter("components:\n", "  web:\n    kind: service\n    build: web\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    port: 80\n    ready: /healthz\n")(s)
			return upstreamNamed("web", "[shop/api/]")(s)
		}, "gateway.upstreams.web.routes[0]", "shop/api/ is also routed to api"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.edit(docsExample))
			problems := s.Check()
			if tt.path == "" {
				if len(problems) > 0 {
					t.Errorf("want no problems, got %v", Problems(problems))
				}
				return
			}
			for _, p := range problems {
				if p.Path == tt.path && strings.Contains(p.Msg, tt.want) {
					return
				}
			}
			t.Errorf("want %s: %s, got %v", tt.path, tt.want, Problems(problems))
		})
	}
}

// upstreamNamed adds an upstream with routes to the docs example's gateway, before the api's.
func upstreamNamed(name, routes string) func(string) string {
	return insertAfter("  upstreams:\n", "    "+name+":\n      routes: "+routes+"\n")
}

// TestUpstreamsAreSortedAndNamed checks that each upstream knows its component's name, and
// that they come back in name order.
func TestUpstreamsAreSortedAndNamed(t *testing.T) {
	s := mustParse(t, func(s string) string {
		s = insertAfter("components:\n", "  web:\n    kind: service\n    build: web\n    pool: general\n    request: { cpu: 100m, memory: 128Mi }\n    port: 80\n    ready: /healthz\n")(s)
		return upstreamNamed("web", "[shop/]")(s)
	}(docsExample))
	var names []string
	for _, u := range s.Upstreams() {
		names = append(names, u.Name)
	}
	if strings.Join(names, ",") != "api,web" {
		t.Errorf("upstreams %v, want api and web", names)
	}
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("problems: %v", Problems(problems))
	}
	if s := mustParse(t, withoutGateway(docsExample)); s.Upstreams() != nil {
		t.Errorf("upstreams without a gateway: %v", s.Upstreams())
	}
}

// TestABrowserBucketNeedsHosts checks that a bucket browsers use has an origin to allow: the
// gateway's hosts, which its CORS rule names.
func TestABrowserBucketNeedsHosts(t *testing.T) {
	s := mustParse(t, withBuckets("  documents: { browser: true }\n")(docsExample))
	if problems := s.Check(); len(problems) > 0 {
		t.Errorf("problems: %v", Problems(problems))
	}
	for _, edit := range []func(string) string{withoutGateway, replace("  hosts:\n    shop: shop.example.com\n", "")} {
		s := mustParse(t, withBuckets("  documents: { browser: true }\n")(func(s string) string {
			s = edit(s)
			return strings.Replace(s, "routes: [shop/api/]", "routes: [/api/]", 1)
		}(docsExample)))
		found := false
		for _, p := range s.Check() {
			if p.Path == "buckets.documents.browser" && strings.Contains(p.Msg, "browsers reach the application on the gateway's hosts") {
				found = true
			}
		}
		if !found {
			t.Errorf("a browser bucket without hosts passed: %v", Problems(s.Check()))
		}
	}
}
