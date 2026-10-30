// Package schedclient calls the scheduler's API on data-1 (docs/protostrap.md#scheduler).
// Every call carries the instance identity document AWS signs, which is how the
// scheduler knows which VM is calling.
package schedclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/configfiles"
)

// Identity is the VM's signed identity document.
type Identity struct {
	Document  string
	Signature string
}

// Client calls one scheduler.
type Client struct {
	base     string
	http     *http.Client
	identity Identity
	// Timeout limits each call. New sets it to 10s.
	Timeout time.Duration
}

// New returns a client for the scheduler at addr, host:port.
func New(addr string, tlsConfig *tls.Config, id Identity) (*Client, error) {
	if addr == "" {
		return nil, errors.New("schedclient: no scheduler address")
	}
	tr := &http.Transport{TLSClientConfig: tlsConfig, Proxy: nil, IdleConnTimeout: time.Minute}
	return &Client{base: "https://" + addr, http: &http.Client{Transport: tr}, identity: id, Timeout: 10 * time.Second}, nil
}

// TLSConfig trusts the CA in a PEM file, which signed the scheduler's certificate.
func TLSConfig(caFile string) (*tls.Config, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s: no certificates", caFile)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-ProtoStrap-Identity", base64.StdEncoding.EncodeToString([]byte(c.identity.Document)))
	req.Header.Set("X-ProtoStrap-Signature", c.identity.Signature)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("scheduler %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("scheduler %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scheduler %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("scheduler %s: decoding the answer: %w", path, err)
	}
	return nil
}

// Join asks for the swarm's join token and manager.
func (c *Client) Join(ctx context.Context) (api.Join, error) {
	var j api.Join
	err := c.do(ctx, http.MethodGet, "/v1/join", nil, &j)
	return j, err
}

// Sync sends the VM's report and returns its assignment. An assignment with an invalid
// replica is refused whole, so the agent never runs part of one.
func (c *Client) Sync(ctx context.Context, r api.Report) (api.Assignment, error) {
	var a api.Assignment
	if err := c.do(ctx, http.MethodPost, "/v1/sync", r, &a); err != nil {
		return api.Assignment{}, err
	}
	seen := map[string]bool{}
	for _, s := range a.Replicas {
		if err := s.Validate(); err != nil {
			return api.Assignment{}, fmt.Errorf("scheduler sent an invalid assignment: %w", err)
		}
		if seen[s.ID] {
			return api.Assignment{}, fmt.Errorf("scheduler sent replica %s twice", s.ID)
		}
		seen[s.ID] = true
	}
	return a, nil
}

// Config fetches the config files a digest names, which the agent checks against it.
func (c *Client) Config(ctx context.Context, digest string) (configfiles.Files, error) {
	if !configfiles.ValidDigest(digest) {
		return nil, fmt.Errorf("config digest %q is invalid", digest)
	}
	var files configfiles.Files
	if err := c.do(ctx, http.MethodGet, "/v1/config/"+digest, nil, &files); err != nil {
		return nil, err
	}
	return files, nil
}
