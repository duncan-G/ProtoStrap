package schedclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"time"
)

// CASource gives the scheduler's certificate, which the agent trusts. With refresh set, it
// fetches the certificate again rather than use what it has.
type CASource interface {
	CA(ctx context.Context, refresh bool) ([]byte, error)
}

// LazyTLSConfig trusts the certificate from src, loaded the first time a connection needs
// it. data-1 makes its certificate when its scheduler first starts, which may be after a
// VM boots; and if data-1 is rebuilt with a new one, a failed check fetches it again.
func LazyTLSConfig(src CASource) *tls.Config {
	l := &lazyRoots{src: src}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The chain and the address are checked in VerifyConnection, against roots that are
		// only known once the connection is made.
		InsecureSkipVerify: true,
		VerifyConnection:   l.verify,
	}
}

type lazyRoots struct {
	src       CASource
	mu        sync.Mutex
	roots     *x509.CertPool
	refreshed time.Time
}

const refreshEvery = 30 * time.Second

func (l *lazyRoots) pool(refresh bool) (*x509.CertPool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.roots != nil && !refresh {
		return l.roots, nil
	}
	// A refresh, after a certificate failed, happens at most every refreshEvery.
	if refresh {
		if time.Since(l.refreshed) < refreshEvery {
			if l.roots == nil {
				return nil, errors.New("the scheduler's certificate isn't available yet")
			}
			return l.roots, nil
		}
		l.refreshed = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pemBytes, err := l.src.CA(ctx, refresh)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("the scheduler's certificate isn't PEM")
	}
	l.roots = pool
	return pool, nil
}

func (l *lazyRoots) verify(cs tls.ConnectionState) error {
	check := func(pool *x509.CertPool) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("the scheduler sent no certificate")
		}
		inter := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			inter.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, DNSName: cs.ServerName})
		return err
	}
	pool, err := l.pool(false)
	if err == nil {
		if err = check(pool); err == nil {
			return nil
		}
	}
	fresh, ferr := l.pool(true)
	if ferr != nil {
		return errors.Join(err, ferr)
	}
	return check(fresh)
}
