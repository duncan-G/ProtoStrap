package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/scheduler"
)

// fakeAPI is the scheduler as `protostrap deploy` sees it: each Status call returns the
// next status in a script.
type fakeAPI struct {
	statuses  []scheduler.Status
	calls     int
	deployed  []string
	deployErr []error
}

func (f *fakeAPI) Status(context.Context) (scheduler.Status, error) {
	i := min(f.calls, len(f.statuses)-1)
	f.calls++
	return f.statuses[i], nil
}

func (f *fakeAPI) Deploy(_ context.Context, commit string) error {
	f.deployed = append(f.deployed, commit)
	if len(f.deployErr) > 0 {
		err := f.deployErr[0]
		f.deployErr = f.deployErr[1:]
		return err
	}
	return nil
}

func testDeployer(api *fakeAPI, version string, out *bytes.Buffer) (*deployer, *[]string) {
	var actions []string
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	d := &deployer{
		api: api,
		release: func(_ context.Context, commit string) (release.Release, error) {
			return release.Release{Commit: commit, App: "shop", Version: version}, nil
		},
		update:  func(_ context.Context, v string) error { actions = append(actions, "update "+v); return nil },
		restart: func(context.Context) error { actions = append(actions, "restart"); return nil },
		report:  func() { actions = append(actions, "report") },
		out:     out, poll: time.Second, timeout: time.Hour,
		clock: func() time.Time { return now },
		sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
	}
	return d, &actions
}

func rolling(version, commit, state, reason string) scheduler.Status {
	return scheduler.Status{Version: version, Target: commit,
		Rollouts:   []scheduler.RolloutStatus{{Commit: commit, State: state, Reason: reason}},
		Components: []scheduler.ComponentStatus{{Name: "api", Kind: "service", Desired: 2, Ready: 1, Old: 1}}}
}

func TestDeployFollowsTheRolloutToTheEnd(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{
		{Version: "1.4.0"},
		rolling("1.4.0", "abc1234", scheduler.RolloutRunning, ""),
		rolling("1.4.0", "abc1234", scheduler.RolloutDone, ""),
	}}
	var out bytes.Buffer
	d, actions := testDeployer(api, "1.4.0", &out)
	if err := d.run(context.Background(), "abc1234"); err != nil {
		t.Fatal(err)
	}
	if len(api.deployed) != 1 || api.deployed[0] != "abc1234" {
		t.Errorf("deployed %v", api.deployed)
	}
	if strings.Join(*actions, ",") != "report" {
		t.Errorf("actions %v: no update for the same version; the reporter runs after a release", *actions)
	}
	if !strings.Contains(out.String(), "api 1/2 (1 old)") || !strings.Contains(out.String(), "done") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestDeployUpdatesTheSchedulerFirst(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{
		{Version: "1.4.0"},
		{Version: "1.4.0"}, // The scheduler is still restarting.
		{Version: "1.5.0"},
		rolling("1.5.0", "abc1234", scheduler.RolloutDone, ""),
	}}
	var out bytes.Buffer
	d, actions := testDeployer(api, "1.5.0", &out)
	if err := d.run(context.Background(), "abc1234"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*actions, ",") != "update 1.5.0,restart,report" {
		t.Errorf("actions %v", *actions)
	}
}

func TestDeployWaitsOutTheWarmUp(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{{Version: "1.4.0"}, rolling("1.4.0", "abc1234", scheduler.RolloutDone, "")},
		deployErr: []error{errors.New("HTTP 503"), errors.New("HTTP 503")}}
	var out bytes.Buffer
	d, _ := testDeployer(api, "1.4.0", &out)
	if err := d.run(context.Background(), "abc1234"); err != nil {
		t.Fatal(err)
	}
	if len(api.deployed) != 3 {
		t.Errorf("deploy tried %d times", len(api.deployed))
	}
}

func TestDeployReportsAFailure(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{
		{Version: "1.4.0"},
		{Version: "1.4.0", Target: "c0ffee1", Rollouts: []scheduler.RolloutStatus{{Commit: "abc1234", State: scheduler.RolloutFailed, Reason: "api replica api-7 failed: exited with code 1"}}},
	}}
	var out bytes.Buffer
	d, _ := testDeployer(api, "1.4.0", &out)
	err := d.run(context.Background(), "abc1234")
	if err == nil || !strings.Contains(err.Error(), "rolled back to \"c0ffee1\"") || !strings.Contains(err.Error(), "api-7") {
		t.Errorf("err %v", err)
	}
}

func TestDeployReportsSuperseded(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{{Version: "1.4.0"}, rolling("1.4.0", "abc1234", scheduler.RolloutSuperseded, "superseded by def5678")}}
	var out bytes.Buffer
	d, _ := testDeployer(api, "1.4.0", &out)
	if err := d.run(context.Background(), "abc1234"); err == nil || !strings.Contains(err.Error(), "superseded") {
		t.Errorf("err %v", err)
	}
}

func TestDeployGivesUpAfterItsTimeout(t *testing.T) {
	api := &fakeAPI{statuses: []scheduler.Status{{Version: "1.4.0"}, rolling("1.4.0", "abc1234", scheduler.RolloutRunning, "")}}
	var out bytes.Buffer
	d, _ := testDeployer(api, "1.4.0", &out)
	d.timeout = time.Minute
	if err := d.run(context.Background(), "abc1234"); err == nil || !strings.Contains(err.Error(), "still rolling out") {
		t.Errorf("err %v", err)
	}
}

func TestCheckCommand(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"protostrap", "check", "--spec", "../../sample/protostrap.yaml"}, &out); code != 0 {
		t.Errorf("check the sample: %d\n%s", code, out.String())
	}
	bad := filepath.Join(t.TempDir(), "protostrap.yaml")
	os.WriteFile(bad, []byte("app: Bad_Name\nprotostrap: x\n"), 0o644)
	out.Reset()
	if code := run([]string{"protostrap", "check", "--spec", bad}, &out); code != 1 || !strings.Contains(out.String(), "problems") {
		t.Errorf("check a bad spec: %d\n%s", code, out.String())
	}
}

func TestSchedulerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scheduler.json")
	os.WriteFile(path, []byte(`{"app":"shop","environment":"dev","region":"us-east-1","deploy_bucket":"b","files_bucket":"f",
		"registry":"r","listen":":7443","address":"10.20.1.10","state_dir":"/x","log_group":"/protostrap/shop/scheduler","report_secret":"protostrap/reporter"}`), 0o644)
	c, err := loadSchedulerConfig(path)
	if err != nil || c.App != "shop" || c.localAPI() != "127.0.0.1:7443" || c.DockerSocket == "" {
		t.Errorf("config %+v %v", c, err)
	}
	os.WriteFile(path, []byte(`{"app":"shop","surprise":1}`), 0o644)
	if _, err := loadSchedulerConfig(path); err == nil {
		t.Errorf("an unknown field was accepted")
	}
}

func TestReportNeedsItsConfig(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"protostrap", "report", "--config", filepath.Join(t.TempDir(), "missing.json")}, &out); code != 1 {
		t.Errorf("report without a config: %d\n%s", code, out.String())
	}
}

func TestStatusPrintsTheSecret(t *testing.T) {
	var out bytes.Buffer
	printStatus(&out, scheduler.Status{
		Version: "1.4.0", Target: "c1", LastGood: "c1",
		Secret: &scheduler.SecretWatch{Name: "shop/prod", Version: "v-3"},
		Components: []scheduler.ComponentStatus{
			{Name: "api", Secret: &scheduler.SecretStatus{Version: "v-2", Pending: "v-3", Waiting: "postgres applies its keys first"}},
			{Name: "postgres", Secret: &scheduler.SecretStatus{Version: "v-3", Failed: "postgres-1 can't apply version v-3: the hook exited with 2"}},
			{Name: "web", Secret: &scheduler.SecretStatus{Version: "v-1"}},
		},
	})
	for _, want := range []string{
		"secret shop/prod: version v-3",
		"keys of api: v-3 waits: postgres applies its keys first",
		"keys of postgres: v-3 fails: postgres-1 can't apply version v-3: the hook exited with 2",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "keys of web") {
		t.Errorf("a component whose keys are settled is listed:\n%s", out.String())
	}
	out.Reset()
	printStatus(&out, scheduler.Status{Secret: &scheduler.SecretWatch{Name: "shop/prod", Error: "reading shop/prod: AccessDeniedException"}})
	if !strings.Contains(out.String(), "secret shop/prod: can't be read: reading shop/prod: AccessDeniedException") {
		t.Errorf("status:\n%s", out.String())
	}
}
