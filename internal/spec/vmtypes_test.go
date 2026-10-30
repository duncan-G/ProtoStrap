package spec

import (
	"strings"
	"testing"
)

// Pools run on Graviton (arm64). Any Graviton type will do, burstable t4g included: traffic
// between VMs is encrypted in hardware only on the types that do it (docs/network.md#encryption).
func TestGravitonTypes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cpu, memory   int64
		instanceStore bool
	}{
		{"c7g.medium", 1000, 2048, false},
		{"m7g.large", 2000, 8192, false},
		{"r8g.xlarge", 4000, 32768, false},
		{"c8g.2xlarge", 8000, 16384, false},
		// Graviton2 doesn't encrypt traffic between VMs in hardware, and still runs a pool.
		{"c6g.medium", 1000, 2048, false},
		{"m6g.large", 2000, 8192, false},
		// Burstable types have sizes of their own: every t4g up to large has two vCPUs.
		{"t4g.nano", 2000, 512, false},
		{"t4g.micro", 2000, 1024, false},
		{"t4g.small", 2000, 2048, false},
		{"t4g.medium", 2000, 4096, false},
		{"t4g.large", 2000, 8192, false},
		{"t4g.xlarge", 4000, 16384, false},
		{"t4g.2xlarge", 8000, 32768, false},
		// A "d" family has local disks.
		{"m7gd.large", 2000, 8192, true},
		{"m6gd.xlarge", 4000, 16384, true},
	} {
		got, err := LookupVM(tc.name)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got.CPU != tc.cpu || got.Memory != tc.memory || got.InstanceStore != tc.instanceStore {
			t.Errorf("%s is %+v, want %dm, %dMi, instance store %v", tc.name, got, tc.cpu, tc.memory, tc.instanceStore)
		}
	}
}

func TestTypesThatCantRunAPool(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"m7i.large", "x86"},
		{"c7a.medium", "x86"},
		{"m5.xlarge", "x86"},
		{"t3.small", "x86"},
		{"t3a.medium", "x86"},
		{"z9g.xlarge", "unknown instance type"},
		{"m7g.3xlarge", "unknown instance size"},
		{"t4g.4xlarge", "unknown instance size"},
		{"m7g", "isn't an instance type"},
	} {
		_, err := LookupVM(tc.name)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want an error about %q", tc.name, err, tc.want)
		}
	}
	// The x86 error says what to use instead.
	if _, err := LookupVM("m7i.large"); err == nil || !strings.Contains(err.Error(), "arm64") {
		t.Errorf("m7i.large: %v doesn't say pools run on arm64", err)
	}
}
