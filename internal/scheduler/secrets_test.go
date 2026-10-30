package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// secretSpec is testSpec with keys in the environment's secret: the api and the worker
// read a shared connection string, PostgreSQL gets its password as a file and applies a new
// one with a hook, and Redis gets its password as a file and restarts for a new one.
var secretSpec = func() string {
	s := testSpec
	for _, edit := range [][2]string{
		{"    env: { MODE: prod }\n", "    env: { MODE: prod }\n    secrets: [ConnectionStrings__db, StripeKey]\n"},
		{"    stop: 60s\n", "    stop: 60s\n    secrets: [ConnectionStrings__db]\n"},
		{"    backup: [pg_dumpall, -U, postgres]\n", "    backup: [pg_dumpall, -U, postgres]\n    secrets: [POSTGRES_PASSWORD]\n    secret_files: true\n    on_secret_change: [sh, -c, alter-role]\n"},
		{"    volume: { path: /data, size: 20Gi }\n", "    volume: { path: /data, size: 20Gi }\n    secrets: [REDIS_PASSWORD]\n    secret_files: true\n"},
	} {
		if !strings.Contains(s, edit[0]) {
			panic("testSpec has no " + edit[0])
		}
		s = strings.Replace(s, edit[0], edit[1], 1)
	}
	return s
}()

// The values below are the secret's. None may reach state.json, the log or the status.
const (
	dbPassword1 = "db-password-one"
	dbPassword2 = "db-password-two"
)

var secretV1 = map[string]string{
	"Shared_ConnectionStrings__db": "Host=postgres;Password=" + dbPassword1,
	"Api_StripeKey":                "sk_live_one",
	"Postgres_POSTGRES_PASSWORD":   dbPassword1,
	"Redis_REDIS_PASSWORD":         "redis-password-one",
	"Edge_TUNNEL_TOKEN":            "tunnel-token-one",
}

// settleSecrets writes the secret's first version and rolls out secretSpec.
func (h *harness) settleSecrets() {
	h.t.Helper()
	h.writeSecret(secretV1)
	h.run(time.Minute) // The scheduler sees it, as it would a secret set before the first deploy.
	h.deploy("c1", secretSpec, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.run(time.Minute) // The data VM's agent syncs, and PostgreSQL applies its keys.
}

// replicaIDs are a component's replicas that aren't being stopped.
func (h *harness) replicaIDs(component string) []string {
	var out []string
	for _, r := range h.replicas(component) {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

func (h *harness) secretStatus(component string) SecretStatus {
	for _, c := range h.s.Status().Components {
		if c.Name == component && c.Secret != nil {
			return *c.Secret
		}
	}
	return SecretStatus{}
}

// readyOn reports whether every replica of a component is ready with a version of its keys.
func (h *harness) readyOn(component, version string, n int) bool {
	rs := h.replicas(component)
	if len(rs) != n {
		return false
	}
	for _, r := range rs {
		if r.State != api.StateReady || r.SecretVersion != version {
			return false
		}
	}
	return true
}

func overlap(a, b []string) bool {
	return slices.ContainsFunc(a, func(x string) bool { return slices.Contains(b, x) })
}

func TestReplicasLearnTheEnvironmentsSecret(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	data := h.agents["i-data1"]
	if data.assignment.Secret != "shop/dev" {
		t.Errorf("assignment secret %q, want <app>/<environment>", data.assignment.Secret)
	}
	if v := data.assignment.SecretVersions["postgres"]; v != "v-1" {
		t.Errorf("postgres follows %q", v)
	}
	var api, pg api.ReplicaSpec
	for _, a := range h.agents {
		for _, r := range a.replicas {
			switch r.spec.Component {
			case "api":
				api = r.spec
			case "postgres":
				pg = r.spec
			}
		}
	}
	if api.SecretPrefix != "Api_" || api.SecretVersion != "v-1" || len(api.SecretDigest) != 32 || len(api.SecretFiles) != 0 {
		t.Errorf("api: prefix %q version %q digest %q files %v", api.SecretPrefix, api.SecretVersion, api.SecretDigest, api.SecretFiles)
	}
	if pg.SecretPrefix != "Postgres_" || !slices.Equal(pg.SecretFiles, []string{"POSTGRES_PASSWORD"}) || !pg.SecretFollows ||
		!slices.Equal(pg.SecretHook, []string{"sh", "-c", "alter-role"}) {
		t.Errorf("postgres: %+v", pg)
	}
	if h.secretStatus("api").Version != "v-1" {
		t.Errorf("status %+v", h.secretStatus("api"))
	}
}

func TestOnlyComponentsWhoseKeysChangedAreUpdated(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge"} {
		before[c] = h.replicaIDs(c)
	}
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.until("the api has the new key", 6*time.Minute, func() bool { return h.readyOn("api", v2, 2) })
	if overlap(h.replicaIDs("api"), before["api"]) {
		t.Errorf("api replicas %v weren't all replaced (were %v)", h.replicaIDs("api"), before["api"])
	}
	h.run(2 * time.Minute)
	for _, c := range []string{"web", "postgres", "redis", "edge"} {
		if !slices.Equal(h.replicaIDs(c), before[c]) {
			t.Errorf("%s was replaced for a key that isn't its: %v, was %v", c, h.replicaIDs(c), before[c])
		}
	}
	if h.rollout("c1") != RolloutDone || h.s.Status().Target != "c1" {
		t.Errorf("a secret change touched the release: %s", h.describe())
	}
}

func TestASharedKeyUpdatesTheComponentsThatListIt(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	h.queues.set("jobs", 20, 0)
	h.until("the worker runs", 5*time.Minute, func() bool { return h.ready("worker") > 0 })
	workers, redis := h.replicaIDs("worker"), h.replicaIDs("redis")
	v2 := h.writeSecret(map[string]string{"Shared_ConnectionStrings__db": "Host=postgres;Password=" + dbPassword2})
	h.until("the api and the worker have the new connection string", 8*time.Minute, func() bool {
		return h.readyOn("api", v2, 2) && len(h.replicas("worker")) > 0 && !overlap(h.replicaIDs("worker"), workers) &&
			h.replicas("worker")[0].SecretVersion == v2
	})
	if !slices.Equal(h.replicaIDs("redis"), redis) {
		t.Errorf("redis doesn't list the shared key, and was replaced")
	}
}

func TestTheSecretIsCheckedAboutOnceAMinute(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	d0, r0 := h.secret.counts()
	h.run(10 * time.Minute)
	d1, r1 := h.secret.counts()
	if n := d1 - d0; n < 9 || n > 11 {
		t.Errorf("%d checks in 10 minutes, want about 10", n)
	}
	if r1 != r0 {
		t.Errorf("%d reads of an unchanged secret: a check reads only its version", r1-r0)
	}
	h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.run(90 * time.Second)
	if _, r2 := h.secret.counts(); r2 != r1+1 {
		t.Errorf("%d reads of a new version, want one", r2-r1)
	}
	for _, id := range h.secret.ids {
		if id != "shop/dev" {
			t.Fatalf("asked for %q", id)
		}
	}
}

func TestASecretChangeWaitsForTheSettleWindow(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	before := h.replicaIDs("api")
	h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.run(90 * time.Second)
	if !slices.Equal(h.replicaIDs("api"), before) {
		t.Fatalf("the api changed before the secret settled")
	}
	if st := h.secretStatus("api"); st.Pending == "" || !strings.Contains(st.Waiting, "settle") {
		t.Errorf("status %+v", st)
	}
	v3 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_three"})
	h.run(90 * time.Second)
	if !slices.Equal(h.replicaIDs("api"), before) {
		t.Fatalf("the api changed before the second write settled")
	}
	var versions []string
	h.until("the api has the last write", 6*time.Minute, func() bool {
		for _, r := range h.replicas("api") {
			if !slices.Contains(versions, r.SecretVersion) {
				versions = append(versions, r.SecretVersion)
			}
		}
		return h.readyOn("api", v3, 2)
	})
	if slices.Contains(versions, "v-2") {
		t.Errorf("replicas started with the first write, though both were applied as one: %v", versions)
	}
}

func TestAFollowerTakesANewVersionWithoutNewReplicas(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	pg := h.replicaIDs("postgres")
	v2 := h.writeSecret(map[string]string{"Postgres_POSTGRES_PASSWORD": dbPassword2})
	h.until("postgres applies it", 5*time.Minute, func() bool {
		rs := h.replicas("postgres")
		return len(rs) == 1 && rs[0].SecretVersion == v2
	})
	if !slices.Equal(h.replicaIDs("postgres"), pg) {
		t.Errorf("postgres was replaced: %v, was %v", h.replicaIDs("postgres"), pg)
	}
	if v := h.agents["i-data1"].assignment.SecretVersions["postgres"]; v != v2 {
		t.Errorf("data-1 is assigned %q", v)
	}
}

func TestDependenciesApplyTheirSecretsFirst(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	api := h.replicaIDs("api")
	// One write changes PostgreSQL's password and the connection string the api uses.
	v2 := h.writeSecret(map[string]string{
		"Postgres_POSTGRES_PASSWORD":   dbPassword2,
		"Shared_ConnectionStrings__db": "Host=postgres;Password=" + dbPassword2,
	})
	h.hookFails["postgres@"+v2] = true
	h.run(6 * time.Minute)
	if !slices.Equal(h.replicaIDs("api"), api) {
		t.Fatalf("the api changed before postgres applied its password")
	}
	if st := h.secretStatus("api"); !strings.Contains(st.Waiting, "postgres") {
		t.Errorf("api status %+v: it should say what it waits for", st)
	}
	if st := h.secretStatus("postgres"); !strings.Contains(st.Failed, "hook exited") {
		t.Errorf("postgres status %+v", st)
	}
	delete(h.hookFails, "postgres@"+v2)
	h.until("the api has the new connection string", 6*time.Minute, func() bool { return h.readyOn("api", v2, 2) })
}

func TestAFailingSecretChangeStopsTheRoll(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	api := h.replicaIDs("api")
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "a-key-with-a-typo"})
	h.badSecret["api"] = v2
	h.run(10 * time.Minute)
	if got := h.replicaIDs("api"); !slices.Equal(got, api) || h.ready("api") != 2 {
		t.Errorf("api %v with %d ready: the replicas that work keep running (were %v)\n%s", got, h.ready("api"), api, h.describe())
	}
	if st := h.secretStatus("api"); st.Version != v2 || st.Failed == "" {
		t.Errorf("status %+v: it moves forward, and says why it is stuck", st)
	}
	if st := h.s.Status(); st.Target != "c1" || st.LastGood != "c1" {
		t.Errorf("a broken secret changed the release: %s", h.describe())
	}

	// A fixed value is the way out.
	v3 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.until("the api has the fixed key", 10*time.Minute, func() bool { return h.readyOn("api", v3, 2) })
	if st := h.secretStatus("api"); st.Failed != "" {
		t.Errorf("status %+v", st)
	}
}

func TestAStatefulComponentRestartsForANewSecret(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	redis := h.replicaIDs("redis")
	v2 := h.writeSecret(map[string]string{"Redis_REDIS_PASSWORD": "redis-password-two"})
	h.until("redis has the new password", 6*time.Minute, func() bool { return h.readyOn("redis", v2, 1) })
	if overlap(h.replicaIDs("redis"), redis) {
		t.Errorf("redis wasn't replaced")
	}
}

func TestTheEdgeTakesANewTunnelToken(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	edges := h.replicaIDs("edge")
	v2 := h.writeSecret(map[string]string{"Edge_TUNNEL_TOKEN": "tunnel-token-two"})
	h.until("the edge has the new token", 6*time.Minute, func() bool {
		if h.ready("edge") == 0 {
			t.Fatalf("no edge was ready: requests stop")
		}
		return h.readyOn("edge", v2, 1)
	})
	if overlap(h.replicaIDs("edge"), edges) {
		t.Errorf("the edge wasn't replaced")
	}
}

func TestSecretRollsWaitForARelease(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	h.readyAfter["web"] = 54 // A new web takes four and a half minutes to get ready.
	h.deploy("c2", secretSpec, map[string]string{"web": "v2"})
	api := h.replicaIDs("api")
	h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	for range int(4 * time.Minute / step) {
		h.step()
		if h.rollout("c2") != RolloutRunning {
			t.Fatalf("c2 finished early: the test needs it rolling out")
		}
		if !slices.Equal(h.replicaIDs("api"), api) {
			t.Fatalf("the api changed while a release rolled out")
		}
	}
	if st := h.secretStatus("api"); !strings.Contains(st.Waiting, "release") {
		t.Errorf("status %+v", st)
	}
	h.until("c2 finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	h.until("then the api changes", 6*time.Minute, func() bool { return !overlap(h.replicaIDs("api"), api) && h.ready("api") == 2 })
}

func TestARedeployDoesntBlameReplicasAStoppedRollLeft(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "a-key-with-a-typo"})
	h.badSecret["api"] = v2
	h.until("an api replica fails", 6*time.Minute, func() bool { return strings.Contains(h.log.String(), `"msg":"replica failed"`) })
	if err := h.s.Deploy(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	h.run(3 * time.Minute)
	if st := h.rollout("c1"); st == RolloutFailed {
		t.Errorf("redeploying c1 failed: %s", h.rolloutReason("c1"))
	}
	if st := h.s.Status(); st.Target != "c1" || h.ready("api") != 2 {
		t.Errorf("target %q, %d api ready: %s", st.Target, h.ready("api"), h.describe())
	}
}

func TestUpgradingRollsNothing(t *testing.T) {
	// A cluster that ran before the environment's secret: its replicas have no digest.
	secret := &fakeSecrets{versions: map[string]map[string]string{}}
	h := newHarness(t, func(h *harness) { h.secret = nil })
	h.deploy("c1", secretSpec, nil)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge"} {
		before[c] = h.replicaIDs(c)
	}
	h.secret = secret
	h.writeSecret(secretV1)
	h.start()
	h.run(10 * time.Minute)
	for c, ids := range before {
		if !slices.Equal(h.replicaIDs(c), ids) {
			t.Errorf("%s was replaced on upgrade: %v, was %v", c, h.replicaIDs(c), ids)
		}
	}
	// The first real change replaces them.
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.until("the api has the new key", 6*time.Minute, func() bool { return h.readyOn("api", v2, 2) })
}

func TestARestartedSchedulerRemembersWhatChanged(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	api := h.replicaIDs("api")
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.run(70 * time.Second) // The change is seen, but hasn't settled.
	h.start()
	h.until("the api has the new key", 8*time.Minute, func() bool { return h.readyOn("api", v2, 2) })
	if overlap(h.replicaIDs("api"), api) {
		t.Errorf("not every api replica was replaced")
	}
	after := h.replicaIDs("api")
	h.start()
	h.run(6 * time.Minute)
	if !slices.Equal(h.replicaIDs("api"), after) {
		t.Errorf("a restarted scheduler applied the change again: %v, was %v", h.replicaIDs("api"), after)
	}
}

func TestAnUnreadableSecretChangesNothing(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	api := h.replicaIDs("api")
	h.secret.mu.Lock()
	h.secret.err = errors.New("AccessDeniedException: not authorized")
	h.secret.mu.Unlock()
	h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.run(6 * time.Minute)
	if !slices.Equal(h.replicaIDs("api"), api) {
		t.Errorf("the api changed")
	}
	if st := h.s.Status(); st.Secret == nil || !strings.Contains(st.Secret.Error, "AccessDenied") {
		t.Errorf("status secret %+v", st.Secret)
	}
}

func TestSecretValuesNeverReachStateOrLogs(t *testing.T) {
	h := newHarness(t)
	h.settleSecrets()
	h.writeSecret(map[string]string{"Postgres_POSTGRES_PASSWORD": dbPassword2, "Shared_ConnectionStrings__db": "Host=postgres;Password=" + dbPassword2})
	h.run(8 * time.Minute)
	state, _ := json.Marshal(h.store.state)
	status, _ := json.Marshal(h.s.Status())
	var assignments []byte
	for _, a := range h.agents {
		b, _ := json.Marshal(a.assignment)
		assignments = append(assignments, b...)
	}
	for _, v := range []string{dbPassword1, dbPassword2, "sk_live_one", "tunnel-token-one", "redis-password-one"} {
		for what, text := range map[string]string{"state.json": string(state), "status": string(status), "log": h.log.String(), "assignments": string(assignments)} {
			if strings.Contains(text, v) {
				t.Errorf("the %s holds a secret value", what)
			}
		}
	}
	if !strings.Contains(string(state), `"secrets"`) {
		t.Errorf("state.json doesn't record the components' keys: %s", state)
	}
}

func TestANewDigestKeyRollsNothing(t *testing.T) {
	// The key lives on data-1's control volume. If it is ever lost, every hash changes: that
	// must not look like every component's keys changing.
	h := newHarness(t)
	h.settleSecrets()
	before := map[string][]string{}
	for _, c := range []string{"api", "web", "postgres", "redis", "edge"} {
		before[c] = h.replicaIDs(c)
	}
	h.secretKey = []byte("another key")
	h.start()
	h.run(6 * time.Minute)
	for c, ids := range before {
		if !slices.Equal(h.replicaIDs(c), ids) {
			t.Errorf("%s was replaced for a new key: %v, was %v", c, h.replicaIDs(c), ids)
		}
	}
	v2 := h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.until("the api has the new key", 6*time.Minute, func() bool { return h.readyOn("api", v2, 2) })
}

func TestTheDigestKeyIsKeptInTheStateDir(t *testing.T) {
	dir := t.TempDir()
	k1, err := LoadOrCreateSecretKey(dir)
	if err != nil || len(k1) != 32 {
		t.Fatalf("key %d bytes, %v", len(k1), err)
	}
	k2, err := LoadOrCreateSecretKey(dir)
	if err != nil || string(k1) != string(k2) {
		t.Errorf("a second load made another key")
	}
	if info, err := os.Stat(filepath.Join(dir, "secret-digest.key")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v %v", info.Mode(), err)
	}
}
