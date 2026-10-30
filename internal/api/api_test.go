package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The report and assignment below are the examples in docs/protostrap.md#sync. The wire
// format must keep reading them.

const docReport = `{
  "vm": "i-0a1b2c3d", "name": "general-3", "pool": "general", "version": "1.4.0",
  "capacity": { "cpu": 4000, "memory": 16384 },
  "replicas": [
    { "id": "api-k3f9", "state": "ready", "cpu": 310, "memory": 402, "restarts": 0,
      "metrics": { "llm_budget_cap": -1 }, "secret_version": "6b1e2f4a-0d3c-4e5f-8a9b-1c2d3e4f5a6b" }
  ]
}`

const docAssignment = `{
  "version": "1.4.0",
  "replicas": [
    { "id": "api-k3f9", "component": "api", "release": "9f2c1e7",
      "image": "<account>.dkr.ecr.<region>.amazonaws.com/protostrap/api:c-4e1a90b2",
      "request": { "cpu": 500, "memory": 512 }, "port": 8080, "ready": "grpc",
      "stop": 30, "env": { "PROTOSTRAP_COMPONENT": "api" },
      "metrics": { "endpoint": "9090/metrics", "names": ["llm_budget_cap"] },
      "secret_prefix": "Api_", "secret_version": "6b1e2f4a-0d3c-4e5f-8a9b-1c2d3e4f5a6b" }
  ],
  "files": { "edge/routes.yaml": "<routes>" }
}`

func TestReportReadsTheDocumentedExample(t *testing.T) {
	var r Report
	if err := json.Unmarshal([]byte(docReport), &r); err != nil {
		t.Fatal(err)
	}
	cpu, mem := int64(310), int64(402)
	want := Report{
		VM: "i-0a1b2c3d", Name: "general-3", Pool: "general", Version: "1.4.0",
		Capacity: Resources{CPU: 4000, Memory: 16384},
		Replicas: []ReplicaReport{{
			ID: "api-k3f9", State: StateReady, CPU: &cpu, Memory: &mem, Restarts: 0,
			Metrics:       map[string]float64{"llm_budget_cap": -1},
			SecretVersion: "6b1e2f4a-0d3c-4e5f-8a9b-1c2d3e4f5a6b",
		}},
	}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("got %+v\nwant %+v", r, want)
	}
}

func TestAssignmentReadsTheDocumentedExample(t *testing.T) {
	var a Assignment
	if err := json.Unmarshal([]byte(docAssignment), &a); err != nil {
		t.Fatal(err)
	}
	want := Assignment{
		Version: "1.4.0",
		Replicas: []ReplicaSpec{{
			ID: "api-k3f9", Component: "api", Release: "9f2c1e7",
			Image:   "<account>.dkr.ecr.<region>.amazonaws.com/protostrap/api:c-4e1a90b2",
			Request: Resources{CPU: 500, Memory: 512}, Port: 8080, Ready: "grpc",
			Stop: 30, Env: map[string]string{"PROTOSTRAP_COMPONENT": "api"},
			Metrics:      &MetricsSpec{Endpoint: "9090/metrics", Names: []string{"llm_budget_cap"}},
			SecretPrefix: "Api_", SecretVersion: "6b1e2f4a-0d3c-4e5f-8a9b-1c2d3e4f5a6b",
		}},
		Files: map[string]string{"edge/routes.yaml": "<routes>"},
	}
	if !reflect.DeepEqual(a, want) {
		t.Fatalf("got %+v\nwant %+v", a, want)
	}
}

func TestReportRoundTrip(t *testing.T) {
	cpu, mem, code := int64(1), int64(2), 3
	in := Report{
		VM: "i-1", Name: "compute-1", Pool: "compute", Zone: "eu-west-1a", Version: "1.4.0",
		Capacity: Resources{CPU: 4000, Memory: 8192},
		Replicas: []ReplicaReport{
			{ID: "w-1", Component: "orders-worker", Release: "abc", Kind: KindWorker,
				Fingerprint: "f1", State: StateStopping, CPU: &cpu, Memory: &mem, Restarts: 2},
			{ID: "m-1", Component: "migrate", Kind: KindTask, State: StateFailed,
				ExitCode: &code, Reason: "exited with code 3"},
		},
		Terminating: &Termination{Reason: TerminationSpot, Deadline: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)},
		Repairs:     []string{"filter/PROTOSTRAP-FORWARD"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Report
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the report:\n%+v\n%+v", in, out)
	}
}

func TestReportOmitsUnknownUsage(t *testing.T) {
	// A replica's CPU is unknown until the agent has two samples. It must be left out, not
	// sent as 0, or it would drag down the component's average.
	b, err := json.Marshal(ReplicaReport{ID: "api-1", State: StateStarting})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"cpu"`, `"memory"`, `"exit_code"`, `"metrics"`, `"reason"`} {
		if strings.Contains(string(b), field) {
			t.Errorf("%s should be omitted: %s", field, b)
		}
	}
	if !strings.Contains(string(b), `"restarts":0`) {
		t.Errorf("restarts should always be sent: %s", b)
	}
}

func TestReportWithNoReplicasSendsAnEmptyList(t *testing.T) {
	b, err := json.Marshal(Report{VM: "i-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"replicas":[]`) {
		t.Fatalf("an empty VM must report an empty list, not null: %s", b)
	}
	if strings.Contains(string(b), `"terminating"`) {
		t.Fatalf("terminating should be omitted when the VM isn't terminating: %s", b)
	}
}

func TestTaskAndEdgeKinds(t *testing.T) {
	for _, k := range []Kind{KindService, KindWorker, KindStateful, KindTask, KindEdge} {
		if !k.Valid() {
			t.Errorf("%q should be valid", k)
		}
	}
	if Kind("cron").Valid() {
		t.Error("an unknown kind should be invalid")
	}
}

func TestJoinResponse(t *testing.T) {
	var j Join
	if err := json.Unmarshal([]byte(`{"swarm_id":"s1","token":"SWMTKN-1-x","manager":"10.20.1.10:2377"}`), &j); err != nil {
		t.Fatal(err)
	}
	if j != (Join{SwarmID: "s1", Token: "SWMTKN-1-x", Manager: "10.20.1.10:2377"}) {
		t.Fatalf("got %+v", j)
	}
}

func TestReplicaSpecValidate(t *testing.T) {
	ok := ReplicaSpec{ID: "api-1", Component: "api", Kind: KindService, Image: "r/api:c-1",
		Request: Resources{CPU: 500, Memory: 512}, Stop: 30}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]func(*ReplicaSpec){
		"no id":           func(s *ReplicaSpec) { s.ID = "" },
		"id with slash":   func(s *ReplicaSpec) { s.ID = "api/1" },
		"id with space":   func(s *ReplicaSpec) { s.ID = "api 1" },
		"no component":    func(s *ReplicaSpec) { s.Component = "" },
		"bad component":   func(s *ReplicaSpec) { s.Component = "Api!" },
		"no image":        func(s *ReplicaSpec) { s.Image = "" },
		"unknown kind":    func(s *ReplicaSpec) { s.Kind = "cron" },
		"no memory":       func(s *ReplicaSpec) { s.Request.Memory = 0 },
		"negative cpu":    func(s *ReplicaSpec) { s.Request.CPU = -1 },
		"negative stop":   func(s *ReplicaSpec) { s.Stop = -1 },
		"port too big":    func(s *ReplicaSpec) { s.Port = 70000 },
		"bad ready":       func(s *ReplicaSpec) { s.Ready = "healthz" },
		"stateful no vol": func(s *ReplicaSpec) { s.Kind = KindStateful },
		"relative volume": func(s *ReplicaSpec) { s.Kind = KindStateful; s.Volume = "data" },

		"lowercase secret prefix":     func(s *ReplicaSpec) { s.SecretPrefix = "api_" },
		"secret prefix with no _":     func(s *ReplicaSpec) { s.SecretPrefix = "Api" },
		"secret file outside the dir": func(s *ReplicaSpec) { s.SecretPrefix = "Api_"; s.SecretFiles = []string{"../x"} },
		"secret file with a slash":    func(s *ReplicaSpec) { s.SecretPrefix = "Api_"; s.SecretFiles = []string{"a/b"} },
		"secret file named ..data":    func(s *ReplicaSpec) { s.SecretPrefix = "Api_"; s.SecretFiles = []string{"..data"} },
		"secret files with no prefix": func(s *ReplicaSpec) { s.SecretFiles = []string{"DB"} },
		"bad secret version":          func(s *ReplicaSpec) { s.SecretVersion = "v/1" },
		"long secret version":         func(s *ReplicaSpec) { s.SecretVersion = strings.Repeat("a", 65) },
		"bad secret digest":           func(s *ReplicaSpec) { s.SecretDigest = "not hex" },
		"follows with no files":       func(s *ReplicaSpec) { s.SecretPrefix = "Api_"; s.SecretFollows = true },
		"hook that doesn't follow": func(s *ReplicaSpec) {
			s.SecretPrefix, s.SecretFiles, s.SecretHook = "Api_", []string{"DB"}, []string{"x"}
		},
		"task that follows": func(s *ReplicaSpec) {
			s.Kind, s.SecretPrefix, s.SecretFiles, s.SecretFollows = KindTask, "Api_", []string{"DB"}, true
		},
	}
	for name, mutate := range cases {
		s := ok
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestAReplicaWithSecretFilesValidates(t *testing.T) {
	pg := ReplicaSpec{ID: "postgres-1", Component: "postgres", Kind: KindStateful, Image: "postgres:18.3",
		Request: Resources{CPU: 1000, Memory: 2048}, Port: 5432, Stop: 60, Volume: "/var/lib/postgresql",
		SecretPrefix: "Postgres_", SecretFiles: []string{"POSTGRES_PASSWORD"}, SecretFollows: true,
		SecretHook: []string{"sh", "-c", "psql"}}
	if err := pg.Validate(); err != nil {
		t.Errorf("a follower with a hook: %v", err)
	}
	edge := ReplicaSpec{ID: "edge-1", Component: "edge", Kind: KindEdge, Image: "protostrap:1.4.0",
		Request: Resources{CPU: 500, Memory: 256}, Stop: 30, SecretPrefix: "Edge_", SecretVersion: "6b1e2f4a-0d3c"}
	if err := edge.Validate(); err != nil {
		t.Errorf("the edge: %v", err)
	}
}

func TestSecretVersionsRoundTrip(t *testing.T) {
	in := Assignment{Version: "1.4.0", Secret: "shop/prod", SecretVersions: map[string]string{"postgres": "6b1e2f4a"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Assignment
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	b, _ = json.Marshal(Assignment{Version: "1.4.0"})
	if strings.Contains(string(b), "secret_versions") {
		t.Errorf("an assignment with no followers sends secret_versions: %s", b)
	}
	b, _ = json.Marshal(ReplicaReport{ID: "postgres-1", State: StateReady,
		SecretVersion: "6b1e2f4a", SecretFailure: &SecretFailure{Version: "7c2f", Reason: "the hook exited with 1"}})
	var r ReplicaReport
	if err := json.Unmarshal(b, &r); err != nil || r.SecretFailure == nil || r.SecretFailure.Version != "7c2f" || r.SecretVersion != "6b1e2f4a" {
		t.Errorf("report %+v %v", r, err)
	}
}
