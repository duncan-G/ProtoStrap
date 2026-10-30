// Package update replaces the agent's own binary with another version of protostrap from
// the deploy bucket, when the scheduler names one.
package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Store reads the deploy bucket.
type Store interface {
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// Updater installs a version over Executable.
type Updater struct {
	Store      Store
	Executable string // It is the running binary's path.
	Arch       string // It is amd64 or arm64.
}

var version = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$`)

// ValidVersion reports whether v is a semantic version, safe to use in a key.
func ValidVersion(v string) bool { return version.MatchString(v) }

// Key is where a version's binary lives in the deploy bucket. Its SHA-256 is beside it,
// in Key + ".sha256".
func (u *Updater) Key(v string) string {
	return "protostrap/" + v + "/protostrap-linux-" + u.Arch
}

// Update downloads a version, checks it against its published SHA-256, and moves it over
// the running binary. The new binary runs from the next start.
func (u *Updater) Update(ctx context.Context, v string) error {
	if !ValidVersion(v) {
		return fmt.Errorf("update: %q is not a version", v)
	}
	key := u.Key(v)
	want, err := u.checksum(ctx, key+".sha256")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(u.Executable), filepath.Base(u.Executable)+".new-*")
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	defer os.Remove(tmp.Name()) // The removal is a no-op once the file is renamed.
	defer tmp.Close()

	body, err := u.Store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("update: fetching %s: %w", key, err)
	}
	defer body.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), body)
	if err != nil {
		return fmt.Errorf("update: fetching %s: %w", key, err)
	}
	if n == 0 {
		return fmt.Errorf("update: %s is empty", key)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return fmt.Errorf("update: %s has SHA-256 %s, published %s", key, got, want)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if err := os.Rename(tmp.Name(), u.Executable); err != nil {
		return fmt.Errorf("update: %w", err)
	}
	return nil
}

// checksum reads a sha256sum-style file: the hex digest, then optionally a file name.
func (u *Updater) checksum(ctx context.Context, key string) (string, error) {
	body, err := u.Store.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("update: fetching %s: %w", key, err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return "", fmt.Errorf("update: fetching %s: %w", key, err)
	}
	fields := bytes.Fields(data)
	if len(fields) == 0 {
		return "", errors.New("update: empty checksum " + key)
	}
	digest := strings.ToLower(string(fields[0]))
	if _, err := hex.DecodeString(digest); err != nil || len(digest) != 64 {
		return "", fmt.Errorf("update: %s is not a SHA-256", key)
	}
	return digest, nil
}
