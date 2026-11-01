package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// authorized is an api whose routes auth authorizes, and auth, routed on /auth/ itself.
func authorized() []Service {
	return []Service{
		{Name: "api", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Host: "admin", Path: "/api/"}},
			Authorize: &Authorize{By: "auth", Path: "/auth/check", Headers: []string{"x-identity"}, Forward: []string{"cookie"}}},
		{Name: "auth", Port: 8080, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Host: "admin", Path: "/auth/"}}},
		{Name: "web", Port: 4000, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Host: "admin", Path: "/"}},
			Authorize: &Authorize{By: "auth", Path: "/", Headers: []string{"x-identity"}, Forward: []string{"cookie"}}},
	}
}

func listenerOf(t *testing.T, r Routing) (filters []map[string]any, routes map[string]map[string]any) {
	t.Helper()
	v := decode(t, Files(r)["edge/lds.json"])
	hcm := v["resources"].([]any)[0].(map[string]any)["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	for _, f := range hcm["http_filters"].([]any) {
		filters = append(filters, f.(map[string]any))
	}
	routes = map[string]map[string]any{}
	for _, vh := range hcm["route_config"].(map[string]any)["virtual_hosts"].([]any) {
		for _, rt := range vh.(map[string]any)["routes"].([]any) {
			route := rt.(map[string]any)
			routes[vh.(map[string]any)["name"].(string)+matchOf(route)] = route
		}
	}
	return filters, routes
}

// The edge asks the authorizer about each request to an authorized service, on that
// service's routes only, after removing the headers the authorizer answers with, so a
// client can't send them itself.
func TestAuthorizedRoutesAskTheAuthorizerFirst(t *testing.T) {
	filters, routes := listenerOf(t, Routing{Services: authorized(), Hosts: map[string]string{"admin": "admin.example.com"}})

	var names []string
	for _, f := range filters {
		names = append(names, f["name"].(string))
	}
	want := "envoy.filters.http.local_ratelimit envoy.filters.http.header_mutation envoy.filters.http.ext_authz/api envoy.filters.http.ext_authz/web envoy.filters.http.grpc_web envoy.filters.http.router"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("filters\n%s\nwant\n%s", got, want)
	}
	authz := jsonOf(filters[2])
	for _, want := range []string{
		`"disabled":true`, `"uri":"http://auth:8080"`, `"cluster":"auth"`, `"path_prefix":"/auth/check"`,
		`"allowed_upstream_headers":{"patterns":[{"exact":"x-identity","ignore_case":true}]}`,
		`"failure_mode_allow":false`, `"status_on_error":{"code":"ServiceUnavailable"}`,
	} {
		if !strings.Contains(authz, want) {
			t.Errorf("api's ext_authz lacks %s:\n%s", want, authz)
		}
	}
	// The headers sent to the authorizer are the filter's own allowed_headers: the http_service's
	// authorization_request.allowed_headers is deprecated, and Envoy warns about it on every load.
	typed := filters[2]["typed_config"].(map[string]any)
	if got := jsonOf(typed["allowed_headers"]); got != `{"patterns":[{"exact":"cookie","ignore_case":true}]}` {
		t.Errorf("api's ext_authz sends the authorizer %s", got)
	}
	if strings.Contains(authz, "authorization_request") {
		t.Errorf("api's ext_authz uses the deprecated authorization_request:\n%s", authz)
	}
	// A path of / asks for the request's own path, with no prefix.
	if web := jsonOf(filters[3]); strings.Contains(web, "path_prefix") {
		t.Errorf("web's ext_authz has a path prefix:\n%s", web)
	}

	api := jsonOf(routes["admin.example.com/api/"]["typed_per_filter_config"])
	for _, want := range []string{`"envoy.filters.http.ext_authz/api"`, `"check_settings":{}`, `"request_mutations":[{"remove":"x-identity"}]`} {
		if !strings.Contains(api, want) {
			t.Errorf("api's route lacks %s:\n%s", want, api)
		}
	}
	if strings.Contains(api, "ext_authz/web") {
		t.Errorf("api's route is checked for web too:\n%s", api)
	}
	if auth := jsonOf(routes["admin.example.com/auth/"]["typed_per_filter_config"]); strings.Contains(auth, "ext_authz") || strings.Contains(auth, "header_mutation") {
		t.Errorf("auth's own routes are authorized:\n%s", auth)
	}
}

// An authorizer that's forwarded no headers is sent only those Envoy always sends an HTTP
// authorizer, Authorization among them: no allowed_headers at all, since an empty list is no
// list to Envoy.
func TestAnAuthorizerForwardedNothingGetsEnvoysDefaults(t *testing.T) {
	s := authorized()
	s[0].Authorize.Forward = nil
	filters, _ := listenerOf(t, Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}})
	if authz := jsonOf(filters[2]); strings.Contains(authz, "allowed_headers\"") || strings.Contains(authz, "authorization_request") {
		t.Errorf("api's ext_authz names headers to send:\n%s", authz)
	}
}

// An authorizer's 200 can carry headers to the client too, such as a session cookie it
// renewed: the edge adds those named in respond to the service's response, matched whatever
// their case. A refusal is the client's response as it is, so it stays as it was.
func TestTheAuthorizersAnswerCanReachTheClient(t *testing.T) {
	s := authorized()
	s[0].Authorize.Respond = []string{"set-cookie", "x-session-expires"}
	filters, routes := listenerOf(t, Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}})

	httpService := func(f map[string]any) map[string]any {
		return f["typed_config"].(map[string]any)["http_service"].(map[string]any)
	}
	want := `{"allowed_client_headers_on_success":{"patterns":[{"exact":"set-cookie","ignore_case":true},{"exact":"x-session-expires","ignore_case":true}]},` +
		`"allowed_upstream_headers":{"patterns":[{"exact":"x-identity","ignore_case":true}]}}`
	if got := jsonOf(httpService(filters[2])["authorization_response"]); got != want {
		t.Errorf("api's authorization_response\n%s\nwant\n%s", got, want)
	}
	// web's authorizer names nothing for the client.
	if got := jsonOf(httpService(filters[3])["authorization_response"]); strings.Contains(got, "client") {
		t.Errorf("web's authorization_response passes the client %s", got)
	}
	// The client's own copies are removed only of the headers that go to the service.
	if got := jsonOf(routes["admin.example.com/api/"]["typed_per_filter_config"].(map[string]any)[headerMutation]); !strings.Contains(got, `"request_mutations":[{"remove":"x-identity"}]`) {
		t.Errorf("api's route removes %s", got)
	}
}

// respond adds allowed_client_headers_on_success and nothing else: the listener without it is
// the listener without respond, byte for byte, and the clusters don't change.
func TestRespondChangesNothingElse(t *testing.T) {
	s := authorized()
	s[0].Authorize.Respond = []string{"set-cookie"}
	with := Files(Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}})
	without := Files(Routing{Services: authorized(), Hosts: map[string]string{"admin": "admin.example.com"}})

	lds := decode(t, with["edge/lds.json"])
	hcm := lds["resources"].([]any)[0].(map[string]any)["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	response := hcm["http_filters"].([]any)[2].(map[string]any)["typed_config"].(map[string]any)["http_service"].(map[string]any)["authorization_response"].(map[string]any)
	delete(response, "allowed_client_headers_on_success")
	if got, want := jsonOf(lds), jsonOf(decode(t, without["edge/lds.json"])); got != want {
		t.Errorf("respond changed more than allowed_client_headers_on_success:\n%s\nwant\n%s", got, want)
	}
	if with["edge/cds.json"] != without["edge/cds.json"] {
		t.Errorf("respond changed the clusters")
	}
}

// An authorizer that's to respond with nothing renders as one with no respond at all, so a spec
// written before respond renders the same edge: authorization_response holds only the
// headers for the service.
func TestNothingToRespondRendersAsBefore(t *testing.T) {
	empty := authorized()
	empty[0].Authorize.Respond = []string{}
	r := Routing{Services: authorized(), Hosts: map[string]string{"admin": "admin.example.com"}}
	if got, want := Files(Routing{Services: empty, Hosts: r.Hosts}), Files(r); got["edge/lds.json"] != want["edge/lds.json"] || got["edge/cds.json"] != want["edge/cds.json"] {
		t.Errorf("an empty respond renders differently from none")
	}
	filters, _ := listenerOf(t, r)
	for _, f := range filters[2:4] {
		got := jsonOf(f["typed_config"].(map[string]any)["http_service"].(map[string]any)["authorization_response"])
		if want := `{"allowed_upstream_headers":{"patterns":[{"exact":"x-identity","ignore_case":true}]}}`; got != want {
			t.Errorf("%s's authorization_response\n%s\nwant\n%s", f["name"], got, want)
		}
	}
}

// An open authorizer's request goes on when it can't answer: no ready replica, no connection,
// no answer in time, or a 5xx. Another's still gets the client a 503.
func TestAnOpenAuthorizerLetsThroughWhatItCantAnswer(t *testing.T) {
	s := authorized()
	s[0].Authorize.Open = true
	filters, routes := listenerOf(t, Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}})
	if api := jsonOf(filters[2]); !strings.Contains(api, `"failure_mode_allow":true`) || strings.Contains(api, "status_on_error") {
		t.Errorf("api's open ext_authz:\n%s", api)
	}
	if web := jsonOf(filters[3]); !strings.Contains(web, `"failure_mode_allow":false`) || !strings.Contains(web, `"status_on_error":{"code":"ServiceUnavailable"}`) {
		t.Errorf("web's ext_authz:\n%s", web)
	}
	// The header mutation before ext_authz removes the client's copies of the authorizer's
	// headers, so a request that goes on unanswered has none.
	if got := jsonOf(routes["admin.example.com/api/"]["typed_per_filter_config"]); !strings.Contains(got, `"request_mutations":[{"remove":"x-identity"}]`) {
		t.Errorf("api's route: %s", got)
	}
}

// The edge waits for an authorizer as long as its timeout, written as Envoy writes a
// duration, or 2s.
func TestAnAuthorizersTimeout(t *testing.T) {
	for timeout, want := range map[string]string{"": "2s", "500ms": "0.5s", "1500ms": "1.5s", "5s": "5s"} {
		s := authorized()
		s[0].Authorize.Timeout = timeout
		filters, _ := listenerOf(t, Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}})
		uri := filters[2]["typed_config"].(map[string]any)["http_service"].(map[string]any)["server_uri"].(map[string]any)
		if uri["timeout"] != want {
			t.Errorf("timeout %q renders as %v, want %s", timeout, uri["timeout"], want)
		}
	}
}

// An authorizer that's closed, waits 2s and skips nothing renders as one that doesn't say, so
// a spec written before open, timeout and skip renders the same edge.
func TestTheDefaultsRenderAsBefore(t *testing.T) {
	r := Routing{Services: authorized(), Hosts: map[string]string{"admin": "admin.example.com"}}
	same := authorized()
	same[0].Authorize.Timeout, same[0].Authorize.Skip = "2s", []string{}
	if got, want := Files(Routing{Services: same, Hosts: r.Hosts}), Files(r); got["edge/lds.json"] != want["edge/lds.json"] || got["edge/cds.json"] != want["edge/cds.json"] {
		t.Errorf("the defaults render differently from none")
	}
}

// adminConsoles are services as ProtoFast's admin host has them: clients on /, whose static
// files the authorizer isn't asked about, and console on /theplot/, authorized throughout.
func adminConsoles() []Service {
	return []Service{
		{Name: "auth", Port: 8080, Ready: "/healthz", Protocol: "http1"},
		{Name: "clients", Port: 4000, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Host: "admin", Path: "/", Rate: 100}},
			Authorize: &Authorize{By: "auth", Path: "/check", Headers: []string{"x-identity"}, Skip: []string{"*.png", "/assets/", "*.js"}}},
		{Name: "console", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Host: "admin", Path: "/theplot/"}},
			Authorize: &Authorize{By: "auth", Path: "/check", Headers: []string{"x-identity"}}},
	}
}

// A skipped prefix is a route of its own, and a route's skipped extensions one pattern, just
// before the route and after every longer prefix, so /theplot/x.js is still console's, and
// authorized, while /x.js and /assets/a.png are clients' and not. Each is the route it's
// under, without asking the authorizer: the client's copies of the authorizer's headers are
// still removed, and its cluster, timeout, retries, rate limit and gRPC-Web are the route's.
func TestSkippedRequestsAreRoutedButNotAuthorized(t *testing.T) {
	r := Routing{Services: adminConsoles(), Hosts: hosts}
	if got, want := strings.Join(virtualHosts(t, decode(t, Files(r)["edge/lds.json"]))["admin.example.com"], " "), `/theplot/ /assets/ ~^/[^?]*\.(?:js|png)$ /`; got != want {
		t.Errorf("admin.example.com routes\n%s\nwant\n%s", got, want)
	}
	_, routes := listenerOf(t, r)
	asRoute := func(route map[string]any) string {
		c := maps.Clone(route)
		delete(c, "match")
		perFilter := maps.Clone(c["typed_per_filter_config"].(map[string]any))
		delete(perFilter, authzFilter("clients"))
		c["typed_per_filter_config"] = perFilter
		return jsonOf(c)
	}
	root := routes["admin.example.com/"]
	if _, ok := root["typed_per_filter_config"].(map[string]any)[authzFilter("clients")]; !ok {
		t.Errorf("clients' / isn't authorized")
	}
	for _, at := range []string{"admin.example.com/assets/", `admin.example.com~^/[^?]*\.(?:js|png)$`} {
		skip := routes[at]
		if skip == nil {
			t.Fatalf("no route %s", at)
		}
		if _, ok := skip["typed_per_filter_config"].(map[string]any)[authzFilter("clients")]; ok {
			t.Errorf("%s asks the authorizer", at)
		}
		if got, want := asRoute(skip), asRoute(root); got != want || !strings.Contains(got, `"request_mutations":[{"remove":"x-identity"}]`) {
			t.Errorf("%s isn't clients' / without the authorizer:\n%s\nwant\n%s", at, got, want)
		}
	}
	if _, ok := routes["admin.example.com/theplot/"]["typed_per_filter_config"].(map[string]any)[authzFilter("console")]; !ok {
		t.Errorf("console's /theplot/ isn't authorized")
	}
}

// A skipped prefix under a longer route of another service, or that is another service's own
// route, isn't skipped: its requests are that service's. A longer route under it keeps its
// requests.
func TestASkippedPrefixNeverTakesAnotherServicesRequests(t *testing.T) {
	s := adminConsoles()
	s[1].Authorize.Skip = []string{"/theplot/assets/", "/assets/", "/static/"}
	s = append(s,
		Service{Name: "files", Port: 8080, Ready: "/healthz", Routes: []Route{{Host: "admin", Path: "/assets/admin/"}}},
		Service{Name: "cdn", Port: 8080, Ready: "/healthz", Routes: []Route{{Path: "/static/"}}})
	r := Routing{Services: s, Hosts: hosts}
	got := virtualHosts(t, decode(t, Files(r)["edge/lds.json"]))
	if got, want := strings.Join(got["admin.example.com"], " "), "/assets/admin/ /theplot/ /assets/ /static/ /"; got != want {
		t.Errorf("admin.example.com routes\n%s\nwant\n%s", got, want)
	}
	if got, want := strings.Join(got["*"], " "), "/static/"; got != want {
		t.Errorf("* routes %s, want %s", got, want)
	}
	_, routes := listenerOf(t, r)
	for at, want := range map[string]string{"admin.example.com/assets/admin/": "files", "admin.example.com/assets/": "clients", "admin.example.com/static/": "cdn"} {
		if got := routes[at]["route"].(map[string]any)["cluster"]; got != want {
			t.Errorf("%s goes to %v, want %s", at, got, want)
		}
	}
}

// A route's skipped extensions are one pattern under the route's path, escaped, with each
// extension once, in each host the route is in. A prefix can be the whole route.
func TestEachRouteHasItsOwnPattern(t *testing.T) {
	s := []Service{
		{Name: "api", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Path: "/sample.Api/"}, {Host: "admin", Path: "/docs/"}},
			Authorize: &Authorize{By: "auth", Skip: []string{"*.png", "*.js", "*.png", "/docs/img/", "/sample.Api/"}}},
		{Name: "auth", Port: 8080, Ready: "/healthz"},
	}
	r := Routing{Services: s, Hosts: hosts}
	got := virtualHosts(t, decode(t, Files(r)["edge/lds.json"]))
	if got, want := strings.Join(got["admin.example.com"], " "),
		`/sample.Api/ ~^/sample\.Api/[^?]*\.(?:js|png)$ /sample.Api/ /docs/img/ ~^/docs/[^?]*\.(?:js|png)$ /docs/`; got != want {
		t.Errorf("admin.example.com routes\n%s\nwant\n%s", got, want)
	}
	if got, want := strings.Join(got["*"], " "), `/sample.Api/ ~^/sample\.Api/[^?]*\.(?:js|png)$ /sample.Api/`; got != want {
		t.Errorf("* routes\n%s\nwant\n%s", got, want)
	}
	every := hostRoutes(t, r, "*")
	if whole, route := jsonOf(every[0]), jsonOf(every[2]); strings.Contains(whole, "ext_authz") || !strings.Contains(route, "ext_authz/api") {
		t.Errorf("/sample.Api/ skipped whole asks the authorizer, or the route behind it doesn't:\n%s\n%s", whole, route)
	}
}

// hostRoutes are a virtual host's routes, in order.
func hostRoutes(t *testing.T, r Routing, host string) []map[string]any {
	t.Helper()
	v := decode(t, Files(r)["edge/lds.json"])
	hcm := v["resources"].([]any)[0].(map[string]any)["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	var routes []map[string]any
	for _, vh := range hcm["route_config"].(map[string]any)["virtual_hosts"].([]any) {
		if vh.(map[string]any)["name"] == host {
			for _, rt := range vh.(map[string]any)["routes"].([]any) {
				routes = append(routes, rt.(map[string]any))
			}
		}
	}
	return routes
}

// Without an authorized service, the listener has no ext_authz or header_mutation filters.
func TestNoAuthorizerNoFilter(t *testing.T) {
	filters, _ := listenerOf(t, config(services()))
	for _, f := range filters {
		if name := f["name"].(string); strings.Contains(name, "ext_authz") || strings.Contains(name, "header_mutation") {
			t.Errorf("filter %s without an authorized service", name)
		}
	}
}

// An authorizer the edge doesn't route to still needs a cluster for the edge to ask it.
func TestAnUnroutedAuthorizerHasACluster(t *testing.T) {
	s := authorized()
	s[1].Routes = nil
	cluster(t, s, "auth")
}

// PROTOSTRAP_ENVOY_TEST=1 has Envoy itself, the edge's version, validate the listener and
// clusters, with authorization and every other filter, through Docker.
func TestEnvoyAcceptsTheConfig(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to validate with Envoy in Docker")
	}
	all := append(authorized(), services()[2]) // py, with a rate limit.
	all[0].Authorize.Respond = []string{"set-cookie"}
	all[0].Authorize.Open, all[0].Authorize.Timeout = true, "500ms"
	// web skips 30 extensions of 10 characters, as many as protostrap check allows, sharing
	// no prefix, so RE2 can't shorten its pattern.
	all[2].Authorize.Skip = []string{"/assets/"}
	for i := range 30 {
		all[2].Authorize.Skip = append(all[2].Authorize.Skip, fmt.Sprintf("*.%c%09d", "abcdefghijklmnopqrstuvwxyz0123"[i], i))
	}
	envoyValidates(t, Files(Routing{Services: all, Hosts: map[string]string{"admin": "admin.example.com"}}))
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of an authorizer whose 200
// sets two cookies and a header for the service, and a service that sets a cookie of its own.
// Envoy adds each header respond names to the service's response as one of its own, so the
// client gets three Set-Cookie headers, none folded into another, and nothing respond doesn't
// name.
func TestEnvoyGivesTheClientEachCookieTheAuthorizerSets(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	r := Routing{
		Hosts: map[string]string{"admin": "admin.example.com"},
		Services: []Service{
			{Name: "api", Port: 9002, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Host: "admin", Path: "/api/"}}, Address: "127.0.0.1:9002",
				Authorize: &Authorize{By: "auth", Path: "/check", Headers: []string{"x-identity"}, Respond: []string{"set-cookie"}}},
			{Name: "auth", Port: 9001, Ready: "/healthz", Protocol: "http1", Address: "127.0.0.1:9001"},
		},
	}
	got := edgeAnswers(t, r, []map[string]any{
		stub("auth", 9001, answer("/", 200, [2]string{"set-cookie", "session=renewed; Path=/; HttpOnly"}, [2]string{"set-cookie", "csrf=t1; Path=/"}, [2]string{"x-identity", "alice"})),
		stub("api", 9002, answer("/", 200, [2]string{"set-cookie", "theme=dark; Path=/"})),
	}, []string{"api", "auth"}, "/api/orders")[0]
	if got.status != 200 {
		t.Fatalf("the edge answered\n%s", got.head)
	}
	if got.header.Get("x-identity") != "" {
		t.Errorf("the client got x-identity, which respond doesn't name:\n%s", got.head)
	}
	// The authorizer's come after the service's, so a browser keeps the authorizer's of a
	// cookie both set.
	if cookies, want := got.header.Values("set-cookie"), []string{"theme=dark; Path=/", "session=renewed; Path=/; HttpOnly", "csrf=t1; Path=/"}; !slices.Equal(cookies, want) {
		t.Errorf("the client's cookies\n%q\nwant\n%q\n%s", cookies, want, got.head)
	}
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of open authorizers that
// can't answer: one with no ready replica, one slower than its timeout, and one that answers
// 500 with an identity. Each request goes on to the service, without the client's x-identity
// or the authorizer's, and the timeout is the authorizer's own. A closed authorizer with no
// ready replica gets the client a 503.
func TestEnvoySendsOnWhatAnOpenAuthorizerCantAnswer(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	headers := []string{"x-identity"}
	service := func(name string, a *Authorize) Service {
		return Service{Name: name, Port: 9010, Ready: "/healthz", Protocol: "http1", Address: "127.0.0.1:9010",
			Routes: []Route{{Host: "admin", Path: "/" + name + "/"}}, Authorize: a}
	}
	authorizer := func(name string, port int) Service {
		return Service{Name: name, Port: port, Ready: "/healthz", Protocol: "http1", Address: "127.0.0.1:" + strconv.Itoa(port)}
	}
	sleepy := authorizer("sleepy", 9002)
	sleepy.HealthPort = 9003
	r := Routing{
		Hosts: map[string]string{"admin": "admin.example.com"},
		Services: []Service{
			service("open", &Authorize{By: "down", Headers: headers, Open: true}),
			service("closed", &Authorize{By: "down", Headers: headers}),
			service("slow", &Authorize{By: "sleepy", Headers: headers, Open: true, Timeout: "300ms"}),
			service("broken", &Authorize{By: "failing", Headers: headers, Open: true}),
			// Nothing listens for down.
			authorizer("down", 9001), sleepy, authorizer("failing", 9004),
		},
	}
	answers := edgeAnswers(t, r, []map[string]any{
		echo("service", 9010),
		// sleepy passes its checks on another port, and answers the edge 3s late.
		slowly(stub("sleepy", 9002, answer("/", 200, [2]string{"x-identity", "alice"})), "3s"),
		stub("sleepy-checks", 9003, answer("/", 200)),
		stub("failing", 9004, answer("/healthz", 200), answer("/", 500, [2]string{"x-identity", "alice"})),
	}, []string{"open", "closed", "slow", "broken", "sleepy", "failing"}, "/open/x", "/closed/x", "/slow/x", "/broken/x")

	for i, path := range []string{"/open/x", "/slow/x", "/broken/x"} {
		got := answers[[]int{0, 2, 3}[i]]
		if got.status != 200 || got.header.Get("x-seen-path") != path || got.header.Get("x-seen-identity") != "" {
			t.Errorf("%s reached the service with x-identity %q, or not at all:\n%s", path, got.header.Get("x-seen-identity"), got.head)
		}
	}
	if got := answers[1]; got.status != 503 || got.header.Get("x-seen-path") != "" {
		t.Errorf("/closed/x, whose authorizer is closed, answered\n%s", got.head)
	}
	// The default of 2s would have the request wait that long.
	if got := answers[2].elapsed; got > 1500*time.Millisecond {
		t.Errorf("/slow/x waited %v for an authorizer whose timeout is 300ms", got)
	}
}

// adminEdge is adminConsoles under Docker: clients and console are echoes, and auth lets each
// request through with the path it was asked for as x-identity.
func adminEdge() (Routing, []map[string]any) {
	s := adminConsoles()
	s[0].Port, s[0].Address = 9001, "127.0.0.1:9001"
	s[1].Address = "127.0.0.1:9010"
	s[2].Ready, s[2].Protocol, s[2].Address = "/healthz", "http1", "127.0.0.1:9011"
	return Routing{Services: s, Hosts: map[string]string{"admin": "admin.example.com"}}, []map[string]any{
		stub("auth", 9001, answer("/", 200, [2]string{"x-identity", "asked:%REQ(:PATH)%"})),
		echo("clients", 9010),
		echo("console", 9011),
	}
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of adminEdge. A request
// for a skipped prefix or extension of clients' reaches clients without the authorizer asked,
// and without the client's x-identity. /theplot/x.js is console's, which skips nothing, so the
// authorizer is asked about it.
func TestEnvoySkipsTheAuthorizerForStaticFiles(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	r, stubs := adminEdge()
	want := []struct{ path, service, identity string }{
		{"/theplot/x.js", "console", "asked:/check/theplot/x.js"},
		{"/x.js", "clients", ""},
		// Envoy matches the pattern against the path without its query.
		{"/x.js?v=1", "clients", ""},
		{"/app/logo.png", "clients", ""},
		{"/assets/a.css", "clients", ""},
		{"/assets/", "clients", ""},
		{"/", "clients", "asked:/check/"},
		{"/app/x.css", "clients", "asked:/check/app/x.css"},
		{"/X.JS", "clients", "asked:/check/X.JS"},
		{"/theplot/assets/a.png", "console", "asked:/check/theplot/assets/a.png"},
	}
	var paths []string
	for _, w := range want {
		paths = append(paths, w.path)
	}
	for i, got := range edgeAnswers(t, r, stubs, []string{"auth", "clients", "console"}, paths...) {
		w := want[i]
		if got.status != 200 || got.header.Get("x-service") != w.service || got.header.Get("x-seen-identity") != w.identity {
			t.Errorf("%s reached %q with x-identity %q, want %s with %q:\n%s", w.path, got.header.Get("x-service"), got.header.Get("x-seen-identity"), w.service, w.identity, got.head)
		}
	}
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of adminEdge. Each path is
// routed as RFC 3986 normalises it, and the authorizer is asked about, and the service gets,
// that same path, so neither /theplot/../x nor /assets/../theplot/x is asked about as a path
// it isn't. A %2F or %5C is redirected to a slash.
func TestEnvoyAsksAboutThePathItRoutes(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	r, stubs := adminEdge()
	want := []struct{ path, service, seen, identity string }{
		{"/theplot/../x", "clients", "/x", "asked:/check/x"},
		{"/theplot/%2e%2e/x", "clients", "/x", "asked:/check/x"},
		{"/theplot/%2E%2E/x", "clients", "/x", "asked:/check/x"},
		{"//theplot/x", "console", "/theplot/x", "asked:/check/theplot/x"},
		{"/theplot//x", "console", "/theplot/x", "asked:/check/theplot/x"},
		{"/theplot/./x", "console", "/theplot/x", "asked:/check/theplot/x"},
		{"/assets/../theplot/x", "console", "/theplot/x", "asked:/check/theplot/x"},
		{"/assets/%2e%2e/theplot/x", "console", "/theplot/x", "asked:/check/theplot/x"},
		{"/theplot/../x.js", "clients", "/x.js", ""},
	}
	paths := []string{"/theplot%2Fx", "/theplot%5cx"}
	for _, w := range want {
		paths = append(paths, w.path)
	}
	answers := edgeAnswers(t, r, stubs, []string{"auth", "clients", "console"}, paths...)
	for _, got := range answers[:2] {
		if got.status != 307 || got.header.Get("location") != "/theplot/x" {
			t.Errorf("an escaped slash answered\n%s", got.head)
		}
	}
	for i, got := range answers[2:] {
		w := want[i]
		if got.status != 200 || got.header.Get("x-service") != w.service || got.header.Get("x-seen-path") != w.seen || got.header.Get("x-seen-identity") != w.identity {
			t.Errorf("%s reached %q as %q with x-identity %q, want %s as %s with %q:\n%s", w.path,
				got.header.Get("x-service"), got.header.Get("x-seen-path"), got.header.Get("x-seen-identity"), w.service, w.seen, w.identity, got.head)
		}
	}
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of an authorizer that
// answers 204, then 201: only a 200 lets a request through, and any other status is the
// client's response.
func TestEnvoyLetsThroughOnlyA200(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	r, _ := adminEdge()
	answers := edgeAnswers(t, r, []map[string]any{
		stub("auth", 9001, answer("/check/no-content", 204), answer("/check/created", 201), answer("/", 200)),
		echo("clients", 9010),
		echo("console", 9011),
	}, []string{"auth", "clients"}, "/no-content", "/created", "/")
	for i, status := range []int{204, 201} {
		if got := answers[i]; got.status != status || got.header.Get("x-service") != "" {
			t.Errorf("the authorizer's %d answered\n%s", status, got.head)
		}
	}
	if got := answers[2]; got.status != 200 || got.header.Get("x-service") != "clients" {
		t.Errorf("the authorizer's 200 answered\n%s", got.head)
	}
}

// stub is a listener of the test's Envoy that stands in for a service or an authorizer at
// 127.0.0.1:port, answering each request by the first of its routes it matches.
func stub(name string, port int, routes ...any) map[string]any {
	return obj{
		"name":    name,
		"address": obj{"socket_address": obj{"address": "127.0.0.1", "port_value": port}},
		"filter_chains": []any{obj{"filters": []any{obj{"name": "envoy.filters.network.http_connection_manager", "typed_config": obj{
			"@type":        "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
			"stat_prefix":  name,
			"route_config": obj{"virtual_hosts": []any{obj{"name": name, "domains": []any{"*"}, "routes": routes}}},
			"http_filters": []any{obj{"name": "envoy.filters.http.router", "typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
		}}}}},
	}
}

// answer is a stub's route: a request under prefix gets status, with headers whose values can
// name the request's, such as %REQ(:PATH)%.
func answer(prefix string, status int, headers ...[2]string) map[string]any {
	route := obj{"match": obj{"prefix": prefix}, "direct_response": obj{"status": status}}
	var add []any
	for _, h := range headers {
		add = append(add, obj{"header": obj{"key": h[0], "value": h[1]}, "append_action": "APPEND_IF_EXISTS_OR_ADD"})
	}
	if len(add) > 0 {
		route["response_headers_to_add"] = add
	}
	return route
}

// echo is a service's stub, which tells the client which service it is, and the path and the
// x-identity it got.
func echo(name string, port int) map[string]any {
	return stub(name, port, answer("/", 200, [2]string{"x-service", name}, [2]string{"x-seen-path", "%REQ(:PATH)%"},
		[2]string{"x-seen-identity", "%REQ(x-identity)%"}))
}

// slowly delays each of a stub's answers.
func slowly(stub map[string]any, delay string) map[string]any {
	hcm := stub["filter_chains"].([]any)[0].(map[string]any)["filters"].([]any)[0].(map[string]any)["typed_config"].(map[string]any)
	hcm["http_filters"] = append([]any{obj{"name": "envoy.filters.http.fault", "typed_config": obj{
		"@type": "type.googleapis.com/envoy.extensions.filters.http.fault.v3.HTTPFault",
		"delay": obj{"fixed_delay": delay, "percentage": obj{"numerator": 100}},
	}}}, hcm["http_filters"].([]any)...)
	return stub
}

// exchange is the edge's answer to one request.
type exchange struct {
	status int
	// head is the status line and headers as they came.
	head    string
	header  http.Header
	elapsed time.Duration
}

// edgeAnswers runs the edge's Envoy, rendered from r, in Docker, with stubs as more listeners
// of the same Envoy, and once each cluster in ready passes its checks, asks it for each path
// on admin.example.com, as a client that sends its own X-Identity.
func edgeAnswers(t *testing.T, r Routing, stubs []map[string]any, ready []string, paths ...string) []exchange {
	t.Helper()
	r.Listen = "127.0.0.1:10000"
	listeners, clusters := staticResources(t, Files(r))
	static, _ := json.Marshal(obj{
		"admin":            obj{"address": obj{"socket_address": obj{"address": "127.0.0.1", "port_value": 9901}}},
		"layered_runtime":  envoyRuntime,
		"static_resources": obj{"listeners": append(listeners, stubs...), "clusters": clusters},
	})
	quote := func(words []string) string {
		var out []string
		for _, w := range words {
			out = append(out, "'"+strings.ReplaceAll(w, "'", `'\''`)+"'")
		}
		return strings.Join(out, " ")
	}
	// The clusters' first checks fail before the stubs listen, and the next pass 5 seconds
	// later.
	script := `cat > /tmp/envoy.json
envoy -c /tmp/envoy.json --log-level error > /tmp/envoy.log 2>&1 &
get() {
  exec 3<>/dev/tcp/127.0.0.1/$1 &&
    printf 'GET %s HTTP/1.1\r\nHost: %s\r\nX-Identity: mallory\r\nConnection: close\r\n\r\n' "$2" "$3" >&3 && cat <&3
}
for i in $(seq 1 100); do
  clusters=$(get 9901 /clusters localhost 2> /dev/null) ready=1
  for c in ` + quote(ready) + `; do
    printf '%s' "$clusters" | grep -q "^$c::.*::health_flags::healthy" || ready=0
  done
  [ $ready = 1 ] && break
  sleep 0.2
done
[ $ready = 1 ] || { printf '%s\n' "$clusters"; cat /tmp/envoy.log; exit 1; }
for path in ` + quote(paths) + `; do
  start=$(date +%s%N)
  out=$(get 10000 "$path" admin.example.com)
  printf '=== %d\n%s\n' $((($(date +%s%N) - start) / 1000000)) "$out"
done`
	cmd := exec.Command("docker", "run", "--rm", "-i", "--entrypoint", "bash", EnvoyImage, "-c", script)
	cmd.Stdin = bytes.NewReader(static)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the edge's clusters never all passed their checks: %v\n%s", err, out)
	}
	var answers []exchange
	for _, part := range strings.Split(string(out), "=== ")[1:] {
		ms, response, _ := strings.Cut(part, "\n")
		head, _, _ := strings.Cut(response, "\r\n\r\n")
		lines := strings.Split(head, "\r\n")
		a := exchange{head: head, header: http.Header{}}
		if n, err := strconv.Atoi(ms); err == nil {
			a.elapsed = time.Duration(n) * time.Millisecond
		}
		if f := strings.Fields(lines[0]); len(f) > 1 {
			a.status, _ = strconv.Atoi(f[1])
		}
		for _, line := range lines[1:] {
			if name, value, ok := strings.Cut(line, ":"); ok {
				a.header.Add(name, strings.TrimSpace(value))
			}
		}
		answers = append(answers, a)
	}
	if len(answers) != len(paths) {
		t.Fatalf("%d answers to %d requests:\n%s", len(answers), len(paths), out)
	}
	return answers
}

// staticResources are the listener and clusters Files rendered, as Envoy's static resources.
func staticResources(t *testing.T, files map[string]string) (listeners, clusters []map[string]any) {
	t.Helper()
	var lds, cds struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal([]byte(files["edge/lds.json"]), &lds); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(files["edge/cds.json"]), &cds); err != nil {
		t.Fatal(err)
	}
	// A static resource is the message itself, without the @type of a dynamic one.
	for _, r := range append(lds.Resources, cds.Resources...) {
		delete(r, "@type")
	}
	return lds.Resources, cds.Resources
}

// envoyValidates has the edge's Envoy validate a listener and clusters Files rendered, as
// static resources: validate mode doesn't read the files a bootstrap points at.
func envoyValidates(t *testing.T, files map[string]string) {
	t.Helper()
	listeners, clusters := staticResources(t, files)
	static, _ := json.Marshal(map[string]any{
		"layered_runtime":  envoyRuntime,
		"static_resources": map[string]any{"listeners": listeners, "clusters": clusters},
	})
	// The config goes in on stdin: Docker Desktop shares only some of the host's directories.
	cmd := exec.Command("docker", "run", "--rm", "-i", "--entrypoint", "sh", EnvoyImage,
		"-c", "cat > /tmp/envoy.json && envoy --mode validate -c /tmp/envoy.json")
	cmd.Stdin = bytes.NewReader(static)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy refused the config: %v\n%s", err, out)
	}
	// A deprecated field is accepted with a warning, until an Envoy upgrade refuses it.
	if strings.Contains(string(out), "Deprecated field") {
		t.Errorf("envoy accepted the config with a deprecated field:\n%s", out)
	}
}
