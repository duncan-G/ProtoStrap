package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegistryOf(t *testing.T) {
	cases := map[string]string{
		"1.dkr.ecr.eu-west-1.amazonaws.com/protostrap/api:c-1": "1.dkr.ecr.eu-west-1.amazonaws.com",
		"localhost:5000/x:1":     "localhost:5000",
		"registry.example.com/x": "registry.example.com",
		"postgres:17.2":          "docker.io",
		"library/postgres:17.2":  "docker.io",
		"user/image":             "docker.io",
	}
	for image, want := range cases {
		if got := registryOf(image); got != want {
			t.Errorf("registryOf(%q) = %q, want %q", image, got, want)
		}
	}
}

// helper writes a fake credential helper script and returns its path.
func helper(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker-credential-ecr-login")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCredentialHelperGet(t *testing.T) {
	h := CredentialHelper{Path: helper(t, `
[ "$1" = get ] || exit 9
read registry
echo "{\"ServerURL\":\"$registry\",\"Username\":\"AWS\",\"Secret\":\"pw-for-$registry\"}"
`)}
	user, secret, ok, err := h.Credentials(t.Context(), "1.dkr.ecr.eu-west-1.amazonaws.com")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if user != "AWS" || secret != "pw-for-1.dkr.ecr.eu-west-1.amazonaws.com" {
		t.Fatalf("user=%q secret=%q", user, secret)
	}
}

func TestCredentialHelperOnlyForECR(t *testing.T) {
	h := CredentialHelper{Path: helper(t, `echo called >&2; exit 7`)}
	_, _, ok, err := h.Credentials(t.Context(), "docker.io")
	if ok || err != nil {
		t.Fatalf("non-ECR registries pull anonymously: ok=%v err=%v", ok, err)
	}
}

func TestCredentialHelperFailure(t *testing.T) {
	h := CredentialHelper{Path: helper(t, `echo "no credentials" >&2; exit 1`)}
	_, _, _, err := h.Credentials(t.Context(), "1.dkr.ecr.eu-west-1.amazonaws.com")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestCredentialHelperCheck(t *testing.T) {
	if err := (CredentialHelper{Path: helper(t, "exit 0")}).Check(); err != nil {
		t.Fatal(err)
	}
	if err := (CredentialHelper{Path: filepath.Join(t.TempDir(), "missing")}).Check(); err == nil {
		t.Fatal("a missing helper must fail the boot check")
	}
}
