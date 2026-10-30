// Package container describes containers the way the agent sees them, independent of the
// Docker API that runs them.
package container

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// These labels are on every replica's container. Labels are the agent's only record of what
// it runs, so a restarted agent, and a restarted scheduler, pick up where they left off.
const (
	LabelReplica     = "protostrap.replica"
	LabelComponent   = "protostrap.component"
	LabelRelease     = "protostrap.release"
	LabelKind        = "protostrap.kind"
	LabelFingerprint = "protostrap.fingerprint"
	LabelApp         = "protostrap.app"
	LabelStop        = "protostrap.stop"  // It is the stop time in seconds.
	LabelRole        = "protostrap.role"  // It is the component role's ARN.
	LabelReady       = "protostrap.ready" // It is the check as a spec writes it, such as "grpc".
	// LabelReadyCommand is a command check, as a JSON array.
	LabelReadyCommand = "protostrap.ready-command"
	// LabelH2C is "true" when the replica's port speaks cleartext HTTP/2.
	LabelH2C     = "protostrap.h2c"
	LabelPort    = "protostrap.port"    // It is the port the replica listens on.
	LabelMetrics = "protostrap.metrics" // It is the Prometheus endpoint, as "port/path".
	LabelBackup  = "protostrap.backup"  // It is the backup command, as a JSON array.
	// LabelSecretVersion is the version of the environment's secret the replica started with.
	LabelSecretVersion = "protostrap.secret-version"
	// LabelSecretDigest is the scheduler's hash of the replica's keys when it was made.
	LabelSecretDigest = "protostrap.secret-digest"
	// LabelSecretFiles are the keys written to the replica's secret files, as a JSON array.
	LabelSecretFiles = "protostrap.secret-files"
	// LabelSecretFollows is set on a replica that takes new versions of its keys without
	// being replaced.
	LabelSecretFollows = "protostrap.secret-follows"
	// LabelConfigDigest names the config files the replica started with.
	LabelConfigDigest = "protostrap.config-digest"
	// LabelConfigFollows is set on a replica that takes new config files without being
	// replaced.
	LabelConfigFollows = "protostrap.config-follows"
)

// Status is Docker's state for a container.
type Status string

const (
	Created    Status = "created"
	Running    Status = "running"
	Paused     Status = "paused"
	Restarting Status = "restarting"
	Removing   Status = "removing"
	Exited     Status = "exited"
	Dead       Status = "dead"
)

// Up reports whether the container's process is running.
func (s Status) Up() bool { return s == Running || s == Paused }

// Container is a container on the VM.
type Container struct {
	ID       string
	Name     string
	Image    string
	Labels   map[string]string
	Env      map[string]string
	Status   Status
	ExitCode int
	Restarts int
	Created  time.Time
	CPUSet   string
	// Addrs are the addresses the VM reaches the container at, which are also the
	// addresses its requests to the VM come from.
	Addrs []string
}

func (c Container) Label(key string) string { return c.Labels[key] }

func (c Container) Replica() string { return c.Labels[LabelReplica] }

// RestartPolicy says what Docker does when a container exits.
type RestartPolicy string

const (
	RestartUnlessStopped RestartPolicy = "unless-stopped"
	RestartNever         RestartPolicy = "no"
)

// Spec is a container to create.
type Spec struct {
	Name    string
	Image   string
	Cmd     []string
	Env     []string // Each entry is KEY=value.
	Labels  map[string]string
	Network string
	Aliases []string

	MemoryBytes int64 // It is a hard limit.
	CPUShares   int64 // It is a relative weight, not a cap.
	CPUSet      string

	StopTimeout time.Duration
	Restart     RestartPolicy
	LogDriver   string
	LogOptions  map[string]string
	Mounts      []Mount
	// Publish binds container ports to ports on one of the VM's addresses.
	Publish []Port
}

// Port publishes a container port on one address of the VM, never on every address.
type Port struct {
	HostIP        string
	HostPort      int
	ContainerPort int
	Protocol      string // It is tcp or udp.
}

// Mount binds a path on the VM into the container.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

// Stats are a container's cumulative CPU time and current memory use.
type Stats struct {
	CPUNanos    uint64
	MemoryBytes int64
	Read        time.Time
}

// Info is the VM as Docker sees it.
type Info struct {
	CPUs        int
	MemoryBytes int64
	Swarm       Swarm
}

// Swarm is this node's place in the swarm.
type Swarm struct {
	State    string   // It is inactive, pending, active, error or locked.
	Manager  bool     // It reports whether this node is a manager.
	Managers []string // They are the managers this node knows, as host:port.
}

// Join is how to join a swarm.
type Join struct {
	Token         string
	Managers      []string
	AdvertiseAddr string
}

// ParseCPUSet reads Docker's cpuset syntax, such as "0-2,4", into sorted core numbers.
func ParseCPUSet(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var cores []int
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		first, err := strconv.Atoi(lo)
		if err != nil || first < 0 {
			return nil, fmt.Errorf("cpuset %q: bad core %q", s, lo)
		}
		last := first
		if isRange {
			if last, err = strconv.Atoi(hi); err != nil || last < first {
				return nil, fmt.Errorf("cpuset %q: bad range %q", s, part)
			}
		}
		for c := first; c <= last; c++ {
			cores = append(cores, c)
		}
	}
	slices.Sort(cores)
	return slices.Compact(cores), nil
}

// FormatCPUSet writes core numbers in Docker's cpuset syntax, with runs as ranges.
func FormatCPUSet(cores []int) string {
	cores = slices.Clone(cores)
	slices.Sort(cores)
	cores = slices.Compact(cores)
	var parts []string
	for i := 0; i < len(cores); {
		j := i
		for j+1 < len(cores) && cores[j+1] == cores[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, strconv.Itoa(cores[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cores[i], cores[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
