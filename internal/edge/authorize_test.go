package edge

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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
		`"allowed_headers":{"patterns":[{"exact":"cookie","ignore_case":true}]}`,
		`"allowed_upstream_headers":{"patterns":[{"exact":"x-identity","ignore_case":true}]}`,
		`"failure_mode_allow":false`, `"status_on_error":{"code":"ServiceUnavailable"}`,
	} {
		if !strings.Contains(authz, want) {
			t.Errorf("api's ext_authz lacks %s:\n%s", want, authz)
		}
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
	files := Files(Routing{Services: all, Hosts: map[string]string{"admin": "admin.example.com"}})
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
	static, _ := json.Marshal(map[string]any{
		"static_resources": map[string]any{"listeners": lds.Resources, "clusters": cds.Resources},
	})
	// The config goes in on stdin: Docker Desktop shares only some of the host's directories.
	cmd := exec.Command("docker", "run", "--rm", "-i", "--entrypoint", "sh", EnvoyImage,
		"-c", "cat > /tmp/envoy.json && envoy --mode validate -c /tmp/envoy.json")
	cmd.Stdin = bytes.NewReader(static)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("envoy refused the config: %v\n%s", err, out)
	}
}
