package spec

import (
	"fmt"
	"strings"
)

// Platform is what every VM runs, and so what every image is built for: Graviton, arm64.
const Platform = "linux/arm64"

// VMType is what ProtoStrap needs to know about an EC2 instance type.
type VMType struct {
	// CPU is in millicores.
	CPU int64
	// Memory is in MiB.
	Memory int64
	// InstanceStore reports whether the type has local disks, which lose data on any stop.
	InstanceStore bool
}

// family is an instance family: memory per vCPU, and the sizes it comes in.
type family struct {
	gibPerVCPU    float64
	instanceStore bool
}

// families are the Graviton families ProtoStrap knows. Every one has a 1-vCPU medium size.
// Graviton3 and later encrypt traffic between VMs in hardware, and Graviton2 doesn't; the
// overlay works on either (docs/network.md#encryption).
var families = map[string]family{
	// Graviton2.
	"c6g": {2, false}, "m6g": {4, false}, "r6g": {8, false}, "c6gn": {2, false},
	"c6gd": {2, true}, "m6gd": {4, true}, "r6gd": {8, true},
	// Graviton3.
	"c7g": {2, false}, "m7g": {4, false}, "r7g": {8, false}, "c7gn": {2, false},
	"c7gd": {2, true}, "m7gd": {4, true}, "r7gd": {8, true},
	// Graviton4.
	"c8g": {2, false}, "m8g": {4, false}, "r8g": {8, false},
	"c8gd": {2, true}, "m8gd": {4, true}, "r8gd": {8, true},
}

var sizes = map[string]int64{
	"medium": 1, "large": 2, "xlarge": 4, "2xlarge": 8, "4xlarge": 16, "8xlarge": 32,
	"12xlarge": 48, "16xlarge": 64, "24xlarge": 96, "48xlarge": 192,
}

// burstable are the t4g sizes, which don't follow a family's memory per vCPU. AWS runs them
// unlimited by default, so CPU above their baseline is billed rather than throttled, and
// ProtoStrap counts all their vCPUs.
var burstable = map[string]struct{ vcpus, mib int64 }{
	"nano": {2, 512}, "micro": {2, 1024}, "small": {2, 2048}, "medium": {2, 4096},
	"large": {2, 8192}, "xlarge": {4, 16384}, "2xlarge": {8, 32768},
}

// LookupVM describes an instance type, or says why ProtoStrap can't run a pool on it.
func LookupVM(name string) (VMType, error) {
	fam, size, ok := strings.Cut(name, ".")
	if !ok || fam == "" || size == "" {
		return VMType{}, fmt.Errorf("%q isn't an instance type", name)
	}
	if !graviton(fam) {
		return VMType{}, fmt.Errorf("%s is x86: pools run on Graviton (arm64), such as m7g or t4g", name)
	}
	if fam == "t4g" {
		b, known := burstable[size]
		if !known {
			return VMType{}, fmt.Errorf("unknown instance size %s", name)
		}
		return VMType{CPU: b.vcpus * 1000, Memory: b.mib}, nil
	}
	f, known := families[fam]
	if !known {
		return VMType{}, fmt.Errorf("unknown instance type %s: ProtoStrap doesn't know its size", name)
	}
	vcpus, known := sizes[size]
	if !known {
		return VMType{}, fmt.Errorf("unknown instance size %s", name)
	}
	return VMType{
		CPU:           vcpus * 1000,
		Memory:        int64(float64(vcpus) * f.gibPerVCPU * 1024),
		InstanceStore: f.instanceStore,
	}, nil
}

// graviton reports whether a family is arm64: a "g" after its generation, as in m7g or c6gn.
func graviton(fam string) bool {
	for i := 1; i < len(fam); i++ {
		if fam[i-1] >= '0' && fam[i-1] <= '9' {
			return fam[i] == 'g'
		}
	}
	return false
}
