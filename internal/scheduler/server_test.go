package scheduler

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/schedclient"
)

type fakeDirectory struct {
	instances map[string]string
	calls     int
}

func (d *fakeDirectory) Instances(context.Context) (map[string]string, error) {
	d.calls++
	return maps.Clone(d.instances), nil
}

func identityDoc(instance, ip, account, region string) string {
	b, _ := json.Marshal(map[string]string{"instanceId": instance, "privateIp": ip, "accountId": account, "region": region, "availabilityZone": region + "a"})
	return string(b)
}

func signedRequest(t *testing.T, method, path, body, doc, sig, remote string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = remote
	if doc != "" {
		r.Header.Set("X-ProtoStrap-Identity", base64.StdEncoding.EncodeToString([]byte(doc)))
		r.Header.Set("X-ProtoStrap-Signature", sig)
	}
	return r
}

func TestIdentity(t *testing.T) {
	dir := &fakeDirectory{instances: map[string]string{"i-1": "10.20.16.5"}}
	v := &IdentityVerifier{Account: testAccount, Region: testRegion, Directory: dir}
	ok := identityDoc("i-1", "10.20.16.5", testAccount, testRegion)
	tests := []struct {
		name   string
		doc    string
		remote string
		want   string // It is "" for success, else a substring of the error.
	}{
		{"a member from its own address", ok, "10.20.16.5:40000", ""},
		{"no identity", "", "10.20.16.5:40000", "no identity"},
		{"another account", identityDoc("i-1", "10.20.16.5", "999999999999", testRegion), "10.20.16.5:40000", "account"},
		{"another region", identityDoc("i-1", "10.20.16.5", testAccount, "eu-west-1"), "10.20.16.5:40000", "region"},
		{"from another address", ok, "10.20.16.99:40000", "address"},
		{"not a VM of the application", identityDoc("i-9", "10.20.16.9", testAccount, testRegion), "10.20.16.9:40000", "isn't one of"},
		{"an address EC2 doesn't give the instance", identityDoc("i-1", "10.20.16.6", testAccount, testRegion), "10.20.16.6:40000", "EC2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := v.Verify(context.Background(), signedRequest(t, "POST", "/v1/sync", "", tt.doc, "sig", tt.remote))
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("err %v", err)
			case tt.want == "" && (id.InstanceID != "i-1" || id.PrivateIP != "10.20.16.5"):
				t.Errorf("identity %+v", id)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err %v, want %q", err, tt.want)
			}
		})
	}
}

func TestIdentityRefreshesForANewVM(t *testing.T) {
	dir := &fakeDirectory{instances: map[string]string{}}
	clock := &fakeClock{now: epoch}
	v := &IdentityVerifier{Account: testAccount, Region: testRegion, Directory: dir, Clock: clock}
	doc := identityDoc("i-new", "10.20.16.7", testAccount, testRegion)
	if _, err := v.Verify(context.Background(), signedRequest(t, "GET", "/v1/join", "", doc, "s", "10.20.16.7:1")); err == nil {
		t.Fatal("an unknown instance was accepted")
	}
	dir.instances["i-new"] = "10.20.16.7"
	// A refresh happens at most every few seconds, so a flood of unknown VMs can't hammer EC2.
	v.Verify(context.Background(), signedRequest(t, "GET", "/v1/join", "", doc, "s", "10.20.16.7:1"))
	calls := dir.calls
	clock.advance(10 * time.Second)
	if _, err := v.Verify(context.Background(), signedRequest(t, "GET", "/v1/join", "", doc, "s", "10.20.16.7:1")); err != nil {
		t.Errorf("the new VM wasn't accepted after a refresh: %v", err)
	}
	if dir.calls != calls+1 {
		t.Errorf("EC2 asked %d times", dir.calls-calls)
	}
}

func TestIdentitySignature(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Amazon Web Services LLC"},
		NotBefore: epoch.Add(-time.Hour), NotAfter: epoch.Add(time.Hour * 24 * 365)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert, _ := x509.ParseCertificate(der)
	doc := identityDoc("i-1", "10.20.16.5", testAccount, testRegion)
	sum := sha256.Sum256([]byte(doc))
	raw, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	sig := base64.StdEncoding.EncodeToString(raw)

	v := &IdentityVerifier{Account: testAccount, Region: testRegion, Certificate: cert,
		Directory: &fakeDirectory{instances: map[string]string{"i-1": "10.20.16.5"}}}
	if _, err := v.Verify(context.Background(), signedRequest(t, "GET", "/v1/join", "", doc, sig, "10.20.16.5:1")); err != nil {
		t.Errorf("a valid signature: %v", err)
	}
	forged := identityDoc("i-1", "10.20.16.5", testAccount, testRegion) + " "
	if _, err := v.Verify(context.Background(), signedRequest(t, "GET", "/v1/join", "", forged, sig, "10.20.16.5:1")); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("a document that doesn't match its signature: %v", err)
	}
}

// server builds the API over a harness's scheduler, with every harness VM a member.
func server(h *harness) (*Server, *fakeDirectory) {
	dir := &fakeDirectory{instances: map[string]string{}}
	for id, a := range h.agents {
		dir.instances[id] = a.ip
	}
	return &Server{Scheduler: h.s, Verifier: &IdentityVerifier{Account: testAccount, Region: testRegion, Directory: dir, Clock: h.clock}}, dir
}

func TestServerJoinAndSync(t *testing.T) {
	h := newHarness(t)
	h.settle()
	srv, _ := server(h)
	a := h.vms("general")[0]
	doc := identityDoc(a.id, a.ip, testAccount, testRegion)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/join", "", doc, "s", a.ip+":5000"))
	var j api.Join
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &j) != nil || j.Token == "" {
		t.Errorf("join: %d %s", w.Code, w.Body)
	}

	report, _ := json.Marshal(api.Report{VM: a.id, Name: a.id, Pool: "general", Version: "1.4.0", Capacity: a.capacity})
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), doc, "s", a.ip+":5000"))
	var as api.Assignment
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &as) != nil || as.Version != "1.4.0" {
		t.Errorf("sync: %d %s", w.Code, w.Body)
	}

	// A report that claims another VM is refused.
	other, _ := json.Marshal(api.Report{VM: "i-other", Pool: "general"})
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(other), doc, "s", a.ip+":5000"))
	if w.Code != http.StatusForbidden {
		t.Errorf("a report for another VM: %d", w.Code)
	}

	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), "", "", a.ip+":5000"))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("a sync without identity: %d", w.Code)
	}
}

func TestServerWarmingUpIs503(t *testing.T) {
	h := newHarness(t)
	h.start()
	srv, _ := server(h)
	a := h.vms("general")[0]
	report, _ := json.Marshal(api.Report{VM: a.id, Pool: "general"})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), identityDoc(a.id, a.ip, testAccount, testRegion), "s", a.ip+":1"))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("a sync while warming up: %d", w.Code)
	}
}

func TestReleasesAndStatusAreLocalOnly(t *testing.T) {
	h := newHarness(t)
	srv, _ := server(h)
	h.publish("abc1234", testSpec, nil)
	a := h.vms("general")[0]
	doc := identityDoc(a.id, a.ip, testAccount, testRegion)

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/releases", `{"commit":"abc1234"}`, doc, "s", a.ip+":1"))
	if w.Code != http.StatusForbidden {
		t.Errorf("a release from a VM: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/releases", `{"commit":"not a commit"}`, "", "", "127.0.0.1:1"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("a bad commit: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/releases", `{"commit":"fedcba9"}`, "", "", "127.0.0.1:1"))
	if w.Code != http.StatusNotFound {
		t.Errorf("a missing release: %d", w.Code)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/releases", `{"commit":"abc1234"}`, "", "", "127.0.0.1:1"))
	if w.Code != http.StatusOK || h.s.Status().Target != "abc1234" {
		t.Errorf("a release from data-1 itself: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/status", "", "", "", "[::1]:1"))
	var st Status
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Target != "abc1234" {
		t.Errorf("status: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "GET", "/v1/status", "", doc, "s", a.ip+":1"))
	if w.Code != http.StatusForbidden {
		t.Errorf("status from a VM: %d", w.Code)
	}
}

func TestTLSCertificateIsMadeOnceAndAgentsTrustIt(t *testing.T) {
	dir := t.TempDir()
	cert, pemBytes, err := LoadOrCreateCertificate(dir, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	again, pem2, err := LoadOrCreateCertificate(dir, "127.0.0.1")
	if err != nil || string(pem2) != string(pemBytes) || len(again.Certificate) == 0 {
		t.Fatalf("the certificate changed on a second start: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("key file %v %v", fi, err)
	}
	block, _ := pem.Decode(pemBytes)
	parsed, _ := x509.ParseCertificate(block.Bytes)
	if len(parsed.IPAddresses) == 0 || !parsed.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("IP addresses %v", parsed.IPAddresses)
	}

	// An agent that trusts the published certificate reaches the scheduler.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"swarm_id":"s","token":"t","manager":"m"}`))
	}))
	caFile := filepath.Join(dir, "ca.pem")
	os.WriteFile(caFile, pemBytes, 0o644)
	cfg, err := schedclient.TLSConfig(caFile)
	if err != nil {
		t.Fatal(err)
	}
	c, err := schedclient.New(ln.Addr().String(), cfg, schedclient.Identity{Document: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Join(context.Background()); err != nil {
		t.Errorf("join over TLS: %v", err)
	}
}
