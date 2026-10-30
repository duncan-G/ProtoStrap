// Package configfiles is a component's config files (docs/config.md): a directory from the
// application's repository that the release carries and the agent mounts, read-only, in the
// component's replicas. A set of files is named by its digest, so a release holds each set
// once, the scheduler serves it to agents by that name, and an agent can check what it was
// sent.
package configfiles

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Files are config files, keyed by their path under the directory they are mounted at, such
// as lds.yaml or conf.d/extra.conf.
type Files map[string][]byte

const (
	// MaxBytes is the most a set of files may hold. Config is small; anything larger belongs
	// in the image.
	MaxBytes = 1 << 20
	// MaxFiles is the most files a set may have.
	MaxFiles = 256
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Digest names a set of files: a hash of each file's path and content.
func Digest(f Files) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range slices.Sorted(maps.Keys(f)) {
		// Each path and content is preceded by its length, so no two sets hash alike.
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
		binary.BigEndian.PutUint64(n[:], uint64(len(f[p])))
		h.Write(n[:])
		h.Write(f[p])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// ValidDigest reports whether d can name a set of files.
func ValidDigest(d string) bool { return digestPattern.MatchString(d) }

// ValidPath reports whether p can name a file in a set: relative, tidy, inside the directory,
// and with no part starting with "..", which the agent keeps for its own links.
func ValidPath(p string) bool {
	if p == "" || strings.ContainsAny(p, "\\\x00") || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || strings.HasPrefix(part, "..") {
			return false
		}
	}
	return true
}

// Validate checks the files can be written as they are named.
func (f Files) Validate() error {
	if len(f) == 0 {
		return errors.New("no files")
	}
	if len(f) > MaxFiles {
		return fmt.Errorf("%d files: at most %d", len(f), MaxFiles)
	}
	var errs []error
	total := 0
	for _, p := range slices.Sorted(maps.Keys(f)) {
		total += len(f[p])
		if !ValidPath(p) {
			errs = append(errs, fmt.Errorf("%q isn't a relative path inside the directory, or uses a name starting with .., such as ..data, which the agent keeps for itself", p))
			continue
		}
		for dir := path.Dir(p); dir != "."; dir = path.Dir(dir) {
			if _, ok := f[dir]; ok {
				errs = append(errs, fmt.Errorf("%q is a file and a directory", dir))
			}
		}
	}
	if total > MaxBytes {
		errs = append(errs, fmt.Errorf("%d bytes: at most %d MiB, since config is small; put larger files in the image", total, MaxBytes>>20))
	}
	return errors.Join(errs...)
}
