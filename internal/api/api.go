// Package api holds the messages between agents and the scheduler: the report an agent
// sends on every sync, the assignment it gets back, and the swarm details it asks for at
// boot (docs/protostrap.md#sync).
//
// Its units are CPU in millicores, memory in MiB and stop times in seconds.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"time"

	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/probe"
	"github.com/duncan-G/protostrap/internal/secret"
)

// These are the replica states agents report. The scheduler sets pending and assigned
// itself (docs/protostrap.md#replica-lifecycle).
const (
	StateStarting  = "starting"
	StateReady     = "ready"
	StateFailed    = "failed"
	StateStopping  = "stopping"
	StateSucceeded = "succeeded" // It means a task exited with 0.
)

// Kind is what a component is.
type Kind string

const (
	KindService  Kind = "service"
	KindWorker   Kind = "worker"
	KindStateful Kind = "stateful"
	KindTask     Kind = "task"
	KindEdge     Kind = "edge"
	// KindTelemetry is ProtoStrap's telemetry replica on data-1 (docs/telemetry.md).
	KindTelemetry Kind = "telemetry"
)

func (k Kind) Valid() bool {
	switch k {
	case KindService, KindWorker, KindStateful, KindTask, KindEdge, KindTelemetry:
		return true
	}
	return false
}

// Resources are CPU in millicores and memory in MiB.
type Resources struct {
	CPU    int64 `json:"cpu"`
	Memory int64 `json:"memory"`
}

// Report is what an agent tells the scheduler about its VM on every sync.
type Report struct {
	VM       string          `json:"vm"`
	Name     string          `json:"name"`
	Pool     string          `json:"pool"`
	Zone     string          `json:"zone,omitempty"`
	Version  string          `json:"version"`
	Capacity Resources       `json:"capacity"`
	Replicas []ReplicaReport `json:"replicas"`

	// Terminating is set once AWS is about to remove the VM.
	Terminating *Termination `json:"terminating,omitempty"`
	// Repairs lists firewall and NAT rules the agent found missing and put back since the
	// last report.
	Repairs []string `json:"repairs,omitempty"`
	// Logs are the agent's log records since its last report, for the infra log
	// (docs/protostrap.md#infra-log). A scheduler of an earlier version ignores them.
	Logs []LogRecord `json:"logs,omitempty"`
}

// MarshalJSON sends an empty replica list as [], never null.
func (r Report) MarshalJSON() ([]byte, error) {
	type plain Report
	if r.Replicas == nil {
		r.Replicas = []ReplicaReport{}
	}
	return json.Marshal(plain(r))
}

// ReplicaReport is one replica as the agent sees it. Component, release, kind and
// fingerprint come from the container's labels, so a restarted scheduler can adopt it.
type ReplicaReport struct {
	ID          string `json:"id"`
	Component   string `json:"component,omitempty"`
	Release     string `json:"release,omitempty"`
	Kind        Kind   `json:"kind,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	State       string `json:"state"`
	// CPU and Memory are left out until they are known.
	CPU      *int64             `json:"cpu,omitempty"`
	Memory   *int64             `json:"memory,omitempty"`
	Restarts int                `json:"restarts"`
	ExitCode *int               `json:"exit_code,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	// SecretVersion is the version of the environment's secret the replica has: the one it
	// started with, or for a follower, the last one it applied.
	SecretVersion string `json:"secret_version,omitempty"`
	// SecretDigest is the scheduler's hash of the replica's keys when it was made, from its
	// label, so a restarted scheduler knows which replicas have the keys they should.
	SecretDigest string `json:"secret_digest,omitempty"`
	// SecretFailure is set when a follower couldn't apply a version.
	SecretFailure *SecretFailure `json:"secret_failure,omitempty"`
	// ConfigDigest names the config files the replica has: those it started with, or for a
	// follower, the last it took.
	ConfigDigest string `json:"config_digest,omitempty"`
	// ConfigFailure is set when a follower couldn't take new config files.
	ConfigFailure *ConfigFailure `json:"config_failure,omitempty"`
}

// SecretFailure is a version of the environment's secret a follower couldn't apply, and
// why. The reason never holds a value.
type SecretFailure struct {
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// ConfigFailure is config files a follower couldn't take, and why.
type ConfigFailure struct {
	Digest string `json:"digest"`
	Reason string `json:"reason"`
}

// These are the reasons a VM is terminated.
const (
	TerminationLifecycle = "lifecycle" // It comes from the Auto Scaling group's terminate hook.
	TerminationSpot      = "spot"      // It comes from a Spot interruption notice.
)

// Termination says AWS is removing the VM, and by when its replicas must be gone.
type Termination struct {
	Reason   string    `json:"reason"`
	Deadline time.Time `json:"deadline"`
}

// Assignment is everything a VM should run. It is complete: whatever runs on the VM and
// isn't in it is stopped.
type Assignment struct {
	Version  string            `json:"version"`
	Replicas []ReplicaSpec     `json:"replicas"`
	Files    map[string]string `json:"files,omitempty"`
	// Secret is the environment's secret, <app>/<environment>, which every replica reads.
	Secret string `json:"secret,omitempty"`
	// SecretVersions are the versions of the environment's secret that followers on the VM
	// should have, by component. Like Files, they change without new replicas.
	SecretVersions map[string]string `json:"secret_versions,omitempty"`
	// ConfigDigests name the config files that followers on the VM should have, by replica,
	// since a release swaps them in a quarter of a component's replicas at a time.
	ConfigDigests map[string]string `json:"config_digests,omitempty"`
}

// ReplicaSpec is one replica to run. Its settings never change: a new release, image,
// request or version of its keys gets a new ID. A follower's secret files and config files
// are the exceptions, and they are data, like Files.
type ReplicaSpec struct {
	ID          string `json:"id"`
	Component   string `json:"component"`
	Kind        Kind   `json:"kind,omitempty"`
	Release     string `json:"release"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Image       string `json:"image"`
	// Command overrides the image's command, for example with a migrate command.
	Command []string  `json:"command,omitempty"`
	Request Resources `json:"request"`
	Port    int       `json:"port,omitempty"`
	// Ready is the readiness check as a spec writes it, such as "grpc" or "9000/healthz", and
	// ReadyCommand is a command run in the replica instead.
	Ready        string   `json:"ready,omitempty"`
	ReadyCommand []string `json:"ready_command,omitempty"`
	// H2C says the replica's port speaks cleartext HTTP/2, so an HTTP check on it does too.
	H2C bool `json:"h2c,omitempty"`
	// Stop is the seconds between SIGTERM and SIGKILL.
	Stop int               `json:"stop"`
	Env  map[string]string `json:"env,omitempty"`
	// Role is the component role's ARN.
	Role    string       `json:"role,omitempty"`
	Metrics *MetricsSpec `json:"metrics,omitempty"`
	// Volume is where a stateful or telemetry replica's data lives in the container.
	Volume string `json:"volume,omitempty"`
	// Backup is a stateful replica's command whose output is the backup.
	Backup []string `json:"backup,omitempty"`
	// LogGroup is the CloudWatch Logs group Docker sends the replica's output to, when the
	// application runs no telemetry. Empty sends it to the telemetry collector on data-1.
	LogGroup string `json:"log_group,omitempty"`

	// SecretPrefix is the prefix of the component's keys in the environment's secret.
	SecretPrefix string `json:"secret_prefix,omitempty"`
	// SecretVersion is the version of the environment's secret the replica starts with,
	// unless it follows. Empty is the current one.
	SecretVersion string `json:"secret_version,omitempty"`
	// SecretDigest is the scheduler's keyed hash of the component's keys at SecretVersion,
	// which says nothing of their values. The agent only labels the replica with it.
	SecretDigest string `json:"secret_digest,omitempty"`
	// SecretFiles are the keys the agent writes to files in /run/secrets.
	SecretFiles []string `json:"secret_files,omitempty"`
	// SecretFollows has the agent rewrite the files, to the assignment's version for the
	// component, rather than the replica being replaced.
	SecretFollows bool `json:"secret_follows,omitempty"`
	// SecretHook is a follower's command, run in it once its files are rewritten.
	SecretHook []string `json:"secret_hook,omitempty"`

	// ConfigPath is where the replica's config files are mounted, read-only.
	ConfigPath string `json:"config_path,omitempty"`
	// ConfigDigest names the config files the replica starts with.
	ConfigDigest string `json:"config_digest,omitempty"`
	// ConfigFollows has the agent swap in the config files the assignment names for the
	// replica, rather than the replica being replaced.
	ConfigFollows bool `json:"config_follows,omitempty"`
	// ConfigHook is a follower's command, run in it once new config files are swapped in.
	ConfigHook []string `json:"config_hook,omitempty"`
}

// MetricsSpec is a replica's Prometheus endpoint, "port/path", and the metrics the
// scheduler wants from it.
type MetricsSpec struct {
	Endpoint string   `json:"endpoint"`
	Names    []string `json:"names,omitempty"`
}

// Join is what an agent needs to join the swarm.
type Join struct {
	SwarmID string `json:"swarm_id"`
	Token   string `json:"token"`
	Manager string `json:"manager"`
}

var (
	// replicaID is Docker's rule for container names, which replica IDs become.
	replicaID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	// Component names are DNS names on the overlay.
	componentName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	// Secret keys become file names, so they are variable names: never a path, nor ..data.
	secretKey    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	secretDigest = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
)

// Validate checks a replica can be run as described. The agent refuses an assignment with
// an invalid replica rather than run part of it.
func (s ReplicaSpec) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !replicaID.MatchString(s.ID) {
		fail("replica id %q is not a valid container name", s.ID)
	}
	if !componentName.MatchString(s.Component) {
		fail("component %q is not a valid name", s.Component)
	}
	if !s.Kind.Valid() {
		fail("kind %q is unknown", s.Kind)
	}
	if s.Image == "" {
		fail("no image")
	}
	if s.Request.CPU < 0 || s.Request.Memory <= 0 {
		fail("request %+v must reserve memory and no negative CPU", s.Request)
	}
	if s.Stop < 0 {
		fail("stop %d is negative", s.Stop)
	}
	if s.Port < 0 || s.Port > 65535 {
		fail("port %d is out of range", s.Port)
	}
	if s.Ready != "" {
		if _, err := probe.ParseCheck(s.Ready); err != nil {
			fail("ready %v", err)
		}
		if s.ReadyCommand != nil {
			fail("ready %q and a ready command: a replica has one check", s.Ready)
		}
	}
	if (s.Kind == KindStateful || s.Kind == KindTelemetry) && (s.Volume == "" || !path.IsAbs(s.Volume)) {
		fail("a %s replica needs an absolute volume path, got %q", s.Kind, s.Volume)
	}
	if s.SecretPrefix != "" && !secret.ValidPrefix(s.SecretPrefix) {
		fail("secret prefix %q is invalid", s.SecretPrefix)
	}
	if s.SecretVersion != "" && !secret.ValidVersion(s.SecretVersion) {
		fail("secret version %q is invalid", s.SecretVersion)
	}
	if s.SecretDigest != "" && !secretDigest.MatchString(s.SecretDigest) {
		fail("secret digest %q is invalid", s.SecretDigest)
	}
	for _, key := range s.SecretFiles {
		if !secretKey.MatchString(key) {
			fail("secret file %q is not a variable name", key)
		}
	}
	switch {
	case len(s.SecretFiles) > 0 && s.SecretPrefix == "":
		fail("secret files need the component's prefix")
	case s.SecretFollows && len(s.SecretFiles) == 0:
		fail("a follower needs secret files")
	case s.SecretFollows && s.Kind == KindTask:
		fail("a task can't follow its secret")
	case len(s.SecretHook) > 0 && !s.SecretFollows:
		fail("a secret hook is a follower's")
	}
	switch {
	case s.ConfigPath != "" && (!path.IsAbs(s.ConfigPath) || path.Clean(s.ConfigPath) != s.ConfigPath || s.ConfigPath == "/"):
		fail("config path %q must be a tidy absolute path other than /", s.ConfigPath)
	case s.ConfigDigest != "" && !configfiles.ValidDigest(s.ConfigDigest):
		fail("config digest %q is invalid", s.ConfigDigest)
	case (s.ConfigPath == "") != (s.ConfigDigest == ""):
		fail("config files need both a path and a digest")
	case s.ConfigFollows && s.ConfigPath == "":
		fail("a config follower needs config files")
	case s.ConfigFollows && s.Kind == KindTask:
		fail("a task can't follow its config")
	case len(s.ConfigHook) > 0 && !s.ConfigFollows:
		fail("a config hook is a follower's")
	}
	if len(errs) > 0 {
		return fmt.Errorf("replica %s: %w", s.ID, errors.Join(errs...))
	}
	return nil
}
