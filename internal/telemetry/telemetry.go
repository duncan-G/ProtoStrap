// Package telemetry is ProtoStrap's telemetry replica (docs/telemetry.md): one container
// on data-1 that runs an OpenTelemetry collector, Loki, Tempo, Prometheus and Grafana.
//
// `protostrap telemetry` is its entrypoint. It writes each program's config, runs all five,
// answers the agent's readiness check, and reloads Prometheus when the scrape targets in the
// assignment change. Only the collector's receivers listen beyond the container's loopback:
// Loki, Tempo, Prometheus and Grafana have no sign-in, so Grafana is reached through a Unix
// socket on data-1, which the scheduler serves to the protostrap-telemetry port forward
// (Proxy).
package telemetry

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// These are where the telemetry replica listens and keeps things, as the scheduler and the
// agent set it up.
const (
	// ScrapeFile is the assignment file, under the agent's files directory, that lists what
	// Prometheus scrapes. The agent mounts FilesDir/telemetry at /etc/protostrap.
	ScrapeFile = "telemetry/scrape.json"
	// StatusPort is where it answers the agent's readiness check, on the overlay.
	StatusPort = 13133
	// OTLPGRPCPort and OTLPHTTPPort are the collector's OTLP receivers, on the overlay.
	OTLPGRPCPort = 4317
	OTLPHTTPPort = 4318
	// ForwardPort is the collector's receiver for Docker's fluentd log driver, which the
	// agent publishes on data-1's address.
	ForwardPort = 24224
	// VolumePath is where its EBS volume is mounted in the container.
	VolumePath = "/var/lib/telemetry"
	// SocketDir holds Grafana's socket, on data-1 and in the container alike.
	SocketDir = "/run/protostrap/telemetry"
	// SocketName is Grafana's socket in SocketDir.
	SocketName = "grafana.sock"
	// ProxyAddr is where the scheduler serves Grafana, for the protostrap-telemetry forward.
	ProxyAddr = "127.0.0.1:7482"
	// GrafanaSocket is Grafana's socket on data-1, which the scheduler and the reporter use.
	GrafanaSocket = SocketDir + "/" + SocketName
	// Name is the replica's component name, which the overlay resolves to it.
	Name = "telemetry"
)

// These are the variables the scheduler sets in the telemetry replica.
const (
	EnvBucket      = "TELEMETRY_BUCKET"
	EnvLogsDays    = "TELEMETRY_LOGS_DAYS"
	EnvTracesDays  = "TELEMETRY_TRACES_DAYS"
	EnvMetricsDays = "TELEMETRY_METRICS_DAYS"
)

// Target is a metrics endpoint Prometheus scrapes on every replica of a component.
type Target struct {
	Component string `json:"component"`
	Port      int    `json:"port"`
	Path      string `json:"path"`
}

var componentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// TargetsFile is the scrape targets as the assignment carries them, sorted, so an unchanged
// list is an unchanged file.
func TargetsFile(ts []Target) string {
	ts = slices.Clone(ts)
	slices.SortFunc(ts, func(a, b Target) int {
		return cmp.Or(strings.Compare(a.Component, b.Component), cmp.Compare(a.Port, b.Port), strings.Compare(a.Path, b.Path))
	})
	if ts == nil {
		ts = []Target{}
	}
	b, _ := json.Marshal(ts)
	return string(b)
}

// ParseTargets reads the scrape targets. No file is no targets.
func ParseTargets(b []byte) ([]Target, error) {
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var ts []Target
	if err := json.Unmarshal(b, &ts); err != nil {
		return nil, fmt.Errorf("scrape targets: %w", err)
	}
	for _, t := range ts {
		switch {
		case !componentName.MatchString(t.Component):
			return nil, fmt.Errorf("scrape target %q: not a component name", t.Component)
		case t.Port < 1 || t.Port > 65535:
			return nil, fmt.Errorf("scrape target %s: port %d", t.Component, t.Port)
		case !strings.HasPrefix(t.Path, "/") || strings.ContainsAny(t.Path, " \t\n?#"):
			return nil, fmt.Errorf("scrape target %s: path %q", t.Component, t.Path)
		}
	}
	slices.SortFunc(ts, func(a, b Target) int {
		return cmp.Or(strings.Compare(a.Component, b.Component), cmp.Compare(a.Port, b.Port), strings.Compare(a.Path, b.Path))
	})
	return ts, nil
}

// Retention is how many days each kind of record is kept.
type Retention struct{ Logs, Traces, Metrics int }

// Binaries are the five programs, as the telemetry image has them.
type Binaries struct {
	Collector, Loki, Tempo, Prometheus, Grafana string
	// GrafanaHome is Grafana's home path, with its public files and default config.
	GrafanaHome string
}

// DefaultBinaries are where telemetry/Dockerfile puts them.
var DefaultBinaries = Binaries{
	Collector: "/usr/local/bin/otelcol-contrib", Loki: "/usr/local/bin/loki", Tempo: "/usr/local/bin/tempo",
	Prometheus: "/usr/local/bin/prometheus", Grafana: "/usr/share/grafana/bin/grafana", GrafanaHome: "/usr/share/grafana",
}

// Checks are the programs' readiness checks. An empty one is the program's own, on loopback,
// and Grafana's goes through its socket.
type Checks struct{ Collector, Loki, Tempo, Prometheus, Grafana string }

// Config is how `protostrap telemetry` runs.
type Config struct {
	Region    string
	Bucket    string
	Retention Retention
	// MemoryMiB is the container's memory limit, which the programs share.
	MemoryMiB int64
	// DataDir is the volume, FilesDir where the agent's files are mounted, SocketDir where
	// Grafana's socket goes, and ConfigDir where the programs' configs are written.
	DataDir, FilesDir, SocketDir, ConfigDir string
	// StatusAddr is where the readiness check is answered, such as :13133.
	StatusAddr string
	Bin        Binaries
	Checks     Checks
	// UID and GID are who the programs run as. They are -1 to run as whoever runs this.
	UID, GID int
	// ScrapeEvery is how often the scrape targets are read. It defaults to 10s.
	ScrapeEvery time.Duration
	// Drain is how long the collector gets to send what it holds before the stores stop.
	// It defaults to 5s.
	Drain  time.Duration
	Logger *slog.Logger
}

// EnvConfig reads the settings the scheduler gives the replica: the region, the bucket and
// each retention, in days.
func EnvConfig(getenv func(string) string) (Config, error) {
	c := Config{Region: getenv("AWS_REGION"), Bucket: getenv(EnvBucket)}
	var errs []error
	if c.Region == "" {
		errs = append(errs, errors.New("AWS_REGION isn't set"))
	}
	if c.Bucket == "" {
		errs = append(errs, fmt.Errorf("%s isn't set", EnvBucket))
	}
	for _, d := range []struct {
		key string
		to  *int
	}{{EnvLogsDays, &c.Retention.Logs}, {EnvTracesDays, &c.Retention.Traces}, {EnvMetricsDays, &c.Retention.Metrics}} {
		n, err := strconv.Atoi(getenv(d.key))
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("%s = %q: want a number of days, at least 1", d.key, getenv(d.key)))
			continue
		}
		*d.to = n
	}
	return c, errors.Join(errs...)
}

// CgroupMemoryMiB reads the container's memory limit from a cgroup file, such as
// /sys/fs/cgroup/memory.max. "max", no limit, is 0.
func CgroupMemoryMiB(file string) (int64, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(b))
	if s == "max" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %q", file, s)
	}
	return n >> 20, nil
}

// fallbackMemoryMiB is what the programs share when the container's limit isn't known.
const fallbackMemoryMiB = 1536

// Shares split the container's memory between the programs: Loki, Tempo and Prometheus a
// quarter each, and the collector and Grafana the rest, three fifths and two fifths.
func Shares(totalMiB int64) map[string]int64 {
	if totalMiB <= 0 {
		totalMiB = fallbackMemoryMiB
	}
	quarter := totalMiB / 4
	rest := totalMiB - 3*quarter
	return map[string]int64{
		"loki": quarter, "tempo": quarter, "prometheus": quarter,
		"collector": rest * 3 / 5, "grafana": rest * 2 / 5,
	}
}

// Program is one of the five programs.
type Program struct {
	Name string
	Path string
	Args []string
	// Env is added to the container's environment.
	Env []string
}

// Programs are the five programs, in the order they start: the stores first, so the
// collector's intake has somewhere to go.
func Programs(c Config) []Program {
	shares := Shares(c.MemoryMiB)
	file := func(name string) string { return filepath.Join(c.ConfigDir, name) }
	mem := func(name string) []string { return []string{fmt.Sprintf("GOMEMLIMIT=%dMiB", shares[name])} }
	return []Program{
		{Name: "loki", Path: c.Bin.Loki, Args: []string{"-config.file=" + file("loki.json")}, Env: mem("loki")},
		{Name: "tempo", Path: c.Bin.Tempo, Args: []string{"-config.file=" + file("tempo.json")}, Env: mem("tempo")},
		{Name: "prometheus", Path: c.Bin.Prometheus, Args: []string{
			"--config.file=" + file("prometheus.json"),
			"--storage.tsdb.path=" + filepath.Join(c.DataDir, "prometheus"),
			fmt.Sprintf("--storage.tsdb.retention.time=%dd", c.Retention.Metrics),
			"--web.listen-address=127.0.0.1:9090",
			"--web.enable-otlp-receiver",
			"--log.level=warn",
		}, Env: mem("prometheus")},
		{Name: "grafana", Path: c.Bin.Grafana, Args: []string{
			"server", "--homepath=" + c.Bin.GrafanaHome, "--config=" + file("grafana.ini"),
		}, Env: append(mem("grafana"),
			// Grafana's image sets these, and they win over grafana.ini.
			"GF_PATHS_DATA="+grafanaData(c),
			"GF_PATHS_PLUGINS="+filepath.Join(grafanaData(c), "plugins"),
			"GF_PATHS_LOGS="+filepath.Join(grafanaData(c), "log"),
			"GF_PATHS_PROVISIONING="+filepath.Join(c.ConfigDir, "grafana", "provisioning"),
			"GF_PATHS_CONFIG="+file("grafana.ini"),
		)},
		{Name: "collector", Path: c.Bin.Collector, Args: []string{"--config=" + file("collector.json")}, Env: mem("collector")},
	}
}
