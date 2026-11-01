package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testSpec = `app: shop
protostrap: 1.4.0
pools:
  ingress: { vm: c8g.medium }
  general: { vm: [m7g.xlarge], max: 4 }
  data: { vm: m7g.xlarge }
tunnel: {}
gateway:
  upstreams:
    api:
      routes: [/api/]
telemetry: {}
queues:
  orders: {}
components:
  api:
    kind: service
    build: services/api
    sources: [services/shared]
    pool: general
    request: { cpu: 500m, memory: 512Mi }
    port: 8080
    ready: grpc
    uses: [orders]
  worker:
    kind: worker
    build: services/worker
    context: services
    pool: general
    request: { cpu: 500m, memory: 512Mi }
    scale: { max: 3, demand: [{ queue: orders, rate: 1/s, drain: 1m }] }
  postgres:
    kind: stateful
    image: postgres:17.2
    vm: data-1
    request: { cpu: 1000m, memory: 2Gi }
    port: 5432
    volume: { path: /var/lib/postgresql/data, size: 10Gi }
  cache:
    kind: stateful
    image: docker.io/library/redis@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    vm: data-1
    request: { cpu: 500m, memory: 1Gi }
    port: 6379
    volume: { path: /data, size: 5Gi }
`

// repo makes a git repository with the app in app/, so paths are relative to the spec's
// directory rather than the repository's root.
func repo(t *testing.T) string {
	t.Helper()
	return repoWith(t, testSpec)
}

// repoWith is a repository whose app/protostrap.yaml is spec.
func repoWith(t *testing.T, spec string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git isn't installed")
	}
	root := t.TempDir()
	files := map[string]string{
		"app/protostrap.yaml":                spec,
		"app/services/api/Dockerfile":        "FROM scratch\n",
		"app/services/api/main.go":           "package main\n",
		"app/services/shared/lib.go":         "package shared\n",
		"app/services/worker/Dockerfile":     "FROM scratch\n",
		"app/services/worker/main.go":        "package main\n",
		"app/services/worker/bin/ignored.go": "untracked\n",
	}
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	git(t, root, "init", "-q")
	git(t, root, "add", "app/protostrap.yaml", "app/services/api", "app/services/shared", "app/services/worker/Dockerfile", "app/services/worker/main.go")
	git(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-qm", "init")
	return root
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestPlan(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	p, err := MakePlan(specPath, "123.dkr.ecr.us-east-1.amazonaws.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.App != "shop" || p.ProtoStrap != "1.4.0" || p.Version != "1.4.0" || p.InfraHash == "" {
		t.Errorf("plan header: %+v", p)
	}
	if len(p.Builds) != 2 {
		t.Fatalf("builds: %+v", p.Builds)
	}
	api := p.Builds[0]
	if api.Component != "api" || !strings.HasPrefix(api.Tag, "c-") || api.Image != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/api:"+api.Tag {
		t.Errorf("api build: %+v", api)
	}
	appDir := filepath.Join(root, "app")
	if api.Context != filepath.Join(appDir, "services/api") || api.Dockerfile != filepath.Join(appDir, "services/api/Dockerfile") {
		t.Errorf("api paths are relative to the spec: %+v", api)
	}
	worker := p.Builds[1]
	if worker.Context != filepath.Join(appDir, "services") || worker.Dockerfile != filepath.Join(appDir, "services/worker/Dockerfile") {
		t.Errorf("worker builds from its context with its own Dockerfile: %+v", worker)
	}
	// A Dockerfile pins its own toolchain: there's no global.json, and none is needed.
	if api.GlobalJSON != "" || worker.GlobalJSON != "" {
		t.Errorf("a Dockerfile build has no global.json: %+v, %+v", api, worker)
	}
	if len(p.Stateful) != 2 {
		t.Fatalf("stateful: %+v", p.Stateful)
	}
	cache, pg := p.Stateful[0], p.Stateful[1]
	if pg.Upstream != "postgres:17.2" || pg.Image != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/postgres:17.2" {
		t.Errorf("postgres: %+v", pg)
	}
	if !strings.HasSuffix(cache.Image, "/protostrap/cache:sha256-0123456789abcdef0123456789abcdef") {
		t.Errorf("a digest becomes a tag ECR accepts: %+v", cache)
	}
	if p.Edge != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/edge:1.4.0" {
		t.Errorf("edge %q", p.Edge)
	}

	// A change to a source moves only the tags of the components built from it.
	os.WriteFile(filepath.Join(appDir, "services/shared/lib.go"), []byte("package shared // changed\n"), 0o644)
	git(t, root, "add", "-A", "app/services/shared")
	p2, err := MakePlan(specPath, "123.dkr.ecr.us-east-1.amazonaws.com", "")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Builds[0].Tag == api.Tag {
		t.Errorf("a change to services/shared didn't move the api's tag")
	}
	if p2.Builds[1].Tag != worker.Tag {
		t.Errorf("a change to services/shared moved the worker's tag")
	}

	// Untracked files never move a tag.
	os.WriteFile(filepath.Join(appDir, "services/worker/scratch.txt"), []byte("x"), 0o644)
	p3, _ := MakePlan(specPath, "r", "")
	if p3.Builds[1].Tag != worker.Tag {
		t.Errorf("an untracked file moved the worker's tag")
	}

	if p4, _ := MakePlan(specPath, "r", "1.4.0-dev.abc"); p4.Version != "1.4.0-dev.abc" || !strings.HasSuffix(p4.Edge, ":1.4.0-dev.abc") {
		t.Errorf("a version override: %+v", p4)
	}
}

// pinned is a global.json that pins the .NET SDK, as ProtoApp's does.
const pinned = `{ "sdk": { "version": "10.0.100", "rollForward": "latestPatch" } }` + "\n"

// write writes files under root.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// asProject makes the api a .NET project, app/services/api/Api.csproj, and commits it. It
// returns the spec's path.
func asProject(t *testing.T, root string) string {
	t.Helper()
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	src, _ := os.ReadFile(specPath)
	write(t, root, map[string]string{
		"app/protostrap.yaml":         strings.Replace(string(src), "    build: services/api\n", "    project: services/api/Api.csproj\n", 1),
		"app/services/api/Api.csproj": "<Project Sdk=\"Microsoft.NET.Sdk.Web\" />\n",
	})
	git(t, root, "add", "app/protostrap.yaml", "app/services/api/Api.csproj")
	return specPath
}

func TestPlanBuildsAProject(t *testing.T) {
	root := repo(t)
	appDir := filepath.Join(root, "app")
	specPath := asProject(t, root)
	write(t, root, map[string]string{"app/global.json": pinned})
	git(t, root, "add", "app/global.json")

	p, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	api := p.Builds[0]
	if api.Project != filepath.Join(appDir, "services/api/Api.csproj") || api.Dockerfile != "" || api.Context != "" {
		t.Errorf("a project builds with the .NET SDK, from its project file: %+v", api)
	}
	// The workflow installs the SDK the project's global.json pins: ProtoStrap never picks one.
	if api.GlobalJSON != filepath.Join(appDir, "global.json") {
		t.Errorf("global.json %q, want the one that pins the project's SDK", api.GlobalJSON)
	}
	// The SDK is part of how the image is built, so its global.json is among the tag's
	// sources, though the spec doesn't list it.
	if strings.Join(api.Sources, ",") != "global.json,services/api,services/shared" {
		t.Errorf("sources %v", api.Sources)
	}

	// The tag moves with the project's directory and its sources, as a Dockerfile build's does.
	write(t, root, map[string]string{"app/services/shared/lib.go": "package shared // changed\n"})
	git(t, root, "add", "-A", "app/services/shared")
	p2, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Builds[0].Tag == api.Tag {
		t.Errorf("a change to a source didn't move the project's tag")
	}
}

func TestPlanRefusesAnUntrackedProject(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	src, _ := os.ReadFile(specPath)
	os.WriteFile(specPath, []byte(strings.Replace(string(src), "    build: services/api\n", "    project: services/api/Missing.csproj\n", 1)), 0o644)
	if _, err := MakePlan(specPath, "r", ""); err == nil || !strings.Contains(err.Error(), "Missing.csproj") {
		t.Errorf("err %v, want it to name the project that isn't tracked", err)
	}
}

// A project without a global.json would build with whatever SDK the workflow chose, which
// ProtoStrap won't do: a new .NET would need a new ProtoStrap, and a tag wouldn't say which
// SDK built its image.
func TestPlanRefusesAProjectWithoutAGlobalJSON(t *testing.T) {
	root := repo(t)
	specPath := asProject(t, root)

	_, err := MakePlan(specPath, "r", "")
	want := "api: no global.json at or above services/api pins the .NET SDK it builds with: add one that sets sdk.version"
	if err == nil || err.Error() != want {
		t.Errorf("err %v, want %q", err, want)
	}
}

// The workflow only has what is inside the spec's directory in the tag, so a global.json
// above it, even a tracked one the SDK would find, doesn't count.
func TestPlanLooksForAGlobalJSONOnlyInsideTheSpecsDirectory(t *testing.T) {
	root := repo(t)
	specPath := asProject(t, root)
	write(t, root, map[string]string{"global.json": pinned})
	git(t, root, "add", "global.json")

	if _, err := MakePlan(specPath, "r", ""); err == nil || !strings.Contains(err.Error(), "no global.json at or above services/api") {
		t.Errorf("err %v, want the global.json above the spec's directory left out", err)
	}
}

// The SDK reads the nearest global.json above the directory it runs in, and the workflow
// publishes from the project's directory, so plan finds the same one.
func TestPlanUsesTheNearestGlobalJSON(t *testing.T) {
	root := repo(t)
	appDir := filepath.Join(root, "app")
	specPath := asProject(t, root)
	write(t, root, map[string]string{
		"app/global.json":          `{ "sdk": { "version": "9.0.100" } }`,
		"app/services/global.json": pinned,
	})
	git(t, root, "add", "app/global.json", "app/services/global.json")

	p, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	api := p.Builds[0]
	if api.GlobalJSON != filepath.Join(appDir, "services/global.json") {
		t.Errorf("global.json %q, want services/global.json, the nearest", api.GlobalJSON)
	}
	if strings.Join(api.Sources, ",") != "services/api,services/global.json,services/shared" {
		t.Errorf("sources %v, want the nearest global.json only", api.Sources)
	}
}

// The workflow checks out what is committed, so a global.json only on this machine would
// leave CI building with a different SDK, or none.
func TestPlanRefusesAnUntrackedGlobalJSON(t *testing.T) {
	root := repo(t)
	specPath := asProject(t, root)
	write(t, root, map[string]string{"app/global.json": pinned})

	_, err := MakePlan(specPath, "r", "")
	want := "api: global.json pins the .NET SDK it builds with, but isn't tracked in git"
	if err == nil || err.Error() != want {
		t.Errorf("err %v, want %q", err, want)
	}
}

// A global.json can configure other things, such as the test runner, without pinning the
// SDK, which leaves the SDK to whatever the machine has.
func TestPlanRefusesAGlobalJSONThatDoesNotPinTheSDK(t *testing.T) {
	for name, content := range map[string]string{
		"test runner only":    `{ "test": { "runner": "Microsoft.Testing.Platform" } }`,
		"roll forward only":   `{ "sdk": { "rollForward": "latestMajor" } }`,
		"an empty version":    `{ "sdk": { "version": "" } }`,
		"a commented-out pin": "{\n  // \"sdk\": { \"version\": \"10.0.100\" }\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := repo(t)
			specPath := asProject(t, root)
			write(t, root, map[string]string{"app/global.json": content})
			git(t, root, "add", "app/global.json")

			_, err := MakePlan(specPath, "r", "")
			want := "api: global.json doesn't pin the .NET SDK it builds with: set sdk.version"
			if err == nil || err.Error() != want {
				t.Errorf("err %v, want %q", err, want)
			}
		})
	}
}

// The SDK reads comments and a byte order mark in global.json, so plan does too. It doesn't
// read a trailing comma, so plan doesn't either.
func TestPlanReadsGlobalJSONAsTheSDKDoes(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		err     string
	}{
		"comments":            {content: "{\n  // The SDK every project builds with.\n  \"sdk\": { /* pinned */ \"version\": \"10.0.100\" }\n}\n"},
		"a byte order mark":   {content: "\ufeff" + pinned},
		"slashes in a string": {content: `{ "sdk": { "version": "10.0.100" }, "msbuild-sdks": { "My.Sdk": "1.0.0//*" } }`},
		"a trailing comma":    {content: `{ "sdk": { "version": "10.0.100", } }`, err: "api: global.json isn't JSON the .NET SDK can read"},
	} {
		t.Run(name, func(t *testing.T) {
			root := repo(t)
			specPath := asProject(t, root)
			write(t, root, map[string]string{"app/global.json": tc.content})
			git(t, root, "add", "app/global.json")

			_, err := MakePlan(specPath, "r", "")
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("err %v, want the global.json read", err)
			case tc.err != "" && (err == nil || !strings.HasPrefix(err.Error(), tc.err)):
				t.Errorf("err %v, want %q", err, tc.err)
			}
		})
	}
}

// A new SDK is a new image: bumping global.json moves the tag of every project it pins,
// and of nothing else.
func TestPlanMovesAProjectsTagWithItsSDK(t *testing.T) {
	root := repo(t)
	specPath := asProject(t, root)
	write(t, root, map[string]string{"app/global.json": pinned})
	git(t, root, "add", "app/global.json")
	before, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}

	write(t, root, map[string]string{"app/global.json": `{ "sdk": { "version": "11.0.100" } }`})
	git(t, root, "add", "app/global.json")
	after, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	if after.Builds[0].Tag == before.Builds[0].Tag {
		t.Errorf("a new SDK in global.json didn't move the project's tag")
	}
	if after.Builds[1].Tag != before.Builds[1].Tag {
		t.Errorf("a new SDK in global.json moved the worker's tag, which a Dockerfile builds")
	}

	// A spec that lists global.json itself, as ProtoStrap.Hosting writes it, gets the same
	// tag, and lists it once.
	src, _ := os.ReadFile(specPath)
	write(t, root, map[string]string{"app/protostrap.yaml": strings.Replace(string(src), "sources: [services/shared]", "sources: [services/shared, global.json]", 1)})
	listed, err := MakePlan(specPath, "r", "")
	if err != nil {
		t.Fatal(err)
	}
	if listed.Builds[0].Tag != after.Builds[0].Tag {
		t.Errorf("listing global.json in sources moved the tag")
	}
	if strings.Join(listed.Builds[0].Sources, ",") != "global.json,services/api,services/shared" {
		t.Errorf("sources %v, want global.json once", listed.Builds[0].Sources)
	}
}

func TestPlanRefusesABuildWithNoTrackedFiles(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	src, _ := os.ReadFile(specPath)
	os.WriteFile(specPath, []byte(strings.Replace(string(src), "build: services/api", "build: services/nothing", 1)), 0o644)
	if _, err := MakePlan(specPath, "r", ""); err == nil || !strings.Contains(err.Error(), "services/nothing") {
		t.Errorf("err %v, want it to name the empty build directory", err)
	}
}

func TestBuildAndReadARelease(t *testing.T) {
	root := repo(t)
	specPath := filepath.Join(root, "app", "protostrap.yaml")
	r, err := Make(specPath, "9f2c1e7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "123.dkr.ecr.us-east-1.amazonaws.com", "1.4.0-dev.x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Commit != "9f2c1e7aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" || r.App != "shop" || r.Version != "1.4.0-dev.x" || r.Spec != testSpec {
		t.Errorf("release: %+v", r)
	}
	if len(r.Components) != 4 {
		t.Fatalf("components: %+v", r.Components)
	}
	for name, c := range r.Components {
		if c.Image == "" || len(c.Fingerprint) != 16 {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if r.Edge.Image != "123.dkr.ecr.us-east-1.amazonaws.com/protostrap/edge:1.4.0-dev.x" {
		t.Errorf("edge %+v", r.Edge)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	s, err := back.ParseSpec()
	if err != nil || s.App != "shop" {
		t.Errorf("the release's spec: %v %v", s, err)
	}
	if back.Components["api"].Fingerprint != r.Components["api"].Fingerprint {
		t.Errorf("fingerprints don't survive the round trip")
	}
}

func TestParseRejectsBrokenReleases(t *testing.T) {
	for _, bad := range []string{
		`{}`,
		// A spec with a gateway needs the edge's image.
		`{"commit":"abc1234","app":"shop","protostrap":"1.0.0","spec":"app: shop\ngateway: {}","components":{}}`,
		`{"commit":"not hex","app":"shop","protostrap":"1.0.0","spec":"x","components":{}}`,
		`{"commit":"abc1234","app":"shop","protostrap":"one","spec":"x","components":{}}`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestStateRoundTrip(t *testing.T) {
	s := State{Target: "abc1234", LastGood: "abc1233", Desired: map[string]int{"api": 5}}
	b, _ := json.Marshal(s)
	var back State
	if err := json.Unmarshal(b, &back); err != nil || back.Desired["api"] != 5 || back.Target != "abc1234" {
		t.Errorf("state: %+v %v", back, err)
	}
}
