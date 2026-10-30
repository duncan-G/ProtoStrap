package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/container"
)

// This file holds config files (docs/config.md): files from the application's repository
// that the release carries, which the agent fetches from the scheduler by their digest and
// mounts, read-only, at the replica's config path.
//
// A replica's files are in ConfigDir/<replica>, on disk, since they hold no secret. As with
// secret files, each set is a directory, ..v-<digest>, and ..data points at the current one,
// so a swap changes every file at once. Each top-level entry, such as lds.yaml or conf.d,
// links to ..data/<entry>.
//
// A follower takes new files without being replaced: the agent swaps them in, then runs its
// hook. ConfigDir/<replica>.json records the set written and the set applied, so a
// restarted agent neither repeats a hook nor forgets one.

// configFollow is a follower's config files.
type configFollow struct {
	record
	running bool
	retryAt time.Time
	failure *api.ConfigFailure
}

func (a *Agent) configDir(replica string) string { return filepath.Join(a.cfg.ConfigDir, replica) }

func (a *Agent) configRecordPath(replica string) string {
	return filepath.Join(a.cfg.ConfigDir, replica+".json")
}

// fetchConfig fetches the files a digest names from the scheduler, and checks they are
// what the digest says.
func (a *Agent) fetchConfig(ctx context.Context, digest string) (configfiles.Files, error) {
	if !configfiles.ValidDigest(digest) {
		return nil, fmt.Errorf("config digest %q is invalid", digest)
	}
	files, err := a.sched.Config(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("fetching config files %s: %w", digest, err)
	}
	if got := configfiles.Digest(files); got != digest {
		return nil, fmt.Errorf("the config files fetched for %s have the digest %s", digest, got)
	}
	if err := files.Validate(); err != nil {
		return nil, fmt.Errorf("config files %s: %w", digest, err)
	}
	return files, nil
}

// writeConfig makes a set of files the replica's current config. The directory stays the
// same, since the replica's mount holds on to it.
func (a *Agent) writeConfig(replica, digest string, files configfiles.Files) error {
	dir := a.configDir(replica)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	current := "..v-" + digest
	if _, err := os.Stat(filepath.Join(dir, current)); err != nil {
		tmp, err := os.MkdirTemp(dir, "..tmp-")
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(files)) {
			p := filepath.Join(tmp, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				os.RemoveAll(tmp)
				return err
			}
			if err := os.WriteFile(p, files[name], 0o444); err != nil {
				os.RemoveAll(tmp)
				return err
			}
			// Readable by everyone, so an image that drops to its own user can read it.
			if err := os.Chmod(p, 0o444); err != nil {
				os.RemoveAll(tmp)
				return err
			}
		}
		filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
		if err := os.Rename(tmp, filepath.Join(dir, current)); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	top := map[string]bool{}
	for name := range files {
		top[strings.SplitN(name, "/", 2)[0]] = true
	}
	for name := range top {
		if target, _ := os.Readlink(filepath.Join(dir, name)); target != "..data/"+name {
			if err := swapLink(dir, name, "..data/"+name); err != nil {
				return err
			}
		}
	}
	if err := swapLink(dir, "..data", current); err != nil {
		return err
	}
	// Links to entries the new set doesn't have, and older sets, go once nothing points at
	// them.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		switch {
		case n == "..data" || n == current:
		case strings.HasPrefix(n, ".."):
			os.RemoveAll(filepath.Join(dir, n))
		case !top[n]:
			os.Remove(filepath.Join(dir, n))
		}
	}
	return nil
}

// prepareConfig writes a new replica's config files before its container is created, and
// returns the digest written: the assignment's for a follower, which may be newer than the
// one it was made with.
func (a *Agent) prepareConfig(ctx context.Context, s api.ReplicaSpec) (string, error) {
	digest := s.ConfigDigest
	if d := a.last.ConfigDigests[s.ID]; s.ConfigFollows && d != "" {
		digest = d
	}
	files, err := a.fetchConfig(ctx, digest)
	if err == nil {
		err = a.writeConfig(s.ID, digest, files)
	}
	if err != nil {
		return "", fmt.Errorf("writing the config files: %w", err)
	}
	if s.ConfigFollows {
		// The replica reads them when it starts, so they are applied.
		if err := a.saveConfigRecord(s.ID, record{Written: digest, Applied: digest}); err != nil {
			return "", fmt.Errorf("writing the config record: %w", err)
		}
	}
	return digest, nil
}

func (a *Agent) loadConfigRecord(replica string) (record, bool) {
	b, err := os.ReadFile(a.configRecordPath(replica))
	if err != nil {
		return record{}, false
	}
	var r record
	if json.Unmarshal(b, &r) != nil {
		return record{}, false
	}
	return r, true
}

func (a *Agent) saveConfigRecord(replica string, r record) error {
	b, _ := json.Marshal(r)
	tmp := a.configRecordPath(replica) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.configRecordPath(replica))
}

// removeConfig removes a replica's config files and record, once it is gone.
func (a *Agent) removeConfig(replica string) {
	os.RemoveAll(a.configDir(replica))
	os.Remove(a.configRecordPath(replica))
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.configFollows, replica)
}

// sweepConfig removes the config files of replicas that are gone, such as those left behind
// when the agent stopped mid-way.
func (a *Agent) sweepConfig(containers []container.Container, inFlight map[string]bool) {
	entries, err := os.ReadDir(a.cfg.ConfigDir)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for id := range inFlight {
		live[id] = true
	}
	for _, c := range containers {
		live[c.Replica()] = true
	}
	for _, e := range entries {
		// A record is <replica>.json, written through <replica>.json.tmp.
		replica := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".tmp"), ".json")
		if !live[replica] {
			os.RemoveAll(filepath.Join(a.cfg.ConfigDir, e.Name()))
			a.log.Info("config files removed", "replica", replica)
		}
	}
}

// followConfig brings each follower on the VM to the config files its assignment names: it
// swaps them in and runs its hook, in the background. A follower takes new files only once
// it is ready, and after a failure only once ConfigRetry has passed.
func (a *Agent) followConfig(ctx context.Context, now time.Time, as api.Assignment, containers []container.Container) {
	byID := map[string]container.Container{}
	for _, c := range containers {
		byID[c.Replica()] = c
	}
	for _, s := range as.Replicas {
		c, ok := byID[s.ID]
		if !s.ConfigFollows || !ok || !c.Status.Up() {
			continue
		}
		want := as.ConfigDigests[s.ID]
		a.mu.Lock()
		f := a.configFollowOf(c)
		start := a.ready[s.ID] && a.ops[s.ID] == nil && !f.running && !now.Before(f.retryAt) &&
			((want != "" && want != f.Written) || f.Applied != f.Written)
		if start {
			f.running = true
		}
		a.mu.Unlock()
		if !start {
			continue
		}
		a.wg.Go(func() {
			a.applyConfig(context.WithoutCancel(ctx), s, c, f, want)
		})
	}
}

// configFollowOf returns a follower's state, from its record, or from its label without
// one. It is called with mu held.
func (a *Agent) configFollowOf(c container.Container) *configFollow {
	id := c.Replica()
	if f, ok := a.configFollows[id]; ok {
		return f
	}
	f := &configFollow{}
	if r, ok := a.loadConfigRecord(id); ok {
		f.record = r
	} else {
		d := c.Label(container.LabelConfigDigest)
		f.record = record{Written: d, Applied: d}
	}
	a.configFollows[id] = f
	return f
}

// applyConfig swaps a follower's files for the set want, if it has another, then runs its
// hook if the set written isn't applied yet.
func (a *Agent) applyConfig(ctx context.Context, s api.ReplicaSpec, c container.Container, f *configFollow, want string) {
	a.mu.Lock()
	r := f.record
	a.mu.Unlock()
	failed := func(digest, reason string) {
		a.log.Error("a follower couldn't take new config files", "replica", s.ID, "config", digest, "reason", reason)
		a.mu.Lock()
		defer a.mu.Unlock()
		f.running = false
		f.failure = &api.ConfigFailure{Digest: digest, Reason: reason}
		f.retryAt = a.clock.Now().Add(a.cfg.ConfigRetry)
	}
	if want != "" && want != r.Written {
		files, err := a.fetchConfig(ctx, want)
		if err == nil {
			err = a.writeConfig(s.ID, want, files)
		}
		if err == nil {
			r.Written = want
			if len(s.ConfigHook) == 0 {
				r.Applied = want // The replica reads its files again itself.
			}
			err = a.saveConfigRecord(s.ID, r)
		}
		if err != nil {
			failed(want, err.Error())
			return
		}
		a.log.Info("config files swapped in", "replica", s.ID, "config", want)
	}
	if r.Applied != r.Written {
		if err := a.runConfigHook(ctx, s, c); err != nil {
			a.mu.Lock()
			f.record = r // The files are swapped in, even if the hook didn't apply them.
			a.mu.Unlock()
			failed(r.Written, err.Error())
			return
		}
		a.log.Info("config hook applied new files", "replica", s.ID, "config", r.Written)
		r.Applied = r.Written
	}
	err := a.saveConfigRecord(s.ID, r)
	a.mu.Lock()
	defer a.mu.Unlock()
	f.record, f.running, f.retryAt = r, false, time.Time{}
	if err != nil {
		f.failure = &api.ConfigFailure{Digest: r.Written, Reason: err.Error()}
		return
	}
	f.failure = nil
}

// runConfigHook runs a follower's hook in its container. What the hook writes to stderr
// goes in the failure, with every value of the replica's secret files replaced by ***.
func (a *Agent) runConfigHook(ctx context.Context, s api.ReplicaSpec, c container.Container) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.ConfigHookTimeout)
	defer cancel()
	var stderr bytes.Buffer
	code, err := a.rt.Exec(ctx, c.ID, s.ConfigHook, &limited{w: &stderr, n: 4096}, &limited{w: &stderr, n: 4096})
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("the hook didn't finish within %v", a.cfg.ConfigHookTimeout)
	case err != nil:
		return fmt.Errorf("running the hook: %w", err)
	case code != 0:
		out := strings.TrimSpace(a.redact(s.ID, stderr.String()))
		return fmt.Errorf("the hook exited with %d: %s", code, out)
	}
	return nil
}

// configStatus is what a replica reports of its config files: those it started with, or a
// follower's last applied, and its failure.
func (a *Agent) configStatus(c container.Container) (string, *api.ConfigFailure) {
	if c.Label(container.LabelConfigFollows) == "" {
		return c.Label(container.LabelConfigDigest), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.configFollowOf(c)
	if f.failure != nil {
		failure := *f.failure
		return f.Applied, &failure
	}
	return f.Applied, nil
}

// configPathClear reports whether a config path stays out of the secret files' mount and the
// volume, which the scheduler's spec check also guards.
func (a *Agent) configPathClear(s api.ReplicaSpec) error {
	in := func(p, dir string) bool { return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") }
	p := path.Clean(s.ConfigPath)
	if in(p, a.cfg.SecretsMount) || in(a.cfg.SecretsMount, p) {
		return fmt.Errorf("config path %s would cover or sit in the secret files at %s", p, a.cfg.SecretsMount)
	}
	if s.Volume != "" && (in(p, s.Volume) || in(s.Volume, p)) {
		return fmt.Errorf("config path %s would cover or sit in the volume at %s", p, s.Volume)
	}
	return nil
}
