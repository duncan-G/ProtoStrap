package scheduler

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/spec"
)

// The scheduler sets these replica states; agents report the rest
// (docs/protostrap.md#replica-lifecycle).
const (
	StatePending  = "pending"  // The replica wants a VM.
	StateAssigned = "assigned" // The replica has a VM, but its agent hasn't started it yet.
)

// A rollout is in one of these states.
const (
	RolloutRunning    = "rolling out"
	RolloutDone       = "done"
	RolloutFailed     = "failed"
	RolloutSuperseded = "superseded"
)

// target is a release and its parsed spec.
type target struct {
	rel  release.Release
	spec *spec.Spec
}

// rollout is one deploy of a release and how it went.
type rollout struct {
	Commit   string
	State    string
	Reason   string
	Started  time.Time
	Finished time.Time
}

// VM is a VM whose agent has synced.
type VM struct {
	ID       string
	Name     string // It is data-1 for a data VM, else the instance ID.
	Pool     string
	Zone     string
	IP       string
	Capacity api.Resources
	Version  string

	FirstSync time.Time
	LastSync  time.Time
	Reported  map[string]api.ReplicaReport

	Terminating *api.Termination // It is set while AWS is removing the VM.
	Lost        bool             // The VM is silent: its replicas no longer count.
	Cordoned    bool             // The VM is being drained to be removed.
	Terminated  bool             // The scheduler asked AWS to remove the VM.
	Unhealthy   bool             // The VM is marked unhealthy for its group to replace.
}

// Replica is one replica as the scheduler sees it.
type Replica struct {
	ID          string
	Component   string
	Kind        api.Kind
	Release     string
	Fingerprint string
	Pool        string // It is where the replica should run: ingress, general, compute or data.
	Pin         string // It is a stateful replica's data VM, by name.
	VM          string // It is the instance the replica is assigned to, or "" while pending.
	Ephemeral   bool   // The replica is a compute worker placed on a general VM.
	State       string
	Stop        bool // It leaves the replica out of assignments, so its agent stops it.
	Request     api.Resources
	Spec        api.ReplicaSpec
	Created     time.Time
	Seen        bool   // The replica has been in a report.
	Reason      string // It is why the replica failed.
	ExitCode    *int
	// SecretDigest is the hash of the component's keys the replica was made with, and
	// SecretVersion their version, or for a follower the version it last applied.
	SecretDigest  string
	SecretVersion string
	SecretFailure *api.SecretFailure // It is a follower's, when it can't apply a version.
	// ConfigDigest names the config files the replica has: those it was made with, or for a
	// follower those it last took. ConfigWant names those a follower should have.
	ConfigDigest  string
	ConfigWant    string
	ConfigFailure *api.ConfigFailure // It is a follower's, when it can't take new files.

	cpu     []sample
	metrics map[string][]sample
}

type sample struct {
	at time.Time
	v  float64
}

// live reports whether a replica counts: it isn't being stopped, hasn't failed and isn't a
// finished task.
func (r *Replica) live() bool {
	return !r.Stop && r.State != api.StateFailed && r.State != api.StateSucceeded && r.State != api.StateStopping
}

// notReady reports whether a replica isn't serving yet: it is pending, assigned or starting.
func (r *Replica) notReady() bool {
	return r.State == StatePending || r.State == StateAssigned || r.State == api.StateStarting
}

// taskRun is a migration in one release.
type taskRun struct {
	replica string
	state   string // It is running, succeeded or failed.
	reason  string
}

// state is everything the scheduler holds in memory.
type state struct {
	started time.Time
	warm    bool

	target      *target
	lastGood    string
	releases    map[string]*target
	rollouts    []*rollout
	lastDesired map[string]int // It comes from state.json, for the hold after a start.
	saved       release.State
	join        *api.Join

	vms      map[string]*VM
	replicas map[string]*Replica
	desired  map[string]int
	why      map[string]*ScaleStatus // It says why each component wants its replicas, as of the last count.
	tasks    map[string]*taskRun     // It is keyed by release and task name.

	demand     map[string][]sample           // It holds each component's demand, for scaling down on the peak.
	cpuTotal   map[string][]sample           // It holds each component's CPU across its ready replicas.
	limits     map[string]sample             // It holds each limit metric's latest value.
	requests   map[string][]sample           // It holds each component's request count, summed across edges.
	edgeCounts map[string]map[string]float64 // It holds each edge replica's latest counts, by component.
	failures   map[string][]time.Time        // It holds each component's failure times outside a rollout.

	groups         []Group
	pools          map[string]poolState
	depths         map[string]Depth
	wantFewerSince map[string]time.Time // It is keyed by pool.
	lastUnhealthy  map[string]time.Time // It is keyed by pool.
	unfilledSince  map[string]time.Time // It is keyed by group.
	spotRetryAt    time.Time            // Until then, compute grows On-Demand.
	goneIPs        map[string]time.Time // It holds gone VMs' addresses, for removing their swarm nodes.
	lastSnapshot   time.Time
	reported       map[string]bool // It holds problems logged once, such as a pool more than half silent.

	secret  secretWatch
	secrets map[string]*componentSecret // It is keyed by component, and edge.

	configures map[string]*configureRun // It is keyed by the component configured.

	hosts hostsWatch
}

func newState() state {
	return state{
		releases: map[string]*target{}, vms: map[string]*VM{}, replicas: map[string]*Replica{},
		desired: map[string]int{}, tasks: map[string]*taskRun{}, demand: map[string][]sample{}, cpuTotal: map[string][]sample{},
		limits: map[string]sample{}, requests: map[string][]sample{}, edgeCounts: map[string]map[string]float64{},
		failures: map[string][]time.Time{}, depths: map[string]Depth{}, wantFewerSince: map[string]time.Time{}, pools: map[string]poolState{},
		lastUnhealthy: map[string]time.Time{}, unfilledSince: map[string]time.Time{}, goneIPs: map[string]time.Time{},
		reported: map[string]bool{}, secrets: map[string]*componentSecret{}, configures: map[string]*configureRun{},
	}
}

// observe records a VM's report: the VM itself, so it is part of its pool from its first
// sync.
func (st *state) observe(id VMIdentity, r api.Report, now time.Time) *VM {
	vm := st.vms[id.InstanceID]
	if vm == nil {
		vm = &VM{ID: id.InstanceID, FirstSync: now}
		st.vms[id.InstanceID] = vm
	}
	vm.Name, vm.Pool, vm.Zone, vm.IP = r.Name, r.Pool, r.Zone, id.PrivateIP
	if vm.Name == "" {
		vm.Name = vm.ID
	}
	vm.Capacity, vm.Version = r.Capacity, r.Version
	vm.LastSync = now
	vm.Terminating = r.Terminating
	vm.Lost = false
	vm.Reported = make(map[string]api.ReplicaReport, len(r.Replicas))
	for _, rr := range r.Replicas {
		vm.Reported[rr.ID] = rr
	}
	return vm
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// randomID is <component>-<6 random characters>.
func randomID(component string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return component + "-" + strings.ToLower(idEncoding.EncodeToString(b))[:6]
}

// Status is what `protostrap status`, `protostrap deploy` and the operator UI show
// (GET /v1/status): everything the scheduler knows, and why each component wants the
// replicas it does.
type Status struct {
	App         string            `json:"app"`
	Environment string            `json:"environment,omitempty"`
	Version     string            `json:"version"`
	Now         time.Time         `json:"now"`               // It is the scheduler's clock, to age the times below by.
	Started     time.Time         `json:"started"`           // It is when this scheduler started.
	Warming     bool              `json:"warming,omitempty"` // The scheduler is still only listening to agents.
	Holding     bool              `json:"holding,omitempty"` // The scheduler started recently and scales nothing down.
	Target      string            `json:"target"`
	LastGood    string            `json:"last_good"`
	Rollouts    []RolloutStatus   `json:"rollouts"`
	Pools       []PoolStatus      `json:"pools"`
	VMs         []VMStatus        `json:"vms"`
	Components  []ComponentStatus `json:"components"`
	Replicas    []ReplicaStatus   `json:"replicas"`
	Queues      []QueueStatus     `json:"queues,omitempty"`
	Secret      *SecretWatch      `json:"secret,omitempty"`
}

// SecretWatch is the environment's secret as the scheduler last read it.
type SecretWatch struct {
	Name    string    `json:"name"`
	Version string    `json:"version,omitempty"`
	Changed time.Time `json:"changed,omitempty"` // It is when the scheduler first saw the version.
	Checked time.Time `json:"checked,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// SecretStatus is a component's keys in the environment's secret.
type SecretStatus struct {
	Version string `json:"version,omitempty"` // It is the version its replicas should have.
	Pending string `json:"pending,omitempty"` // It is a newer version with other keys, not applied yet.
	Waiting string `json:"waiting,omitempty"` // It says what the pending version waits for.
	Failed  string `json:"failed,omitempty"`  // It says why replicas with Version fail.
}

// RolloutStatus is how a release's rollout went.
type RolloutStatus struct {
	Commit   string    `json:"commit"`
	State    string    `json:"state"`
	Reason   string    `json:"reason,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

// PoolStatus is one pool's size.
type PoolStatus struct {
	Name      string        `json:"name"`
	VMs       int           `json:"vms"`
	Booting   int           `json:"booting"`
	Desired   int           `json:"desired"` // It is the groups' desired capacity.
	Wanted    int           `json:"wanted"`  // It is what sizing and placement want.
	Requested api.Resources `json:"requested"`
	Pending   int           `json:"pending"` // It counts replicas waiting for a VM of this pool.
	Scale     *PoolScale    `json:"scale,omitempty"`
}

// PoolScale is how a scaling pool is sized (README.md#scaling-vms).
type PoolScale struct {
	VMType string        `json:"vm_type"` // It is the pool's first VM type, which sizing assumes.
	Min    int           `json:"min"`
	Max    int           `json:"max"`
	Spare  int           `json:"spare"`
	Target float64       `json:"target"`
	Usable api.Resources `json:"usable"` // It is one VM's room for replicas: (capacity − reserved) × target.
}

// VMStatus is one VM.
type VMStatus struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Pool        string        `json:"pool"`
	Zone        string        `json:"zone"`
	IP          string        `json:"ip,omitempty"`
	Version     string        `json:"version"`
	Capacity    api.Resources `json:"capacity"`
	Reserved    api.Resources `json:"reserved"` // It is what the VM's pool keeps back for the OS.
	Free        api.Resources `json:"free"`     // It is the capacity less the reserve and every request on it.
	Replicas    int           `json:"replicas"`
	FirstSync   time.Time     `json:"first_sync"`
	LastSync    time.Time     `json:"last_sync"`
	Lost        bool          `json:"lost,omitempty"`
	Cordoned    bool          `json:"cordoned,omitempty"`
	Terminating bool          `json:"terminating,omitempty"`
	Unhealthy   bool          `json:"unhealthy,omitempty"`
}

// ComponentStatus is one component's replicas.
type ComponentStatus struct {
	Name     string        `json:"name"`
	Kind     string        `json:"kind"`
	Pool     string        `json:"pool,omitempty"` // It is the pool or, for a stateful component, its data VM.
	Request  api.Resources `json:"request"`
	Desired  int           `json:"desired"`
	Ready    int           `json:"ready"`
	Starting int           `json:"starting"`
	Pending  int           `json:"pending"`
	Old      int           `json:"old"` // It counts replicas of an older fingerprint during a rollout.
	Scale    *ScaleStatus  `json:"scale,omitempty"`
	Secret   *SecretStatus `json:"secret,omitempty"`
	// Config is a follower's config files: those the target has, and how many replicas
	// don't have them yet.
	Config *ConfigStatus `json:"config,omitempty"`
	// Configure is the component's configure task.
	Configure *ConfigureStatus `json:"configure,omitempty"`
}

// ConfigStatus is a follower's config files (docs/config.md).
type ConfigStatus struct {
	Digest string `json:"digest"`           // It names the target release's files.
	Behind int    `json:"behind,omitempty"` // It counts replicas that don't have them yet.
	Failed string `json:"failed,omitempty"` // It is why a replica can't take them.
}

// ConfigureStatus is a component's configure task (docs/config.md): done, pending, running
// or failed.
type ConfigureStatus struct {
	Task    string `json:"task"`
	State   string `json:"state"`
	Waiting string `json:"waiting,omitempty"` // It is why a run that is due hasn't started.
	Failed  string `json:"failed,omitempty"`  // It is why the last run failed.
}

// ScaleStatus is why a service or worker wants the replicas it does, as of the last cycle
// (README.md#scaling-replicas).
type ScaleStatus struct {
	Min     int            `json:"min"`
	Max     int            `json:"max"`
	Reason  string         `json:"reason"`
	Signals []SignalStatus `json:"signals"`
	Limits  []LimitStatus  `json:"limits,omitempty"`
}

// SignalStatus is one demand signal: its reading, and the replicas it asks for.
type SignalStatus struct {
	Signal string `json:"signal"`           // It is as the spec writes it, such as "cpu: 70%".
	Demand *int   `json:"demand,omitempty"` // It is nil without a reading.
	Detail string `json:"detail"`
}

// LimitStatus is one limit, and the cap it sets.
type LimitStatus struct {
	Limit  string `json:"limit"`         // It is as the spec writes it, such as "metric: llm_budget_cap".
	Cap    *int   `json:"cap,omitempty"` // It is nil when the limit caps nothing.
	Detail string `json:"detail"`
}

// ReplicaStatus is one replica.
type ReplicaStatus struct {
	ID          string        `json:"id"`
	Component   string        `json:"component"`
	Kind        string        `json:"kind"`
	Release     string        `json:"release"`
	Fingerprint string        `json:"fingerprint"`
	Pool        string        `json:"pool,omitempty"`
	VM          string        `json:"vm,omitempty"`
	State       string        `json:"state"`
	Stopping    bool          `json:"stopping,omitempty"`
	Ephemeral   bool          `json:"ephemeral,omitempty"`
	Old         bool          `json:"old,omitempty"` // The replica is of an older fingerprint than the target's.
	Reason      string        `json:"reason,omitempty"`
	Request     api.Resources `json:"request"`
	CPU         *int64        `json:"cpu,omitempty"`    // It is in millicores, as its agent last reported.
	Memory      *int64        `json:"memory,omitempty"` // It is in MiB, as its agent last reported.
	Restarts    int           `json:"restarts,omitempty"`
	Created     time.Time     `json:"created"`
	// SecretVersion is the version of the component's keys the replica has.
	SecretVersion string `json:"secret_version,omitempty"`
	// ConfigDigest names the config files the replica has.
	ConfigDigest string `json:"config_digest,omitempty"`
}

// QueueStatus is one queue's jobs, as the scheduler last read them.
type QueueStatus struct {
	Name     string `json:"name"`
	Visible  int    `json:"visible"`
	InFlight int    `json:"in_flight"`
}
