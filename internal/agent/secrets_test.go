package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// The values below are the environment's secret in these tests. None of them may ever
// reach a report, a label, a log or an error.
const (
	pgPassword1 = "pg-password-one"
	pgPassword2 = "pg-password-two"
	kcPassword  = "kc-db-password"
	smtpShared  = "smtp-shared"
)

func secretV1() map[string]string {
	return map[string]string{
		"Postgres_POSTGRES_PASSWORD": pgPassword1,
		"Keycloak_KC_DB_PASSWORD":    kcPassword,
		"Shared_SMTP_PASSWORD":       smtpShared,
		"Api_ConnectionStrings__db":  "Host=postgres;Password=" + pgPassword1,
	}
}

func secretV2() map[string]string {
	v := secretV1()
	v["Postgres_POSTGRES_PASSWORD"] = pgPassword2
	v["Api_ConnectionStrings__db"] = "Host=postgres;Password=" + pgPassword2
	return v
}

// secretFile reads a replica's secret file the way the replica would, through the
// directory mounted at /run/secrets.
func (h *harness) secretFile(replica, key string) (string, error) {
	b, err := os.ReadFile(filepath.Join(h.cfg.SecretsDir, replica, key))
	return string(b), err
}

func (h *harness) follow(component, version string) {
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	if h.sched.assignment.SecretVersions == nil {
		h.sched.assignment.SecretVersions = map[string]string{}
	}
	h.sched.assignment.SecretVersions[component] = version
}

func (h *harness) execCount() int {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()
	return len(h.rt.execs)
}

// onData makes the harness a data VM whose volumes are mounted.
func onData(h *harness, cfg *Config, d *Deps) {
	dataVM(h, cfg, d)
	for _, c := range []string{"postgres", "keycloak"} {
		h.volumes.mounted[filepath.Join(cfg.DataDir, c)] = true
	}
}

func TestReplicasGetTheSecretAndTheirPrefix(t *testing.T) {
	h := newHarness(t)
	s := service("api-1")
	s.SecretVersion, s.SecretDigest = "v-1", "d1"
	h.sched.assign(s)
	h.mustTick()
	spec := h.rt.specOf("api-1")
	env := envMap(spec.Env)
	if env["SECRET_ID"] != "shop/prod" || env["SECRET_PREFIX"] != "Api_" {
		t.Errorf("SECRET_ID %q SECRET_PREFIX %q", env["SECRET_ID"], env["SECRET_PREFIX"])
	}
	if _, ok := env["SECRET_VERSION_ID"]; ok {
		t.Errorf("replicas read the current version: nothing pins them")
	}
	if spec.Labels[container.LabelSecretVersion] != "v-1" || spec.Labels[container.LabelSecretDigest] != "d1" {
		t.Errorf("labels %v", spec.Labels)
	}
	if len(spec.Mounts) != 0 || h.secrets.readCount() != 0 {
		t.Errorf("a replica that reads its own keys gets no files: mounts %v, reads %d", spec.Mounts, h.secrets.readCount())
	}
	h.mustTick()
	if r := h.replica("api-1"); r.SecretVersion != "v-1" || r.SecretDigest != "d1" {
		t.Errorf("reported secret version %q digest %q", r.SecretVersion, r.SecretDigest)
	}
}

func TestWithoutTheSchedulersSecretReplicasKeepTheirOld(t *testing.T) {
	// A scheduler from before the environment's secret names none; its replicas keep reading
	// their component's own secret, as they did.
	h := newHarness(t)
	h.sched.assignment.Secret = ""
	h.sched.assign(service("api-1"))
	h.mustTick()
	if env := envMap(h.rt.specOf("api-1").Env); env["SECRET_ID"] != "protostrap/api" {
		t.Errorf("SECRET_ID %q", env["SECRET_ID"])
	}
}

func TestSecretFilesHoldTheComponentsKeys(t *testing.T) {
	h := newHarness(t, onData)
	v := secretV1()
	v["Keycloak_SMTP_PASSWORD"] = "keycloak-own-smtp" // Its own key wins over the shared one.
	h.secrets.put("v-1", v)
	h.running(keycloak("keycloak-1", "v-1"))

	for key, want := range map[string]string{"KC_DB_PASSWORD": kcPassword, "SMTP_PASSWORD": "keycloak-own-smtp"} {
		if got, err := h.secretFile("keycloak-1", key); err != nil || got != want {
			t.Errorf("%s = %q %v, want %q with no newline", key, got, err, want)
		}
	}
	if _, err := h.secretFile("keycloak-1", "POSTGRES_PASSWORD"); err == nil {
		t.Errorf("another component's key was written")
	}
	dir := filepath.Join(h.cfg.SecretsDir, "keycloak-1")
	modes := map[string]os.FileMode{h.cfg.SecretsDir: 0o700, dir: 0o755}
	for p, want := range modes {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v %v, want %v", p, info.Mode().Perm(), err, want)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "KC_DB_PASSWORD")); err != nil || info.Mode().Perm() != 0o444 {
		t.Errorf("a secret file must be readable by the replica's own user, and not writable: %v %v", info.Mode(), err)
	}
}

func TestSecretFilesAreWrittenBeforeTheReplicaStarts(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	var atStart string
	h.rt.onStart = func(name string) { atStart, _ = h.secretFile(name, "KC_DB_PASSWORD") }
	h.sched.assign(keycloak("keycloak-1", ""))
	h.mustTick()
	if atStart != kcPassword {
		t.Fatalf("at start the file held %q: the replica reads it as it starts", atStart)
	}
	spec := h.rt.specOf("keycloak-1")
	want := container.Mount{Source: filepath.Join(h.cfg.SecretsDir, "keycloak-1"), Target: "/run/secrets", ReadOnly: true}
	if !slices.Contains(spec.Mounts, want) {
		t.Errorf("mounts %+v, want %+v: the directory, never single files, so rewrites are seen", spec.Mounts, want)
	}
	// With no version assigned the agent reads the current one, and the label says which.
	if spec.Labels[container.LabelSecretVersion] != "v-1" {
		t.Errorf("labels %v", spec.Labels)
	}
	var files []string
	if err := json.Unmarshal([]byte(spec.Labels[container.LabelSecretFiles]), &files); err != nil ||
		!slices.Equal(files, []string{"KC_DB_PASSWORD", "SMTP_PASSWORD"}) {
		t.Errorf("secret files label %q", spec.Labels[container.LabelSecretFiles])
	}
}

func TestSecretFilesAreReadWithTheComponentsRole(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.sched.assign(keycloak("keycloak-1", "v-1"))
	h.mustTick()
	want := "shop/prod|arn:aws:iam::123456789012:role/protostrap-shop-keycloak|keycloak|v-1"
	if !slices.Equal(h.secrets.reads, []string{want}) {
		t.Errorf("reads %v, want %s: the assigned version, with the component's own role", h.secrets.reads, want)
	}
}

func TestAMissingSecretKeyFailsTheReplica(t *testing.T) {
	h := newHarness(t, onData)
	v := secretV1()
	delete(v, "Shared_SMTP_PASSWORD")
	h.secrets.put("v-1", v)
	h.sched.assign(keycloak("keycloak-1", "v-1"))
	h.mustTick()
	h.mustTick()
	r := h.replica("keycloak-1")
	if r.State != api.StateFailed || !strings.Contains(r.Reason, "SMTP_PASSWORD") || !strings.Contains(r.Reason, "v-1") {
		t.Errorf("replica %+v: the reason names the key and the version", r)
	}
	if strings.Contains(r.Reason, kcPassword) {
		t.Errorf("the reason holds a value: %q", r.Reason)
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Errorf("a container was created without its secret files: %v", creates)
	}
}

func TestAnUnreadableSecretFailsTheReplica(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.err = errors.New("AccessDeniedException")
	h.sched.assign(keycloak("keycloak-1", "v-1"))
	h.mustTick()
	h.mustTick()
	if r := h.replica("keycloak-1"); r.State != api.StateFailed || !strings.Contains(r.Reason, "AccessDeniedException") {
		t.Errorf("replica %+v", r)
	}
	if creates, _, _, _, _ := h.rt.snapshot(); len(creates) != 0 {
		t.Errorf("creates %v", creates)
	}
}

func TestTasksGetSecretFiles(t *testing.T) {
	h := newHarness(t)
	h.secrets.put("v-1", secretV1())
	s := task("mig-1", "migrate")
	s.SecretPrefix, s.SecretFiles = "Postgres_", []string{"POSTGRES_PASSWORD"}
	h.sched.assign(s)
	h.mustTick()
	if got, err := h.secretFile("mig-1", "POSTGRES_PASSWORD"); err != nil || got != pgPassword1 {
		t.Errorf("file %q %v", got, err)
	}
}

func TestReloadRewritesFilesInPlace(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	pg := postgres("postgres-1")
	pg.SecretHook = nil // It reloads its files itself.
	h.follow("postgres", "v-1")
	h.running(pg)
	if got := h.replica("postgres-1").SecretVersion; got != "v-1" {
		t.Errorf("version %q", got)
	}
	dir := filepath.Join(h.cfg.SecretsDir, "postgres-1")
	before, _ := os.Stat(dir)

	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	if got, _ := h.secretFile("postgres-1", "POSTGRES_PASSWORD"); got != pgPassword2 {
		t.Errorf("file %q, want the new password", got)
	}
	if got, _ := h.secretFile("postgres-1", "previous/POSTGRES_PASSWORD"); got != pgPassword1 {
		t.Errorf("previous %q, want the old password", got)
	}
	after, _ := os.Stat(dir)
	if !os.SameFile(before, after) {
		t.Errorf("the directory was replaced: the replica's mount would still see the old one")
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-2" {
		t.Errorf("version %q, want v-2", got)
	}
	if creates, _, _, stops, _ := h.rt.snapshot(); len(creates) != 1 || len(stops) != 0 || h.execCount() != 0 {
		t.Errorf("a reload replaced or ran something: creates %v stops %v execs %d", creates, stops, h.execCount())
	}
}

func TestTheHookRunsWhenTheVersionChanges(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	var mu sync.Mutex
	var seen []string // It is the password the hook found in the file each time.
	h.rt.exec = func(_ context.Context, name string, cmd []string, _, _ io.Writer) (int, error) {
		pw, _ := h.secretFile(name, "POSTGRES_PASSWORD")
		mu.Lock()
		seen = append(seen, pw)
		mu.Unlock()
		return 0, nil
	}
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()

	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{pgPassword1, pgPassword2}) {
		t.Errorf("the hook saw %v: once when first ready, then once with the new file", seen)
	}
	if !slices.Contains(h.rt.execs, "postgres-1: sh -c alter-role") {
		t.Errorf("execs %v", h.rt.execs)
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-2" {
		t.Errorf("version %q", got)
	}
}

func TestAVersionWithTheSameKeysRunsNoHook(t *testing.T) {
	// Another component's keys changed, so the secret has a new version, but this one's
	// keys are the same: the files are rewritten and nothing is applied again.
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	v2 := secretV1()
	v2["Api_StripeKey"] = "sk_live_2"
	h.secrets.put("v-2", v2)
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	n := h.execCount()
	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	if h.execCount() != n {
		t.Errorf("the hook ran for keys that didn't change")
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-2" {
		t.Errorf("version %q", got)
	}
}

func TestTheHookRunsOnceAfterTheReplicaIsFirstReady(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.follow("postgres", "v-1")
	h.sched.assign(postgres("postgres-1"))
	h.mustTick()
	h.mustTick()
	if n := h.execCount(); n != 0 {
		t.Fatalf("%d hooks ran before the replica was ready", n)
	}
	if got := h.replica("postgres-1").SecretVersion; got != "" {
		t.Errorf("version %q reported before the hook applied it", got)
	}
	h.prober.setReady("postgres", true)
	for range 3 {
		h.mustTick()
	}
	if n := h.execCount(); n != 1 {
		t.Errorf("%d hooks ran, want one", n)
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-1" {
		t.Errorf("version %q", got)
	}
}

func TestAFailingHookIsReportedAndRetried(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()

	fail := true
	h.rt.exec = func(_ context.Context, _ string, _ []string, _, stderr io.Writer) (int, error) {
		if fail {
			io.WriteString(stderr, "psql: could not connect")
			return 2, nil
		}
		return 0, nil
	}
	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	r := h.replica("postgres-1")
	if r.SecretVersion != "v-1" || r.SecretFailure == nil || r.SecretFailure.Version != "v-2" ||
		!strings.Contains(r.SecretFailure.Reason, "exited with 2") || !strings.Contains(r.SecretFailure.Reason, "could not connect") {
		t.Fatalf("replica %+v %+v", r, r.SecretFailure)
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
	h.clock.advance(h.cfg.SecretRetry)
	h.mustTick()
	h.mustTick()
	if r := h.replica("postgres-1"); r.SecretVersion != "v-2" || r.SecretFailure != nil {
		t.Errorf("after the retry: %+v %+v", r, r.SecretFailure)
	}
}

func TestAHookThatHangsTimesOut(t *testing.T) {
	h := newHarness(t, onData, func(_ *harness, cfg *Config, _ *Deps) { cfg.SecretHookTimeout = 20 * time.Millisecond })
	h.secrets.put("v-1", secretV1())
	h.rt.exec = func(ctx context.Context, _ string, _ []string, _, _ io.Writer) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	r := h.replica("postgres-1")
	if r.SecretFailure == nil || !strings.Contains(r.SecretFailure.Reason, "within") {
		t.Errorf("replica %+v %+v", r, r.SecretFailure)
	}
}

func TestHookStderrIsRedacted(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	h.rt.exec = func(_ context.Context, _ string, _ []string, _, stderr io.Writer) (int, error) {
		fmt.Fprintf(stderr, "ERROR: LINE 1: ALTER ROLE postgres PASSWORD '%s' (was '%s')", pgPassword2, pgPassword1)
		return 3, nil
	}
	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	f := h.replica("postgres-1").SecretFailure
	if f == nil || strings.Contains(f.Reason, pgPassword1) || strings.Contains(f.Reason, pgPassword2) || !strings.Contains(f.Reason, "PASSWORD '***'") {
		t.Errorf("failure %+v", f)
	}
}

func TestAnAgentRestartDoesNotRerunAppliedHooks(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	if h.execCount() != 1 {
		t.Fatalf("execs %v", h.rt.execs)
	}
	h.restartAgent()
	for range 3 {
		h.mustTick()
	}
	if n := h.execCount(); n != 1 {
		t.Errorf("a restarted agent ran %d hooks: it updates itself with every release", n-1)
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-1" {
		t.Errorf("version %q", got)
	}
}

func TestAnAgentRestartRerunsAnUnfinishedHook(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	h.rt.exec = func(context.Context, string, []string, io.Writer, io.Writer) (int, error) { return 1, nil }
	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	h.rt.exec = nil
	n := h.execCount()
	h.restartAgent()
	h.mustTick()
	h.mustTick()
	if h.execCount() == n {
		t.Errorf("a restarted agent didn't finish applying v-2")
	}
	if got := h.replica("postgres-1").SecretVersion; got != "v-2" {
		t.Errorf("version %q", got)
	}
}

func TestSecretFilesAreRewrittenAfterAReboot(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.running(keycloak("keycloak-1", "v-1"))

	// A reboot empties /run, and Docker can't start a container whose bind source is gone.
	h.rt.set("keycloak-1", func(c *container.Container) { c.Status, c.ExitCode = container.Exited, 128 })
	if err := os.RemoveAll(h.cfg.SecretsDir); err != nil {
		t.Fatal(err)
	}
	h.restartAgent()
	h.mustTick()
	if got, err := h.secretFile("keycloak-1", "KC_DB_PASSWORD"); err != nil || got != kcPassword {
		t.Errorf("file %q %v", got, err)
	}
	if !slices.Equal(h.secrets.reads[len(h.secrets.reads)-1:], []string{"shop/prod|arn:aws:iam::123456789012:role/protostrap-shop-keycloak|keycloak|v-1"}) {
		t.Errorf("reads %v: the version the replica started with", h.secrets.reads)
	}
	if c, _ := h.rt.byName("keycloak-1"); c.Status != container.Running {
		t.Errorf("status %s, want started again", c.Status)
	}
	if st := h.state("keycloak-1"); st == api.StateFailed {
		t.Errorf("a replica whose files were restored failed")
	}
	h.mustTick()
	if st := h.state("keycloak-1"); st != api.StateReady {
		t.Errorf("state %s", st)
	}
}

func TestAReplicaWhoseSecretCantBeRestoredFailsAfterTheReadyTimeout(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.running(keycloak("keycloak-1", "v-1"))
	h.rt.set("keycloak-1", func(c *container.Container) { c.Status, c.ExitCode = container.Exited, 128 })
	os.RemoveAll(h.cfg.SecretsDir)
	h.secrets.err = errors.New("network unreachable")
	h.restartAgent()
	h.mustTick()
	if st := h.state("keycloak-1"); st != api.StateStarting {
		t.Errorf("state %s, want starting while the agent keeps trying", st)
	}
	h.clock.advance(h.cfg.ReadyTimeout + time.Second)
	h.mustTick()
	if r := h.replica("keycloak-1"); r.State != api.StateFailed || !strings.Contains(r.Reason, "secret files") {
		t.Errorf("replica %+v", r)
	}
}

func TestSecretFilesAreRemovedWithTheReplica(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	h.mustTick()
	h.sched.assign()
	h.mustTick()
	if entries, _ := os.ReadDir(h.cfg.SecretsDir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestARecordBeingWrittenIsNotSwept(t *testing.T) {
	h := newHarness(t, onData)
	h.secrets.put("v-1", secretV1())
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"))
	tmp := filepath.Join(h.cfg.SecretsDir, "postgres-1.json.tmp")
	os.WriteFile(tmp, []byte(`{}`), 0o600)
	h.mustTick()
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("a live replica's record, half written, was removed: %v", err)
	}
}

func TestOrphanedSecretFilesAreRemoved(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(filepath.Join(h.cfg.SecretsDir, "gone-1", "..v-1"), 0o755)
	os.WriteFile(filepath.Join(h.cfg.SecretsDir, "gone-1.json"), []byte(`{}`), 0o600)
	h.mustTick()
	if entries, _ := os.ReadDir(h.cfg.SecretsDir); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestSecretValuesNeverReachTheReportOrLogs(t *testing.T) {
	var logs bytes.Buffer
	h := newHarness(t, onData, func(_ *harness, _ *Config, d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	h.secrets.put("v-1", secretV1())
	h.secrets.put("v-2", secretV2())
	h.rt.exec = func(_ context.Context, _ string, _ []string, _, stderr io.Writer) (int, error) {
		pw, _ := h.secretFile("postgres-1", "POSTGRES_PASSWORD")
		fmt.Fprintf(stderr, "bad password %s", pw)
		return 1, nil
	}
	h.follow("postgres", "v-1")
	h.running(postgres("postgres-1"), keycloak("keycloak-1", "v-1"))
	h.follow("postgres", "v-2")
	h.mustTick()
	h.mustTick()
	reports, _ := json.Marshal(h.sched.reports)
	labels, _ := json.Marshal(h.rt.creates)
	for _, v := range []string{pgPassword1, pgPassword2, kcPassword, smtpShared} {
		for what, text := range map[string]string{"report": string(reports), "container spec": string(labels), "log": logs.String()} {
			if strings.Contains(text, v) {
				t.Errorf("the %s holds a secret value", what)
			}
		}
	}
}

// restartAgent replaces the agent with a new one, as systemd would after it exits: what it
// held in memory is gone, and only containers, labels and files remain.
func (h *harness) restartAgent() {
	h.t.Helper()
	h.a.Wait()
	a, err := New(h.cfg, Deps{
		Runtime: h.rt, Scheduler: h.sched, Metadata: h.meta, Firewall: h.fw,
		Credentials: h.creds, Prober: h.prober, Scraper: h.scraper, Volumes: h.volumes,
		Backups: h.a.store, Secrets: h.secrets, Updater: h.updater, Notifier: h.notify, Clock: h.clock,
		NewToken: h.a.newToken, Logger: h.a.log,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.a = a
}
