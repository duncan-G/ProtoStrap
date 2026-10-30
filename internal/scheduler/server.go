package scheduler

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
	"github.com/duncan-G/protostrap/internal/release"
)

// Server is the scheduler's API (docs/protostrap.md#scheduler): TCP 7443 over TLS, at
// data-1's fixed address.
//
//	GET  /v1/join      an agent, at boot          the swarm's join token and manager
//	POST /v1/sync      every agent, every 5 s     takes the VM's report and log records, returns its assignment
//	POST /v1/releases  protostrap deploy          sets the target release; from data-1 only
//	GET  /v1/status    protostrap deploy, status  pools, VMs, replicas, rollouts; from data-1 only
//
// Agents prove which VM they are with the instance identity document AWS signs.
// Containers can't reach the API: the agents' container firewall keeps them off their VM's
// ports and off data-1.
type Server struct {
	Scheduler *Scheduler
	Verifier  Verifier
	// Logs takes the log records agents send with their reports. It is optional.
	Logs   LogSink
	Logger *slog.Logger
}

// LogSink is the infra log (docs/protostrap.md#infra-log).
type LogSink interface {
	// AddAgent adds an agent's records: vm is the instance ID it proved, and name the VM's
	// name.
	AddAgent(vm, name string, records []api.LogRecord)
}

// Verifier says which VM a request comes from.
type Verifier interface {
	Verify(ctx context.Context, r *http.Request) (VMIdentity, error)
}

// errUnauthorized is a request with no identity at all.
var errUnauthorized = errors.New("no identity")

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/join":
		s.join(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sync":
		s.sync(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/releases":
		s.releases(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/status":
		s.status(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/config/"):
		s.config(w, r)
	default:
		http.NotFound(w, r)
	}
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	reply(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) identify(w http.ResponseWriter, r *http.Request) (VMIdentity, bool) {
	id, err := s.Verifier.Verify(r.Context(), r)
	switch {
	case errors.Is(err, errUnauthorized):
		fail(w, http.StatusUnauthorized, err)
	case err != nil:
		s.log().Warn("a request that didn't prove its VM", "from", r.RemoteAddr, "path", r.URL.Path, "err", err)
		fail(w, http.StatusForbidden, err)
	default:
		return id, true
	}
	return VMIdentity{}, false
}

// local reports whether a request comes from data-1 itself.
func local(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

func (s *Server) join(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.identify(w, r); !ok {
		return
	}
	j, err := s.Scheduler.Join(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, err)
		return
	}
	reply(w, http.StatusOK, j)
}

func (s *Server) sync(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	var report api.Report
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&report); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if report.VM != id.InstanceID {
		fail(w, http.StatusForbidden, fmt.Errorf("%s sent a report for %s", id.InstanceID, report.VM))
		return
	}
	as, err := s.Scheduler.Sync(r.Context(), id, report)
	switch {
	case errors.Is(err, ErrWarmingUp):
		fail(w, http.StatusServiceUnavailable, err)
	case err != nil:
		fail(w, http.StatusBadRequest, err)
	default:
		// Only a sync that worked keeps the records: the agent sends them again otherwise.
		if s.Logs != nil && len(report.Logs) > 0 {
			s.Logs.AddAgent(id.InstanceID, report.Name, report.Logs)
		}
		reply(w, http.StatusOK, as)
	}
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	if !local(r) {
		fail(w, http.StatusForbidden, errors.New("releases are set from data-1 only, by protostrap deploy"))
		return
	}
	var req struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil || !release.ValidCommit(req.Commit) {
		fail(w, http.StatusBadRequest, fmt.Errorf("want {\"commit\": \"<git commit>\"}"))
		return
	}
	err := s.Scheduler.Deploy(r.Context(), req.Commit)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, err)
	case err != nil:
		fail(w, http.StatusBadRequest, err)
	default:
		reply(w, http.StatusOK, map[string]string{"commit": req.Commit, "state": RolloutRunning})
	}
}

// config serves an agent the config files a digest names.
func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	digest := strings.TrimPrefix(r.URL.Path, "/v1/config/")
	if !configfiles.ValidDigest(digest) {
		http.NotFound(w, r)
		return
	}
	if _, ok := s.identify(w, r); !ok {
		return
	}
	files, ok := s.Scheduler.ConfigFiles(digest)
	if !ok {
		fail(w, http.StatusNotFound, fmt.Errorf("no release holds config files %s", digest))
		return
	}
	reply(w, http.StatusOK, files)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if !local(r) {
		fail(w, http.StatusForbidden, errors.New("the status is for data-1 only"))
		return
	}
	reply(w, http.StatusOK, s.Scheduler.Status())
}

// InstanceDirectory lists the application's VMs: every instance tagged with its app, and
// its private address, from EC2.
type InstanceDirectory interface {
	Instances(ctx context.Context) (map[string]string, error)
}

// IdentityVerifier checks a request's instance identity document: that it is this
// account's and region's, that it comes from the address the document names, which EC2
// agrees is the instance's, and that the instance is one of the application's VMs. Inside
// the VPC an address can't be spoofed for a TCP connection, so this proves the VM. With
// AWS's certificate for the region, it also checks the document's signature.
type IdentityVerifier struct {
	Account     string
	Region      string
	Directory   InstanceDirectory
	Certificate *x509.Certificate // It is AWS's public certificate for the region, and may be nil.
	Clock       Clock

	mu        sync.Mutex
	instances map[string]string
	fetched   time.Time
}

const (
	directoryTTL     = 30 * time.Second
	directoryRefresh = 5 * time.Second // An unknown instance refreshes the directory at most this often.
)

func (v *IdentityVerifier) now() time.Time {
	if v.Clock != nil {
		return v.Clock.Now()
	}
	return time.Now()
}

// Verify returns the VM a request comes from.
func (v *IdentityVerifier) Verify(ctx context.Context, r *http.Request) (VMIdentity, error) {
	enc := r.Header.Get("X-ProtoStrap-Identity")
	if enc == "" {
		return VMIdentity{}, errUnauthorized
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return VMIdentity{}, fmt.Errorf("identity document isn't base64")
	}
	var doc struct {
		InstanceID string `json:"instanceId"`
		PrivateIP  string `json:"privateIp"`
		AccountID  string `json:"accountId"`
		Region     string `json:"region"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return VMIdentity{}, fmt.Errorf("identity document: %v", err)
	}
	if doc.AccountID != v.Account {
		return VMIdentity{}, fmt.Errorf("identity document is for account %s", doc.AccountID)
	}
	if doc.Region != v.Region {
		return VMIdentity{}, fmt.Errorf("identity document is for region %s", doc.Region)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if host != doc.PrivateIP {
		return VMIdentity{}, fmt.Errorf("identity document for %s came from another address, %s", doc.PrivateIP, host)
	}
	if v.Certificate != nil {
		sig, err := base64.StdEncoding.DecodeString(r.Header.Get("X-ProtoStrap-Signature"))
		pub, ok := v.Certificate.PublicKey.(*rsa.PublicKey)
		sum := sha256.Sum256(raw)
		if err != nil || !ok || rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
			return VMIdentity{}, errors.New("identity document doesn't match its signature")
		}
	}
	ip, known, err := v.lookup(ctx, doc.InstanceID)
	if err != nil {
		return VMIdentity{}, err
	}
	if !known {
		return VMIdentity{}, fmt.Errorf("%s isn't one of the application's VMs", doc.InstanceID)
	}
	if ip != doc.PrivateIP {
		return VMIdentity{}, fmt.Errorf("EC2 says %s is at %s, not %s", doc.InstanceID, ip, doc.PrivateIP)
	}
	return VMIdentity{InstanceID: doc.InstanceID, PrivateIP: doc.PrivateIP}, nil
}

func (v *IdentityVerifier) lookup(ctx context.Context, instance string) (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	ip, known := v.instances[instance]
	stale := now.Sub(v.fetched) > directoryTTL
	if (known && !stale) || (!known && !stale && now.Sub(v.fetched) < directoryRefresh && v.instances != nil) {
		return ip, known, nil
	}
	got, err := v.Directory.Instances(ctx)
	if err != nil {
		if v.instances != nil {
			ip, known = v.instances[instance]
			return ip, known, nil // The verifier keeps answering from what it knew.
		}
		return "", false, fmt.Errorf("listing the application's VMs: %w", err)
	}
	v.instances, v.fetched = got, now
	ip, known = got[instance]
	return ip, known, nil
}

// LoadOrCreateCertificate is the scheduler's TLS certificate, kept in dir on data-1's
// control volume so it outlives the instance: self-signed, for the scheduler's address and
// loopback. It returns the certificate's PEM, which the scheduler publishes in the deploy
// bucket for agents to trust.
func LoadOrCreateCertificate(dir, address string) (tls.Certificate, []byte, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if certPEM, err := os.ReadFile(certPath); err == nil {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		return cert, certPEM, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if ip := net.ParseIP(address); ip != nil && !ip.IsLoopback() {
		ips = append([]net.IP{ip}, ips...)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "protostrap scheduler"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           ips,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return tls.Certificate{}, nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	return cert, certPEM, err
}

// ParseCertificate reads a PEM certificate, such as AWS's for a region.
func ParseCertificate(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(string(pemBytes))))
	if block == nil {
		return nil, errors.New("no PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
