package edge

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// local is the edge `aspire run` runs: Envoy in a container, listening on every address of
// the container, reaching services where the AppHost has them rather than by name on the
// overlay.
func local() Routing {
	return Routing{
		Listen: "0.0.0.0:10000",
		Hosts:  map[string]string{"admin": "admin.dev.localhost"},
		Services: []Service{
			{Name: "api", Port: 8080, Ready: "grpc", Protocol: "grpc", Routes: []Route{{Host: "admin", Path: "/api/"}},
				Address: "host.docker.internal:51234"},
			{Name: "keycloak", Port: 8080, Ready: "9000/health/ready", Routes: []Route{{Host: "admin", Path: "/realms/"}},
				Address: "keycloak:8080", HealthPort: 9000},
			{Name: "web", Port: 4000, Ready: "/healthz", Routes: []Route{{Host: "admin", Path: "/"}},
				Address: "host.docker.internal:51240", HealthPort: 51240},
		},
	}
}

func TestTheListenerCanListenElsewhere(t *testing.T) {
	lds := Files(local())["edge/lds.json"]
	for _, want := range []string{`"address": "0.0.0.0"`, `"port_value": 10000`} {
		if !strings.Contains(lds, want) {
			t.Errorf("listener lacks %s", want)
		}
	}
	// By default it listens where cloudflared reaches it, on the container's loopback.
	if lds := Files(config(services()))["edge/lds.json"]; !strings.Contains(lds, `"address": "127.0.0.1"`) || !strings.Contains(lds, `"port_value": 8080`) {
		t.Errorf("the default listener moved")
	}
}

// Browsers send the host with the port they used, such as admin.dev.localhost:20000, so the
// edge matches a host whatever its port, and leaves the Host header as it came, for the
// redirect URLs services build from it.
func TestHostsMatchWhateverThePort(t *testing.T) {
	got := jsonOf(decode(t, Files(local())["edge/lds.json"]))
	if !strings.Contains(got, `"domains":["admin.dev.localhost","admin.dev.localhost:*"]`) {
		t.Errorf("the virtual host matches admin.dev.localhost only without a port:\n%s", got)
	}
	if strings.Contains(got, "strip_any_host_port") || strings.Contains(got, "strip_matching_host_port") {
		t.Errorf("the listener strips the port from the Host header services see")
	}
}

// A service's address overrides its name and port on the overlay, and its check's port
// follows the AppHost's mapping too.
func TestAServiceCanBeReachedAtAnAddress(t *testing.T) {
	api := jsonOf(cluster(t, local().Services, "api"))
	if !strings.Contains(api, `"address":"host.docker.internal","port_value":51234`) {
		t.Errorf("api's endpoint:\n%s", api)
	}
	kc := jsonOf(cluster(t, local().Services, "keycloak"))
	if !strings.Contains(kc, `"address":"keycloak","port_value":8080`) || !strings.Contains(kc, `"health_check_config":{"port_value":9000}`) {
		t.Errorf("keycloak's endpoint:\n%s", kc)
	}
	// A check on the service's own port is moved with it, but stays on that port.
	web := jsonOf(cluster(t, local().Services, "web"))
	if strings.Contains(web, "health_check_config") {
		t.Errorf("web's check is on its own port, which moved with its address:\n%s", web)
	}
}

// The routing goes to `protostrap edge render` as JSON, which Hosting writes.
func TestRoutingIsJSON(t *testing.T) {
	src := `{
	  "listen": "0.0.0.0:10000",
	  "hosts": { "admin": "admin.dev.localhost" },
	  "services": [
	    { "name": "api", "port": 8080, "ready": "grpc", "protocol": "grpc", "address": "host.docker.internal:51234",
	      "routes": [ { "host": "admin", "path": "/api/", "rate": 50 } ],
	      "authorize": { "by": "auth", "path": "/auth/check", "headers": ["x-user"], "forward": ["cookie"], "respond": ["set-cookie"],
	        "open": true, "timeout": "500ms", "skip": ["/api/assets/", "*.js"] } },
	    { "name": "auth", "port": 8080, "ready": "/healthz", "health_port": 51300, "address": "host.docker.internal:51300" }
	  ]
	}`
	var r Routing
	if err := json.Unmarshal([]byte(src), &r); err != nil {
		t.Fatal(err)
	}
	api := r.Services[0]
	if r.Listen != "0.0.0.0:10000" || r.Hosts["admin"] != "admin.dev.localhost" || api.Address != "host.docker.internal:51234" ||
		api.Routes[0] != (Route{Host: "admin", Path: "/api/", Rate: 50}) || api.Authorize.By != "auth" || r.Services[1].HealthPort != 51300 ||
		!slices.Equal(api.Authorize.Respond, []string{"set-cookie"}) || !api.Authorize.Open || api.Authorize.Timeout != "500ms" ||
		!slices.Equal(api.Authorize.Skip, []string{"/api/assets/", "*.js"}) {
		t.Errorf("routing %+v", r)
	}
}

// PROTOSTRAP_ENVOY_TEST=1 has Envoy validate the local edge's bootstrap, and its listener and
// clusters, which validate mode doesn't read through the bootstrap.
func TestEnvoyAcceptsTheLocalEdge(t *testing.T) {
	if os.Getenv("PROTOSTRAP_ENVOY_TEST") != "1" {
		t.Skip("set PROTOSTRAP_ENVOY_TEST=1 to validate with Envoy in Docker")
	}
	dir := t.TempDir()
	// It logs each request, and api and web are authorized, one with a header forwarded and
	// a cookie for the client, the other open, with its static files skipped.
	r := local()
	r.Local = true
	r.Services = append(r.Services, Service{Name: "auth", Port: 8080, Ready: "/healthz", Routes: []Route{{Host: "admin", Path: "/auth/"}},
		Address: "host.docker.internal:51300"})
	r.Services[0].Authorize = &Authorize{By: "auth", Path: "/auth/check", Headers: []string{"x-identity"}, Forward: []string{"cookie"}, Respond: []string{"set-cookie"}}
	r.Services[2].Authorize = &Authorize{By: "auth", Path: "/auth/check", Headers: []string{"x-identity"}, Open: true, Timeout: "500ms",
		Skip: []string{"/assets/", "*.js", "*.css"}}
	if err := WriteLocal(dir, r); err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, name := range []string{"envoy.json", ListenerFile, ClustersFile} {
		b, err := os.ReadFile(dir + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, "cat > /etc/protostrap/"+name+" <<'EOF_"+name+"'\n"+string(b)+"EOF_"+name+"\n")
	}
	script := "mkdir -p /etc/protostrap\n" + strings.Join(files, "") + "envoy --mode validate -c /etc/protostrap/envoy.json"
	cmd := exec.Command("docker", "run", "--rm", "-i", "--user", "root", "--entrypoint", "sh", EnvoyImage, "-s")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("envoy refused the local edge: %v\n%s", err, out)
	}
	envoyValidates(t, Files(r))
}

// WriteLocal writes what the local edge's Envoy reads: its bootstrap, which points at the
// listener and clusters beside it.
func TestWriteLocal(t *testing.T) {
	dir := t.TempDir()
	if err := WriteLocal(dir, local()); err != nil {
		t.Fatal(err)
	}
	boot, err := os.ReadFile(dir + "/envoy.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(boot), `"path": "/etc/protostrap/lds.json"`) {
		t.Errorf("the bootstrap doesn't read the listener where the container mounts it:\n%s", boot)
	}
	for _, name := range []string{ListenerFile, ClustersFile} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Locally a gRPC check asks for the server as a whole: a service's name protects against an
// address reused by another service on the overlay, which doesn't happen under `aspire run`,
// where the AppHost gives no replica PROTOSTRAP_COMPONENT to answer for.
func TestALocalGRPCCheckDoesntNameTheService(t *testing.T) {
	r := local()
	r.Local = true
	api := jsonOf(cluster(t, r.Services, "api"))
	if !strings.Contains(api, `"service_name":"api"`) {
		t.Errorf("the test's cluster helper renders without Local:\n%s", api)
	}
	v := decode(t, Files(r)["edge/cds.json"])
	for _, res := range v["resources"].([]any) {
		c := res.(map[string]any)
		if c["name"] == "api" {
			if got := jsonOf(c["health_checks"]); !strings.Contains(got, `"grpc_health_check":{}`) {
				t.Errorf("api's local check:\n%s", got)
			}
		}
	}
}

// The local edge logs each request it routes to its console, the AppHost's dashboard, so a
// developer sees what the edge did with it: the host and path (without the query, which may
// hold a sign-in's code), where it went, and Envoy's response flags, such as UAEX for a request
// the authorizer refused or NR for one no route matched. An account's edge logs nothing.
func TestTheLocalEdgeLogsEachRequest(t *testing.T) {
	r := local()
	r.Local = true
	got := jsonOf(decode(t, Files(r)["edge/lds.json"]))
	for _, want := range []string{
		`"access_log":[{"name":"envoy.access_loggers.stdout"`,
		`"@type":"type.googleapis.com/envoy.extensions.access_loggers.stream.v3.StdoutAccessLog"`,
		`%REQ(:METHOD)% %REQ(:AUTHORITY)%%PATH(NQ:ORIG_OR_PATH)%`,
		`%RESPONSE_CODE% %RESPONSE_FLAGS%`, `%UPSTREAM_CLUSTER%`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the local edge's listener lacks %s:\n%s", want, got)
		}
	}
	if lds := Files(local())["edge/lds.json"]; strings.Contains(lds, "access_log") {
		t.Errorf("an account's edge logs each request")
	}
}
