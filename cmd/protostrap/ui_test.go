package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/ui"
)

func loadWithUI(t *testing.T, extra string) (schedulerConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scheduler.json")
	os.WriteFile(path, []byte(`{"app":"shop","region":"us-east-1","deploy_bucket":"b","registry":"r","address":"10.20.1.10"`+extra+`}`), 0o644)
	return loadSchedulerConfig(path)
}

func TestTheUIListensOnLoopbackByDefault(t *testing.T) {
	c, err := loadWithUI(t, "")
	if err != nil || c.UIListen != "127.0.0.1:7480" {
		t.Errorf("ui_listen %q, %v; want 127.0.0.1:7480", c.UIListen, err)
	}
	c, err = loadWithUI(t, `,"ui_listen":""`)
	if err != nil || c.UIListen != "" {
		t.Errorf("ui_listen %q, %v; want it off", c.UIListen, err)
	}
	for _, addr := range []string{"localhost:7480", "[::1]:7480", "127.0.0.1:9000"} {
		if _, err := loadWithUI(t, `,"ui_listen":"`+addr+`"`); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestTheUINeverListensBeyondLoopback(t *testing.T) {
	// The UI has no sign-in of its own: SSM's port forward, with the operator's IAM sign-in,
	// is the only way to it.
	for _, addr := range []string{":7480", "0.0.0.0:7480", "[::]:7480", "10.20.1.10:7480", "example.com:7480", "7480", "localhost"} {
		_, err := loadWithUI(t, `,"ui_listen":"`+addr+`"`)
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%q: %v, want it refused as not loopback", addr, err)
		}
	}
}

type fakeBucket struct {
	objects map[string]string
	lists   []string
	err     error
}

func (f *fakeBucket) List(_ context.Context, bucket, prefix string) ([]aws.Object, error) {
	f.lists = append(f.lists, bucket+"/"+prefix)
	if f.err != nil {
		return nil, f.err
	}
	var out []aws.Object
	for k, v := range f.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, aws.Object{Key: k, Size: int64(len(v)), Modified: time.Date(2026, 10, 25, 6, 0, 0, 0, time.UTC)})
		}
	}
	return out, nil
}

func (f *fakeBucket) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("s3: %w", aws.ErrNotFound)
	}
	return io.NopCloser(strings.NewReader(v)), nil
}

func TestReportsFromTheDeployBucket(t *testing.T) {
	b := &fakeBucket{objects: map[string]string{
		"reports/2026-10-24.md":    "# the 24th",
		"reports/2026-10-25.md":    "# the 25th",
		"reports/draft.txt":        "not a report",
		"reports/2026-10-25.md.gz": "not one either",
		"releases/9f2c1e7.json":    "{}",
	}}
	r := reportsBucket{s3: b, bucket: "deploy"}
	list, err := r.Reports(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || b.lists[0] != "deploy/reports/" {
		t.Fatalf("reports %+v from %v, want the two dated ones", list, b.lists)
	}
	dates := map[string]int64{}
	for _, rep := range list {
		dates[rep.Date] = rep.Size
	}
	if dates["2026-10-25"] != int64(len("# the 25th")) || dates["2026-10-24"] == 0 {
		t.Errorf("reports %+v", list)
	}

	text, err := r.Report(t.Context(), "2026-10-25")
	if err != nil || string(text) != "# the 25th" {
		t.Errorf("report %q, %v", text, err)
	}
	if _, err := r.Report(t.Context(), "2026-10-23"); !errors.Is(err, ui.ErrNotFound) {
		t.Errorf("a missing report: %v, want ui.ErrNotFound", err)
	}
}

func TestTheOrgAdminsUIHasAPortOfItsOwn(t *testing.T) {
	// The port is the role: an Org Admin's SSM session document forwards 7481, a Platform
	// Admin's only 7480, so the UI knows who it serves without a sign-in.
	c, err := loadWithUI(t, "")
	if err != nil || c.UIAdminListen != "127.0.0.1:7481" {
		t.Errorf("ui_admin_listen %q, %v; want 127.0.0.1:7481", c.UIAdminListen, err)
	}
	c, err = loadWithUI(t, `,"ui_admin_listen":""`)
	if err != nil || c.UIAdminListen != "" {
		t.Errorf("ui_admin_listen %q, %v; want it off", c.UIAdminListen, err)
	}
	for _, addr := range []string{":7481", "0.0.0.0:7481", "10.20.1.10:7481"} {
		if _, err := loadWithUI(t, `,"ui_admin_listen":"`+addr+`"`); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("%q: %v, want it refused as not loopback", addr, err)
		}
	}
	_, err = loadWithUI(t, `,"ui_listen":"127.0.0.1:7480","ui_admin_listen":"127.0.0.1:7480"`)
	if err == nil || !strings.Contains(err.Error(), "ui_admin_listen") {
		t.Errorf("one address for both roles: %v, want it refused", err)
	}
}

func TestEachUIListenerServesItsRole(t *testing.T) {
	for extra, want := range map[string][]uiListener{
		"":                                     {{"127.0.0.1:7480", ui.RolePlatformAdmin}, {"127.0.0.1:7481", ui.RoleOrgAdmin}},
		`,"ui_admin_listen":""`:                {{"127.0.0.1:7480", ui.RolePlatformAdmin}},
		`,"ui_listen":""`:                      {{"127.0.0.1:7481", ui.RoleOrgAdmin}},
		`,"ui_listen":"","ui_admin_listen":""`: nil,
		`,"ui_listen":"[::1]:9000"`:            {{"[::1]:9000", ui.RolePlatformAdmin}, {"127.0.0.1:7481", ui.RoleOrgAdmin}},
	} {
		c, err := loadWithUI(t, extra)
		if err != nil {
			t.Fatalf("%s: %v", extra, err)
		}
		if got := c.uiListeners(); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%q: listeners %v, want %v", extra, got, want)
		}
	}
}

func TestAConfigThatNamesTheSchedulersLogGroupStillLoads(t *testing.T) {
	// data-1's config is written once, when it boots. One written before the infra log
	// names the scheduler's CloudWatch group, which the scheduler no longer writes to.
	if _, err := loadWithUI(t, `,"log_group":"/protostrap/shop/scheduler"`); err != nil {
		t.Error(err)
	}
}

func TestTheInfraLogsWriterNamesTheRun(t *testing.T) {
	// Each run of the scheduler writes its own objects, so a restart never overwrites the
	// records of the run before it.
	got := infraLogWriter(time.Date(2026, 10, 29, 8, 0, 0, 123, time.FixedZone("PDT", -7*3600)), "i-0data1")
	if got != "20261029T150000Z-i-0data1" {
		t.Errorf("writer %q", got)
	}
}
