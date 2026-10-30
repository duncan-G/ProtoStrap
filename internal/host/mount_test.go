package host

import (
	"strings"
	"testing"
)

const mountinfo = `22 1 259:1 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p1 rw
24 22 0:5 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
51 22 259:3 / /data/postgres rw,noatime shared:30 - xfs /dev/nvme1n1 rw
52 22 259:4 / /data/redis rw,noatime shared:31 - xfs /dev/nvme2n1 rw
53 22 259:5 / /mnt/with\040space rw,noatime shared:32 - xfs /dev/nvme3n1 rw
`

func TestMountpoints(t *testing.T) {
	points, err := parseMountinfo(strings.NewReader(mountinfo))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/proc", "/data/postgres", "/data/redis", "/mnt/with space"} {
		if !points[p] {
			t.Errorf("%s should be a mount point", p)
		}
	}
	if points["/data"] || points["/data/kafka"] {
		t.Error("/data and /data/kafka are not mount points")
	}
}

func TestMountedOnThisMachine(t *testing.T) {
	ok, err := Mounted("/proc")
	if err != nil || !ok {
		t.Fatalf("/proc should be mounted: %v %v", ok, err)
	}
	ok, err = Mounted(t.TempDir())
	if err != nil || ok {
		t.Fatalf("a temp dir is not a mount point: %v %v", ok, err)
	}
}

func TestMountedCleansThePath(t *testing.T) {
	ok, _ := Mounted("/proc/")
	if !ok {
		t.Fatal("trailing slash should not matter")
	}
}

func TestParseMountinfoRejectsGarbage(t *testing.T) {
	if _, err := parseMountinfo(strings.NewReader("short line\n")); err == nil {
		t.Fatal("expected an error")
	}
}
