package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"time"

	"github.com/duncan-G/protostrap/internal/spec"
)

// This file gives the spec's hosts their hostnames in this account (docs/protostrap.md,
// "Hosts"). The account's Terraform config writes them to hosts.json in the deploy bucket,
// which the scheduler reads about once a minute. A name the account doesn't give a hostname
// keeps the spec's.
//
// Every replica learns each hostname as HOST_<NAME> and ORIGIN_<NAME>, so a changed
// hostname changes what a replica is. It is folded into the component's fingerprint, which
// is what decides whether a replica is current: a change replaces every replica, a quarter
// at a time, and runs configure tasks again, as a release would. The edge reloads its routes.

// hostsWatch is hosts.json as the scheduler last read it.
type hostsWatch struct {
	checked time.Time
	// account is the account's hostnames, by name, or nil before any were read.
	account map[string]string
	err     string
}

// checkHosts reads hosts.json when a read is due. It runs outside the lock. A file that
// can't be read changes nothing: the hostnames last read stay.
func (s *Scheduler) checkHosts(ctx context.Context, now time.Time) {
	s.mu.Lock()
	due := now.Sub(s.st.hosts.checked) >= s.cfg.HostsCheck
	if due {
		s.st.hosts.checked = now
	}
	s.mu.Unlock()
	if !due {
		return
	}
	hosts, err := s.store.Hosts(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if msg := err.Error(); s.st.hosts.err != msg {
			s.log.Warn("hosts.json can't be read: the hostnames last read stay", "err", msg)
			s.st.hosts.err = msg
		}
		return
	}
	s.st.hosts.err = ""
	if hosts == nil {
		hosts = map[string]string{}
	}
	if !maps.Equal(hosts, s.st.hosts.account) {
		if s.st.hosts.account != nil {
			s.log.Info("the account's hostnames changed", "hosts", hosts)
		}
		s.st.hosts.account = hosts
	}
}

// hosts are the target's hosts and their hostnames in this account.
func (s *Scheduler) hosts(tgt *target) map[string]string {
	if tgt == nil || len(tgt.spec.Hosts) == 0 {
		return nil
	}
	out := make(map[string]string, len(tgt.spec.Hosts))
	for name, hostname := range tgt.spec.Hosts {
		if h := s.st.hosts.account[name]; h != "" {
			hostname = h
		}
		out[name] = hostname
	}
	return out
}

// hostsEnv adds each host's HOST_ and ORIGIN_ variables. Browsers reach every host through
// Cloudflare, over https.
func hostsEnv(env, hosts map[string]string) {
	for name, hostname := range hosts {
		env[spec.HostEnv(name)] = hostname
		env[spec.OriginEnv(name)] = "https://" + hostname
	}
}

// fingerprint is a component's fingerprint at a release, with the account's hostnames:
// what its replicas should have. Without hosts it is the release's.
func (s *Scheduler) fingerprint(tgt *target, component string) string {
	fp := tgt.rel.Components[component].Fingerprint
	hosts := s.hosts(tgt)
	if len(hosts) == 0 || fp == "" {
		return fp
	}
	b, _ := json.Marshal(struct {
		Fingerprint string            `json:"fingerprint"`
		Hosts       map[string]string `json:"hosts"`
	}{fp, hosts})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
