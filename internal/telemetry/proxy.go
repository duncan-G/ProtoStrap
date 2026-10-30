package telemetry

import (
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
)

// Proxy serves Grafana, from its socket on data-1, on the scheduler's loopback listener for
// the protostrap-telemetry port forward (docs/telemetry.md#reading-telemetry). Like the
// operator UI, it answers only requests from loopback that are for localhost, so a page
// elsewhere can't read Grafana through a person's forward. It passes every method: Grafana
// queries its data sources with POST, and its data sources and users can't be changed.
type Proxy struct {
	// Socket is Grafana's socket.
	Socket string
	Logger *slog.Logger

	once sync.Once
	rp   *httputil.ReverseProxy
}

func (p *Proxy) proxy() *httputil.ReverseProxy {
	p.once.Do(p.build)
	return p.rp
}

func (p *Proxy) build() {
	target := &url.URL{Scheme: "http", Host: "grafana"}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host // Grafana builds its links for the host the browser used.
		},
		Transport: SocketClient(p.Socket).Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if p.Logger != nil {
				p.Logger.Warn("Grafana didn't answer", "path", r.URL.Path, "err", err)
			}
			http.Error(w, "telemetry isn't answering: Grafana's socket on data-1 is down. The telemetry replica may be starting, or stopped; Platform Admins see why in the operator UI.", http.StatusBadGateway)
		},
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !fromLoopback(r.RemoteAddr) || !forLocalhost(r.Host) {
		http.Error(w, "forbidden: only through the protostrap-telemetry port forward, at localhost", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/protostrap/help" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(help))
		return
	}
	p.proxy().ServeHTTP(w, r)
}

func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func forLocalhost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// help is /protostrap/help, for an agent such as Claude Code to read before it queries.
const help = `# ProtoStrap telemetry: logs, traces and metrics

This is Grafana, from the telemetry replica on data-1, through the protostrap-telemetry port
forward. It holds the application's own telemetry: every replica's stdout and stderr, what
services send with OpenTelemetry, and every component's metrics endpoint. It doesn't hold
ProtoStrap's infra log (the scheduler's and agents' records): that is in the operator UI,
for Platform Admins and Org Admins.

Queries are read-only. Query each store's own API through Grafana's data source
proxy, by the data source's uid: loki, tempo or prometheus. Times are RFC 3339 or Unix
nanoseconds (Loki), seconds (Prometheus).

## Labels

| Label                 | Is                                                           |
| --------------------- | ------------------------------------------------------------ |
| service_name          | The component, such as api, or edge                         |
| service_instance_id   | The replica, such as api-k3f9, as the infra log names it     |
| service_version       | The release (commit) the replica runs                        |
| log_iostream          | stdout or stderr, on lines from a replica's output; none on OTLP records |
| trace_id, span_id     | On OTLP log records sent within a trace                      |

## Logs (Loki, LogQL)

    GET /api/datasources/proxy/uid/loki/loki/api/v1/query_range
        ?query={service_name="api"} |~ "(?i)error|exception"
        &since=1h&limit=200&direction=backward

One replica: {service_instance_id="api-k3f9"}. Lines that are JSON can be parsed:
{service_name="api"} | json | level="error". Label values:
GET /api/datasources/proxy/uid/loki/loki/api/v1/label/service_name/values

## Traces (Tempo)

Search with TraceQL, then fetch a trace by its ID:

    GET /api/datasources/proxy/uid/tempo/api/search?q={ resource.service.name = "api" && duration > 500ms }&limit=20
    GET /api/datasources/proxy/uid/tempo/api/traces/<trace id>

## Metrics (Prometheus, PromQL)

    GET /api/datasources/proxy/uid/prometheus/api/v1/query?query=sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_total[5m]))
    GET /api/datasources/proxy/uid/prometheus/api/v1/query_range?query=...&start=...&end=...&step=60

Each metrics endpoint in the spec is scraped every 30 seconds, on every replica, with the
component as service_name. The edge's request counts are envoy_cluster_upstream_rq_total,
by envoy_cluster_name, the service.

URL-encode queries, as curl's -G --data-urlencode does.
`
