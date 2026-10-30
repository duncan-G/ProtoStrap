package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fakeIMDS serves IMDSv2: it hands out a token from PUT /latest/api/token and requires it
// on every GET.
type fakeIMDS struct {
	paths  map[string]string
	tokens atomic.Int32
	// rejectNth is the number of the GET whose token is rejected, to force a refresh.
	rejectNth int32
	gets      atomic.Int32
}

func (f *fakeIMDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/latest/api/token" {
		if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
			http.Error(w, "bad token request", http.StatusBadRequest)
			return
		}
		n := f.tokens.Add(1)
		w.Write([]byte("token-" + string(rune('0'+n))))
		return
	}
	n := f.gets.Add(1)
	if r.Header.Get("X-aws-ec2-metadata-token") == "" || n == f.rejectNth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	body, ok := f.paths[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Write([]byte(body))
}

func newIMDS(t *testing.T, f *fakeIMDS) *IMDS {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &IMDS{Endpoint: srv.URL}
}

func TestIMDSUsesSessionToken(t *testing.T) {
	f := &fakeIMDS{paths: map[string]string{"/latest/meta-data/instance-id": "i-0a1b2c3d"}}
	m := newIMDS(t, f)
	for range 3 {
		id, err := m.Get(t.Context(), "meta-data/instance-id")
		if err != nil || id != "i-0a1b2c3d" {
			t.Fatalf("got %q, %v", id, err)
		}
	}
	if f.tokens.Load() != 1 {
		t.Fatalf("token should be reused, fetched %d", f.tokens.Load())
	}
}

func TestIMDSRefreshesARejectedToken(t *testing.T) {
	f := &fakeIMDS{paths: map[string]string{"/latest/meta-data/instance-id": "i-1"}, rejectNth: 2}
	m := newIMDS(t, f)
	m.Get(t.Context(), "meta-data/instance-id")
	id, err := m.Get(t.Context(), "meta-data/instance-id")
	if err != nil || id != "i-1" {
		t.Fatalf("got %q, %v", id, err)
	}
	if f.tokens.Load() != 2 {
		t.Fatalf("expected a new token after a 401, fetched %d", f.tokens.Load())
	}
}

func TestIMDSNotFound(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{})
	_, err := m.Get(t.Context(), "meta-data/spot/instance-action")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestIMDSSpotInstanceAction(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/meta-data/spot/instance-action": `{"action": "terminate", "time": "2026-10-24T08:22:00Z"}`,
	}})
	at, ok, err := m.SpotInterruption(t.Context())
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !at.Equal(time.Date(2026, 10, 24, 8, 22, 0, 0, time.UTC)) {
		t.Fatalf("time %v", at)
	}
}

func TestIMDSSpotStopIsAlsoAnInterruption(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/meta-data/spot/instance-action": `{"action": "stop", "time": "2026-10-24T08:22:00Z"}`,
	}})
	if _, ok, _ := m.SpotInterruption(t.Context()); !ok {
		t.Fatal("a stop is an interruption too")
	}
}

func TestIMDSNoSpotInterruption(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{})
	if _, ok, err := m.SpotInterruption(t.Context()); ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestIMDSTargetLifecycleState(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/meta-data/autoscaling/target-lifecycle-state": "Terminated",
	}})
	s, err := m.TargetLifecycleState(t.Context())
	if err != nil || s != "Terminated" {
		t.Fatalf("got %q %v", s, err)
	}
}

func TestIMDSTargetLifecycleStateOutsideAnASG(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{})
	s, err := m.TargetLifecycleState(t.Context())
	if err != nil || s != "" {
		t.Fatalf("a VM outside an ASG has no lifecycle state, got %q %v", s, err)
	}
}

func TestIMDSRoleCredentials(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/meta-data/iam/security-credentials/":                   "protostrap-general\n",
		"/latest/meta-data/iam/security-credentials/protostrap-general": `{"Code":"Success","AccessKeyId":"AK","SecretAccessKey":"SK","Token":"T","Expiration":"2026-10-24T09:00:00Z"}`,
	}})
	c, err := m.Retrieve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "T",
		Expires: time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)}
	if c != want {
		t.Fatalf("got %+v", c)
	}
}

func TestIMDSRoleCredentialsFailure(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/meta-data/iam/security-credentials/":  "r",
		"/latest/meta-data/iam/security-credentials/r": `{"Code":"AssumeRoleUnauthorizedAccess","Message":"nope"}`,
	}})
	if _, err := m.Retrieve(t.Context()); err == nil {
		t.Fatal("a failed code must be an error")
	}
}

func TestIMDSIdentity(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{paths: map[string]string{
		"/latest/dynamic/instance-identity/document":  `{"instanceId":"i-1","region":"eu-west-1","availabilityZone":"eu-west-1a","privateIp":"10.20.16.5"}`,
		"/latest/dynamic/instance-identity/signature": "MIAG\nCSqG\n",
	}})
	id, err := m.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if id.InstanceID != "i-1" || id.Region != "eu-west-1" || id.Zone != "eu-west-1a" || id.PrivateIP != "10.20.16.5" {
		t.Fatalf("got %+v", id)
	}
	if id.Signature != "MIAGCSqG" {
		t.Fatalf("signature should have its newlines removed, got %q", id.Signature)
	}
	if id.Document == "" {
		t.Fatal("the raw document must be kept, the scheduler verifies it")
	}
}

func TestIMDSContextCancelled(t *testing.T) {
	m := newIMDS(t, &fakeIMDS{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := m.Get(ctx, "meta-data/instance-id"); err == nil {
		t.Fatal("expected an error")
	}
}
