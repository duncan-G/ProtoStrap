package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func general() map[string]any {
	return map[string]any{
		"app": "shop", "pool": "general",
		"scheduler": "10.20.1.10:7443", "scheduler_ca": "/etc/protostrap/ca.pem",
		"auto_scaling_group": "shop-general", "launch_hook": "launch", "terminate_hook": "terminate",
		"deploy_bucket": "shop-deploy",
		"network":       map[string]any{"vpc": "10.20.0.0/16", "resolver": "10.20.0.2", "managed": []string{"10.20.2.0/24"}},
	}
}

func TestLoadGeneralConfig(t *testing.T) {
	c, err := loadConfig(writeConfig(t, general()))
	if err != nil {
		t.Fatal(err)
	}
	if c.App != "shop" || c.Pool != "general" || c.AutoScalingGroup != "shop-general" {
		t.Fatalf("got %+v", c)
	}
	// The fields the file leaves out take their defaults.
	if c.CredentialsPort != 51679 || c.CredentialHelper != "docker-credential-ecr-login" ||
		c.SSMService != "amazon-ssm-agent" || c.DockerSocket != "/var/run/docker.sock" ||
		c.TerminateHookTimeout.Duration != 10*time.Minute || c.BackupHour != 3 {
		t.Fatalf("defaults %+v", c)
	}
	if !slices.Equal(c.Network.Containers, []string{"172.17.0.0/24", "172.18.0.0/24", "172.19.0.0/16"}) {
		t.Fatalf("Docker's ranges from network.md by default, got %v", c.Network.Containers)
	}
}

// TestTelemetryIsAtTheSchedulersAddress checks where Docker sends replicas' output: the
// collector on data-1, whose fixed address agents already know as the scheduler's. No new key
// in agent.json, which an earlier agent would refuse.
func TestTelemetryIsAtTheSchedulersAddress(t *testing.T) {
	c, err := loadConfig(writeConfig(t, general()))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.telemetryAddress(); got != "10.20.1.10:24224" {
		t.Errorf("telemetry address %q", got)
	}
}

func TestLoadDataConfig(t *testing.T) {
	cfg := general()
	cfg["pool"] = "data"
	cfg["name"] = "data-1"
	cfg["swarm_manager"] = true
	cfg["backups_bucket"] = "shop-backups"
	delete(cfg, "auto_scaling_group")
	delete(cfg, "launch_hook")
	delete(cfg, "terminate_hook")
	c, err := loadConfig(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if !c.SwarmManager || c.BackupsBucket != "shop-backups" {
		t.Fatalf("got %+v", c)
	}
}

func TestLoadIngressConfig(t *testing.T) {
	cfg := general()
	cfg["pool"] = "ingress"
	cfg["ingress"] = map[string]any{"interface": "ens5", "elastic_ip_allocation": "eipalloc-1", "route_tables": []string{"rtb-1"}}
	c, err := loadConfig(writeConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if c.Ingress == nil || c.Ingress.Interface != "ens5" {
		t.Fatalf("got %+v", c)
	}
}

func TestDurationsAreStrings(t *testing.T) {
	cfg := general()
	cfg["terminate_hook_timeout"] = "15m"
	c, err := loadConfig(writeConfig(t, cfg))
	if err != nil || c.TerminateHookTimeout.Duration != 15*time.Minute {
		t.Fatalf("%v %v", c.TerminateHookTimeout, err)
	}
	cfg["terminate_hook_timeout"] = "soon"
	if _, err := loadConfig(writeConfig(t, cfg)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestConfigRejects(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no app":                func(c map[string]any) { delete(c, "app") },
		"unknown pool":          func(c map[string]any) { c["pool"] = "gpu" },
		"no scheduler":          func(c map[string]any) { delete(c, "scheduler") },
		"no deploy bucket":      func(c map[string]any) { delete(c, "deploy_bucket") },
		"pool VM without group": func(c map[string]any) { delete(c, "auto_scaling_group") },
		"pool VM without hooks": func(c map[string]any) { delete(c, "terminate_hook") },
		"data VM with a group":  func(c map[string]any) { c["pool"] = "data"; c["backups_bucket"] = "b" },
		"data VM no backups": func(c map[string]any) {
			c["pool"] = "data"
			delete(c, "auto_scaling_group")
			delete(c, "launch_hook")
			delete(c, "terminate_hook")
		},
		"ingress without zone": func(c map[string]any) { c["pool"] = "ingress" },
		"zone off ingress": func(c map[string]any) {
			c["ingress"] = map[string]any{"interface": "ens5", "elastic_ip_allocation": "e", "route_tables": []string{"r"}}
		},
		"manager off data": func(c map[string]any) { c["swarm_manager"] = true },
		"bad vpc":          func(c map[string]any) { c["network"] = map[string]any{"vpc": "x", "resolver": "10.20.0.2"} },
		"unknown field":    func(c map[string]any) { c["shedular"] = "typo" },
		"backup hour":      func(c map[string]any) { c["backup_hour"] = 25 },
	}
	for name, mutate := range cases {
		cfg := general()
		mutate(cfg)
		if _, err := loadConfig(writeConfig(t, cfg)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestMissingConfigFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("expected an error")
	}
}

// testdata/agent has a sample agent.json for each pool, shaped like the ones
// infra/platform/agent.tf writes.
func TestExampleConfigsLoad(t *testing.T) {
	paths, _ := filepath.Glob("testdata/agent/*.json")
	if len(paths) == 0 {
		t.Fatal("no example configs in testdata/agent")
	}
	for _, p := range paths {
		if _, err := loadConfig(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestSystemdUnitHasAWatchdog(t *testing.T) {
	b, err := os.ReadFile("../../infra/platform/templates/protostrap-agent.service")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(b)
	for _, want := range []string{"Type=notify", "WatchdogSec=", "Restart=always", "ExecStart=/usr/local/bin/protostrap agent"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q", want)
		}
	}
}

func TestRunWithoutACommand(t *testing.T) {
	var out strings.Builder
	if code := run([]string{"protostrap"}, &out); code != 2 || !strings.Contains(out.String(), "usage") {
		t.Fatalf("code %d output %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"protostrap", "frobnicate"}, &out); code != 2 {
		t.Fatalf("code %d", code)
	}
	out.Reset()
	if code := run([]string{"protostrap", "version"}, &out); code != 0 || strings.TrimSpace(out.String()) != version {
		t.Fatalf("code %d output %q", code, out.String())
	}
	out.Reset()
	if code := run([]string{"protostrap", "scheduler", "--config", "/nonexistent/scheduler.json"}, &out); code != 1 {
		t.Fatalf("the scheduler without its config: code %d", code)
	}
}
