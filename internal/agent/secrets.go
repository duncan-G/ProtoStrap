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
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/container"
	"github.com/duncan-G/protostrap/internal/secret"
)

// This file holds secret files (docs/secrets.md#secret-files): the keys of the
// environment's secret that the agent writes for images that can't read the secret
// themselves, such as PostgreSQL's POSTGRES_PASSWORD_FILE.
//
// A replica's files are in SecretsDir/<replica>, on tmpfs, mounted read-only at
// /run/secrets. Each version of its keys is a directory, ..v-<version>, and ..data points
// at the current one, so a new version changes every key at once. previous points at the
// version before, which a hook may need to log in with. Each key, such as
// POSTGRES_PASSWORD, links to ..data/POSTGRES_PASSWORD.
//
// A follower takes a new version without being replaced: the agent rewrites its files,
// then runs its hook. SecretsDir/<replica>.json records the version written and the
// version applied, so a restarted agent neither repeats a hook nor forgets one. A reboot
// empties tmpfs, and with it the record, so after one the hook runs again.

// follow is a follower's secret files.
type follow struct {
	record
	running bool
	retryAt time.Time
	failure *api.SecretFailure
}

// record is what SecretsDir/<replica>.json holds.
type record struct {
	Written string `json:"written"`
	Applied string `json:"applied"`
}

// unrestored is a replica whose secret files a reboot removed and which the agent couldn't
// write again: since when, and why.
type unrestored struct {
	since  time.Time
	reason string
}

// secretID is the environment's secret, which the scheduler names. A scheduler from before
// there was one leaves it out, and each component reads its own, as it did then.
func (a *Agent) secretID(component string) string {
	if a.last.Secret != "" {
		return a.last.Secret
	}
	return "protostrap/" + component
}

func (a *Agent) secretDir(replica string) string { return filepath.Join(a.cfg.SecretsDir, replica) }

func (a *Agent) recordPath(replica string) string {
	return filepath.Join(a.cfg.SecretsDir, replica+".json")
}

// readKeys reads a version of a component's keys: an empty version is the current one. A
// key it lacks is an error naming the key, never a value.
func (a *Agent) readKeys(ctx context.Context, secretID, version, role, component, prefix string, names []string) (map[string]string, string, error) {
	if a.secrets == nil {
		return nil, "", errors.New("this agent can't read secrets")
	}
	if version != "" && !secret.ValidVersion(version) {
		return nil, "", fmt.Errorf("secret version %q is invalid", version)
	}
	value, id, err := a.secrets.Read(ctx, secretID, version, role, component)
	if err != nil {
		return nil, "", fmt.Errorf("reading the secret %s: %w", secretID, err)
	}
	if !secret.ValidVersion(id) {
		return nil, "", fmt.Errorf("the secret %s has an invalid version %q", secretID, id)
	}
	all, err := secret.Parse(value)
	if err != nil {
		return nil, "", fmt.Errorf("the secret %s, version %s: %w", secretID, id, err)
	}
	keys := secret.Keys(all, prefix, names)
	if missing := secret.Missing(keys, names); len(missing) > 0 {
		var want []string
		for _, name := range missing {
			want = append(want, prefix+name+" or "+secret.Shared+name)
		}
		return nil, "", fmt.Errorf("the secret %s, version %s, lacks %s's %s: set %s",
			secretID, id, component, strings.Join(missing, ", "), strings.Join(want, ", "))
	}
	// Only the keys it names are written.
	files := map[string]string{}
	for _, name := range names {
		files[name] = keys[name]
	}
	return files, id, nil
}

// writeSecretFiles makes version the replica's current keys. The directory stays the same,
// since the replica's mount holds on to it.
func (a *Agent) writeSecretFiles(replica, version string, keys map[string]string) error {
	if err := os.MkdirAll(a.cfg.SecretsDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(a.cfg.SecretsDir, 0o700); err != nil {
		return err
	}
	dir := a.secretDir(replica)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	current := "..v-" + version
	if _, err := os.Stat(filepath.Join(dir, current)); err != nil {
		tmp, err := os.MkdirTemp(dir, "..tmp-")
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(keys)) {
			p := filepath.Join(tmp, name)
			if err := os.WriteFile(p, []byte(keys[name]), 0o400); err != nil {
				os.RemoveAll(tmp)
				return err
			}
			// Readable by everyone, so an image that drops to its own user can read it.
			if err := os.Chmod(p, 0o444); err != nil {
				os.RemoveAll(tmp)
				return err
			}
		}
		if err := os.Chmod(tmp, 0o755); err != nil {
			os.RemoveAll(tmp)
			return err
		}
		if err := os.Rename(tmp, filepath.Join(dir, current)); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	for name := range keys {
		if target, _ := os.Readlink(filepath.Join(dir, name)); target != "..data/"+name {
			if err := swapLink(dir, name, "..data/"+name); err != nil {
				return err
			}
		}
	}
	prev, _ := os.Readlink(filepath.Join(dir, "..data"))
	if prev != "" && prev != current {
		if err := swapLink(dir, "previous", prev); err != nil {
			return err
		}
	}
	if err := swapLink(dir, "..data", current); err != nil {
		return err
	}
	keep, _ := os.Readlink(filepath.Join(dir, "previous"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if (strings.HasPrefix(n, "..v-") && n != current && n != keep) || strings.HasPrefix(n, "..tmp-") {
			os.RemoveAll(filepath.Join(dir, n))
		}
	}
	return nil
}

// swapLink points dir/name at target in one step.
func swapLink(dir, name, target string) error {
	tmp := filepath.Join(dir, "..tmp-link")
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// removeSecretFiles removes a replica's secret files and record, once it is gone.
func (a *Agent) removeSecretFiles(replica string) {
	os.RemoveAll(a.secretDir(replica))
	os.Remove(a.recordPath(replica))
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.follows, replica)
	delete(a.unrestored, replica)
}

// sweepSecretFiles removes the secret files of replicas that are gone, such as those left
// behind when the agent stopped mid-way.
func (a *Agent) sweepSecretFiles(containers []container.Container, inFlight map[string]bool) {
	entries, err := os.ReadDir(a.cfg.SecretsDir)
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
			os.RemoveAll(filepath.Join(a.cfg.SecretsDir, e.Name()))
			a.log.Info("secret files removed", "replica", replica)
		}
	}
}

func (a *Agent) loadRecord(replica string) (record, bool) {
	b, err := os.ReadFile(a.recordPath(replica))
	if err != nil {
		return record{}, false
	}
	var r record
	if json.Unmarshal(b, &r) != nil {
		return record{}, false
	}
	return r, true
}

func (a *Agent) saveRecord(replica string, r record) error {
	b, _ := json.Marshal(r)
	tmp := a.recordPath(replica) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.recordPath(replica))
}

// prepareSecretFiles writes a new replica's secret files before its container is created,
// and returns the version written.
func (a *Agent) prepareSecretFiles(ctx context.Context, s api.ReplicaSpec, secretID, version string) (string, error) {
	keys, id, err := a.readKeys(ctx, secretID, version, s.Role, s.Component, s.SecretPrefix, s.SecretFiles)
	if err != nil {
		return "", err
	}
	if err := a.writeSecretFiles(s.ID, id, keys); err != nil {
		return "", fmt.Errorf("writing the secret files: %w", err)
	}
	if s.SecretFollows {
		// A hook still has to run once the replica is ready, in case its data holds an older
		// version; a replica that reloads has what it reads.
		r := record{Written: id}
		if len(s.SecretHook) == 0 {
			r.Applied = id
		}
		if err := a.saveRecord(s.ID, r); err != nil {
			return "", fmt.Errorf("writing the secret record: %w", err)
		}
	}
	return id, nil
}

// restoreSecretFiles writes the secret files of replicas that have lost them to a reboot,
// which empties tmpfs, and starts those Docker couldn't start without them. It reports
// whether it started any.
func (a *Agent) restoreSecretFiles(ctx context.Context, now time.Time, containers []container.Container) bool {
	started := false
	for _, c := range containers {
		var names []string
		if json.Unmarshal([]byte(c.Label(container.LabelSecretFiles)), &names) != nil || len(names) == 0 {
			continue
		}
		id := c.Replica()
		if _, err := os.Lstat(filepath.Join(a.secretDir(id), "..data")); err == nil {
			continue
		}
		component := c.Label(container.LabelComponent)
		keys, version, err := a.readKeys(ctx, c.Env["SECRET_ID"], c.Label(container.LabelSecretVersion),
			c.Label(container.LabelRole), component, c.Env["SECRET_PREFIX"], names)
		if err == nil {
			err = a.writeSecretFiles(id, version, keys)
		}
		if err != nil {
			a.log.Error("writing a replica's secret files again", "replica", id, "err", err)
			a.mu.Lock()
			if _, ok := a.unrestored[id]; !ok {
				a.unrestored[id] = unrestored{since: now}
			}
			u := a.unrestored[id]
			u.reason = err.Error()
			a.unrestored[id] = u
			a.mu.Unlock()
			continue
		}
		a.log.Info("secret files written again", "replica", id, "version", version)
		a.mu.Lock()
		delete(a.unrestored, id)
		a.mu.Unlock()
		if c.Status != container.Exited && c.Status != container.Dead {
			continue
		}
		if api.Kind(c.Label(container.LabelKind)) == api.KindStateful {
			// Its data must be there first.
			if mounted, err := a.volumes.Mounted(filepath.Join(a.cfg.DataDir, component)); err != nil || !mounted {
				continue
			}
		}
		if err := a.rt.Start(ctx, c.ID); err != nil {
			a.log.Error("starting a replica again", "replica", id, "err", err)
			continue
		}
		started = true
	}
	return started
}

// followSecrets brings each follower on the VM to the version the assignment names: it
// rewrites the follower's files and runs its hook, in the background. A follower takes a
// version only once it is ready, and after a failure only once SecretRetry has passed.
func (a *Agent) followSecrets(ctx context.Context, now time.Time, as api.Assignment, containers []container.Container) {
	byID := map[string]container.Container{}
	for _, c := range containers {
		byID[c.Replica()] = c
	}
	for _, s := range as.Replicas {
		c, ok := byID[s.ID]
		if !s.SecretFollows || !ok || !c.Status.Up() {
			continue
		}
		want := as.SecretVersions[s.Component]
		a.mu.Lock()
		f := a.followOf(c)
		start := a.ready[s.ID] && a.ops[s.ID] == nil && !f.running && !now.Before(f.retryAt) &&
			((want != "" && want != f.Written) || f.Applied != f.Written)
		if start {
			f.running = true
		}
		a.mu.Unlock()
		if !start {
			continue
		}
		secretID := a.secretID(s.Component)
		a.wg.Go(func() {
			a.applySecret(context.WithoutCancel(ctx), s, c, f, secretID, want)
		})
	}
}

// followOf returns a follower's state, from its record, or from its label when a reboot has
// taken the record. It is called with mu held.
func (a *Agent) followOf(c container.Container) *follow {
	id := c.Replica()
	if f, ok := a.follows[id]; ok {
		return f
	}
	f := &follow{}
	if r, ok := a.loadRecord(id); ok {
		f.record = r
	} else {
		f.Written = c.Label(container.LabelSecretVersion)
	}
	a.follows[id] = f
	return f
}

// applySecret rewrites a follower's files to version want, if it has another, then runs
// its hook if the version written isn't applied yet.
func (a *Agent) applySecret(ctx context.Context, s api.ReplicaSpec, c container.Container, f *follow, secretID, want string) {
	a.mu.Lock()
	r := f.record
	a.mu.Unlock()
	failed := func(version, reason string) {
		a.log.Error("a follower couldn't take a new version of its keys", "replica", s.ID, "version", version, "reason", reason)
		a.mu.Lock()
		defer a.mu.Unlock()
		f.running = false
		f.failure = &api.SecretFailure{Version: version, Reason: reason}
		f.retryAt = a.clock.Now().Add(a.cfg.SecretRetry)
	}
	if want != "" && want != r.Written {
		keys, id, err := a.readKeys(ctx, secretID, want, s.Role, s.Component, s.SecretPrefix, s.SecretFiles)
		same := err == nil && maps.Equal(keys, a.currentKeys(s.ID))
		if err == nil {
			err = a.writeSecretFiles(s.ID, id, keys)
		}
		if err != nil {
			failed(want, err.Error())
			return
		}
		if same && r.Applied == r.Written {
			// Only another component's keys changed: there's nothing to apply.
			r.Applied = id
		}
		r.Written = id
		if err := a.saveRecord(s.ID, r); err != nil {
			failed(want, err.Error())
			return
		}
		a.log.Info("secret files rewritten", "replica", s.ID, "version", id)
	}
	if r.Applied != r.Written && len(s.SecretHook) > 0 {
		if err := a.runSecretHook(ctx, s, c); err != nil {
			a.mu.Lock()
			f.record = r // The files are written, even if the hook didn't apply them.
			a.mu.Unlock()
			failed(r.Written, err.Error())
			return
		}
		a.log.Info("secret hook applied a new version", "replica", s.ID, "version", r.Written)
	}
	r.Applied = r.Written
	err := a.saveRecord(s.ID, r)
	a.mu.Lock()
	defer a.mu.Unlock()
	f.record, f.running, f.retryAt = r, false, time.Time{}
	if err != nil {
		f.failure = &api.SecretFailure{Version: r.Written, Reason: err.Error()}
		return
	}
	f.failure = nil
}

// runSecretHook runs a follower's hook in its container. What the hook writes to stderr
// goes in the failure, with every current and previous value replaced by ***.
func (a *Agent) runSecretHook(ctx context.Context, s api.ReplicaSpec, c container.Container) error {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.SecretHookTimeout)
	defer cancel()
	var stderr bytes.Buffer
	code, err := a.rt.Exec(ctx, c.ID, s.SecretHook, &limited{w: &stderr, n: 4096}, &limited{w: &stderr, n: 4096})
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return fmt.Errorf("the hook didn't finish within %v", a.cfg.SecretHookTimeout)
	case err != nil:
		return fmt.Errorf("running the hook: %w", err)
	case code != 0:
		out := strings.TrimSpace(a.redact(s.ID, stderr.String()))
		return fmt.Errorf("the hook exited with %d: %s", code, out)
	}
	return nil
}

// currentKeys reads a replica's current secret files.
func (a *Agent) currentKeys(replica string) map[string]string {
	dir := filepath.Join(a.secretDir(replica), "..data")
	entries, _ := os.ReadDir(dir)
	keys := map[string]string{}
	for _, e := range entries {
		if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			keys[e.Name()] = string(b)
		}
	}
	return keys
}

// redact replaces every value in a replica's current and previous secret files with ***.
func (a *Agent) redact(replica, text string) string {
	var values []string
	for _, version := range []string{"..data", "previous"} {
		entries, _ := os.ReadDir(filepath.Join(a.secretDir(replica), version))
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(a.secretDir(replica), version, e.Name())); err == nil && len(b) > 0 {
				values = append(values, string(b))
			}
		}
	}
	// The longest first, so a value inside another is never left half shown.
	slices.SortFunc(values, func(x, y string) int { return len(y) - len(x) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
}

// secretStatus is what a replica reports of its keys: the version it started with, or a
// follower's version applied, and its failure.
func (a *Agent) secretStatus(c container.Container) (string, *api.SecretFailure) {
	if c.Label(container.LabelSecretFollows) == "" {
		return c.Label(container.LabelSecretVersion), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.followOf(c)
	if f.failure != nil {
		failure := *f.failure
		return f.Applied, &failure
	}
	return f.Applied, nil
}
