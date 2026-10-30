// Package edge is ProtoStrap's edge: cloudflared and Envoy in one container, alone on each
// zone's ingress VM (docs/protostrap.md#edge). The scheduler renders Envoy's listener and
// clusters from the spec's routes (Files), the agent writes them where the edge reads them,
// and `protostrap edge`, the container's entrypoint, runs both programs (Run).
package edge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/duncan-G/protostrap/internal/probe"
)

// Route is a host and path prefix the gateway sends to a service. Host is the host's name
// in the spec, such as admin; empty matches every host.
type Route struct {
	Host string `json:"host,omitempty"`
	Path string `json:"path"`
	// Rate is the number of requests per second allowed. Zero means no limit.
	Rate float64 `json:"rate,omitempty"`
}

// Service is a component the gateway routes to.
type Service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	// Ready is the readiness check as a spec writes it, such as "grpc" or "9000/healthz".
	Ready string `json:"ready"`
	// Protocol is what the edge speaks to it: http1, http2 or grpc, which is HTTP/2 with
	// browsers' gRPC-Web translated on its routes. Empty is grpc for a grpc check on its
	// own port, otherwise http1.
	Protocol string  `json:"protocol,omitempty"`
	Routes   []Route `json:"routes,omitempty"`
	// Authorize is the service the edge asks about each request to this one's routes, or nil.
	Authorize *Authorize `json:"authorize,omitempty"`
	// Address is where the edge reaches the service, host:port, if not by its name and Port
	// on the overlay, as under `aspire run`.
	Address string `json:"address,omitempty"`
	// HealthPort is the port its check runs on at Address, if Address moves the check's
	// port too.
	HealthPort int `json:"health_port,omitempty"`
}

// Authorize is a service the edge asks about each request to another's routes before
// sending it on (docs/protostrap.md, "Authorizing requests").
type Authorize struct {
	// By is the service that answers.
	By string `json:"by"`
	// Path is the prefix of the path the edge asks for; the request's own path follows it.
	Path string `json:"path,omitempty"`
	// Headers are the authorizer's response headers added to a request it lets through. The
	// edge removes them from the request as the client sent it first.
	Headers []string `json:"headers,omitempty"`
	// Forward are the request's headers sent to the authorizer, besides Authorization.
	Forward []string `json:"forward,omitempty"`
}

// EnvoyImage is the Envoy the edge runs, which `aspire run` runs as the local edge too.
const EnvoyImage = "envoyproxy/envoy:v1.40.2"

func (s Service) protocol() string {
	switch {
	case s.Protocol != "":
		return s.Protocol
	case s.Ready == probe.CheckGRPC:
		return "grpc"
	}
	return "http1"
}

func (s Service) http2() bool { return s.protocol() == "http2" || s.protocol() == "grpc" }

// ListenerFile and ClustersFile are the files the agent writes into the edge's directory,
// which the container sees at /etc/protostrap. Envoy watches the directory and reloads
// either file when the agent renames a new one into place, so a route change never
// restarts the edge.
const (
	ListenerFile = "lds.json"
	ClustersFile = "cds.json"
	// ListenPort is where Envoy listens for cloudflared, inside the container only.
	ListenPort = 8080
)

// RequestsMetric is the counter of requests the edge sends a service, which the agent
// scrapes from the edge for `requests` signals.
func RequestsMetric(service string) string {
	return fmt.Sprintf(`envoy_cluster_upstream_rq_total{envoy_cluster_name="%s"}`, service)
}

// Routing is what the edge's Envoy is rendered from.
type Routing struct {
	Services []Service `json:"services"`
	// Hosts maps each host's name to its hostname in this account
	// (docs/protostrap.md, "Hosts").
	Hosts map[string]string `json:"hosts,omitempty"`
	// Listen is where Envoy listens, host:port. Empty is 127.0.0.1:8080, where cloudflared
	// reaches it.
	Listen string `json:"listen,omitempty"`
	// Local is the edge `aspire run` runs, where gRPC checks don't name the service: no
	// address is reused there, and no replica has PROTOSTRAP_COMPONENT to answer for.
	Local bool `json:"local,omitempty"`
}

// listen is where the listener listens.
func (r Routing) listen() (string, int) {
	if r.Listen == "" {
		return "127.0.0.1", ListenPort
	}
	host, port, err := net.SplitHostPort(r.Listen)
	n, perr := strconv.Atoi(port)
	if err != nil || perr != nil {
		return "127.0.0.1", ListenPort
	}
	return host, n
}

// endpoint is where the edge reaches a service, and the port of its check if that isn't
// the same port, or 0.
func (s Service) endpoint(check probe.Check) (string, int, int) {
	host, port := s.Name, s.Port
	if s.Address != "" {
		if h, p, err := net.SplitHostPort(s.Address); err == nil {
			if n, err := strconv.Atoi(p); err == nil {
				host, port = h, n
			}
		}
	}
	health := check.Port
	if s.HealthPort != 0 {
		health = s.HealthPort
	}
	if health == port {
		health = 0
	}
	return host, port, health
}

// Files renders Envoy's dynamic config, keyed by where the agent writes it.
func Files(cfg Routing) map[string]string {
	services := slices.Clone(cfg.Services)
	slices.SortFunc(services, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return map[string]string{
		"edge/" + ListenerFile: render(listener(services, cfg)),
		"edge/" + ClustersFile: render(clusters(services, cfg.Local)),
	}
}

type obj = map[string]any

func render(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return b.String()
}

type routeEntry struct {
	Route
	service string
	// grpcWeb says browsers' gRPC-Web is translated for the service.
	grpcWeb bool
	// authorize is the service's authorizer, or nil.
	authorize *Authorize
}

// authzFilter is the name of the ext_authz filter that asks about a service's requests.
func authzFilter(service string) string { return "envoy.filters.http.ext_authz/" + service }

const headerMutation = "envoy.filters.http.header_mutation"

// authzFilters are the listener's filters that authorize requests: one that removes the
// headers authorizers answer with, then an ext_authz filter for each authorized service.
// Each is off, and only an authorized service's routes turn it on.
func authzFilters(services []Service) []any {
	var filters []any
	for _, s := range services {
		a := s.Authorize
		if a == nil || len(s.Routes) == 0 {
			continue
		}
		if filters == nil {
			filters = append(filters, obj{"name": headerMutation, "disabled": true, "typed_config": obj{
				"@type": "type.googleapis.com/envoy.extensions.filters.http.header_mutation.v3.HeaderMutation",
			}})
		}
		port := 0
		for _, by := range services {
			if by.Name == a.By {
				port = by.Port
			}
		}
		httpService := obj{
			"server_uri": obj{"uri": fmt.Sprintf("http://%s:%d", a.By, port), "cluster": a.By, "timeout": "2s"},
			// Authorization is sent without asking; these are the others.
			"authorization_request":  obj{"allowed_headers": patterns(a.Forward)},
			"authorization_response": obj{"allowed_upstream_headers": patterns(a.Headers)},
		}
		if p := strings.TrimSuffix(a.Path, "/"); p != "" {
			httpService["path_prefix"] = p
		}
		filters = append(filters, obj{"name": authzFilter(s.Name), "disabled": true, "typed_config": obj{
			"@type":                 "type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthz",
			"transport_api_version": "V3",
			"http_service":          httpService,
			// A request the authorizer can't answer for isn't let through.
			"failure_mode_allow": false,
			"status_on_error":    obj{"code": "ServiceUnavailable"},
		}})
	}
	return filters
}

func patterns(headers []string) obj {
	ps := []any{}
	for _, h := range headers {
		ps = append(ps, obj{"exact": h, "ignore_case": true})
	}
	return obj{"patterns": ps}
}

// listener is one HTTP listener for cloudflared. It holds each host's routes plus the routes
// for every host, longest prefix first, and it translates gRPC-Web, retries requests that
// never reached a replica, and applies each route's rate limit.
func listener(services []Service, cfg Routing) obj {
	hosts := cfg.Hosts
	address, port := cfg.listen()
	byHost := map[string][]routeEntry{}
	var everyHost []routeEntry
	for _, s := range services {
		for _, r := range s.Routes {
			e := routeEntry{r, s.Name, s.protocol() == "grpc", s.Authorize}
			if r.Host == "" {
				everyHost = append(everyHost, e)
			} else if hostname := hosts[r.Host]; hostname != "" {
				byHost[hostname] = append(byHost[hostname], e)
			}
		}
	}
	var vhosts []any
	for _, host := range sortedKeys(byHost) {
		vhosts = append(vhosts, virtualHost(host, append(byHost[host], everyHost...)))
	}
	vhosts = append(vhosts, virtualHost("*", everyHost))

	return obj{"resources": []any{obj{
		"@type":   "type.googleapis.com/envoy.config.listener.v3.Listener",
		"name":    "edge",
		"address": obj{"socket_address": obj{"address": address, "port_value": port}},
		"filter_chains": []any{obj{"filters": []any{obj{
			"name": "envoy.filters.network.http_connection_manager",
			"typed_config": obj{
				"@type":       "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
				"stat_prefix": "edge",
				// cloudflared is the only client, so the address Cloudflare passes is trusted.
				"use_remote_address":           false,
				"common_http_protocol_options": obj{"idle_timeout": "300s"},
				"route_config":                 obj{"name": "routes", "virtual_hosts": vhosts},
				"http_filters": slices.Concat(
					// A route's rate limit comes first, so it protects the authorizer too.
					[]any{obj{"name": "envoy.filters.http.local_ratelimit", "typed_config": obj{
						"@type":       "type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit",
						"stat_prefix": "edge_rate",
					}}},
					authzFilters(services),
					[]any{
						obj{"name": "envoy.filters.http.grpc_web", "typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.filters.http.grpc_web.v3.GrpcWeb"}},
						obj{"name": "envoy.filters.http.router", "typed_config": obj{"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router"}},
					},
				),
			},
		}}}},
	}}}
}

func virtualHost(host string, routes []routeEntry) obj {
	routes = slices.Clone(routes)
	slices.SortStableFunc(routes, func(a, b routeEntry) int {
		if d := len(b.Path) - len(a.Path); d != 0 {
			return d
		}
		return strings.Compare(a.Path, b.Path)
	})
	var rs []any
	for _, r := range routes {
		route := obj{
			"match": obj{"prefix": r.Path},
			"route": obj{
				"cluster": r.service,
				// Cloudflare gives up on a response that takes 125 seconds to start.
				"timeout": "120s",
				// Retry only what never reached a replica, such as one that just stopped.
				"retry_policy": obj{"retry_on": "connect-failure,refused-stream,reset-before-request", "num_retries": 2},
			},
		}
		perFilter := obj{}
		if !r.grpcWeb {
			// Any other service gets gRPC-Web as sent: translated to gRPC and sent over
			// HTTP/1.1, it would lose its trailers, and with them every call's status.
			perFilter["envoy.filters.http.grpc_web"] = obj{"@type": "type.googleapis.com/envoy.config.route.v3.FilterConfig", "disabled": true}
		}
		if r.Rate > 0 {
			tokens := int(r.Rate + 0.5)
			perFilter["envoy.filters.http.local_ratelimit"] = obj{
				"@type":           "type.googleapis.com/envoy.extensions.filters.http.local_ratelimit.v3.LocalRateLimit",
				"stat_prefix":     r.service + "_rate",
				"token_bucket":    obj{"max_tokens": tokens, "tokens_per_fill": tokens, "fill_interval": "1s"},
				"filter_enabled":  obj{"default_value": obj{"numerator": 100, "denominator": "HUNDRED"}},
				"filter_enforced": obj{"default_value": obj{"numerator": 100, "denominator": "HUNDRED"}},
			}
		}
		if a := r.authorize; a != nil {
			// The client's own copies of the authorizer's headers go first, so the service
			// gets only the authorizer's.
			if len(a.Headers) > 0 {
				var removals []any
				for _, h := range a.Headers {
					removals = append(removals, obj{"remove": h})
				}
				perFilter[headerMutation] = obj{
					"@type":     "type.googleapis.com/envoy.extensions.filters.http.header_mutation.v3.HeaderMutationPerRoute",
					"mutations": obj{"request_mutations": removals},
				}
			}
			perFilter[authzFilter(r.service)] = obj{
				"@type":          "type.googleapis.com/envoy.extensions.filters.http.ext_authz.v3.ExtAuthzPerRoute",
				"check_settings": obj{},
			}
		}
		if len(perFilter) > 0 {
			route["typed_per_filter_config"] = perFilter
		}
		rs = append(rs, route)
	}
	if rs == nil {
		rs = []any{}
	}
	domains := []any{host}
	if host != "*" {
		// A browser sends the port it used, such as admin.dev.localhost:20000 under `aspire
		// run`. The host matches whatever it is, and services still see the port.
		domains = append(domains, host+":*")
	}
	return obj{"name": host, "domains": domains, "routes": rs}
}

// clusters is one cluster per routed service. Each cluster finds the service's replicas by
// looking its name up on the overlay every 5 seconds, balances each request across them,
// and sends only to those passing their readiness check, which asks for the service by its
// own name.
func clusters(services []Service, local bool) obj {
	authorizers := map[string]bool{}
	for _, s := range services {
		if s.Authorize != nil && len(s.Routes) > 0 {
			authorizers[s.Authorize.By] = true
		}
	}
	resources := []any{}
	for _, s := range services {
		if len(s.Routes) == 0 && !authorizers[s.Name] {
			continue
		}
		protocol := obj{"http_protocol_options": obj{}}
		if s.http2() {
			protocol = obj{"http2_protocol_options": obj{}}
		}
		hc := obj{"timeout": "2s", "interval": "5s", "no_traffic_interval": "5s", "unhealthy_threshold": 2, "healthy_threshold": 1}
		check, err := probe.ParseCheck(s.Ready)
		if err != nil {
			// protostrap check refuses such a spec; a connection is the least a replica owes.
			check = probe.Check{Kind: probe.CheckTCP}
		}
		switch check.Kind {
		case probe.CheckGRPC:
			if local {
				hc["grpc_health_check"] = obj{}
			} else {
				hc["grpc_health_check"] = obj{"service_name": s.Name}
			}
		case probe.CheckTCP:
			hc["tcp_health_check"] = obj{}
		default:
			http := obj{
				"path": check.Path, "host": s.Name,
				// Envoy alone passes only 200; the agent's probe passes 2xx and 3xx.
				"expected_statuses": []any{obj{"start": probe.ReadyStatuses.Start, "end": probe.ReadyStatuses.End}},
			}
			if _, _, health := s.endpoint(check); health == 0 && s.http2() {
				// On its own port, the check speaks what the service speaks.
				http["codec_client_type"] = "HTTP2"
			}
			hc["http_health_check"] = http
		}
		host, port, health := s.endpoint(check)
		endpoint := obj{"address": obj{"socket_address": obj{"address": host, "port_value": port}}}
		if health != 0 {
			endpoint["health_check_config"] = obj{"port_value": health}
		}
		resources = append(resources, obj{
			"@type": "type.googleapis.com/envoy.config.cluster.v3.Cluster",
			"name":  s.Name,
			"cluster_type": obj{"name": "envoy.cluster.dns", "typed_config": obj{
				"@type":             "type.googleapis.com/envoy.extensions.clusters.dns.v3.DnsCluster",
				"dns_lookup_family": "V4_ONLY",
				"dns_refresh_rate":  "5s",
				// Docker's DNS answers with a TTL of minutes; replicas come and go faster.
				"respect_dns_ttl": false,
				// Every address is a replica of its own, so requests balance across them.
				"all_addresses_in_single_endpoint": false,
			}},
			"connect_timeout": "2s",
			"lb_policy":       "ROUND_ROBIN",
			"load_assignment": obj{"cluster_name": s.Name, "endpoints": []any{obj{"lb_endpoints": []any{obj{
				"endpoint": endpoint,
			}}}}},
			"typed_extension_protocol_options": obj{"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": obj{
				"@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
				// This must stay shorter than every replica's keep-alive timeout, so Envoy never
				// reuses a connection the replica is closing. Node's default is only 5s, which
				// is why the sample web server raises its keepAliveTimeout to 65s.
				"common_http_protocol_options": obj{"idle_timeout": "60s"},
				"explicit_http_config":         protocol,
			}},
			"health_checks": []any{hc},
			// Never send to replicas failing their check, even if all are.
			"common_lb_config":              obj{"healthy_panic_threshold": obj{"value": 0}},
			"ignore_health_on_host_removal": true,
		})
	}
	return obj{"resources": resources}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// LocalFilesDir is where the local edge's container mounts the files WriteLocal writes.
const LocalFilesDir = "/etc/protostrap"

// WriteLocal writes the edge `aspire run` runs into dir: Envoy's bootstrap, envoy.json,
// and the listener and clusters it reads from LocalFilesDir, where the container mounts dir.
// Each file is renamed into place, so a running Envoy reloads a whole new one.
func WriteLocal(dir string, r Routing) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{"envoy.json": Bootstrap(LocalFilesDir, 9901)}
	for path, content := range Files(r) {
		files[filepath.Base(path)] = content
	}
	for _, name := range sortedKeys(files) {
		tmp := filepath.Join(dir, "."+name+".tmp")
		if err := os.WriteFile(tmp, []byte(files[name]), 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
