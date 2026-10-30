package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type store map[string][]byte

func (s store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := s[key]
	if !ok {
		return nil, errors.New("NoSuchKey: " + key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func setup(t *testing.T) (*Updater, store, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "protostrap")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := store{}
	return &Updater{Store: s, Executable: exe, Arch: "amd64"}, s, exe
}

func TestUpdateReplacesTheBinary(t *testing.T) {
	u, s, exe := setup(t)
	bin := []byte("new binary 1.4.1")
	s["protostrap/1.4.1/protostrap-linux-amd64"] = bin
	s["protostrap/1.4.1/protostrap-linux-amd64.sha256"] = []byte(digest(bin) + "  protostrap-linux-amd64\n")
	if err := u.Update(t.Context(), "1.4.1"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(exe)
	if !bytes.Equal(got, bin) {
		t.Fatalf("binary is %q", got)
	}
	info, _ := os.Stat(exe)
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("binary must be executable, mode %v", info.Mode())
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "*.new*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestUpdateRefusesABadChecksum(t *testing.T) {
	u, s, exe := setup(t)
	s["protostrap/1.4.1/protostrap-linux-amd64"] = []byte("tampered")
	s["protostrap/1.4.1/protostrap-linux-amd64.sha256"] = []byte(digest([]byte("genuine")))
	if err := u.Update(t.Context(), "1.4.1"); err == nil {
		t.Fatal("expected a checksum error")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old binary" {
		t.Fatalf("the running binary must be kept, got %q", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(exe), "*.new*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestUpdateNeedsAChecksum(t *testing.T) {
	u, s, _ := setup(t)
	s["protostrap/1.4.1/protostrap-linux-amd64"] = []byte("new")
	if err := u.Update(t.Context(), "1.4.1"); err == nil {
		t.Fatal("an unchecked binary must not be installed")
	}
}

func TestUpdateMissingVersion(t *testing.T) {
	u, _, exe := setup(t)
	if err := u.Update(t.Context(), "9.9.9"); err == nil {
		t.Fatal("expected an error")
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "old binary" {
		t.Fatal("binary changed")
	}
}

func TestUpdateRejectsUnsafeVersions(t *testing.T) {
	u, s, _ := setup(t)
	s["protostrap/../evil/protostrap-linux-amd64"] = []byte("x")
	for _, v := range []string{"", "../evil", "1.4.1/../../x", "1.4.1 ", "latest/", "a/b"} {
		if err := u.Update(t.Context(), v); err == nil {
			t.Errorf("version %q should be rejected", v)
		}
	}
}

func TestUpdateRejectsAnEmptyBinary(t *testing.T) {
	u, s, _ := setup(t)
	s["protostrap/1.4.1/protostrap-linux-amd64"] = nil
	s["protostrap/1.4.1/protostrap-linux-amd64.sha256"] = []byte(digest(nil))
	if err := u.Update(t.Context(), "1.4.1"); err == nil {
		t.Fatal("an empty binary is never right")
	}
}

func TestValidVersion(t *testing.T) {
	for _, v := range []string{"1.4.0", "1.4.1-rc.1", "2.0.0+build.5"} {
		if !ValidVersion(v) {
			t.Errorf("%q should be valid", v)
		}
	}
}
