package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func (c *Config) defaults() {
	if c.StatusAddr == "" {
		c.StatusAddr = fmt.Sprintf(":%d", StatusPort)
	}
	if c.ScrapeEvery == 0 {
		c.ScrapeEvery = 10 * time.Second
	}
	if c.Drain == 0 {
		c.Drain = 5 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Checks.Collector == "" {
		c.Checks.Collector = "http://" + collectorCheck + "/"
	}
	if c.Checks.Loki == "" {
		c.Checks.Loki = "http://" + lokiHTTP + "/ready"
	}
	if c.Checks.Tempo == "" {
		c.Checks.Tempo = "http://" + tempoHTTP + "/ready"
	}
	if c.Checks.Prometheus == "" {
		c.Checks.Prometheus = "http://" + prometheusHTTP + "/-/ready"
	}
}

// status answers the agent's readiness check, and nothing else: it is on the overlay, where
// only the collector's receivers may be reached.
type status struct {
	checks   Checks
	socket   string // Grafana's socket, when its check isn't a URL.
	client   *http.Client
	draining atomic.Bool
}

func newStatus(checks Checks, socket string, client *http.Client) *status {
	return &status{checks: checks, socket: socket, client: client}
}

func (s *status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ready" {
		http.NotFound(w, r)
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	if host != Name {
		http.Error(w, "not "+Name, http.StatusNotFound)
		return
	}
	if s.draining.Load() {
		http.Error(w, "stopping", http.StatusServiceUnavailable)
		return
	}
	var down []string
	for _, c := range []struct{ name, url string }{
		{"loki", s.checks.Loki}, {"tempo", s.checks.Tempo}, {"prometheus", s.checks.Prometheus},
		{"grafana", s.checks.Grafana}, {"collector", s.checks.Collector},
	} {
		if !s.ok(r.Context(), c.name, c.url) {
			down = append(down, c.name)
		}
	}
	if len(down) > 0 {
		http.Error(w, "not ready: "+strings.Join(down, ", "), http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready\n"))
}

func (s *status) ok(ctx context.Context, name, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := s.client
	if name == "grafana" && url == "" {
		client, url = SocketClient(s.socket), "http://grafana/api/health"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// SocketClient talks HTTP over a Unix socket, such as Grafana's: the host in a URL is
// ignored.
func SocketClient(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}
}

// Run runs the telemetry replica until ctx is done. It writes the programs' configs, makes
// their directories on the volume, starts them stores first, serves the readiness check, and
// reloads Prometheus when the scrape targets change. When ctx is done it fails its check,
// stops the collector first, so nothing arrives at a store that is stopping, and Loki and
// Tempo last, as they flush what they hold. If any program exits on its own, Run stops the
// others and returns an error, and Docker restarts the container.
func Run(ctx context.Context, c Config) error {
	c.defaults()
	log := c.Logger
	socket := filepath.Join(c.SocketDir, SocketName)

	targets, targetsText := readTargets(c, nil, "", log)
	if err := prepare(c, targets); err != nil {
		return err
	}

	st := newStatus(c.Checks, socket, &http.Client{Transport: &http.Transport{Proxy: nil}})
	ln, err := net.Listen("tcp", c.StatusAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: st, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	exited := make(chan string, 5)
	var running []*program
	stopAll := func() {
		for _, p := range slices.Backward(running) {
			p.stop(30 * time.Second)
		}
	}
	for _, def := range Programs(c) {
		p := &program{def: def}
		if err := p.start(c, exited); err != nil {
			stopAll()
			return err
		}
		running = append(running, p)
	}
	byName := map[string]*program{}
	for _, p := range running {
		byName[p.def.Name] = p
	}
	log.Info("telemetry running")

	tick := time.NewTicker(c.ScrapeEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("telemetry stopping: failing readiness, then the collector, then the stores")
			st.draining.Store(true)
			byName["collector"].stop(30 * time.Second)
			time.Sleep(c.Drain)
			for _, name := range []string{"grafana", "prometheus", "tempo", "loki"} {
				byName[name].stop(60 * time.Second)
			}
			return nil
		case name := <-exited:
			stopAll()
			return fmt.Errorf("%s exited", name)
		case <-tick.C:
			var changed bool
			targets, targetsText, changed = readTargetsChanged(c, targets, targetsText, log)
			if !changed {
				continue
			}
			if err := writeFile(c, "prometheus.json", PrometheusConfig(c, targets)); err != nil {
				log.Error("writing Prometheus's config", "err", err)
				continue
			}
			byName["prometheus"].signal(syscall.SIGHUP)
			log.Info("scrape targets changed: Prometheus reloaded", "targets", len(targets))
		}
	}
}

// readTargets reads the agent's scrape targets file. A missing file is no targets; a broken
// one keeps the targets there were.
func readTargets(c Config, current []Target, currentText string, log *slog.Logger) ([]Target, string) {
	ts, text, _ := readTargetsChanged(c, current, currentText, log)
	return ts, text
}

func readTargetsChanged(c Config, current []Target, currentText string, log *slog.Logger) ([]Target, string, bool) {
	b, err := os.ReadFile(filepath.Join(c.FilesDir, filepath.Base(ScrapeFile)))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("reading the scrape targets", "err", err)
		return current, currentText, false
	}
	if string(b) == currentText {
		return current, currentText, false
	}
	ts, err := ParseTargets(b)
	if err != nil {
		log.Warn("keeping the scrape targets there were", "err", err)
		return current, string(b), false
	}
	return ts, string(b), true
}

// prepare writes every program's config and makes their directories, owned by the user
// they run as.
func prepare(c Config, targets []Target) error {
	dirs := []string{grafanaData(c)}
	for _, dir := range []string{"loki", "tempo", "prometheus"} {
		dirs = append(dirs, filepath.Join(c.DataDir, dir))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(c.SocketDir, 0o750); err != nil {
		return err
	}
	// Grafana can't listen on a socket left by the last run.
	if err := os.Remove(filepath.Join(c.SocketDir, SocketName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The programs make everything under these as the user they run as, so the directories
	// alone are given to it: walking the volume would take minutes once it holds metrics.
	if c.UID >= 0 {
		for _, d := range append(dirs, c.DataDir, c.SocketDir) {
			if err := os.Lchown(d, c.UID, c.GID); err != nil {
				return err
			}
		}
	}
	files := map[string]string{
		"collector.json":  CollectorConfig(c),
		"loki.json":       LokiConfig(c),
		"tempo.json":      TempoConfig(c),
		"prometheus.json": PrometheusConfig(c, targets),
		"grafana.ini":     GrafanaINI(c),
		"grafana/provisioning/datasources/protostrap.json": GrafanaDatasources(),
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := writeFile(c, name, files[name]); err != nil {
			return err
		}
	}
	return nil
}

// writeFile writes a config readable by the programs, replacing it in one rename.
func writeFile(c Config, name, content string) error {
	p := filepath.Join(c.ConfigDir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// program is one of the five programs, running.
type program struct {
	def  Program
	cmd  *exec.Cmd
	done chan struct{}
}

func (p *program) start(c Config, exited chan<- string) error {
	p.cmd = exec.Command(p.def.Path, p.def.Args...)
	p.cmd.Env = append(os.Environ(), p.def.Env...)
	p.cmd.Dir = c.DataDir
	p.cmd.Stdout, p.cmd.Stderr = os.Stdout, os.Stderr
	if c.UID >= 0 && c.UID != os.Geteuid() {
		p.cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(c.UID), Gid: uint32(c.GID)}}
	}
	if err := p.cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", p.def.Name, err)
	}
	p.done = make(chan struct{})
	go func() {
		p.cmd.Wait()
		close(p.done)
		exited <- p.def.Name
	}()
	return nil
}

func (p *program) signal(sig os.Signal) {
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Signal(sig)
	}
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
