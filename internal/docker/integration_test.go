package docker

import (
	"os"
	"testing"
)

// TestAgainstLocalDocker checks the client against a real daemon, read-only. It runs only
// with PROTOSTRAP_DOCKER_TEST=1, and it needs DOCKER_SOCKET if Docker isn't at
// /var/run/docker.sock.
func TestAgainstLocalDocker(t *testing.T) {
	if os.Getenv("PROTOSTRAP_DOCKER_TEST") != "1" {
		t.Skip("set PROTOSTRAP_DOCKER_TEST=1 to run against the local Docker daemon")
	}
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	c := New(socket, nil)
	ctx := t.Context()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if c.version == "" {
		t.Fatal("the daemon's API version wasn't learned")
	}
	info, err := c.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.CPUs < 1 || info.MemoryBytes <= 0 || info.Swarm.State == "" {
		t.Fatalf("info %+v", info)
	}
	if _, err := c.Containers(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := c.HasImage(ctx, "protostrap.invalid/nothing:here"); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	t.Logf("API %s, %d CPUs, %d MiB, swarm %s", c.version, info.CPUs, info.MemoryBytes>>20, info.Swarm.State)
}
