package schedclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func selfSigned(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "scheduler"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return cert, certPEM
}

type source struct {
	answers [][]byte
	errs    []error
	calls   []bool
}

func (s *source) CA(_ context.Context, refresh bool) ([]byte, error) {
	s.calls = append(s.calls, refresh)
	i := min(len(s.calls)-1, len(s.answers)-1)
	return s.answers[i], s.errs[i]
}

func serve(t *testing.T, cert tls.Certificate) string {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"swarm_id":"s","token":"t","manager":"m"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestLazyTLSFetchesTheCertificateWhenFirstNeeded(t *testing.T) {
	cert, certPEM := selfSigned(t)
	src := &source{answers: [][]byte{certPEM}, errs: []error{nil}}
	c, _ := New(serve(t, cert), LazyTLSConfig(src), Identity{Document: "{}"})
	if len(src.calls) != 0 {
		t.Errorf("fetched before it was needed")
	}
	if _, err := c.Join(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Join(context.Background())
	if len(src.calls) != 1 {
		t.Errorf("fetched %d times, want once", len(src.calls))
	}
}

func TestLazyTLSFetchesANewCertificate(t *testing.T) {
	_, oldPEM := selfSigned(t)
	cert, newPEM := selfSigned(t)
	src := &source{answers: [][]byte{oldPEM, newPEM}, errs: []error{nil, nil}}
	c, _ := New(serve(t, cert), LazyTLSConfig(src), Identity{Document: "{}"})
	if _, err := c.Join(context.Background()); err != nil {
		t.Fatalf("data-1's new certificate wasn't fetched: %v", err)
	}
	if len(src.calls) != 2 || !src.calls[1] {
		t.Errorf("calls %v, want a refresh after the old certificate failed", src.calls)
	}
}

func TestLazyTLSRefusesAnUntrustedScheduler(t *testing.T) {
	cert, _ := selfSigned(t)
	_, otherPEM := selfSigned(t)
	src := &source{answers: [][]byte{otherPEM}, errs: []error{nil}}
	c, _ := New(serve(t, cert), LazyTLSConfig(src), Identity{Document: "{}"})
	if _, err := c.Join(context.Background()); err == nil {
		t.Errorf("a scheduler with an untrusted certificate was accepted")
	}
}

func TestLazyTLSWithoutACertificateYet(t *testing.T) {
	cert, _ := selfSigned(t)
	src := &source{answers: [][]byte{nil}, errs: []error{errors.New("scheduler/ca.pem: not found")}}
	c, _ := New(serve(t, cert), LazyTLSConfig(src), Identity{Document: "{}"})
	if _, err := c.Join(context.Background()); err == nil {
		t.Errorf("joined with no certificate to trust")
	}
}
