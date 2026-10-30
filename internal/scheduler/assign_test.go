package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These tests check what an agent gets back on every sync (docs/protostrap.md#sync).

func specOf(h *harness, component string) (api.ReplicaSpec, bool) {
	for _, a := range h.agents {
		for _, s := range a.assignment.Replicas {
			if s.Component == component && s.Kind != api.KindTask {
				return s, true
			}
		}
	}
	return api.ReplicaSpec{}, false
}

func TestAServiceReplicasSpec(t *testing.T) {
	h := newHarness(t)
	h.settle()
	s, ok := specOf(h, "api")
	if !ok {
		t.Fatal("no api replica assigned")
	}
	if s.Image != testRegistry+"/protostrap/api:v1" || s.Release != "c1" || len(s.Fingerprint) != 16 {
		t.Errorf("image %q, release %q, fingerprint %q", s.Image, s.Release, s.Fingerprint)
	}
	if s.Request != (api.Resources{CPU: 500, Memory: 512}) || s.Port != 8080 || s.Ready != "grpc" || s.Stop != 30 {
		t.Errorf("request %+v, port %d, ready %q, stop %d", s.Request, s.Port, s.Ready, s.Stop)
	}
	if s.Role != "arn:aws:iam::123456789012:role/protostrap/protostrap-c-api" {
		t.Errorf("role %q", s.Role)
	}
	want := map[string]string{
		"MODE":           "prod",
		"QUEUE_JOBS_URL": "https://sqs.us-east-1.amazonaws.com/123456789012/protostrap-jobs",
		"FILES_BUCKET":   "protostrap-123456789012-us-east-1-files",
		"BUCKET_REPORTS": "protostrap-123456789012-us-east-1-reports",
	}
	for k, v := range want {
		if s.Env[k] != v {
			t.Errorf("env %s = %q, want %q", k, s.Env[k], v)
		}
	}
	if !strings.HasPrefix(s.ID, "api-") {
		t.Errorf("id %q", s.ID)
	}
}

func TestAStatefulReplicasSpec(t *testing.T) {
	h := newHarness(t)
	h.settle()
	s, _ := specOf(h, "postgres")
	if s.Kind != api.KindStateful || s.Volume != "/var/lib/postgresql/data" || strings.Join(s.Backup, " ") != "pg_dumpall -U postgres" {
		t.Errorf("postgres %+v", s)
	}
	if s.Image != testRegistry+"/protostrap/postgres:v1" {
		t.Errorf("postgres image %q: VMs never pull from Docker Hub", s.Image)
	}
}

func TestTheEdgesSpecAndRoutes(t *testing.T) {
	h := newHarness(t)
	h.settle()
	ingress := h.vms("ingress")[0]
	var edge api.ReplicaSpec
	for _, s := range ingress.assignment.Replicas {
		if s.Kind == api.KindEdge {
			edge = s
		}
	}
	if edge.Component != "edge" || edge.Image != testRegistry+"/protostrap/edge:1.4.0" || edge.Request != (api.Resources{CPU: 500, Memory: 256}) {
		t.Errorf("edge %+v", edge)
	}
	if edge.Role != "arn:aws:iam::123456789012:role/protostrap/protostrap-c-edge" {
		t.Errorf("edge role %q", edge.Role)
	}
	if len(ingress.assignment.Files) == 0 {
		t.Fatal("the ingress VM got no routes")
	}
	all := ""
	for k, v := range ingress.assignment.Files {
		if !strings.HasPrefix(k, "edge/") {
			t.Errorf("file %q is outside edge/", k)
		}
		all += v
	}
	for _, want := range []string{"/api/", "/web/", "api", "web"} {
		if !strings.Contains(all, want) {
			t.Errorf("the routes don't mention %q", want)
		}
	}
	for _, a := range h.vms("general") {
		if len(a.assignment.Files) != 0 {
			t.Errorf("general VM %s got edge files", a.id)
		}
	}
}

func TestATaskIsLeftOutOnceItHasFinished(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, a := range h.agents {
		for _, s := range a.assignment.Replicas {
			if s.Kind == api.KindTask {
				t.Errorf("finished task %s still assigned to %s", s.ID, a.id)
			}
		}
	}
	if h.ids["migrate"] != 1 {
		t.Errorf("the migration ran %d times, want once", h.ids["migrate"])
	}
}

func TestAssignmentsNameTheReleasesVersion(t *testing.T) {
	h := newHarness(t)
	h.settle()
	for _, a := range h.agents {
		if a.assignment.Version != "1.4.0" {
			t.Errorf("vm %s told version %q", a.id, a.assignment.Version)
		}
	}
}

func TestBeforeAnyReleaseVMsGetEmptyAssignments(t *testing.T) {
	h := newHarness(t)
	h.run(5 * time.Minute)
	for _, a := range h.agents {
		if len(a.assignment.Replicas) != 0 {
			t.Errorf("vm %s assigned %d replicas before any release", a.id, len(a.assignment.Replicas))
		}
		if a.lastErr != nil {
			t.Errorf("vm %s: %v", a.id, a.lastErr)
		}
		if a.assignment.Version != "1.4.0" {
			t.Errorf("vm %s told version %q, want the scheduler's own", a.id, a.assignment.Version)
		}
	}
}

func TestJoinGivesTheSwarm(t *testing.T) {
	h := newHarness(t)
	j, err := h.s.Join(context.Background())
	if err != nil || j.Token == "" || j.Manager != "10.20.1.10:2377" {
		t.Errorf("join %+v, %v", j, err)
	}
}

func TestAnUnknownVMIsAddedToItsPoolOnItsFirstSync(t *testing.T) {
	h := newHarness(t)
	h.settle()
	n := len(h.s.Status().VMs)
	h.groups.group("protostrap-general-us-east-1a").Desired++
	h.until("the new VM shows in the status", 3*time.Minute, func() bool { return len(h.s.Status().VMs) == n+1 })
}

// A service's protocol reaches the edge and its replicas: a grpc service speaks cleartext
// HTTP/2, so its replicas' HTTP checks would too, and the edge translates gRPC-Web for it
// alone. A command check reaches the agent as it is.
func TestProtocolAndChecksReachTheEdgeAndReplicas(t *testing.T) {
	h := newHarness(t)
	h.settle()
	if s, _ := specOf(h, "api"); !s.H2C {
		t.Errorf("api, ready: grpc, should speak h2c: %+v", s)
	}
	if s, _ := specOf(h, "web"); s.H2C {
		t.Errorf("web, an HTTP/1.1 service, gets h2c")
	}

	next := strings.Replace(testSpec, "    port: 5432\n", "    port: 5432\n    ready: [pg_isready, -U, postgres]\n", 1)
	next = strings.Replace(next, "    ready: /healthz\n", "    ready: /healthz\n    protocol: http2\n", 1)
	h.deploy("c2", next, nil)
	h.until("the second release finishes", 10*time.Minute, func() bool { return h.rollout("c2") == RolloutDone })
	if s, _ := specOf(h, "postgres"); strings.Join(s.ReadyCommand, " ") != "pg_isready -U postgres" || s.Ready != "" {
		t.Errorf("postgres ready %q, command %q", s.Ready, s.ReadyCommand)
	}
	// A new protocol keeps web's replicas, which are ready already: only the edge changes.
	routes := ""
	for _, v := range h.vms("ingress")[0].assignment.Files {
		routes += v
	}
	if !strings.Contains(routes, `"name": "web"`) || strings.Count(routes, `"http2_protocol_options": {}`) != 2 {
		t.Errorf("the edge should speak HTTP/2 to api and web:\n%s", routes)
	}
}
