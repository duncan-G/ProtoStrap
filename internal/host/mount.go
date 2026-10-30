// Package host answers questions about the VM itself.
package host

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Mounted reports whether path is a mount point, such as a stateful component's EBS
// volume at /data/<component>.
func Mounted(path string) (bool, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer f.Close()
	points, err := parseMountinfo(f)
	if err != nil {
		return false, err
	}
	return points[filepath.Clean(path)], nil
}

// Volumes checks volumes with Mounted.
type Volumes struct{}

func (Volumes) Mounted(path string) (bool, error) { return Mounted(path) }

// parseMountinfo returns the mount points in /proc/self/mountinfo, whose fifth field is
// the mount point with spaces and the like escaped in octal.
func parseMountinfo(r io.Reader) (map[string]bool, error) {
	points := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			return nil, fmt.Errorf("mountinfo: short line %q", sc.Text())
		}
		points[unescape(fields[4])] = true
	}
	return points, sc.Err()
}

func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
