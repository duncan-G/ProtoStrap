package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
)

type fakeSTS struct {
	mu       sync.Mutex
	calls    []string // Each call is recorded as role|session.
	err      error
	lifetime time.Duration
	now      func() time.Time
	delay    time.Duration
}

func (f *fakeSTS) AssumeRole(_ context.Context, role, session string) (aws.Credentials, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, role+"|"+session)
	if f.err != nil {
		return aws.Credentials{}, f.err
	}
	n := len(f.calls)
	return aws.Credentials{
		AccessKeyID:     "ASIA" + string(rune('0'+n)),
		SecretAccessKey: "secret",
		SessionToken:    "session",
		Expires:         f.now().Add(f.lifetime),
	}, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup() (*Server, *fakeSTS, *clock) {
	c := &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	sts := &fakeSTS{lifetime: time.Hour, now: c.now}
	s := NewServer(sts, Options{Now: c.now})
	return s, sts, c
}

func get(s *Server, path, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remote
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

type response struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string
	Token           string
	Expiration      string
	RoleArn         string
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) response {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var r response
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

const role = "arn:aws:iam::123456789012:role/protostrap-shop-api"

func TestServesTheReplicasRoleCredentials(t *testing.T) {
	s, sts, _ := setup()
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role})
	r := decode(t, get(s, "/v1/credentials/tok-1", "172.18.0.5:41000"))
	if r.AccessKeyID != "ASIA1" || r.SecretAccessKey != "secret" || r.Token != "session" || r.RoleArn != role {
		t.Fatalf("got %+v", r)
	}
	if r.Expiration != "2026-10-24T13:00:00Z" {
		t.Fatalf("expiration %q", r.Expiration)
	}
	if !slices.Equal(sts.calls, []string{role + "|api"}) {
		t.Fatalf("assumed %v", sts.calls)
	}
}

func TestUnknownTokenIsNotFound(t *testing.T) {
	s, sts, _ := setup()
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role})
	for _, path := range []string{"/v1/credentials/tok-2", "/v1/credentials/", "/v1/credentials", "/", "/v1/credentials/tok-1/extra"} {
		if rec := get(s, path, "172.18.0.5:1"); rec.Code != 404 {
			t.Errorf("%s: status %d", path, rec.Code)
		}
	}
	if len(sts.calls) != 0 {
		t.Fatal("no role should be assumed for an unknown token")
	}
}

func TestOnlyGET(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role})
	req := httptest.NewRequest(http.MethodPost, "/v1/credentials/tok-1", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestServedOnlyToTheReplicaItself(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role, Addrs: []string{"172.18.0.5"}})
	if rec := get(s, "/v1/credentials/tok-1", "172.18.0.9:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("another container's address must be refused, status %d", rec.Code)
	}
	decode(t, get(s, "/v1/credentials/tok-1", "172.18.0.5:5000"))
}

func TestAddressesLearnedLaterAreChecked(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role})
	decode(t, get(s, "/v1/credentials/tok-1", "172.18.0.9:5000"))
	s.Put(Entry{Token: "tok-1", Replica: "api-1", Component: "api", Role: role, Addrs: []string{"172.18.0.5"}})
	if rec := get(s, "/v1/credentials/tok-1", "172.18.0.9:5000"); rec.Code != http.StatusForbidden {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestOneAssumeRolePerRole(t *testing.T) {
	s, sts, _ := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	s.Put(Entry{Token: "b", Replica: "api-2", Component: "api", Role: role})
	decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
	decode(t, get(s, "/v1/credentials/b", "1.1.1.1:1"))
	decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
	if len(sts.calls) != 1 {
		t.Fatalf("replicas of one component share its role's credentials, assumed %d times", len(sts.calls))
	}
}

func TestRoleCredentialsShareTheReplicasCache(t *testing.T) {
	s, sts, _ := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	creds, err := s.RoleCredentials(context.Background(), role, "api")
	if err != nil || creds.AccessKeyID != "ASIA1" {
		t.Fatalf("credentials %+v %v", creds, err)
	}
	if r := decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1")); r.AccessKeyID != "ASIA1" {
		t.Errorf("the replica got %+v", r)
	}
	if len(sts.calls) != 1 {
		t.Errorf("the agent and the replica share a role's credentials, assumed %d times", len(sts.calls))
	}
}

func TestRoleCredentialsBeforeAnyReplica(t *testing.T) {
	s, sts, c := setup()
	for range 2 {
		if _, err := s.RoleCredentials(context.Background(), role, "postgres"); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(sts.calls, []string{role + "|postgres"}) {
		t.Errorf("assumed %v", sts.calls)
	}
	c.t = c.t.Add(50 * time.Minute)
	if _, err := s.RoleCredentials(context.Background(), role, "postgres"); err != nil || len(sts.calls) != 2 {
		t.Errorf("credentials close to expiring weren't refreshed: %v %v", sts.calls, err)
	}
	sts.err = errors.New("sts down")
	c.t = c.t.Add(2 * time.Hour)
	if _, err := s.RoleCredentials(context.Background(), role, "postgres"); err == nil {
		t.Errorf("expired credentials and a failed assume gave no error")
	}
}

func TestEachComponentGetsItsOwnRole(t *testing.T) {
	s, sts, _ := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	s.Put(Entry{Token: "w", Replica: "w-1", Component: "orders-worker", Role: "arn:aws:iam::1:role/worker"})
	ra := decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
	rw := decode(t, get(s, "/v1/credentials/w", "1.1.1.1:1"))
	if ra.RoleArn == rw.RoleArn || ra.AccessKeyID == rw.AccessKeyID {
		t.Fatalf("roles mixed up: %+v %+v", ra, rw)
	}
	if !slices.Equal(sts.calls, []string{role + "|api", "arn:aws:iam::1:role/worker|orders-worker"}) {
		t.Fatalf("assumed %v", sts.calls)
	}
}

func TestRefreshedBeforeTheyExpire(t *testing.T) {
	s, sts, c := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
	c.t = c.t.Add(40 * time.Minute) // With 20 minutes left, the credentials are still served.
	if r := decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1")); r.AccessKeyID != "ASIA1" {
		t.Fatalf("got %s", r.AccessKeyID)
	}
	c.t = c.t.Add(10 * time.Minute) // With 10 minutes left, the credentials are refreshed.
	if r := decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1")); r.AccessKeyID != "ASIA2" {
		t.Fatalf("got %s", r.AccessKeyID)
	}
	if len(sts.calls) != 2 {
		t.Fatalf("assumed %d times", len(sts.calls))
	}
}

func TestStillValidCredentialsSurviveAFailedRefresh(t *testing.T) {
	s, sts, c := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
	c.t = c.t.Add(50 * time.Minute)
	sts.err = errors.New("throttled")
	if r := decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1")); r.AccessKeyID != "ASIA1" {
		t.Fatalf("got %s", r.AccessKeyID)
	}
	c.t = c.t.Add(11 * time.Minute) // The credentials have now expired.
	if rec := get(s, "/v1/credentials/a", "1.1.1.1:1"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("expired credentials must not be served, status %d", rec.Code)
	}
}

func TestAssumeRoleFailure(t *testing.T) {
	s, sts, _ := setup()
	sts.err = errors.New("AccessDenied")
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	rec := get(s, "/v1/credentials/a", "1.1.1.1:1")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	sts.err = nil
	decode(t, get(s, "/v1/credentials/a", "1.1.1.1:1"))
}

func TestRemovedReplicaGetsNothing(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	s.Remove("api-1")
	if rec := get(s, "/v1/credentials/a", "1.1.1.1:1"); rec.Code != 404 {
		t.Fatalf("status %d", rec.Code)
	}
	if len(s.Replicas()) != 0 {
		t.Fatalf("replicas %v", s.Replicas())
	}
}

func TestReplacingAnEntryDropsTheOldToken(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "old", Replica: "api-1", Component: "api", Role: role})
	s.Put(Entry{Token: "new", Replica: "api-1", Component: "api", Role: role})
	if rec := get(s, "/v1/credentials/old", "1.1.1.1:1"); rec.Code != 404 {
		t.Fatalf("old token still works: %d", rec.Code)
	}
	decode(t, get(s, "/v1/credentials/new", "1.1.1.1:1"))
}

func TestReplicasLists(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "a", Replica: "api-2", Component: "api", Role: role})
	s.Put(Entry{Token: "b", Replica: "api-1", Component: "api", Role: role})
	if got := s.Replicas(); !slices.Equal(got, []string{"api-1", "api-2"}) {
		t.Fatalf("got %v", got)
	}
}

func TestConcurrentRequestsShareOneAssume(t *testing.T) {
	s, sts, _ := setup()
	sts.delay = 20 * time.Millisecond
	s.Put(Entry{Token: "a", Replica: "api-1", Component: "api", Role: role})
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if rec := get(s, "/v1/credentials/a", "1.1.1.1:1"); rec.Code != 200 {
				t.Errorf("status %d", rec.Code)
			}
		})
	}
	wg.Wait()
	if len(sts.calls) != 1 {
		t.Fatalf("assumed %d times", len(sts.calls))
	}
}

func TestTokenComparisonIsExact(t *testing.T) {
	s, _, _ := setup()
	s.Put(Entry{Token: "abcdef", Replica: "api-1", Component: "api", Role: role})
	for _, tok := range []string{"abcde", "abcdefg", "ABCDEF"} {
		if rec := get(s, "/v1/credentials/"+tok, "1.1.1.1:1"); rec.Code != 404 {
			t.Errorf("%s: status %d", tok, rec.Code)
		}
	}
}

func TestNewToken(t *testing.T) {
	a, b := NewToken(), NewToken()
	if len(a) < 32 || a == b {
		t.Fatalf("tokens must be long and random: %q %q", a, b)
	}
}
