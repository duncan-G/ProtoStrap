package scheduler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/secret"
	"github.com/duncan-G/protostrap/internal/spec"
)

// This file applies changes to the environment's secret (docs/secrets.md).
//
// The secret is one JSON object of keys, each prefixed by who it is for. Every write makes
// a new version, and the version doesn't say whose keys changed. So once a minute the
// scheduler reads the version's ID, and when it is new, reads its value and hashes each
// component's keys, with a key that never leaves data-1. A component whose hash changed has
// a change to apply; the rest are left alone. Values are never kept, logged or saved.
//
// A change waits until the secret has settled, so keys written together are applied
// together, and until the components it depends on have applied theirs. Then its replicas
// are replaced, as in a release, or a follower's agents rewrite its files. Nothing goes
// back: if replicas fail with the new keys, the ones still running are kept, and the next
// value written is applied.

// secretWatch is the secret as the scheduler last read it.
type secretWatch struct {
	checked time.Time
	current string    // It is the current version's ID.
	changed time.Time // It is when the scheduler first saw the current version.
	err     string
	// digests are each watched component's keys, hashed, at version digestsAt of the
	// secret, for release digestsFor.
	digests    map[string]string
	digestsAt  string
	digestsFor string
}

// componentSecret is a component's keys: what its replicas should have.
type componentSecret struct {
	release.SecretState
	pending string // It is a newer version with other keys.
	waiting string
	failed  string
}

// LoadOrCreateSecretKey returns the key the scheduler hashes components' keys with, from
// dir on data-1's control volume, making it the first time.
func LoadOrCreateSecretKey(dir string) ([]byte, error) {
	p := filepath.Join(dir, "secret-digest.key")
	if key, err := os.ReadFile(p); err == nil && len(key) == 32 {
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// keyID names the key hashes are made with: each hash starts with it, so a hash made with
// another key is never taken for a change.
func keyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("protostrap secret digest key\n"), key...))
	return hex.EncodeToString(sum[:4])
}

// digest is a component's keys, hashed: the key's ID, then the keyed hash.
func (s *Scheduler) digest(keys map[string]string) string {
	return s.keyID + secret.Digest(s.secretKey, keys)[:24]
}

// secretName is the environment's secret, <app>/<environment>, or "" without an
// environment.
func (s *Scheduler) secretName() string {
	if s.cfg.Environment == "" {
		return ""
	}
	return secret.Name(s.cfg.App, s.cfg.Environment)
}

func (s *Scheduler) loadSecrets(saved release.State) {
	for name, ss := range saved.Secrets {
		if !strings.HasPrefix(ss.Digest, s.keyID) {
			continue // The key changed: what runs is taken as it is, as when first watched.
		}
		s.st.secrets[name] = &componentSecret{SecretState: ss}
	}
	s.st.secret.current, s.st.secret.changed = saved.SecretVersion, saved.SecretChanged
}

func (s *Scheduler) saveSecrets(st *release.State) {
	if len(s.st.secrets) > 0 {
		st.Secrets = map[string]release.SecretState{}
		for name, cs := range s.st.secrets {
			st.Secrets[name] = cs.SecretState
		}
	}
	st.SecretVersion, st.SecretChanged = s.st.secret.current, s.st.secret.changed
}

// watched reports whether a change to a component's keys reaches its replicas. A task
// reads its keys each time it runs, but a configure task runs again for new ones.
func watched(sp *spec.Spec, c *spec.Component) bool {
	return c.Kind != api.KindTask || sp.ConfiguredBy(c.Name) != nil
}

// checkSecret reads the secret's version when a check is due, and when the version or the
// release is new, hashes each component's keys at it. It runs outside the lock.
func (s *Scheduler) checkSecret(ctx context.Context, now time.Time, tgt *target) {
	name := s.secretName()
	if s.secrets == nil || name == "" {
		return
	}
	release := ""
	if tgt != nil {
		release = tgt.rel.Commit
	}
	s.mu.Lock()
	w := s.st.secret
	s.mu.Unlock()
	version := w.current
	if now.Sub(w.checked) >= s.cfg.SecretCheck {
		v, err := s.secrets.CurrentVersion(ctx, name)
		s.mu.Lock()
		s.st.secret.checked = now
		if err != nil {
			s.secretError(fmt.Sprintf("reading the version of %s: %v", name, err))
			s.mu.Unlock()
			return
		}
		s.st.secret.err = ""
		if v != s.st.secret.current {
			s.st.secret.current, s.st.secret.changed = v, now
			s.log.Info("the environment's secret has a new version", "secret", name, "version", v)
		}
		s.mu.Unlock()
		version = v
	}
	if version == "" || tgt == nil || (w.digestsAt == version && w.digestsFor == release) {
		return
	}
	value, err := s.secrets.Read(ctx, name, version)
	if err != nil {
		s.mu.Lock()
		s.secretError(fmt.Sprintf("reading %s: %v", name, err))
		s.mu.Unlock()
		return
	}
	all, err := secret.Parse(value)
	if err != nil {
		s.mu.Lock()
		s.secretError(fmt.Sprintf("%s, version %s: %v", name, version, err))
		s.mu.Unlock()
		return
	}
	digests := map[string]string{}
	if tgt.spec.Gateway != nil && tgt.spec.Tunnel != nil {
		// Only the tunnel reads the edge's keys.
		digests["edge"] = s.digest(secret.Keys(all, secret.Edge, nil))
	}
	for _, c := range tgt.spec.Sorted() {
		if watched(tgt.spec, c) {
			digests[c.Name] = s.digest(secret.Keys(all, c.Prefix(), c.Secrets))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.secret.current == version {
		s.st.secret.digests, s.st.secret.digestsAt, s.st.secret.digestsFor = digests, version, release
	}
}

// secretError records why the secret couldn't be read, logging it when it is new. It is
// called with the lock held.
func (s *Scheduler) secretError(msg string) {
	if s.st.secret.err != msg {
		s.log.Warn("the environment's secret can't be read: nothing changes with it", "err", msg)
	}
	s.st.secret.err = msg
}

// secretChanges finds the components whose keys changed and, once each change may go
// ahead, makes the new keys what the component's replicas should have.
func (s *Scheduler) secretChanges(now time.Time) {
	tgt, w := s.st.target, s.st.secret
	if tgt == nil || w.current == "" || w.digestsAt != w.current || w.digestsFor != tgt.rel.Commit {
		return // The keys haven't been hashed at this version yet.
	}
	names := slices.Sorted(maps.Keys(w.digests))
	for _, name := range names {
		d := w.digests[name]
		cs := s.st.secrets[name]
		if cs == nil {
			// The component is new, or the scheduler is: its replicas have what it has now.
			s.st.secrets[name] = &componentSecret{SecretState: release.SecretState{Version: w.current, Read: w.current, Digest: d, Baseline: true}}
			continue
		}
		cs.pending, cs.waiting = "", ""
		if d == cs.Digest {
			cs.Read = w.current // Other keys changed: new replicas read the newest version.
			continue
		}
		cs.pending = w.current
	}
	settled := now.Sub(w.changed) >= s.cfg.SecretSettle
	for _, name := range names {
		cs := s.st.secrets[name]
		if cs.pending == "" {
			continue
		}
		switch dep := s.secretBusy(tgt, name); {
		case !settled:
			cs.waiting = fmt.Sprintf("the secret changed %v ago: it settles for %v first", now.Sub(w.changed).Round(time.Second), s.cfg.SecretSettle)
		case s.running() != nil:
			cs.waiting = "a release is rolling out"
		case dep != "":
			cs.waiting = dep + " applies its keys first"
		default:
			s.log.Info("applying a component's new keys", "component", name, "from", cs.Version, "to", cs.pending)
			cs.Version, cs.Read, cs.Digest, cs.Baseline = cs.pending, cs.pending, w.digests[name], false
			cs.pending, cs.failed = "", ""
		}
	}
	for _, name := range names {
		s.secretFailure(name)
	}
}

// secretBusy names a component that name depends on and whose keys are changing, or "".
func (s *Scheduler) secretBusy(tgt *target, name string) string {
	c := tgt.spec.Components[name]
	if c == nil {
		return ""
	}
	for _, dep := range c.DependsOn {
		if cs := s.st.secrets[dep]; cs != nil && cs.pending != "" || s.secretInProgress(dep) || s.configureBusy(tgt, dep, name) {
			return dep
		}
	}
	return ""
}

// secretInProgress reports whether a component's replicas don't all have its keys yet.
func (s *Scheduler) secretInProgress(name string) bool {
	cs := s.st.secrets[name]
	if cs == nil {
		return false
	}
	for _, r := range s.st.replicas {
		if r.Component != name || r.Kind == api.KindTask || !r.live() {
			continue
		}
		if r.Spec.SecretFollows {
			if r.SecretVersion != cs.Read {
				return true
			}
		} else if !s.keysCurrent(r) {
			return true
		}
	}
	return false
}

// secretFailure records why a component's replicas can't take its keys: a follower that
// can't apply them, or a replica made with them that failed. It is kept until the change is
// done or another starts, since failed replicas are soon forgotten.
func (s *Scheduler) secretFailure(name string) {
	cs := s.st.secrets[name]
	for _, id := range slices.Sorted(maps.Keys(s.st.replicas)) {
		r := s.st.replicas[id]
		if r.Component != name {
			continue
		}
		var why string
		switch {
		case r.SecretFailure != nil && r.SecretFailure.Version == cs.Read:
			why = fmt.Sprintf("%s can't apply version %s: %s", r.ID, r.SecretFailure.Version, r.SecretFailure.Reason)
		case !cs.Baseline && !r.Spec.SecretFollows && r.Kind != api.KindTask && r.SecretDigest == cs.Digest && r.State == api.StateFailed:
			why = fmt.Sprintf("%s failed with version %s: %s", r.ID, cs.Version, r.Reason)
		}
		if why != "" {
			if cs.failed != why {
				s.log.Error("a component's replicas can't take its new keys", "component", name, "reason", why)
			}
			cs.failed = why
			return
		}
	}
	if !s.secretInProgress(name) {
		cs.failed = ""
	}
}

// keysCurrent reports whether a replica has the keys its component should have. A
// follower always does: its agent rewrites its files. So does a replica made before the
// scheduler first saw the keys, until they change.
func (s *Scheduler) keysCurrent(r *Replica) bool {
	cs := s.st.secrets[r.Component]
	if cs == nil || r.Kind == api.KindTask || r.Spec.SecretFollows {
		return true
	}
	if !strings.HasPrefix(r.SecretDigest, s.keyID) {
		return cs.Baseline // It has no hash, or one made with another key.
	}
	return r.SecretDigest == cs.Digest
}

// secretSpec fills in a replica's keys: its prefix, its secret files and how it takes a
// change, and the version and hash of its keys.
func (s *Scheduler) secretSpec(rs *api.ReplicaSpec, component, prefix string) {
	rs.SecretPrefix = prefix
	if cs := s.st.secrets[component]; cs != nil {
		rs.SecretVersion, rs.SecretDigest = cs.Read, cs.Digest
	}
}

// secretStatus is a component's keys, for the status.
func (s *Scheduler) secretStatus(name string) *SecretStatus {
	cs := s.st.secrets[name]
	if cs == nil {
		return nil
	}
	return &SecretStatus{Version: cs.Version, Pending: cs.pending, Waiting: cs.waiting, Failed: cs.failed}
}
