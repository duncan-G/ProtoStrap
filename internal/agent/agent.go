// Package agent is `protostrap agent`: it runs exactly the replicas the scheduler assigns
// to its VM, and reports on them (docs/protostrap.md#agent).
//
// Every 5 seconds the agent observes its containers, sends the scheduler a report and gets
// back the VM's assignment, then converges: it starts every assigned replica that isn't
// running and stops every running one that isn't assigned. It keeps no state of its own.
// Container labels are the record, so a restarted agent picks up where it left off, and
// what it holds in memory is rebuilt from what it observes.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/credentials"
	"github.com/duncan-G/protostrap/internal/probe"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

var (
	// ErrUpdated means the agent installed another version of itself and must restart.
	ErrUpdated = errors.New("agent updated: restart to run the new version")
	// ErrTerminated means the VM has been drained and its terminate hook completed.
	ErrTerminated = errors.New("VM terminated")
)

// Config is the agent's VM and its timings.
type Config struct {
	App        string
	InstanceID string
	// Name defaults to the instance ID.
	Name string
	// Pool is ingress, general, compute or data.
	Pool   string
	Zone   string
	Region string
	// Version is this binary's ProtoStrap version.
	Version string
	// PrivateIP is advertised to the swarm.
	PrivateIP string
	// SwarmManager is set on data-1, where the swarm manager runs, so the agent never joins.
	SwarmManager bool

	// Network is the overlay. It defaults to protostrap.
	Network string
	// DataDir is where stateful volumes are mounted, at DataDir/<component>. It defaults to
	// /data.
	DataDir string
	// FilesDir is where assignment files are written. It defaults to
	// /var/lib/protostrap/files.
	FilesDir string
	// EdgeFilesMount is where the edge sees FilesDir/edge. It defaults to /etc/protostrap.
	EdgeFilesMount string
	// SecretsDir is where replicas' secret files are written, at SecretsDir/<replica>. It
	// is on tmpfs, so they never reach a disk, and must not be under FilesDir. It defaults
	// to /run/protostrap/secrets.
	SecretsDir string
	// SecretsMount is where a replica sees its secret files. It defaults to /run/secrets.
	SecretsMount string
	// ConfigDir is where replicas' config files are written, at ConfigDir/<replica>, and
	// must not be under FilesDir. It defaults to /var/lib/protostrap/config.
	ConfigDir string
	// TelemetryAddress is where Docker sends every replica's output: the telemetry
	// collector on data-1's fixed address, such as 10.20.1.10:24224 (docs/telemetry.md).
	TelemetryAddress string
	// TelemetrySocketDir holds Grafana's socket, which the telemetry replica on data-1
	// makes and the scheduler serves. It defaults to /run/protostrap/telemetry.
	TelemetrySocketDir string
	// CredentialsPath is what credentials URIs start with. It defaults to /v1/credentials/.
	CredentialsPath string

	// SyncInterval is how often the agent runs a cycle. It defaults to 5s.
	SyncInterval time.Duration
	// ReadyTimeout is how long a replica has to become ready before it has failed. It
	// defaults to 5m.
	ReadyTimeout time.Duration
	// ProbeTimeout is how long one command check may run. It defaults to 5s.
	ProbeTimeout time.Duration
	// SilentAfter is how long, while the VM is terminating, the scheduler can go without
	// answering before the agent counts it as down. It defaults to 30s.
	SilentAfter time.Duration
	// HookTimeout is the terminate hook's heartbeat timeout. It defaults to 10m.
	HookTimeout time.Duration
	// DrainMargin is how long before the deadline draining should finish. It defaults to 15s.
	DrainMargin time.Duration
	// SpotStopLimit is what a Spot interruption cuts stop times to. It defaults to 90s.
	SpotStopLimit time.Duration
	// SpotWarning is how long the VM has left when a Spot notice gives no time. It defaults
	// to 2m.
	SpotWarning time.Duration
	// BackupHour is the UTC hour from which the daily backups run.
	BackupHour int
	// BackupRetry is the wait after a failed backup. It defaults to 1h.
	BackupRetry time.Duration
	// JoinRetry is the wait between attempts to join the swarm. It defaults to 5s.
	JoinRetry time.Duration
	// HookRetry is the wait between attempts to complete the launch hook. It defaults to
	// 10s.
	HookRetry time.Duration
	// SecretHookTimeout is how long a follower's hook has to apply a new version of its
	// keys. It defaults to 2m.
	SecretHookTimeout time.Duration
	// SecretRetry is the wait after a follower failed to apply a version. It defaults to 1m.
	SecretRetry time.Duration
	// ConfigHookTimeout is how long a follower's hook has to apply new config files. It
	// defaults to 2m.
	ConfigHookTimeout time.Duration
	// ConfigRetry is the wait after a follower failed to take new config files. It defaults
	// to 1m.
	ConfigRetry time.Duration
}

// WithDefaults fills in every unset field that has a default.
func (c Config) WithDefaults() Config {
	set := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	setD := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	set(&c.Name, c.InstanceID)
	set(&c.Network, "protostrap")
	set(&c.DataDir, "/data")
	set(&c.FilesDir, "/var/lib/protostrap/files")
	set(&c.EdgeFilesMount, "/etc/protostrap")
	set(&c.SecretsDir, "/run/protostrap/secrets")
	set(&c.SecretsMount, "/run/secrets")
	set(&c.ConfigDir, "/var/lib/protostrap/config")
	set(&c.TelemetrySocketDir, telemetry.SocketDir)
	set(&c.CredentialsPath, "/v1/credentials/")
	setD(&c.SyncInterval, 5*time.Second)
	setD(&c.ReadyTimeout, 5*time.Minute)
	setD(&c.ProbeTimeout, 5*time.Second)
	setD(&c.SilentAfter, 30*time.Second)
	setD(&c.HookTimeout, 10*time.Minute)
	setD(&c.DrainMargin, 15*time.Second)
	setD(&c.SpotStopLimit, 90*time.Second)
	setD(&c.SpotWarning, 2*time.Minute)
	setD(&c.BackupRetry, time.Hour)
	setD(&c.JoinRetry, 5*time.Second)
	setD(&c.HookRetry, 10*time.Second)
	setD(&c.SecretHookTimeout, 2*time.Minute)
	setD(&c.SecretRetry, time.Minute)
	setD(&c.ConfigHookTimeout, 2*time.Minute)
	setD(&c.ConfigRetry, time.Minute)
	return c
}

func (c Config) Validate() error {
	var errs []error
	for name, v := range map[string]string{"app": c.App, "instance id": c.InstanceID, "region": c.Region, "version": c.Version} {
		if v == "" {
			errs = append(errs, fmt.Errorf("no %s", name))
		}
	}
	switch c.Pool {
	case "ingress", "general", "compute", "data":
	default:
		errs = append(errs, fmt.Errorf("pool %q is not ingress, general, compute or data", c.Pool))
	}
	if c.BackupHour < 0 || c.BackupHour > 23 {
		errs = append(errs, fmt.Errorf("backup hour %d", c.BackupHour))
	}
	if host, port, err := net.SplitHostPort(c.TelemetryAddress); err != nil || host == "" || port == "" {
		errs = append(errs, fmt.Errorf("telemetry address %q: want data-1's address and port, such as 10.20.1.10:24224", c.TelemetryAddress))
	}
	for name, dir := range map[string]string{"data dir": c.DataDir, "files dir": c.FilesDir, "edge files mount": c.EdgeFilesMount,
		"secrets dir": c.SecretsDir, "secrets mount": c.SecretsMount, "config dir": c.ConfigDir, "telemetry socket dir": c.TelemetrySocketDir} {
		if !path.IsAbs(dir) {
			errs = append(errs, fmt.Errorf("%s %q is not absolute", name, dir))
		}
	}
	// The agent removes whatever under the files directory its assignment doesn't name.
	if rel, err := filepath.Rel(c.FilesDir, c.SecretsDir); err == nil && !strings.HasPrefix(rel, "..") {
		errs = append(errs, fmt.Errorf("secrets dir %q is in the files dir", c.SecretsDir))
	}
	if rel, err := filepath.Rel(c.FilesDir, c.ConfigDir); err == nil && !strings.HasPrefix(rel, "..") {
		errs = append(errs, fmt.Errorf("config dir %q is in the files dir", c.ConfigDir))
	}
	if len(c.CredentialsPath) < 2 || c.CredentialsPath[0] != '/' || c.CredentialsPath[len(c.CredentialsPath)-1] != '/' {
		errs = append(errs, fmt.Errorf("credentials path %q must start and end with /", c.CredentialsPath))
	}
	for name, d := range map[string]time.Duration{
		"sync interval": c.SyncInterval, "ready timeout": c.ReadyTimeout, "silent after": c.SilentAfter,
		"hook timeout": c.HookTimeout, "drain margin": c.DrainMargin, "spot stop limit": c.SpotStopLimit,
		"spot warning": c.SpotWarning, "backup retry": c.BackupRetry, "join retry": c.JoinRetry, "hook retry": c.HookRetry,
		"secret hook timeout": c.SecretHookTimeout, "secret retry": c.SecretRetry,
		"config hook timeout": c.ConfigHookTimeout, "config retry": c.ConfigRetry,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive", name))
		}
	}
	return errors.Join(errs...)
}

// Runtime runs containers: Docker.
type Runtime interface {
	Ping(ctx context.Context) error
	Info(ctx context.Context) (container.Info, error)
	Containers(ctx context.Context) ([]container.Container, error)
	Stats(ctx context.Context, id string) (container.Stats, error)
	HasImage(ctx context.Context, image string) (bool, error)
	Pull(ctx context.Context, image string) error
	Create(ctx context.Context, spec container.Spec) (string, error)
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Remove(ctx context.Context, id string) error
	Exec(ctx context.Context, id string, cmd []string, stdout, stderr io.Writer) (int, error)
	JoinSwarm(ctx context.Context, join container.Join) error
	LeaveSwarm(ctx context.Context) error
}

// Scheduler is the scheduler's API.
type Scheduler interface {
	Join(ctx context.Context) (api.Join, error)
	Sync(ctx context.Context, report api.Report) (api.Assignment, error)
	// Config fetches the config files a digest names.
	Config(ctx context.Context, digest string) (configfiles.Files, error)
}

// Outbox holds the agent's log records until a sync carries them to the scheduler, for the
// infra log (docs/protostrap.md#infra-log).
type Outbox interface {
	// Take removes and returns the oldest n records.
	Take(n int) []api.LogRecord
	// Return puts back records a failed sync didn't deliver.
	Return(records []api.LogRecord)
}

// syncRecords is the most log records one sync carries.
const syncRecords = 500

// Hook is one of the Auto Scaling group's lifecycle hooks.
type Hook string

const (
	LaunchHook    Hook = "launch"
	TerminateHook Hook = "terminate"

	HookContinue = "CONTINUE"
	HookAbandon  = "ABANDON"
)

// ErrHookDone is what Hooks return, wrapped, when AWS has no such hook in progress for the
// VM: it was completed before the agent restarted, or it timed out.
var ErrHookDone = errors.New("no lifecycle action in progress")

// Hooks completes the VM's lifecycle hooks. VMs outside an Auto Scaling group, the data
// VMs, have none.
type Hooks interface {
	Complete(ctx context.Context, hook Hook, result string) error
	Heartbeat(ctx context.Context, hook Hook) error
}

// Notice says AWS is about to remove the VM: through its Auto Scaling group's terminate
// hook, or a Spot interruption at At.
type Notice struct {
	Spot bool
	At   time.Time
}

// Metadata watches instance metadata for the VM's removal.
type Metadata interface {
	Termination(ctx context.Context) (*Notice, error)
}

// Firewall keeps the container firewall, the credentials redirect and, on an ingress VM,
// the NAT rules in place, returning what it had to put back.
type Firewall interface {
	Ensure(ctx context.Context) ([]string, error)
}

// Zone takes over an ingress VM's zone: its Elastic IP and the route out.
type Zone interface {
	TakeOver(ctx context.Context) error
}

// Credentials serves replicas their component role's credentials.
type Credentials interface {
	Put(e credentials.Entry)
	Remove(replica string)
	Replicas() []string
}

// Prober runs readiness checks.
type Prober interface {
	Probe(ctx context.Context, t probe.Target) error
}

// Scraper reads metrics from a replica's Prometheus endpoint.
type Scraper interface {
	Scrape(ctx context.Context, addr, endpoint string, names []string) (map[string]float64, error)
}

// Volumes checks a stateful component's volume is mounted.
type Volumes interface {
	Mounted(path string) (bool, error)
}

// Check is a boot check, such as the credential helper or the SSM Agent.
type Check struct {
	Name string
	Run  func(ctx context.Context) error
}

// Backups is the backups bucket.
type Backups interface {
	Exists(ctx context.Context, key string) (bool, error)
	Upload(ctx context.Context, key string, body io.Reader) error
}

// Secrets reads the environment's secret with a component's role: a version of it, or the
// current one when version is empty, and that version's ID.
type Secrets interface {
	Read(ctx context.Context, secretID, version, role, component string) (value, versionID string, err error)
}

// Updater installs another version of the agent's binary.
type Updater interface {
	Update(ctx context.Context, version string) error
}

// Notifier tells systemd the agent is ready, and alive for its watchdog.
type Notifier interface {
	Notify(state string) error
}

// Clock is time, replaceable in tests.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Deps are what the agent works through.
type Deps struct {
	Runtime     Runtime
	Scheduler   Scheduler
	Hooks       Hooks // It is nil outside an Auto Scaling group.
	Metadata    Metadata
	Firewall    Firewall
	Zone        Zone // Only ingress VMs have one.
	Credentials Credentials
	Prober      Prober
	Scraper     Scraper
	Volumes     Volumes
	Checks      []Check
	Backups     Backups // Only data VMs have one.
	Secrets     Secrets // It reads secret files; without it, replicas with any fail.
	Updater     Updater
	Notifier    Notifier
	Clock       Clock         // It defaults to the system clock.
	NewToken    func() string // It defaults to credentials.NewToken.
	Logger      *slog.Logger  // It defaults to slog.Default().
	Logs        Outbox        // It is optional: without it, reports carry no log records.
}

// Agent runs one VM's replicas.
type Agent struct {
	cfg      Config
	rt       Runtime
	sched    Scheduler
	hooks    Hooks
	meta     Metadata
	fw       Firewall
	zone     Zone
	creds    Credentials
	prober   Prober
	scraper  Scraper
	volumes  Volumes
	checks   []Check
	store    Backups // It is the backups bucket, on data VMs only.
	secrets  Secrets
	updater  Updater
	notifier Notifier
	clock    Clock
	newToken func() string
	log      *slog.Logger
	outbox   Outbox

	// Only the goroutine calling Tick uses the fields below.
	capacity api.Resources
	lastSync time.Time
	// last is the last assignment, kept for the metrics it asks for.
	last api.Assignment
	// term is set once AWS is removing the VM.
	term *termination
	// done is set once the VM is drained and terminated.
	done bool
	// cpu holds the last CPU sample of each container, by container ID.
	cpu map[string]cpuSample

	// Background starts, stops and backups change the fields below too.
	mu sync.Mutex
	// ops holds the starts and stops in flight, by replica.
	ops map[string]*op
	// failed holds the replicas that failed, by replica.
	failed map[string]failure
	// ready holds the replicas that have been ready.
	ready map[string]bool
	// seen holds when each replica was first seen, by replica.
	seen map[string]sighting
	// runs holds today's backup of each component, by component.
	runs map[string]*backupRun
	// follows holds each follower's secret files, by replica.
	follows map[string]*follow
	// unrestored holds the replicas whose secret files a reboot removed, and which the agent
	// hasn't been able to write again, by replica.
	unrestored map[string]unrestored
	// configFollows holds each follower's config files, by replica.
	configFollows map[string]*configFollow
	// launchHookDone is closed once the agent has stopped trying to complete the launch
	// hook.
	launchHookDone chan struct{}

	wg sync.WaitGroup
}

// New returns an agent. Hooks, Zone and Backups are optional; so are the clock, token
// generator and logger, which have defaults.
func New(cfg Config, d Deps) (*Agent, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("agent config: %w", err)
	}
	required := map[string]bool{
		"runtime": d.Runtime == nil, "scheduler": d.Scheduler == nil, "metadata": d.Metadata == nil,
		"firewall": d.Firewall == nil, "credentials": d.Credentials == nil, "prober": d.Prober == nil,
		"scraper": d.Scraper == nil, "volumes": d.Volumes == nil, "updater": d.Updater == nil,
		"notifier": d.Notifier == nil,
	}
	for name, missing := range required {
		if missing {
			return nil, fmt.Errorf("agent: no %s", name)
		}
	}
	a := &Agent{
		cfg: cfg, rt: d.Runtime, sched: d.Scheduler, hooks: d.Hooks, meta: d.Metadata, fw: d.Firewall,
		zone: d.Zone, creds: d.Credentials, prober: d.Prober, scraper: d.Scraper, volumes: d.Volumes,
		checks: d.Checks, store: d.Backups, secrets: d.Secrets, updater: d.Updater, notifier: d.Notifier,
		clock: d.Clock, newToken: d.NewToken, log: d.Logger, outbox: d.Logs,
		cpu: map[string]cpuSample{}, ops: map[string]*op{}, failed: map[string]failure{},
		ready: map[string]bool{}, seen: map[string]sighting{}, runs: map[string]*backupRun{},
		follows: map[string]*follow{}, unrestored: map[string]unrestored{}, configFollows: map[string]*configFollow{},
	}
	if a.clock == nil {
		a.clock = SystemClock{}
	}
	if a.newToken == nil {
		a.newToken = credentials.NewToken
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	return a, nil
}

// Wait waits for the starts, stops and backups in flight.
func (a *Agent) Wait() { a.wg.Wait() }

// Run boots the agent, then runs a cycle every SyncInterval until ctx is done. It returns
// ErrUpdated when the agent must restart to run a new version. After the VM is drained it
// stays idle rather than exit, since systemd would start it again.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.Boot(ctx); err != nil {
		return err
	}
	for {
		err := a.Tick(ctx)
		switch {
		case errors.Is(err, ErrUpdated):
			a.Wait()
			return err
		case errors.Is(err, ErrTerminated):
			a.log.Info("VM drained; idle until it is gone")
			<-ctx.Done()
			return nil
		case err != nil && ctx.Err() == nil:
			a.log.Warn("cycle", "err", err)
		}
		if err := a.clock.Sleep(ctx, a.cfg.SyncInterval); err != nil {
			return nil // The agent is shutting down.
		}
	}
}
