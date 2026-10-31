package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/agent"
	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/credentials"
)

func imds(t *testing.T, paths map[string]string) *aws.IMDS {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			w.Write([]byte("tok"))
			return
		}
		body, ok := paths[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &aws.IMDS{Endpoint: srv.URL}
}

func TestMetadataSpot(t *testing.T) {
	m := metadata{imds(t, map[string]string{
		"/latest/meta-data/spot/instance-action":               `{"action":"terminate","time":"2026-10-24T08:22:00Z"}`,
		"/latest/meta-data/autoscaling/target-lifecycle-state": "InService",
	})}
	n, err := m.Termination(t.Context())
	if err != nil || n == nil || !n.Spot || !n.At.Equal(time.Date(2026, 10, 24, 8, 22, 0, 0, time.UTC)) {
		t.Fatalf("got %+v %v", n, err)
	}
}

func TestMetadataLifecycle(t *testing.T) {
	m := metadata{imds(t, map[string]string{"/latest/meta-data/autoscaling/target-lifecycle-state": "Terminated"})}
	n, err := m.Termination(t.Context())
	if err != nil || n == nil || n.Spot {
		t.Fatalf("got %+v %v", n, err)
	}
}

func TestMetadataInService(t *testing.T) {
	for _, state := range []string{"InService", "Pending:Wait", "Warmed:Stopped"} {
		m := metadata{imds(t, map[string]string{"/latest/meta-data/autoscaling/target-lifecycle-state": state})}
		if n, err := m.Termination(t.Context()); n != nil || err != nil {
			t.Fatalf("%s: got %+v %v", state, n, err)
		}
	}
}

func TestMetadataOutsideAnASG(t *testing.T) {
	m := metadata{imds(t, nil)}
	if n, err := m.Termination(t.Context()); n != nil || err != nil {
		t.Fatalf("got %+v %v", n, err)
	}
}

type staticCreds struct{}

func (staticCreds) Retrieve(context.Context) (aws.Credentials, error) {
	return aws.Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}, nil
}

func TestHooksUseTheConfiguredNames(t *testing.T) {
	var forms []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		forms = append(forms, r.PostForm.Get("Action")+" "+r.PostForm.Get("AutoScalingGroupName")+" "+
			r.PostForm.Get("LifecycleHookName")+" "+r.PostForm.Get("InstanceId")+" "+r.PostForm.Get("LifecycleActionResult"))
		w.Write([]byte("<R/>"))
	}))
	defer srv.Close()
	h := hooks{
		as:    &aws.AutoScaling{Endpoint: srv.URL, Region: "eu-west-1", Credentials: staticCreds{}},
		group: "shop-general", launch: "launch-hook", terminate: "terminate-hook", instance: "i-1",
	}
	ctx := t.Context()
	h.Complete(ctx, agent.LaunchHook, agent.HookContinue)
	h.Complete(ctx, agent.TerminateHook, agent.HookContinue)
	h.Heartbeat(ctx, agent.TerminateHook)
	want := []string{
		"CompleteLifecycleAction shop-general launch-hook i-1 CONTINUE",
		"CompleteLifecycleAction shop-general terminate-hook i-1 CONTINUE",
		"RecordLifecycleActionHeartbeat shop-general terminate-hook i-1 ",
	}
	if strings.Join(forms, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s", strings.Join(forms, "\n"))
	}
}

// After an agent restart, or once the hook has timed out, AWS has no launch hook in
// progress for the VM. The agent stops trying then, so the adapter says so.
func TestHooksReportAHookThatIsDone(t *testing.T) {
	answer := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(answer))
	}))
	defer srv.Close()
	h := hooks{
		as:    &aws.AutoScaling{Endpoint: srv.URL, Region: "eu-west-1", Credentials: staticCreds{}},
		group: "shop-general", launch: "launch-hook", terminate: "terminate-hook", instance: "i-1",
	}
	answer = `<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code>` +
		`<Message>No active Lifecycle Action found with instance ID i-1</Message></Error></ErrorResponse>`
	if err := h.Complete(t.Context(), agent.LaunchHook, agent.HookContinue); !errors.Is(err, agent.ErrHookDone) {
		t.Fatalf("no active lifecycle action is ErrHookDone, got %v", err)
	}
	answer = `<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code>` +
		`<Message>1 validation error detected</Message></Error></ErrorResponse>`
	if err := h.Complete(t.Context(), agent.LaunchHook, agent.HookContinue); err == nil || errors.Is(err, agent.ErrHookDone) {
		t.Fatalf("another error isn't ErrHookDone, got %v", err)
	}
}

func TestRealClockSleepIsCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	if err := (realClock{}).Sleep(ctx, time.Hour); err == nil || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if err := (realClock{}).Sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

type fixedAssumer struct{ calls []string }

func (f *fixedAssumer) AssumeRole(_ context.Context, role, session string) (aws.Credentials, error) {
	f.calls = append(f.calls, role+"|"+session)
	return aws.Credentials{AccessKeyID: "ASIACOMPONENT", SecretAccessKey: "s", SessionToken: "t", Expires: time.Now().Add(time.Hour)}, nil
}

func TestComponentSecretsReadWithTheComponentsRole(t *testing.T) {
	var auth, target, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, target = r.Header.Get("Authorization"), r.Header.Get("X-Amz-Target")
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		body = string(b)
		w.Write([]byte(`{"SecretString":"{\"Postgres_POSTGRES_PASSWORD\":\"pw\"}","VersionId":"v-1"}`))
	}))
	defer srv.Close()
	sts := &fixedAssumer{}
	s := componentSecrets{creds: credentials.NewServer(sts, credentials.Options{}), region: "eu-west-1", endpoint: srv.URL}
	value, version, err := s.Read(context.Background(), "shop/prod", "v-1", "arn:aws:iam::1:role/protostrap/protostrap-c-postgres", "postgres")
	if err != nil || value != `{"Postgres_POSTGRES_PASSWORD":"pw"}` || version != "v-1" {
		t.Fatalf("read %q %q %v", value, version, err)
	}
	if !strings.Contains(auth, "Credential=ASIACOMPONENT/") || target != "secretsmanager.GetSecretValue" ||
		!strings.Contains(body, `"VersionId":"v-1"`) || !strings.Contains(body, `"SecretId":"shop/prod"`) {
		t.Errorf("auth %q target %q body %s", auth, target, body)
	}
	if len(sts.calls) != 1 || sts.calls[0] != "arn:aws:iam::1:role/protostrap/protostrap-c-postgres|postgres" {
		t.Errorf("assumed %v", sts.calls)
	}
}
