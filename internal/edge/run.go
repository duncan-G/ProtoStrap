package edge

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/duncan-G/protostrap/internal/secret"
)

// EnvTunnel is the edge's variable that the scheduler sets to off when the spec has a
// gateway and no tunnel: the edge then runs Envoy alone.
const EnvTunnel = "EDGE_TUNNEL"

// Config is how `protostrap edge` runs the edge's two programs.
type Config struct {
	// Envoy is Envoy's binary.
	Envoy string
	// Cloudflared is cloudflared's binary.
	Cloudflared string
	// FilesDir is where the agent writes Envoy's listener and clusters, such as
	// /etc/protostrap.
	FilesDir string
	// BootstrapPath is where Envoy's bootstrap is written.
	BootstrapPath string
	// StatusAddr is where the edge serves its readiness check and metrics, such as :9902.
	StatusAddr string
	// AdminPort is Envoy's admin port, on loopback only. It defaults to 9901.
	AdminPort int
	// TunnelMetrics is cloudflared's metrics server. It defaults to 127.0.0.1:2000.
	TunnelMetrics string
	Drain         time.Duration
	TokenRetry    time.Duration
	// Token returns the tunnel token. Without one, the edge runs no tunnel: Envoy alone.
	Token  func(ctx context.Context) (string, error)
	Logger *slog.Logger
}

func (c *Config) defaults() {
	if c.AdminPort == 0 {
		c.AdminPort = 9901
	}
	if c.TunnelMetrics == "" {
		c.TunnelMetrics = "127.0.0.1:2000"
	}
	if c.Drain == 0 {
		c.Drain = 10 * time.Second
	}
	if c.TokenRetry == 0 {
		c.TokenRetry = 15 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Bootstrap is Envoy's static config: its admin on loopback, and its listener and clusters
// read from the files the agent writes, reloaded whenever one is renamed into place.
func Bootstrap(filesDir string, adminPort int) string {
	source := func(file string) obj {
		return obj{"resource_api_version": "V3", "path_config_source": obj{
			"path":              filepath.Join(filesDir, file),
			"watched_directory": obj{"path": filesDir},
		}}
	}
	return render(obj{
		"node":  obj{"id": "edge", "cluster": "edge"},
		"admin": obj{"address": obj{"socket_address": obj{"address": "127.0.0.1", "port_value": adminPort}}},
		"dynamic_resources": obj{
			"lds_config": source(ListenerFile),
			"cds_config": source(ClustersFile),
		},
		"overload_manager": obj{"resource_monitors": []any{obj{
			"name": "envoy.resource_monitors.global_downstream_max_connections",
			"typed_config": obj{
				"@type":                             "type.googleapis.com/envoy.extensions.resource_monitors.downstream_connections.v3.DownstreamConnectionsConfig",
				"max_active_downstream_connections": 50000,
			},
		}}},
		"layered_runtime": envoyRuntime,
	})
}

// envoyRuntime lets Envoy compile a pattern up to 1000 in RE2's program size, not 100: the
// pattern of a route's skipped extensions, up to 30 of 10 characters, can pass 100, and Envoy
// refuses a listener with a larger pattern than its limit.
var envoyRuntime = obj{"layers": []any{obj{"name": "static", "static_layer": obj{"re2.max_program_size.error_level": 1000}}}}

// tunnelToken reads the token from the environment's secret: its key TUNNEL_TOKEN under the
// edge's prefix, Edge_TUNNEL_TOKEN. With no prefix, from an agent older than the
// environment's secret, the secret is the edge's own, {"TUNNEL_TOKEN": "..."}.
func tunnelToken(value, prefix string) (string, error) {
	all, err := secret.Parse(value)
	if err != nil {
		return "", err
	}
	if token := secret.Keys(all, prefix, nil)["TUNNEL_TOKEN"]; token != "" {
		return token, nil
	}
	return "", fmt.Errorf("the secret has no %sTUNNEL_TOKEN: an Org Admin sets it (scripts/set-edge-token.sh)", prefix)
}

// TokenFrom returns a Token func that reads the tunnel token, under prefix, from the
// secret value that get returns.
func TokenFrom(get func(ctx context.Context) (string, error), prefix string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		value, err := get(ctx)
		if err != nil {
			return "", err
		}
		return tunnelToken(value, prefix)
	}
}

// status is the edge's own HTTP server: its readiness check, which the agent and nothing
// else uses, and Envoy's request counts for the agent to scrape. Envoy's admin stays on
// loopback, so nothing on the overlay can drain or stop it.
type status struct {
	// envoyAdmin is Envoy's admin URL, such as http://127.0.0.1:9901.
	envoyAdmin string
	// tunnelMetrics is cloudflared's metrics URL, such as http://127.0.0.1:2000, or empty
	// without a tunnel.
	tunnelMetrics string
	client        *http.Client
	draining      atomic.Bool
}

func (s *status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ready":
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		switch {
		case host != "edge":
			http.Error(w, "not edge", http.StatusNotFound)
		case s.draining.Load():
			http.Error(w, "draining", http.StatusServiceUnavailable)
		case !s.ok(r.Context(), s.envoyAdmin+"/ready"):
			http.Error(w, "envoy isn't ready", http.StatusServiceUnavailable)
		case s.tunnelMetrics != "" && !s.ok(r.Context(), s.tunnelMetrics+"/ready"):
			http.Error(w, "the tunnel has no connection", http.StatusServiceUnavailable)
		default:
			w.Write([]byte("ready\n"))
		}
	case "/metrics":
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, s.envoyAdmin+"/stats/prometheus?usedonly", nil)
		resp, err := s.client.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		io.Copy(w, io.LimitReader(resp.Body, 16<<20))
	default:
		http.NotFound(w, r)
	}
}

func (s *status) ok(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Run runs the edge until ctx is done: it waits for the tunnel token, writes Envoy's
// bootstrap, starts Envoy and cloudflared, and serves its status. When ctx is done, it fails
// its readiness check, stops cloudflared so that Cloudflare sends requests to another
// connector, gives Envoy time to finish what it has, and then stops Envoy. If either
// program exits on its own, Run returns an error, and Docker restarts the container. Without
// a Token it runs Envoy alone.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	log := cfg.Logger

	var token string
	for cfg.Token != nil {
		var err error
		if token, err = cfg.Token(ctx); err == nil {
			break
		}
		log.Warn("waiting for the tunnel token", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(cfg.TokenRetry):
		}
	}

	if err := os.WriteFile(cfg.BootstrapPath, []byte(Bootstrap(cfg.FilesDir, cfg.AdminPort)), 0o644); err != nil {
		return err
	}
	st := &status{envoyAdmin: fmt.Sprintf("http://127.0.0.1:%d", cfg.AdminPort), client: &http.Client{Transport: &http.Transport{Proxy: nil}}}
	if cfg.Token != nil {
		st.tunnelMetrics = "http://" + cfg.TunnelMetrics
	}
	ln, err := net.Listen("tcp", cfg.StatusAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: st, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	envoy := &program{name: "envoy", cmd: exec.Command(cfg.Envoy, "-c", cfg.BootstrapPath, "--log-level", "warn",
		"--drain-time-s", fmt.Sprint(int(cfg.Drain.Seconds())))}
	programs := []*program{envoy}
	var tunnel *program
	if cfg.Token != nil {
		// The token goes in the environment, not the command line, where ps would show it.
		tunnel = &program{name: "cloudflared", cmd: exec.Command(cfg.Cloudflared, "tunnel", "--no-autoupdate", "--metrics", cfg.TunnelMetrics, "run")}
		tunnel.cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+token)
		programs = append(programs, tunnel)
	}
	stopAll := func(d time.Duration) {
		for _, p := range programs {
			p.stop(d)
		}
	}
	exited := make(chan string, 2)
	for _, p := range programs {
		if err := p.start(exited); err != nil {
			stopAll(time.Second)
			return err
		}
	}
	log.Info("edge running", "tunnel", tunnel != nil)

	select {
	case <-ctx.Done():
	case name := <-exited:
		stopAll(cfg.Drain)
		return fmt.Errorf("%s exited", name)
	}
	log.Info("edge stopping: failing readiness, then the tunnel, then Envoy")
	st.draining.Store(true)
	if tunnel != nil {
		tunnel.stop(30 * time.Second)
	}
	time.Sleep(cfg.Drain)
	envoy.stop(5 * time.Second)
	return nil
}

// program is one of the edge's two processes.
type program struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *program) start(exited chan<- string) error {
	p.cmd.Stdout, p.cmd.Stderr = os.Stdout, os.Stderr
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", p.name, err)
	}
	p.done = make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(p.done)
		exited <- p.name
	}()
	return nil
}

// stop sends SIGTERM and waits up to grace for the program to exit, then kills it.
func (p *program) stop(grace time.Duration) {
	if p.done == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(grace):
		p.cmd.Process.Kill()
		<-p.done
	}
}
