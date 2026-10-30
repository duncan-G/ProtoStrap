package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// backupHarness is a data VM running PostgreSQL and Redis, both ready, at 04:00 UTC.
func backupHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, dataVM)
	h.clock.now = time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC) // 01:00 is before the backup hour.
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "redis")] = true
	h.rt.exec = func(_ context.Context, name string, cmd []string, stdout, _ io.Writer) (int, error) {
		fmt.Fprintf(stdout, "dump of %s by %v", name, cmd)
		return 0, nil
	}
	h.running(
		stateful("postgres-a1", "postgres", 2000, "/var/lib/postgresql/data", "pg_dumpall", "-U", "postgres"),
		stateful("redis-b2", "redis", 1000, "/data", "redis-cli", "--rdb", "-"),
	)
	if len(h.rt.execs) != 0 {
		t.Fatal("no backup before the backup hour")
	}
	h.clock.now = time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
	return h
}

func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("backup is not gzip: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestDailyBackupOfEachStatefulComponent(t *testing.T) {
	h := backupHarness(t)
	h.mustTick()
	pg, ok := h.backups.object("postgres/2026-10-24.gz")
	if !ok {
		t.Fatalf("no postgres backup: %v", h.backups.uploads)
	}
	if got := gunzip(t, pg); got != "dump of postgres-a1 by [pg_dumpall -U postgres]" {
		t.Fatalf("postgres backup %q", got)
	}
	redis, ok := h.backups.object("redis/2026-10-24.gz")
	if !ok || gunzip(t, redis) != "dump of redis-b2 by [redis-cli --rdb -]" {
		t.Fatalf("redis backup %q %v", redis, ok)
	}
}

func TestBackupOncePerDay(t *testing.T) {
	h := backupHarness(t)
	h.mustTick()
	for range 5 {
		h.clock.advance(time.Hour)
		h.mustTick()
	}
	if len(h.rt.execs) != 2 {
		t.Fatalf("backups ran %d times on one day: %v", len(h.rt.execs), h.rt.execs)
	}
	h.clock.now = time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)
	h.mustTick()
	if _, ok := h.backups.object("postgres/2026-10-25.gz"); !ok {
		t.Fatalf("no backup the next day: %v", h.backups.uploads)
	}
}

func TestBackupAlreadyInTheBucketIsNotRepeated(t *testing.T) {
	// The agent restarted after today's backup.
	h := backupHarness(t)
	h.backups.objects["postgres/2026-10-24.gz"] = []byte("earlier")
	h.mustTick()
	if got := h.backups.objects["postgres/2026-10-24.gz"]; string(got) != "earlier" {
		t.Fatal("today's backup was overwritten")
	}
	if slices.ContainsFunc(h.rt.execs, func(e string) bool { return e[:8] == "postgres" }) {
		t.Fatalf("execs %v", h.rt.execs)
	}
}

func TestAFailedBackupLeavesNothingAndIsRetriedLater(t *testing.T) {
	h := backupHarness(t)
	h.rt.exec = func(_ context.Context, name string, cmd []string, stdout, stderr io.Writer) (int, error) {
		fmt.Fprint(stdout, "partial")
		fmt.Fprint(stderr, "pg_dumpall: connection refused")
		return 1, nil
	}
	h.mustTick()
	if _, ok := h.backups.object("postgres/2026-10-24.gz"); ok {
		t.Fatal("a failed dump must not be stored as today's backup")
	}
	execs := len(h.rt.execs)
	h.clock.advance(30 * time.Minute)
	h.mustTick()
	if len(h.rt.execs) != execs {
		t.Fatal("retried too soon")
	}
	h.rt.exec = func(_ context.Context, name string, cmd []string, stdout, _ io.Writer) (int, error) {
		fmt.Fprint(stdout, "good")
		return 0, nil
	}
	h.clock.advance(31 * time.Minute)
	h.mustTick()
	if pg, ok := h.backups.object("postgres/2026-10-24.gz"); !ok || gunzip(t, pg) != "good" {
		t.Fatal("not retried after the retry interval")
	}
}

func TestBackupExecError(t *testing.T) {
	h := backupHarness(t)
	h.rt.exec = func(context.Context, string, []string, io.Writer, io.Writer) (int, error) {
		return -1, errors.New("container not running")
	}
	h.mustTick()
	if len(h.backups.objects) != 0 {
		t.Fatalf("objects %v", h.backups.objects)
	}
}

func TestBackupCheckFailureIsRetried(t *testing.T) {
	h := backupHarness(t)
	h.backups.existsErr = errors.New("AccessDenied")
	h.mustTick()
	if len(h.rt.execs) != 0 {
		t.Fatal("must not back up without knowing whether today's exists")
	}
	h.backups.existsErr = nil
	h.clock.advance(h.cfg.BackupRetry)
	h.mustTick()
	if len(h.rt.execs) != 2 {
		t.Fatalf("execs %v", h.rt.execs)
	}
}

func TestBackupsOnlyOnDataVMs(t *testing.T) {
	h := newHarness(t) // A general VM has no backups bucket.
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.running(stateful("postgres-a1", "postgres", 1000, "/data", "pg_dumpall"))
	h.clock.now = time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
	h.mustTick()
	if len(h.rt.execs) != 0 {
		t.Fatal("no backups off data VMs")
	}
}

func TestOnlyReadyStatefulReplicasWithACommandAreBackedUp(t *testing.T) {
	h := newHarness(t, dataVM)
	h.clock.now = time.Date(2026, 10, 24, 4, 0, 0, 0, time.UTC)
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "postgres")] = true
	h.volumes.mounted[filepath.Join(h.cfg.DataDir, "cache")] = true
	h.prober.setReady("cache", true)
	h.sched.assign(
		stateful("postgres-a1", "postgres", 1000, "/data", "pg_dumpall"), // It isn't ready yet.
		stateful("cache-1", "cache", 1000, "/data"),                      // It has no backup command.
		worker("w-1"),
	)
	h.mustTick()
	h.mustTick()
	if len(h.rt.execs) != 0 {
		t.Fatalf("execs %v", h.rt.execs)
	}
}

func TestBackupDoesNotHoldUpTheCycle(t *testing.T) {
	h := backupHarness(t)
	release := make(chan struct{})
	h.rt.exec = func(ctx context.Context, name string, cmd []string, stdout, _ io.Writer) (int, error) {
		<-release
		fmt.Fprint(stdout, "slow dump")
		return 0, nil
	}
	start := time.Now()
	h.a.Tick(t.Context())
	h.a.Tick(t.Context())
	if time.Since(start) > time.Second {
		t.Fatal("a backup must run beside the sync loop")
	}
	h.rt.mu.Lock()
	execs := len(h.rt.execs)
	h.rt.mu.Unlock()
	if execs > 2 {
		t.Fatalf("a running backup must not be started again: %d", execs)
	}
	close(release)
	h.a.Wait()
	if _, ok := h.backups.object("postgres/2026-10-24.gz"); !ok {
		t.Fatal("backup not stored")
	}
}
