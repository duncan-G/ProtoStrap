package scheduler

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
)

// configSpec is testSpec with config files and a configure task:
//
//   - the api takes its config files without being replaced: the agent swaps them in and
//     the api reloads them itself;
//   - web restarts for new config, and depends on idp too;
//   - idp is a service configured through its API by the idp-config task, which runs once
//     idp is ready, and again whenever the task, its keys or idp change.
var configSpec = func() string {
	s := testSpec
	for _, edit := range [][2]string{
		{"    env: { MODE: prod }\n", "    env: { MODE: prod }\n    config: { from: config/api, path: /etc/api }\n    on_config_change: reload\n"},
		{"    depends_on: [api]\n", "    depends_on: [api, idp]\n    config: { from: config/web, path: /etc/web }\n"},
		{"components:\n", `components:
  idp:
    kind: service
    build: idp
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    port: 8080
    ready: /health
    depends_on: [postgres]
    configure: idp-config
    scale:
      min: 1
      max: 2
      demand: [{ cpu: 70% }]
  idp-config:
    kind: task
    build: idp-config
    pool: general
    request: { cpu: 250m, memory: 256Mi }
    depends_on: [idp]
    secrets: [ADMIN_SECRET]
    config: { from: config/idp, path: /config }
`},
	} {
		if !strings.Contains(s, edit[0]) {
			panic("testSpec has no " + edit[0])
		}
		s = strings.Replace(s, edit[0], edit[1], 1)
	}
	return s
}()

// configV1 is each component's config files in the first release, by their content.
var configV1 = map[string]string{"api": "api-1", "web": "web-1", "idp-config": "realm-1"}

func digestOf(content string) string {
	return configfiles.Digest(configfiles.Files{"conf": []byte(content)})
}

func with(m map[string]string, changes ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(changes); i += 2 {
		out[changes[i]] = changes[i+1]
	}
	return out
}

// settleConfig rolls out configSpec with configV1.
func (h *harness) settleConfig() {
	h.t.Helper()
	h.deployWith("c1", configSpec, nil, configV1)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
}

// apiConfig is each live api replica's config digest: what it has applied.
func (h *harness) appliedConfig(component string) map[string]string {
	out := map[string]string{}
	for _, a := range h.agents {
		for id, r := range a.replicas {
			if r.spec.Component == component && r.state != api.StateStopping && !r.gone {
				out[id] = r.configApplied
			}
		}
	}
	return out
}

// wantedConfig is each live replica's digest in its VM's assignment.
func (h *harness) wantedConfig(component string) map[string]string {
	out := map[string]string{}
	for _, a := range h.agents {
		for _, s := range a.assignment.Replicas {
			if s.Component == component {
				out[s.ID] = a.assignment.ConfigDigests[s.ID]
			}
		}
	}
	return out
}

func allEqual(m map[string]string, v string) bool {
	if len(m) == 0 {
		return false
	}
	for _, x := range m {
		if x != v {
			return false
		}
	}
	return true
}

func (h *harness) configureStatus(component string) ConfigureStatus {
	for _, c := range h.s.Status().Components {
		if c.Name == component && c.Configure != nil {
			return *c.Configure
		}
	}
	return ConfigureStatus{}
}

// runImages are the images of a task's runs, in order.
func (h *harness) runImages(task string) []string {
	var out []string
	for _, r := range h.taskRuns[task] {
		out = append(out, r.image[strings.LastIndex(r.image, ":")+1:])
	}
	return out
}

// liveTasks counts a task's replicas that are running on any VM.
func (h *harness) liveTasks(task string) int {
	n := 0
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == task && r.spec.Kind == api.KindTask && r.state == api.StateStarting {
				n++
			}
		}
	}
	return n
}

func TestConfigReachesReplicas(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	apiDigest := digestOf("api-1")
	var apiSpec, webSpec, taskSpec api.ReplicaSpec
	for _, a := range h.agents {
		for _, r := range a.replicas {
			switch r.spec.Component {
			case "api":
				apiSpec = r.spec
			case "web":
				webSpec = r.spec
			}
		}
	}
	for _, run := range h.taskRuns["idp-config"] {
		taskSpec = run.spec
	}
	if apiSpec.ConfigPath != "/etc/api" || apiSpec.ConfigDigest != apiDigest || !apiSpec.ConfigFollows || len(apiSpec.ConfigHook) != 0 {
		t.Errorf("api: path %q digest %q follows %v", apiSpec.ConfigPath, apiSpec.ConfigDigest, apiSpec.ConfigFollows)
	}
	if webSpec.ConfigPath != "/etc/web" || webSpec.ConfigDigest != digestOf("web-1") || webSpec.ConfigFollows {
		t.Errorf("web: path %q digest %q follows %v", webSpec.ConfigPath, webSpec.ConfigDigest, webSpec.ConfigFollows)
	}
	if taskSpec.ConfigPath != "/config" || taskSpec.ConfigDigest != digestOf("realm-1") {
		t.Errorf("idp-config: path %q digest %q", taskSpec.ConfigPath, taskSpec.ConfigDigest)
	}
	if want := h.wantedConfig("api"); !allEqual(want, apiDigest) || len(want) != 2 {
		t.Errorf("the api's assignments name %v, want %s for each replica", want, apiDigest)
	}
	files, ok := h.s.ConfigFiles(apiDigest)
	if !ok || string(files["conf"]) != "api-1" {
		t.Errorf("the scheduler serves %v %v for the api's digest", files, ok)
	}
	if _, ok := h.s.ConfigFiles(digestOf("never released")); ok {
		t.Errorf("the scheduler serves files no release has")
	}
	if got := h.runImages("idp-config"); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("idp-config ran %v, want once", got)
	}
	if st := h.configureStatus("idp"); st.Task != "idp-config" || st.State != "done" {
		t.Errorf("idp's configure status %+v", st)
	}
}

func TestTheConfigureTaskRunsOnceItsComponentIsReady(t *testing.T) {
	h := newHarness(t)
	h.taskSteps["idp-config"] = 30 // It runs long after everything else could have started.
	h.deployWith("c1", configSpec, nil, configV1)
	var idpReadyAtRun, configuredAtWeb = -1, false
	webSeen := false
	h.until("the first release finishes", 10*time.Minute, func() bool {
		if idpReadyAtRun < 0 && len(h.taskRuns["idp-config"]) > 0 {
			idpReadyAtRun = h.ready("idp")
		}
		if !webSeen && len(h.replicas("web")) > 0 {
			webSeen = true
			configuredAtWeb = h.configureStatus("idp").State == "done"
		}
		return h.rollout("c1") == RolloutDone
	})
	if idpReadyAtRun < 1 {
		t.Errorf("idp-config started with %d ready idp replicas: it configures idp through its API", idpReadyAtRun)
	}
	if !configuredAtWeb {
		t.Errorf("web started before idp was configured: a component waits for what it depends on to be configured")
	}
	if got := h.runImages("idp-config"); !slices.Equal(got, []string{"v1"}) {
		t.Errorf("idp-config ran %v, want once", got)
	}
}

func TestNewConfigIsSwappedIntoFollowers(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	apiBefore := h.replicaIDs("api")
	webBefore := h.replicaIDs("web")
	oldDigest, newDigest := digestOf("api-1"), digestOf("api-2")
	h.deployWith("c2", configSpec, nil, with(configV1, "api", "api-2"))
	staged := false
	h.until("the release finishes", 6*time.Minute, func() bool {
		wanted := h.wantedConfig("api")
		if slices.Contains(slices.Collect(maps.Values(wanted)), newDigest) && slices.Contains(slices.Collect(maps.Values(wanted)), oldDigest) {
			staged = true
		}
		return h.rollout("c2") == RolloutDone
	})
	if !staged {
		t.Errorf("both api replicas were told to take the new config at once: a quarter at a time, at least one")
	}
	if !slices.Equal(h.replicaIDs("api"), apiBefore) {
		t.Errorf("api replicas %v, were %v: a follower takes new config without being replaced", h.replicaIDs("api"), apiBefore)
	}
	if got := h.appliedConfig("api"); !allEqual(got, newDigest) {
		t.Errorf("api replicas have %v, want %s", got, newDigest)
	}
	if !slices.Equal(h.replicaIDs("web"), webBefore) {
		t.Errorf("web was replaced, but its config didn't change")
	}
	if got := h.runImages("idp-config"); len(got) != 1 {
		t.Errorf("idp-config ran %v: nothing it depends on changed", got)
	}
	for _, c := range h.s.Status().Components {
		if c.Name == "api" && (c.Config == nil || c.Config.Digest != newDigest || c.Config.Behind != 0) {
			t.Errorf("api's config status %+v", c.Config)
		}
	}
}

func TestNewConfigReplacesAComponentThatRestarts(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	apiBefore := h.replicaIDs("api")
	webBefore := h.replicaIDs("web")
	h.deployWith("c2", configSpec, nil, with(configV1, "web", "web-2"))
	h.until("the release finishes", 6*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	if overlap(h.replicaIDs("web"), webBefore) {
		t.Errorf("web replicas %v weren't all replaced (were %v)", h.replicaIDs("web"), webBefore)
	}
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == "web" && !r.gone && r.state != api.StateStopping && r.spec.ConfigDigest != digestOf("web-2") {
				t.Errorf("web replica %s has %s", r.spec.ID, r.spec.ConfigDigest)
			}
		}
	}
	if !slices.Equal(h.replicaIDs("api"), apiBefore) {
		t.Errorf("the api was replaced, but its config didn't change")
	}
}

func TestAFollowerThatCantTakeNewConfigRollsTheReleaseBack(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	apiBefore := h.replicaIDs("api")
	oldDigest, newDigest := digestOf("api-1"), digestOf("api-2")
	h.configHookFails["api@"+newDigest] = true
	h.deployWith("c2", configSpec, nil, with(configV1, "api", "api-2"))
	h.until("the release fails", 6*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if reason := h.rolloutReason("c2"); !strings.Contains(reason, "api") || !strings.Contains(reason, "config") {
		t.Errorf("reason %q: it names the component and its config", reason)
	}
	h.until("the api has its old config back", 3*time.Minute, func() bool {
		return allEqual(h.wantedConfig("api"), oldDigest) && allEqual(h.appliedConfig("api"), oldDigest)
	})
	if !slices.Equal(h.replicaIDs("api"), apiBefore) {
		t.Errorf("api replicas %v, were %v: rolling back swaps the old files in again", h.replicaIDs("api"), apiBefore)
	}
	if h.s.Status().Target != "c1" {
		t.Errorf("target %s, want the last good release", h.s.Status().Target)
	}
}

func TestNewFollowerReplicasStartWithTheTargetsConfig(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	h.deployWith("c2", configSpec, nil, with(configV1, "api", "api-2"))
	h.until("the release finishes", 6*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	before := h.replicaIDs("api")
	h.totalCPU["api"] = 5 * 500 * 105 / 100
	h.until("the api scales out", 5*time.Minute, func() bool { return len(h.replicaIDs("api")) > len(before) })
	for _, a := range h.agents {
		for _, r := range a.replicas {
			if r.spec.Component == "api" && !slices.Contains(before, r.spec.ID) && r.spec.ConfigDigest != digestOf("api-2") {
				t.Errorf("new replica %s starts with %s", r.spec.ID, r.spec.ConfigDigest)
			}
		}
	}
}

func TestConfigureRunsAgainOnlyWhenItsInputsChange(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	idpBefore := h.replicaIDs("idp")

	// The task's image is new: it runs once, and idp is left alone.
	h.deployWith("c2", configSpec, map[string]string{"idp-config": "v2"}, configV1)
	h.until("c2 finishes", 6*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	if got := h.runImages("idp-config"); !slices.Equal(got, []string{"v1", "v2"}) {
		t.Errorf("runs %v, want v1 then v2, once each: a configure task is never also run as a migration", got)
	}
	if !slices.Equal(h.replicaIDs("idp"), idpBefore) {
		t.Errorf("idp was replaced, but only its configure task changed")
	}

	// Something else changes: it doesn't run.
	h.deployWith("c3", strings.Replace(configSpec, "MODE: prod", "MODE: staging", 1), map[string]string{"idp-config": "v2"}, configV1)
	h.until("c3 finishes", 6*time.Minute, func() bool { return h.rollout("c3") == RolloutDone })
	if got := h.runImages("idp-config"); len(got) != 2 {
		t.Errorf("runs %v: the api changed, nothing idp-config depends on", got)
	}

	// The task's files are new: it runs.
	h.deployWith("c4", strings.Replace(configSpec, "MODE: prod", "MODE: staging", 1), map[string]string{"idp-config": "v2"}, with(configV1, "idp-config", "realm-2"))
	h.until("c4 finishes", 6*time.Minute, func() bool { return h.rollout("c4") == RolloutDone })
	if got := h.runImages("idp-config"); len(got) != 3 {
		t.Errorf("runs %v: its config changed", got)
	}

	// idp itself is new: it runs again once idp's new replicas are all that run.
	h.deployWith("c5", strings.Replace(configSpec, "MODE: prod", "MODE: staging", 1), map[string]string{"idp-config": "v2", "idp": "v2"}, with(configV1, "idp-config", "realm-2"))
	settledAtRun := false
	h.until("c5 finishes", 6*time.Minute, func() bool {
		if len(h.taskRuns["idp-config"]) == 4 && !settledAtRun {
			settledAtRun = true
			for _, r := range h.replicas("idp") {
				if r.Release != "c5" || r.State != api.StateReady {
					t.Errorf("idp-config ran while idp replica %s (release %s) was %s: it waits for idp to roll out", r.ID, r.Release, r.State)
				}
			}
		}
		return h.rollout("c5") == RolloutDone
	})
	if got := h.runImages("idp-config"); len(got) != 4 {
		t.Errorf("runs %v: idp changed", got)
	}
}

func TestAFailedConfigureRollsBackAndAppliesTheLastGoodAgain(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	h.failingImages["idp-config:v2"] = true
	h.deployWith("c2", configSpec, map[string]string{"idp-config": "v2"}, configV1)
	h.until("the release fails", 6*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if reason := h.rolloutReason("c2"); !strings.Contains(reason, "idp-config") {
		t.Errorf("reason %q: it names the configure task", reason)
	}
	h.until("the last good configure task runs again", 3*time.Minute, func() bool { return h.configureStatus("idp").State == "done" })
	if got := h.runImages("idp-config"); !slices.Equal(got, []string{"v1", "v2", "v1"}) {
		t.Errorf("runs %v, want v1, v2, then v1 again: what's configured matches what runs", got)
	}
}

func TestAReleaseThatFailsElsewhereAppliesTheLastGoodConfigurationAgain(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	h.failOnStart["web"] = true // New web replicas fail; the running ones carry on.
	h.deployWith("c2", configSpec, map[string]string{"idp-config": "v2", "web": "v2"}, configV1)
	h.until("the release fails", 8*time.Minute, func() bool { return h.rollout("c2") == RolloutFailed })
	if reason := h.rolloutReason("c2"); !strings.Contains(reason, "web") {
		t.Errorf("reason %q", reason)
	}
	h.until("the last good configure task runs again", 3*time.Minute, func() bool {
		return h.configureStatus("idp").State == "done" && len(h.taskRuns["idp-config"]) == 3
	})
	if got := h.runImages("idp-config"); !slices.Equal(got, []string{"v1", "v2", "v1"}) {
		t.Errorf("runs %v, want v1, v2, then v1 again", got)
	}
}

func TestConfigureFollowsItsTasksKeys(t *testing.T) {
	h := newHarness(t)
	h.writeSecret(map[string]string{"IdpConfig_ADMIN_SECRET": "admin-1"})
	h.run(time.Minute)
	h.settleConfig()
	h.run(time.Minute)
	runs := len(h.taskRuns["idp-config"])
	idpBefore := h.replicaIDs("idp")

	// Another component's key changes: nothing runs.
	h.writeSecret(map[string]string{"Api_StripeKey": "sk_live_two"})
	h.run(4 * time.Minute)
	if got := len(h.taskRuns["idp-config"]); got != runs {
		t.Errorf("idp-config ran %d more times for another component's key", got-runs)
	}

	// Its own key changes: it runs again with the new version, once the secret settles.
	v3 := h.writeSecret(map[string]string{"IdpConfig_ADMIN_SECRET": "admin-2"})
	h.until("idp-config runs with the new key", 5*time.Minute, func() bool {
		n := len(h.taskRuns["idp-config"])
		return n > runs && h.taskRuns["idp-config"][n-1].spec.SecretVersion == v3 && h.configureStatus("idp").State == "done"
	})
	if got := len(h.taskRuns["idp-config"]); got != runs+1 {
		t.Errorf("idp-config ran %d times for one change of its keys", got-runs)
	}
	if !slices.Equal(h.replicaIDs("idp"), idpBefore) {
		t.Errorf("idp was replaced: only its configure task's keys changed")
	}

	// A run that fails for a new key is retried, a little slower each time, and nothing goes
	// back: a key can't be undone.
	h.taskFails["idp-config"] = true
	runs = len(h.taskRuns["idp-config"])
	h.writeSecret(map[string]string{"IdpConfig_ADMIN_SECRET": "admin-3"})
	h.until("the failure shows", 5*time.Minute, func() bool { return h.configureStatus("idp").State == "failed" })
	if st := h.configureStatus("idp"); st.Failed == "" || strings.Contains(st.Failed, "admin-3") {
		t.Errorf("status %+v: it says why, never with a value", st)
	}
	h.until("it tries again", 5*time.Minute, func() bool { return len(h.taskRuns["idp-config"]) >= runs+2 })
	if h.s.Status().Target != "c1" || h.rollout("c1") != RolloutDone {
		t.Errorf("a key that doesn't work rolled the release back")
	}
	h.taskFails["idp-config"] = false
	h.until("it applies once it can", 15*time.Minute, func() bool { return h.configureStatus("idp").State == "done" })
}

func TestADependentWaitsForItsDependencysConfigureToTakeNewKeys(t *testing.T) {
	h := newHarness(t)
	h.writeSecret(map[string]string{"IdpConfig_ADMIN_SECRET": "admin-1", "Shared_CLIENT_SECRET": "client-1"})
	h.run(time.Minute)
	spec := strings.Replace(configSpec, "    secrets: [ADMIN_SECRET]\n", "    secrets: [ADMIN_SECRET, CLIENT_SECRET]\n", 1)
	spec = strings.Replace(spec, "    depends_on: [api, idp]\n", "    depends_on: [api, idp]\n    secrets: [CLIENT_SECRET]\n", 1)
	h.deployWith("c1", spec, nil, configV1)
	h.until("the first release finishes", 10*time.Minute, func() bool { return h.rollout("c1") == RolloutDone })
	h.run(time.Minute)
	webBefore := h.replicaIDs("web")
	runs := len(h.taskRuns["idp-config"])

	// One write changes the client secret for idp-config, which sets it in idp, and for web,
	// which uses it: web waits until idp-config has run.
	h.writeSecret(map[string]string{"Shared_CLIENT_SECRET": "client-2"})
	seenNew, configuredFirst := false, false
	h.until("web has the new key", 8*time.Minute, func() bool {
		for _, id := range h.replicaIDs("web") {
			if !seenNew && !slices.Contains(webBefore, id) {
				seenNew = true
				configuredFirst = len(h.taskRuns["idp-config"]) > runs && h.configureStatus("idp").State == "done"
			}
		}
		return seenNew && !overlap(h.replicaIDs("web"), webBefore)
	})
	if !configuredFirst {
		t.Errorf("web took the new key before idp-config had run with it")
	}
}

func TestOnlyOneConfigureRunAtATime(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	h.taskSteps["idp-config"] = 20 // A run takes over a minute and a half.
	h.deployWith("c2", configSpec, map[string]string{"idp-config": "v2"}, configV1)
	h.until("idp-config v2 runs", 3*time.Minute, func() bool { return h.liveTasks("idp-config") > 0 })
	h.start() // The scheduler restarts mid-run.
	h.until("idp is configured", 10*time.Minute, func() bool {
		if n := h.liveTasks("idp-config"); n > 1 {
			t.Fatalf("%d idp-config runs at once: a configure task never runs beside itself", n)
		}
		return h.s.Status().Target == "c2" && h.configureStatus("idp").State == "done" && h.liveTasks("idp-config") == 0
	})
}

func TestConfigureStateSurvivesARestart(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	runs := len(h.taskRuns["idp-config"])
	h.start()
	h.run(3 * time.Minute)
	if got := len(h.taskRuns["idp-config"]); got != runs {
		t.Errorf("idp-config ran %d more times after a restart: what was applied is kept in state.json", got-runs)
	}
	if st := h.configureStatus("idp"); st.State != "done" {
		t.Errorf("status after a restart %+v", st)
	}
}

func TestAgentsFetchConfigFiles(t *testing.T) {
	h := newHarness(t)
	h.settleConfig()
	srv, _ := server(h)
	a := h.vms("general")[0]
	doc := identityDoc(a.id, a.ip, testAccount, testRegion)
	digest := digestOf("api-1")

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/config/"+digest, "", doc, "s", a.ip+":5000"))
	var files configfiles.Files
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &files) != nil || string(files["conf"]) != "api-1" {
		t.Errorf("config: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/config/"+digestOf("nothing"), "", doc, "s", a.ip+":5000"))
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown digest: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/config/../state", "", doc, "s", a.ip+":5000"))
	if w.Code != http.StatusNotFound && w.Code != http.StatusBadRequest {
		t.Errorf("a path that isn't a digest: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/config/"+digest, "", "", "", a.ip+":5000"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("config without identity: %d", w.Code)
	}
}
