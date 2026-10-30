package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
)

// backupRun tracks a component's daily backup.
type backupRun struct {
	done    string // It is the date of the last backup.
	running bool
	retryAt time.Time
}

// backup starts each stateful component's daily backup, on a data VM: its `backup`
// command runs in its container and the output streams, compressed, to
// <component>/<date>.gz in the backups bucket (docs/protostrap.md#agent, "Backups").
// The bucket is the record of what's done, so a restarted agent doesn't repeat a backup.
func (a *Agent) backup(ctx context.Context, now time.Time, containers []container.Container) {
	if a.store == nil {
		return
	}
	utc := now.UTC()
	if utc.Hour() < a.cfg.BackupHour {
		return
	}
	date := utc.Format("2006-01-02")
	for _, c := range containers {
		var cmd []string
		json.Unmarshal([]byte(c.Label(container.LabelBackup)), &cmd)
		if api.Kind(c.Label(container.LabelKind)) != api.KindStateful || !c.Status.Up() || len(cmd) == 0 {
			continue
		}
		component := c.Label(container.LabelComponent)
		a.mu.Lock()
		run := a.runs[component]
		if run == nil {
			run = &backupRun{}
			a.runs[component] = run
		}
		start := a.ready[c.Replica()] && run.done != date && !run.running && !now.Before(run.retryAt)
		if start {
			run.running = true
		}
		a.mu.Unlock()
		if !start {
			continue
		}
		a.wg.Go(func() {
			err := a.runBackup(context.WithoutCancel(ctx), c, cmd, component+"/"+date+".gz")
			a.mu.Lock()
			defer a.mu.Unlock()
			run.running = false
			if err != nil {
				run.retryAt = a.clock.Now().Add(a.cfg.BackupRetry)
				a.log.Error("backup failed", "component", component, "err", err)
				return
			}
			run.done = date
		})
	}
}

func (a *Agent) runBackup(ctx context.Context, c container.Container, cmd []string, key string) error {
	exists, err := a.store.Exists(ctx, key)
	if err != nil {
		return fmt.Errorf("checking for %s: %w", key, err)
	}
	if exists {
		return nil
	}
	a.log.Info("backing up", "replica", c.Replica(), "key", key)
	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		var stderr bytes.Buffer
		gz := gzip.NewWriter(pw)
		code, err := a.rt.Exec(ctx, c.ID, cmd, gz, &limited{w: &stderr, n: 4096})
		if err == nil && code != 0 {
			err = fmt.Errorf("%s exited with %d: %s", cmd[0], code, strings.TrimSpace(stderr.String()))
		}
		if cerr := gz.Close(); err == nil {
			err = cerr
		}
		// A failed command fails the upload, which then stores nothing.
		pw.CloseWithError(err)
		execDone <- err
	}()
	uploadErr := a.store.Upload(ctx, key, pr)
	// If the upload stopped early, closing the reader unblocks the command.
	pr.CloseWithError(io.ErrClosedPipe)
	if err := <-execDone; err != nil {
		return err
	}
	if uploadErr != nil {
		return fmt.Errorf("uploading %s: %w", key, uploadErr)
	}
	a.log.Info("backed up", "replica", c.Replica(), "key", key)
	return nil
}

// limited keeps the first n bytes written to it, and discards the rest.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
