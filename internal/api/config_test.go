package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const testConfigDigest = "0123456789abcdef0123456789abcdef"

func TestAReplicaWithConfigValidates(t *testing.T) {
	gw := ReplicaSpec{ID: "gateway-1", Component: "gateway", Kind: KindService, Image: "r/gateway:c-1",
		Request: Resources{CPU: 500, Memory: 256}, Stop: 30, ConfigPath: "/etc/envoy/xds", ConfigDigest: testConfigDigest}
	if err := gw.Validate(); err != nil {
		t.Errorf("a replica that restarts for new config: %v", err)
	}
	gw.ConfigFollows = true
	if err := gw.Validate(); err != nil {
		t.Errorf("a follower: %v", err)
	}
	gw.ConfigHook = []string{"nginx", "-s", "reload"}
	if err := gw.Validate(); err != nil {
		t.Errorf("a follower with a hook: %v", err)
	}
	task := ReplicaSpec{ID: "idp-config-1", Component: "idp-config", Kind: KindTask, Image: "r/idp-config:c-1",
		Request: Resources{CPU: 250, Memory: 512}, Stop: 30, ConfigPath: "/config", ConfigDigest: testConfigDigest}
	if err := task.Validate(); err != nil {
		t.Errorf("a task with config: %v", err)
	}

	cases := map[string]func(*ReplicaSpec){
		"relative path":            func(s *ReplicaSpec) { s.ConfigPath = "etc/envoy" },
		"the root":                 func(s *ReplicaSpec) { s.ConfigPath = "/" },
		"untidy path":              func(s *ReplicaSpec) { s.ConfigPath = "/etc//envoy/" },
		"bad digest":               func(s *ReplicaSpec) { s.ConfigDigest = "not hex" },
		"path with no digest":      func(s *ReplicaSpec) { s.ConfigDigest = "" },
		"digest with no path":      func(s *ReplicaSpec) { s.ConfigPath = "" },
		"hook that doesn't follow": func(s *ReplicaSpec) { s.ConfigFollows = false },
		"follows with no config":   func(s *ReplicaSpec) { s.ConfigPath, s.ConfigDigest, s.ConfigHook = "", "", nil },
		"task that follows":        func(s *ReplicaSpec) { s.Kind = KindTask },
	}
	for name, mutate := range cases {
		s := gw
		mutate(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConfigDigestsRoundTrip(t *testing.T) {
	in := Assignment{Version: "1.4.0", ConfigDigests: map[string]string{"gateway-1": testConfigDigest}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Assignment
	if err := json.Unmarshal(b, &out); err != nil || !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	b, _ = json.Marshal(Assignment{Version: "1.4.0"})
	if strings.Contains(string(b), "config_digests") {
		t.Errorf("an assignment with no followers sends config_digests: %s", b)
	}
	b, _ = json.Marshal(ReplicaReport{ID: "gateway-1", State: StateReady, ConfigDigest: testConfigDigest,
		ConfigFailure: &ConfigFailure{Digest: "fedcba9876543210fedcba9876543210", Reason: "the hook exited with 1"}})
	var r ReplicaReport
	if err := json.Unmarshal(b, &r); err != nil || r.ConfigDigest != testConfigDigest || r.ConfigFailure == nil ||
		r.ConfigFailure.Digest != "fedcba9876543210fedcba9876543210" {
		t.Errorf("report %+v %v", r, err)
	}
}
