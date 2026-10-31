package edge

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
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
			routes[vh.(map[string]any)["name"].(string)+route["match"].(map[string]any)["prefix"].(string)] = route
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

// An authorizer's 2xx can carry headers to the client too, such as a session cookie it
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
	envoyValidates(t, Files(Routing{Services: all, Hosts: map[string]string{"admin": "admin.example.com"}}))
}

// PROTOSTRAP_ENVOY_TEST=1 runs the edge's Envoy, in Docker, in front of an authorizer whose 2xx
// sets two cookies and a header for the service, and a service that sets a cookie of its own.
// Envoy adds each header respond names to the service's response as one of its own, so the
// client gets three Set-Cookie headers, none folded into another, and nothing respond doesn't
// name.
func TestEnvoyGivesTheClientEachCookieTheAuthorizerSets(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to run Envoy in Docker")
	}
	r := Routing{
		Listen: "127.0.0.1:10000",
		Hosts:  map[string]string{"admin": "admin.example.com"},
		Services: []Service{
			{Name: "api", Port: 9002, Ready: "/healthz", Protocol: "http1", Routes: []Route{{Host: "admin", Path: "/api/"}}, Address: "127.0.0.1:9002",
				Authorize: &Authorize{By: "auth", Path: "/check", Headers: []string{"x-identity"}, Respond: []string{"set-cookie"}}},
			{Name: "auth", Port: 9001, Ready: "/healthz", Protocol: "http1", Address: "127.0.0.1:9001"},
		},
	}
	listeners, clusters := staticResources(t, Files(r))
	// The same Envoy stands in for the authorizer and the service: a listener for each that
	// answers every request, the checks included, with a 200 and its headers.
	stub := func(name string, port int, headers ...[2]string) map[string]any {
		var add []any
		for _, h := range headers {
			add = append(add, obj{"header": obj{"key": h[0], "value": h[1]}, "append_action": "APPEND_IF_EXISTS_OR_ADD"})
		}
		return obj{
			"name":    name,
			"address": obj{"socket_address": obj{"address": "127.0.0.1", "port_value": port}},
			"filter_chains": []any{obj{"filters": []any{obj{"name": "envoy.filters.network.http_connection_manager", "typed_config": obj{
				"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
				"stat_prefix": name,
				"route_config": obj{"virtual_hosts": []any{obj{"name": name, "domains": []any{"*"}, "routes": []any{obj{
					"match":                   obj{"prefix": "/"},
					"direct_response":         obj{"status": 200, "body": obj{"inline_string": name}},
					"response_headers_to_add": add,
				}}}}},
				"http_filters": []any{obj{"name": "envoy.filters.http.router", "typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}}},
			}}}}},
		}
	}
	listeners = append(listeners,
		stub("auth", 9001, [2]string{"set-cookie", "session=renewed; Path=/; HttpOnly"}, [2]string{"set-cookie", "csrf=t1; Path=/"}, [2]string{"x-identity", "alice"}),
		stub("api", 9002, [2]string{"set-cookie", "theme=dark; Path=/"}))
	static, _ := json.Marshal(obj{"static_resources": obj{"listeners": listeners, "clusters": clusters}})

	// The clusters' first checks fail before the stubs listen, so the edge answers 503 until
	// the next ones pass, 5 seconds later.
	script := `cat > /tmp/envoy.json
envoy -c /tmp/envoy.json --log-level error > /tmp/envoy.log 2>&1 &
for i in $(seq 1 100); do
  out=$(exec 2> /dev/null 3<>/dev/tcp/127.0.0.1/10000 &&
    printf 'GET /api/orders HTTP/1.1\r\nHost: admin.example.com\r\nConnection: close\r\n\r\n' >&3 && cat <&3)
  case "$out" in "HTTP/1.1 200"*) printf '%s' "$out"; exit 0 ;; esac
  sleep 0.2
done
printf '%s\n' "$out"
cat /tmp/envoy.log
exit 1`
	cmd := exec.Command("docker", "run", "--rm", "-i", "--entrypoint", "bash", EnvoyImage, "-c", script)
	cmd.Stdin = bytes.NewReader(static)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the edge never answered 200: %v\n%s", err, out)
	}
	var cookies []string
	head, _, _ := strings.Cut(string(out), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n")[1:] {
		name, value, _ := strings.Cut(line, ":")
		switch strings.ToLower(name) {
		case "set-cookie":
			cookies = append(cookies, strings.TrimSpace(value))
		case "x-identity":
			t.Errorf("the client got x-identity, which respond doesn't name:\n%s", head)
		}
	}
	// The authorizer's come after the service's, so a browser keeps the authorizer's of a
	// cookie both set.
	if want := []string{"theme=dark; Path=/", "session=renewed; Path=/; HttpOnly", "csrf=t1; Path=/"}; !slices.Equal(cookies, want) {
		t.Errorf("the client's cookies\n%q\nwant\n%q\n%s", cookies, want, head)
	}
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
