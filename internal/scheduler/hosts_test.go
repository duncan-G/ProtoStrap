package scheduler

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/edge"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/spec"
)

// hostsSpec is testSpec with two named hosts: the api on the shop host's /api/, and web
// on the whole admin host as well as /web/ on every host.
var hostsSpec = func() string {
	s := testSpec
	for _, edit := range [][2]string{
		{"protostrap: 1.4.0\n", "protostrap: 1.4.0\nhosts:\n  shop: shop.example.com\n  admin: admin.example.com\n"},
		{"    routes: [/api/]\n", "    routes: [shop/api/]\n"},
		{"    routes: [/web/]\n", "    routes: [admin/, /web/]\n"},
	} {
		if !strings.Contains(s, edit[0]) {
			panic("testSpec has no " + edit[0])
		}
		s = strings.Replace(s, edit[0], edit[1], 1)
	}
	return s
}()

// envOf returns the environment of a component's running replicas, by replica.
func (h *harness) envOf(component string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, a := range h.agents {
		for id, r := range a.replicas {
			if r.spec.Component == component && !r.gone && r.state != api.StateStopping {
				out[id] = r.spec.Env
			}
		}
	}
	return out
}

// edgeListener is the listener the scheduler gives the ingress VMs' edges.
func (h *harness) edgeListener() string {
	return h.vms("ingress")[0].assignment.Files["edge/lds.json"]
}

func (h *harness) deployHosts() {
	h.t.Helper()
	h.deploy("c1", hostsSpec, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
}

// Every replica gets each host's hostname and origin, for the redirect URLs, cookie domains
// and allowed origins an application builds from its own hostnames. Without an account's
// hostnames, the spec's are used.
func TestReplicasLearnTheHostnames(t *testing.T) {
	h := newHarness(t)
	h.deployHosts()

	for _, component := range []string{"api", "web", "postgres", "redis"} {
		envs := h.envOf(component)
		if len(envs) == 0 {
			t.Fatalf("no %s replicas", component)
		}
		for id, env := range envs {
			want := map[string]string{
				"HOST_SHOP": "shop.example.com", "ORIGIN_SHOP": "https://shop.example.com",
				"HOST_ADMIN": "admin.example.com", "ORIGIN_ADMIN": "https://admin.example.com",
			}
			for k, v := range want {
				if env[k] != v {
					t.Errorf("%s: %s = %q, want %q", id, k, env[k], v)
				}
			}
		}
	}
	lds := h.edgeListener()
	if !strings.Contains(lds, `"admin.example.com"`) || !strings.Contains(lds, `"shop.example.com"`) {
		t.Errorf("the edge's listener lacks the spec's hostnames:\n%s", lds)
	}
}

// An account's Terraform config gives the spec's names hostnames of its own, as hosts.json
// in the deploy bucket. Its hostnames are what the edge serves and the replicas learn.
func TestAnAccountsHostnamesWin(t *testing.T) {
	h := newHarness(t)
	h.store.setHosts(map[string]string{"shop": "shop.staging.example.com", "admin": "admin.staging.example.com"})
	h.run(time.Minute)
	h.deployHosts()

	for id, env := range h.envOf("api") {
		if env["HOST_SHOP"] != "shop.staging.example.com" || env["ORIGIN_ADMIN"] != "https://admin.staging.example.com" {
			t.Errorf("%s: HOST_SHOP %q ORIGIN_ADMIN %q", id, env["HOST_SHOP"], env["ORIGIN_ADMIN"])
		}
	}
	lds := h.edgeListener()
	if !strings.Contains(lds, `"admin.staging.example.com"`) || strings.Contains(lds, `"admin.example.com"`) {
		t.Errorf("the edge serves the spec's hostname, not the account's:\n%s", lds)
	}
}

// A changed hostname reloads the edge's routes and replaces every replica, a quarter at a
// time, since its HOST_ and ORIGIN_ variables changed. The edge itself keeps running.
func TestAChangedHostnameReplacesEveryReplica(t *testing.T) {
	h := newHarness(t)
	h.deployHosts()
	edges := h.running("edge")
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres"} {
		before[c] = h.running(c)
	}

	h.store.setHosts(map[string]string{"shop": "shop.example.com", "admin": "console.example.com"})
	h.until("every replica has the new hostname", 10*time.Minute, func() bool {
		for _, c := range []string{"api", "web", "postgres", "redis"} {
			envs := h.envOf(c)
			if len(envs) == 0 {
				return false
			}
			for _, env := range envs {
				if env["HOST_ADMIN"] != "console.example.com" {
					return false
				}
			}
		}
		return true
	})
	h.settle()

	for c, ids := range before {
		for _, id := range ids {
			if slices.Contains(h.running(c), id) {
				t.Errorf("%s still runs %s, which has the old hostname", c, id)
			}
		}
	}
	if got := h.running("edge"); !slices.Equal(got, edges) {
		t.Errorf("the edges were replaced: %v, then %v", edges, got)
	}
	if lds := h.edgeListener(); !strings.Contains(lds, `"console.example.com"`) || strings.Contains(lds, `"admin.example.com"`) {
		t.Errorf("the edge's listener has the old hostname:\n%s", lds)
	}
	if h.ready("api") < 2 {
		t.Errorf("api has %d ready replicas, want its minimum of 2", h.ready("api"))
	}
}

// Replacing every replica of a service a quarter at a time keeps it serving: it never has
// fewer ready replicas than its count less a quarter.
func TestAHostnameChangeKeepsServicesServing(t *testing.T) {
	h := newHarness(t)
	h.deployHosts()
	h.store.setHosts(map[string]string{"shop": "store.example.com", "admin": "admin.example.com"})
	for range 60 {
		h.step()
		if got := h.runningReady("api"); got < 1 {
			t.Fatalf("api has %d ready replicas mid-change:\n%s", got, h.describe())
		}
	}
}

// The scheduler reads hosts.json about once a minute, not every cycle.
func TestHostsAreReadAboutOnceAMinute(t *testing.T) {
	h := newHarness(t)
	h.deployHosts()
	before := h.store.hostsReads()
	h.run(5 * time.Minute)
	if n := h.store.hostsReads() - before; n < 4 || n > 6 {
		t.Errorf("hosts.json read %d times in 5 minutes", n)
	}
}

// When hosts.json can't be read, the hostnames last read stay: an unreadable file replaces
// nothing. They survive a restart, in state.json.
func TestUnreadableHostsKeepTheLastHostnames(t *testing.T) {
	h := newHarness(t)
	h.store.setHosts(map[string]string{"shop": "shop.staging.example.com", "admin": "admin.staging.example.com"})
	h.run(time.Minute)
	h.deployHosts()
	ids := h.running("api")

	h.store.failHosts(errors.New("AccessDenied"))
	h.run(5 * time.Minute)
	h.start() // The scheduler restarts, and still can't read the file.
	h.run(5 * time.Minute)

	if got := h.running("api"); !slices.Equal(got, ids) {
		t.Errorf("api's replicas were replaced: %v, then %v", ids, got)
	}
	if lds := h.edgeListener(); !strings.Contains(lds, `"shop.staging.example.com"`) {
		t.Errorf("the edge lost the account's hostnames:\n%s", lds)
	}
	if st := h.store.saved(); st.Hosts["shop"] != "shop.staging.example.com" {
		t.Errorf("state.json hosts %v", st.Hosts)
	}
}

// A spec without hosts has nothing for hosts.json to change: replicas get no HOST_
// variables, and their fingerprints are the release's.
func TestASpecWithoutHostsIgnoresHostsJSON(t *testing.T) {
	h := newHarness(t)
	h.deploy("c1", testSpec, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	ids := h.running("api")

	h.store.setHosts(map[string]string{"shop": "shop.example.com"})
	h.run(5 * time.Minute)

	if got := h.running("api"); !slices.Equal(got, ids) {
		t.Errorf("api's replicas were replaced: %v, then %v", ids, got)
	}
	for id, env := range h.envOf("api") {
		for k := range env {
			if strings.HasPrefix(k, "HOST_") || strings.HasPrefix(k, "ORIGIN_") {
				t.Errorf("%s has %s", id, k)
			}
		}
	}
	for _, r := range h.replicas("api") {
		if r.Fingerprint != h.store.releases["c1"].Components["api"].Fingerprint {
			t.Errorf("%s has fingerprint %s, not the release's", r.ID, r.Fingerprint)
		}
	}
}

// A name the account doesn't give a hostname keeps the spec's, and one the spec doesn't have
// is ignored. Terraform refuses such a map; the scheduler doesn't depend on it.
func TestAccountHostnamesOnlyForTheSpecsNames(t *testing.T) {
	h := newHarness(t)
	h.store.setHosts(map[string]string{"shop": "shop.staging.example.com", "blog": "blog.example.com"})
	h.run(time.Minute)
	h.deployHosts()
	for id, env := range h.envOf("api") {
		if env["HOST_SHOP"] != "shop.staging.example.com" || env["HOST_ADMIN"] != "admin.example.com" {
			t.Errorf("%s: HOST_SHOP %q HOST_ADMIN %q", id, env["HOST_SHOP"], env["HOST_ADMIN"])
		}
		if _, ok := env["HOST_BLOG"]; ok {
			t.Errorf("%s has HOST_BLOG", id)
		}
	}
}

// The fake deploy bucket's hosts.json.

func (s *fakeStore) Hosts(context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostsRead++
	if s.hostsErr != nil {
		return nil, s.hostsErr
	}
	return maps.Clone(s.hosts), nil
}

func (s *fakeStore) setHosts(hosts map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts, s.hostsErr = hosts, nil
}

func (s *fakeStore) failHosts(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hostsErr = err
}

func (s *fakeStore) hostsReads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hostsRead
}

func (s *fakeStore) saved() release.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return release.State{}
	}
	return *s.state
}

// A service authorized by another reaches the edge with its authorizer, which gets a
// cluster even when it has no routes of its own, and with what of the authorizer's answer
// goes to the request and what to the client.
func TestTheEdgeAsksAnAuthorizer(t *testing.T) {
	sp := strings.Replace(hostsSpec, "    routes: [shop/api/]\n",
		"    routes: [shop/api/]\n    authorize: { by: web, path: /check, headers: [x-user], forward: [cookie], respond: [set-cookie] }\n", 1)
	h := newHarness(t)
	h.deploy("c1", sp, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	lds := h.edgeListener()
	for _, want := range []string{`"envoy.filters.http.ext_authz/api"`, `"path_prefix": "/check"`, `"exact": "x-user"`, `"exact": "cookie"`,
		`"allowed_client_headers_on_success"`, `"exact": "set-cookie"`} {
		if !strings.Contains(lds, want) {
			t.Errorf("the edge's listener lacks %s", want)
		}
	}
}

// EdgeServices hands the edge each part of a service's authorize as the spec has it.
func TestEdgeServicesCarryTheAuthorizer(t *testing.T) {
	sp, err := spec.Parse([]byte(strings.Replace(hostsSpec, "    routes: [shop/api/]\n",
		"    routes: [shop/api/]\n    authorize: { by: web, headers: [x-user], forward: [cookie], respond: [set-cookie, x-expires] }\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	var api *edge.Service
	for _, s := range EdgeServices(sp) {
		if s.Name == "api" {
			api = &s
		}
	}
	want := edge.Authorize{By: "web", Path: "/", Headers: []string{"x-user"}, Forward: []string{"cookie"}, Respond: []string{"set-cookie", "x-expires"}}
	if api == nil || api.Authorize == nil || !reflect.DeepEqual(*api.Authorize, want) {
		t.Errorf("api reaches the edge as %+v, want authorize %+v", api, want)
	}
}
