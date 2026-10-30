package edge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/duncan-G/protostrap/internal/probe"
)

func services() []Service {
	return []Service{
		{Name: "api", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Path: "/api/"}, {Path: "/sample.Api/"}}},
		{Name: "web", Port: 3000, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Path: "/web/"}, {Host: "admin", Path: "/"}}},
		{Name: "py", Port: 8000, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Path: "/py/", Rate: 200}}},
	}
}

// hosts are the account's hostnames for the hosts the routes name.
var hosts = map[string]string{"admin": "admin.example.com"}

func config(s []Service) Routing { return Routing{Services: s, Hosts: hosts} }

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return v
}

func TestFilesAreTheListenerAndClusters(t *testing.T) {
	files := Files(config(services()))
	if len(files) != 2 || files["edge/lds.json"] == "" || files["edge/cds.json"] == "" {
		t.Fatalf("files %v", keys(files))
	}
	decode(t, files["edge/lds.json"])
	decode(t, files["edge/cds.json"])
}

func TestFilesAreDeterministic(t *testing.T) {
	a, b := Files(config(services())), Files(config(services()))
	for k := range a {
		if a[k] != b[k] {
			t.Errorf("%s differs between two renders: Envoy would reload for nothing", k)
		}
	}
}

func TestClusters(t *testing.T) {
	cds := Files(config(services()))["edge/cds.json"]
	for _, want := range []string{
		`"name": "api"`, `"envoy.cluster.dns"`, `"type.googleapis.com/envoy.extensions.clusters.dns.v3.DnsCluster"`,
		`"dns_refresh_rate": "5s"`, `"respect_dns_ttl": false`, `"dns_lookup_family": "V4_ONLY"`,
		`"all_addresses_in_single_endpoint": false`,
		`"address": "api"`, `"port_value": 8080`, `"service_name": "api"`, `"http2_protocol_options": {}`,
		`"path": "/healthz"`, `"host": "web"`, `"port_value": 3000`,
	} {
		if !strings.Contains(cds, want) {
			t.Errorf("clusters lack %s", want)
		}
	}
	// The DNS settings live in the DNS cluster extension. Envoy 1.39 warns that the Cluster's
	// own fields will be removed, so a newer Envoy may refuse them.
	v := decode(t, cds)
	for _, r := range v["resources"].([]any) {
		c := r.(map[string]any)
		for _, deprecated := range []string{"type", "dns_refresh_rate", "respect_dns_ttl", "dns_lookup_family"} {
			if _, ok := c[deprecated]; ok {
				t.Errorf("cluster %s sets the deprecated field %s", c["name"], deprecated)
			}
		}
	}
	// web isn't gRPC, so it gets HTTP/1.1 upstream.
	for _, r := range v["resources"].([]any) {
		c := r.(map[string]any)
		opts, _ := json.Marshal(c["typed_extension_protocol_options"])
		if c["name"] == "web" && strings.Contains(string(opts), "http2") {
			t.Errorf("web gets HTTP/2 upstream")
		}
		if c["name"] == "api" && !strings.Contains(string(opts), "http2") {
			t.Errorf("api, a gRPC service, gets HTTP/1.1 upstream")
		}
	}
}

// The edge's check passes exactly the answers the agent's probe passes. Envoy on its own
// passes only 200, so a service whose check redirects would be ready to the agent and get
// no requests from the edge.
func TestHTTPCheckPassesWhatTheProbePasses(t *testing.T) {
	v := decode(t, Files(config(services()))["edge/cds.json"])
	for _, r := range v["resources"].([]any) {
		c := r.(map[string]any)
		hc := c["health_checks"].([]any)[0].(map[string]any)
		if _, ok := hc["http_health_check"]; !ok {
			continue
		}
		statuses, _ := hc["http_health_check"].(map[string]any)["expected_statuses"].([]any)
		if len(statuses) != 1 {
			t.Fatalf("%s: expected_statuses %v, want one range", c["name"], statuses)
		}
		rng := statuses[0].(map[string]any)
		start, end := int(rng["start"].(float64)), int(rng["end"].(float64))
		if start != probe.ReadyStatuses.Start || end != probe.ReadyStatuses.End {
			t.Errorf("%s: the edge passes %d up to %d, the probe %d up to %d", c["name"], start, end, probe.ReadyStatuses.Start, probe.ReadyStatuses.End)
		}
	}
}

// cluster is the named cluster from rendered clusters.
func cluster(t *testing.T, services []Service, name string) map[string]any {
	t.Helper()
	v := decode(t, Files(config(services))["edge/cds.json"])
	for _, r := range v["resources"].([]any) {
		if c := r.(map[string]any); c["name"] == name {
			return c
		}
	}
	t.Fatalf("no cluster %s", name)
	return nil
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// A check that names its own port runs there: Keycloak answers requests on 8080 and its
// health on 9000.
func TestCheckOnAnotherPort(t *testing.T) {
	c := cluster(t, []Service{{Name: "keycloak", Port: 8080, Ready: "9000/health/ready", Routes: []Route{{Path: "/realms/"}}}}, "keycloak")
	got := jsonOf(c)
	for _, want := range []string{`"port_value":8080`, `"health_check_config":{"port_value":9000}`, `"path":"/health/ready"`} {
		if !strings.Contains(got, want) {
			t.Errorf("cluster lacks %s:\n%s", want, got)
		}
	}
	c = cluster(t, []Service{{Name: "api", Port: 8080, Ready: "grpc:9001", Protocol: "http2", Routes: []Route{{Path: "/api/"}}}}, "api")
	if got := jsonOf(c); !strings.Contains(got, `"health_check_config":{"port_value":9001}`) || !strings.Contains(got, `"grpc_health_check"`) {
		t.Errorf("gRPC check on 9001 missing:\n%s", got)
	}
}

func TestTCPCheck(t *testing.T) {
	c := cluster(t, []Service{{Name: "raw", Port: 7000, Ready: "tcp", Routes: []Route{{Path: "/raw/"}}}}, "raw")
	hc := c["health_checks"].([]any)[0].(map[string]any)
	if _, ok := hc["tcp_health_check"]; !ok {
		t.Errorf("health check %v", hc)
	}
}

// What the edge speaks to a service is its protocol, whatever its check: an HTTP/2 service
// with an HTTP check gets HTTP/2, and its check is HTTP/2 too, on its own port.
func TestProtocolIsNotTheCheck(t *testing.T) {
	c := cluster(t, []Service{{Name: "web", Port: 8080, Ready: "/healthz", Protocol: "http2", Routes: []Route{{Path: "/"}}}}, "web")
	got := jsonOf(c)
	if !strings.Contains(got, `"http2_protocol_options":{}`) {
		t.Errorf("an http2 service gets HTTP/1.1:\n%s", got)
	}
	if !strings.Contains(got, `"codec_client_type":"HTTP2"`) {
		t.Errorf("its HTTP check on its own port should speak HTTP/2:\n%s", got)
	}
	c = cluster(t, []Service{{Name: "web", Port: 8080, Ready: "9000/healthz", Protocol: "http2", Routes: []Route{{Path: "/"}}}}, "web")
	if got := jsonOf(c); strings.Contains(got, `"codec_client_type"`) {
		t.Errorf("a check on another port speaks HTTP/1.1:\n%s", got)
	}
	c = cluster(t, []Service{{Name: "gw", Port: 8080, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Path: "/"}}}}, "gw")
	if got := jsonOf(c); strings.Contains(got, "http2") {
		t.Errorf("an http1 service gets HTTP/2:\n%s", got)
	}
}

// Browsers' gRPC-Web is translated only on routes to grpc services. Any other service, such
// as an application's own gateway, gets it as sent: translated to gRPC and sent over
// HTTP/1.1, its trailers, and with them every call's status, would be lost.
func TestGRPCWebIsTranslatedOnlyForGRPCServices(t *testing.T) {
	services := []Service{
		{Name: "api", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Path: "/api/"}}},
		{Name: "gw", Port: 8080, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Path: "/"}}},
		{Name: "h2", Port: 8080, Ready: "/healthz", Protocol: "http2", Routes: []Route{{Path: "/h2/"}}},
	}
	v := decode(t, Files(config(services))["edge/lds.json"])
	listener := v["resources"].([]any)[0].(map[string]any)
	hcm := listener["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	for _, vh := range hcm["route_config"].(map[string]any)["virtual_hosts"].([]any) {
		for _, r := range vh.(map[string]any)["routes"].([]any) {
			route := r.(map[string]any)
			cl := route["route"].(map[string]any)["cluster"].(string)
			disabled := strings.Contains(jsonOf(route["typed_per_filter_config"]), `"envoy.filters.http.grpc_web":{"@type":"type.googleapis.com/envoy.config.route.v3.FilterConfig","disabled":true}`)
			if want := cl != "api"; disabled != want {
				t.Errorf("route to %s: gRPC-Web translation off = %v, want %v", cl, disabled, want)
			}
		}
	}
}

func TestRoutes(t *testing.T) {
	lds := Files(config(services()))["edge/lds.json"]
	for _, want := range []string{
		`"port_value": 8080`, `"address": "127.0.0.1"`, // Only cloudflared can reach the listener.
		`"prefix": "/api/"`, `"prefix": "/sample.Api/"`, `"prefix": "/web/"`, `"prefix": "/py/"`,
		`"cluster": "api"`, `"envoy.filters.http.grpc_web"`, `"connect-failure,refused-stream,reset-before-request"`,
	} {
		if !strings.Contains(lds, want) {
			t.Errorf("listener lacks %s\n", want)
		}
	}
	// The rate limit sits on py's route alone, 200 a second, in both virtual hosts it is in.
	if !strings.Contains(lds, `"max_tokens": 200`) || strings.Count(lds, `"max_tokens"`) != 2 {
		t.Errorf("py's rate limit is missing or on other routes")
	}
}

func TestPathOnlyRoutesMatchEveryHost(t *testing.T) {
	v := decode(t, Files(config(services()))["edge/lds.json"])
	hosts := virtualHosts(t, v)
	admin, any := hosts["admin.example.com"], hosts["*"]
	if admin == nil || any == nil {
		t.Fatalf("virtual hosts %v", hosts)
	}
	// admin.example.com has its own route to web as well as every path-only route, all
	// longest first.
	if got := strings.Join(admin, " "); got != "/sample.Api/ /api/ /web/ /py/ /" {
		t.Errorf("admin.example.com routes %q", got)
	}
	if got := strings.Join(any, " "); got != "/sample.Api/ /api/ /web/ /py/" {
		t.Errorf("* routes %q", got)
	}
}

func virtualHosts(t *testing.T, lds map[string]any) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	listener := lds["resources"].([]any)[0].(map[string]any)
	chain := listener["filter_chains"].([]any)[0].(map[string]any)
	hcm := chain["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	for _, vh := range hcm["route_config"].(map[string]any)["virtual_hosts"].([]any) {
		m := vh.(map[string]any)
		domain := m["domains"].([]any)[0].(string)
		for _, r := range m["routes"].([]any) {
			out[domain] = append(out[domain], r.(map[string]any)["match"].(map[string]any)["prefix"].(string))
		}
	}
	return out
}

func TestNoServicesStillMakesValidFiles(t *testing.T) {
	files := Files(Routing{})
	decode(t, files["edge/lds.json"])
	if v := decode(t, files["edge/cds.json"]); len(v["resources"].([]any)) != 0 {
		t.Errorf("clusters without services: %v", v)
	}
}

func TestRequestsMetric(t *testing.T) {
	if got := RequestsMetric("api"); got != `envoy_cluster_upstream_rq_total{envoy_cluster_name="api"}` {
		t.Errorf("RequestsMetric = %q", got)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A route names its host, and the account's hostname for that name is the virtual host's
// domain, so the same spec serves other hostnames in another account.
func TestRoutesFindTheirHostnameInTheAccount(t *testing.T) {
	staging := Routing{Services: services(), Hosts: map[string]string{"admin": "admin.staging.example.com"}}
	got := virtualHosts(t, decode(t, Files(staging)["edge/lds.json"]))
	if _, ok := got["admin.staging.example.com"]; !ok {
		t.Errorf("virtual hosts %v, want admin.staging.example.com", got)
	}
	if _, ok := got["admin.example.com"]; ok {
		t.Errorf("the spec's hostname is served in an account that gives admin another")
	}
}

// A route to a host the account has no hostname for has nowhere to go. protostrap check
// refuses such a spec; the edge leaves the route out rather than guess.
func TestARouteToAnUnknownHostIsLeftOut(t *testing.T) {
	got := virtualHosts(t, decode(t, Files(Routing{Services: services()})["edge/lds.json"]))
	if len(got) != 1 || got["*"] == nil {
		t.Errorf("virtual hosts %v, want only *", got)
	}
}
