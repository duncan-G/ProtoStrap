package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/container"
)

// xdsV1 and xdsV2 are a gateway's config files: Envoy's dynamic config, one release apart.
// v2 drops a file and a nested one, adds one and changes one.
var (
	xdsV1 = configfiles.Files{
		"lds.yaml":       []byte("listeners: v1\n"),
		"rds.yaml":       []byte("routes: v1\n"),
		"cds.yaml":       []byte("clusters: v1\n"),
		"extra/old.yaml": []byte("old\n"),
	}
	xdsV2 = configfiles.Files{
		"lds.yaml":       []byte("listeners: v2\n"),
		"cds.yaml":       []byte("clusters: v1\n"),
		"extra/new.yaml": []byte("new\n"),
	}
	digestV1 = configfiles.Digest(xdsV1)
	digestV2 = configfiles.Digest(xdsV2)
)

// gateway is a service whose config files the agent delivers at /etc/envoy/xds.
func gateway(id string) api.ReplicaSpec {
	return api.ReplicaSpec{
		ID: id, Component: "gateway", Kind: api.KindService, Release: "9f2c1e7", Fingerprint: "fp-gateway",
		Image: registry + "gateway:c-1", Request: api.Resources{CPU: 500, Memory: 256},
		Port: 8080, Ready: "/ready", Stop: 30, Role: "arn:aws:iam::123456789012:role/protostrap-shop-gateway",
		ConfigPath: "/etc/envoy/xds", ConfigDigest: digestV1,
	}
}

// follower is a gateway that takes new config files without being replaced, running hook
// once they are swapped in, if it has one.
func follower(id string, hook ...string) api.ReplicaSpec {
	s := gateway(id)
	s.ConfigFollows, s.ConfigHook = true, hook
	return s
}

// withConfigs has the scheduler serve both versions of the gateway's files.
func withConfigs(h *harness, cfg *Config, d *Deps) {
	h.sched.configs = map[string]configfiles.Files{digestV1: xdsV1, digestV2: xdsV2}
}

// configFile reads a replica's config file the way the replica would, through the
// directory mounted at its config path.
func (h *harness) configFile(replica, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(h.cfg.ConfigDir, replica, filepath.FromSlash(name)))
	return string(b), err
}

func (h *harness) configFor(replica, digest string) {
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	if h.sched.assignment.ConfigDigests == nil {
		h.sched.assignment.ConfigDigests = map[string]string{}
	}
	h.sched.assignment.ConfigDigests[replica] = digest
}

func TestConfigFilesAreWrittenBeforeTheReplicaStarts(t *testing.T) {
	h := newHarness(t, withConfigs)
	var atStart string
	h.rt.onStart = func(name string) { atStart, _ = h.configFile(name, "lds.yaml") }
	h.sched.assign(gateway("gateway-1"))
	h.mustTick()
	if atStart != "listeners: v1\n" {
		t.Fatalf("at start lds.yaml held %q: the replica reads it as it starts", atStart)
	}
	if got, err := h.configFile("gateway-1", "extra/old.yaml"); err != nil || got != "old\n" {
		t.Errorf("a nested file: %q %v", got, err)
	}
	spec := h.rt.specOf("gateway-1")
	want := container.Mount{Source: filepath.Join(h.cfg.ConfigDir, "gateway-1"), Target: "/etc/envoy/xds", ReadOnly: true}
	if !slices.Contains(spec.Mounts, want) {
		t.Errorf("mounts %+v, want %+v: the directory, read-only, so a swap is seen", spec.Mounts, want)
	}
	if spec.Labels[container.LabelConfigDigest] != digestV1 {
		t.Errorf("labels %v", spec.Labels)
	}
	if info, err := os.Stat(filepath.Join(h.cfg.ConfigDir, "gateway-1", "lds.yaml")); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("a config file is readable by the replica's own user, and not writable: %v %v", info.Mode(), err)
	}
	if !slices.Equal(h.sched.configFetches, []string{digestV1}) {
		t.Errorf("fetches %v", h.sched.configFetches)
	}
	h.mustTick()
	if r := h.replica("gateway-1"); r.ConfigDigest != digestV1 || r.ConfigFailure != nil {
		t.Errorf("reported %q %+v", r.ConfigDigest, r.ConfigFailure)
	}
}

func TestConfigThatCantBeFetchedFailsTheReplica(t *testing.T) {
	for name, setup := range map[string]func(h *harness){
		"the scheduler can't serve it": func(h *harness) { h.sched.configErr = errors.New("HTTP 404: no config") },
		"the files aren't what the digest says": func(h *harness) {
			h.sched.configs = map[string]configfiles.Files{digestV1: xdsV2}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, withConfigs)
			setup(h)
			h.sched.assign(gateway("gateway-1"))
			h.mustTick()
			h.mustTick()
			r := h.replica("gateway-1")
			if r.State != api.StateFailed || !strings.Contains(r.Reason, "config") {
				t.Errorf("replica %+v: the reason says it's the config", r)
			}
			if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
				t.Errorf("a container was created without its config: %v", creates)
			}
		})
	}
}

func TestAFollowerSwapsInNewConfig(t *testing.T) {
	h := newHarness(t, withConfigs)
	h.running(follower("gateway-1"))
	h.configFor("gateway-1", digestV2)
	h.mustTick()
	for name, want := range map[string]string{"lds.yaml": "listeners: v2\n", "cds.yaml": "clusters: v1\n", "extra/new.yaml": "new\n"} {
		if got, err := h.configFile("gateway-1", name); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q", name, got, err, want)
		}
	}
	if _, err := h.configFile("gateway-1", "extra/old.yaml"); err == nil {
		t.Errorf("a file v2 dropped is still there")
	}
	if _, err := os.Lstat(filepath.Join(h.cfg.ConfigDir, "gateway-1", "rds.yaml")); err == nil {
		t.Errorf("the link to rds.yaml, which v2 dropped, is still there, pointing at nothing")
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 1 {
		t.Errorf("creates %v: a follower isn't replaced", creates)
	}
	if h.execCount() != 0 {
		t.Errorf("reload runs nothing: the replica watches its files")
	}
	dir := filepath.Join(h.cfg.ConfigDir, "gateway-1")
	if target, err := os.Readlink(filepath.Join(dir, "..data")); err != nil || !strings.HasPrefix(target, "..v-") {
		t.Errorf("..data -> %q %v: every file changes at once, through one link", target, err)
	}
	if entries, _ := os.ReadDir(dir); slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return e.Name() == "..v-"+digestV1
	}) {
		t.Errorf("the old version is still in %s", dir)
	}
	h.mustTick()
	if r := h.replica("gateway-1"); r.ConfigDigest != digestV2 || r.ConfigFailure != nil {
		t.Errorf("reported %q %+v", r.ConfigDigest, r.ConfigFailure)
	}
}

func TestAFollowersHookRunsOnceItsFilesAreSwapped(t *testing.T) {
	h := newHarness(t, withConfigs)
	var mu sync.Mutex
	var seen []string
	h.rt.exec = func(_ context.Context, name string, cmd []string, _, _ io.Writer) (int, error) {
		lds, _ := h.configFile(name, "lds.yaml")
		mu.Lock()
		seen = append(seen, lds)
		mu.Unlock()
		return 0, nil
	}
	h.running(follower("gateway-1", "kill", "-HUP", "1"))
	h.mustTick()
	if h.execCount() != 0 {
		t.Fatalf("the hook ran before any change: a replica reads its files when it starts")
	}
	h.configFor("gateway-1", digestV2)
	h.mustTick()
	h.mustTick()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"listeners: v2\n"}) {
		t.Errorf("the hook saw %v: once, with the new files", seen)
	}
	if !slices.Contains(h.rt.execs, "gateway-1: kill -HUP 1") {
		t.Errorf("execs %v", h.rt.execs)
	}
	if got := h.replica("gateway-1").ConfigDigest; got != digestV2 {
		t.Errorf("digest %q", got)
	}
}

func TestAFailingConfigHookIsReportedAndRetried(t *testing.T) {
	h := newHarness(t, withConfigs)
	h.secrets.put("v-1", secretV1())
	s := follower("gateway-1", "sh", "-c", "check-and-reload")
	s.SecretPrefix, s.SecretFiles, s.SecretVersion = "Postgres_", []string{"POSTGRES_PASSWORD"}, "v-1"
	h.running(s)
	fail := true
	h.rt.exec = func(_ context.Context, _ string, _ []string, _, stderr io.Writer) (int, error) {
		if fail {
			io.WriteString(stderr, "bad listener; password was "+pgPassword1)
			return 1, nil
		}
		return 0, nil
	}
	h.configFor("gateway-1", digestV2)
	h.mustTick()
	h.mustTick()
	r := h.replica("gateway-1")
	if r.ConfigDigest != digestV1 || r.ConfigFailure == nil || r.ConfigFailure.Digest != digestV2 ||
		!strings.Contains(r.ConfigFailure.Reason, "exited with 1") || !strings.Contains(r.ConfigFailure.Reason, "bad listener") {
		t.Fatalf("replica %+v %+v", r, r.ConfigFailure)
	}
	if strings.Contains(r.ConfigFailure.Reason, pgPassword1) {
		t.Errorf("the reason holds a secret value: %q", r.ConfigFailure.Reason)
	}
	if r.State != api.StateReady {
		t.Errorf("a failed hook leaves the replica running: %s", r.State)
	}
	n := h.execCount()
	h.mustTick()
	if h.execCount() != n {
		t.Errorf("the hook was retried at once")
	}
	fail = false
	h.clock.advance(h.cfg.ConfigRetry)
	h.mustTick()
	h.mustTick()
	if r := h.replica("gateway-1"); r.ConfigDigest != digestV2 || r.ConfigFailure != nil {
		t.Errorf("after the retry: %+v %+v", r, r.ConfigFailure)
	}
}

func TestAFollowerTakesNewConfigOnlyOnceReady(t *testing.T) {
	h := newHarness(t, withConfigs)
	h.sched.assign(follower("gateway-1", "kill", "-HUP", "1"))
	h.configFor("gateway-1", digestV1)
	h.mustTick() // It starts, not ready.
	h.configFor("gateway-1", digestV2)
	h.mustTick()
	if got, _ := h.configFile("gateway-1", "lds.yaml"); got != "listeners: v1\n" || h.execCount() != 0 {
		t.Errorf("lds.yaml %q, execs %v: a replica that isn't ready yet keeps what it started with", got, h.rt.execs)
	}
	h.prober.setReady("gateway", true)
	h.mustTick()
	h.mustTick()
	if got, _ := h.configFile("gateway-1", "lds.yaml"); got != "listeners: v2\n" {
		t.Errorf("once ready, lds.yaml %q", got)
	}
}

func TestANewFollowerStartsWithTheAssignedConfig(t *testing.T) {
	// A replica made mid-rollout starts with the files its assignment names, which may be
	// newer than those it was made with.
	h := newHarness(t, withConfigs)
	h.configFor("gateway-1", digestV2)
	h.sched.assign(follower("gateway-1"))
	h.mustTick()
	if got, _ := h.configFile("gateway-1", "lds.yaml"); got != "listeners: v2\n" {
		t.Errorf("lds.yaml %q", got)
	}
	if got := h.rt.specOf("gateway-1").Labels[container.LabelConfigDigest]; got != digestV2 {
		t.Errorf("label %q", got)
	}
}

func TestAnAgentRestartDoesNotRerunAConfigHook(t *testing.T) {
	h := newHarness(t, withConfigs)
	h.running(follower("gateway-1", "kill", "-HUP", "1"))
	h.configFor("gateway-1", digestV2)
	h.mustTick()
	h.mustTick()
	if h.execCount() != 1 {
		t.Fatalf("execs %v", h.rt.execs)
	}
	h.restartAgent()
	for range 3 {
		h.mustTick()
	}
	if n := h.execCount(); n != 1 {
		t.Errorf("a restarted agent ran the hook %d more times", n-1)
	}
	if got := h.replica("gateway-1").ConfigDigest; got != digestV2 {
		t.Errorf("digest %q after a restart", got)
	}
}

func TestConfigFilesGoWithTheirReplica(t *testing.T) {
	h := newHarness(t, withConfigs)
	h.running(follower("gateway-1"))
	// Files left behind by a replica the agent never finished with are swept too.
	stale := filepath.Join(h.cfg.ConfigDir, "gateway-0")
	os.MkdirAll(stale, 0o755)
	os.WriteFile(stale+".json", []byte(`{}`), 0o600)
	h.sched.assign()
	h.mustTick()
	h.mustTick()
	if left := fileList(t, h.cfg.ConfigDir); len(left) != 0 {
		t.Errorf("left in the config dir: %v", left)
	}
}

func TestTheConfigDirIsNotTheFilesDir(t *testing.T) {
	// The agent removes whatever under the files directory its assignment doesn't name.
	cfg := Config{App: "shop", InstanceID: "i-1", Region: "eu-west-1", Version: "1.4.0", Pool: "general",
		FilesDir: "/var/lib/protostrap/files", ConfigDir: "/var/lib/protostrap/files/config"}.WithDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "config dir") {
		t.Errorf("err %v", err)
	}
	if got := (Config{}).WithDefaults().ConfigDir; got != "/var/lib/protostrap/config" {
		t.Errorf("default config dir %q", got)
	}
}
