package aws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// IMDS reads the instance metadata service, version 2.
type IMDS struct {
	Endpoint string // It defaults to http://169.254.169.254.
	HTTP     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

const imdsTokenTTL = 6 * time.Hour

func (m *IMDS) endpoint() string {
	if m.Endpoint != "" {
		return strings.TrimSuffix(m.Endpoint, "/")
	}
	return "http://169.254.169.254"
}

func (m *IMDS) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	// Metadata is link-local, so requests for it never go through a proxy.
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

func (m *IMDS) sessionToken(ctx context.Context, refresh bool) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !refresh && m.token != "" && time.Now().Before(m.expires) {
		return m.token, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, m.endpoint()+"/latest/api/token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", fmt.Sprint(int(imdsTokenTTL.Seconds())))
	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("imds token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("imds token: HTTP %d", resp.StatusCode)
	}
	m.token = strings.TrimSpace(string(body))
	m.expires = time.Now().Add(imdsTokenTTL - time.Minute)
	return m.token, nil
}

// Get reads a path under /latest/, such as "meta-data/instance-id".
func (m *IMDS) Get(ctx context.Context, path string) (string, error) {
	for attempt := range 2 {
		token, err := m.sessionToken(ctx, attempt > 0)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.endpoint()+"/latest/"+path, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-aws-ec2-metadata-token", token)
		resp, err := m.client().Do(req)
		if err != nil {
			return "", fmt.Errorf("imds %s: %w", path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		switch {
		case err != nil:
			return "", fmt.Errorf("imds %s: %w", path, err)
		case resp.StatusCode == http.StatusOK:
			return string(body), nil
		case resp.StatusCode == http.StatusNotFound:
			return "", fmt.Errorf("imds %s: %w", path, ErrNotFound)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			continue // The token expired, so the next attempt gets another.
		default:
			return "", fmt.Errorf("imds %s: HTTP %d", path, resp.StatusCode)
		}
	}
	return "", fmt.Errorf("imds %s: token refused", path)
}

// SpotInterruption reports a Spot interruption notice and when it takes effect.
func (m *IMDS) SpotInterruption(ctx context.Context) (time.Time, bool, error) {
	body, err := m.Get(ctx, "meta-data/spot/instance-action")
	if errors.Is(err, ErrNotFound) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	var action struct {
		Action string `json:"action"`
		Time   string `json:"time"`
	}
	if err := json.Unmarshal([]byte(body), &action); err != nil {
		return time.Time{}, false, fmt.Errorf("spot instance-action: %w", err)
	}
	switch action.Action {
	case "terminate", "stop", "hibernate":
	default:
		return time.Time{}, false, nil
	}
	at, _ := time.Parse(time.RFC3339, action.Time)
	return at, true, nil
}

// TargetLifecycleState is the Auto Scaling lifecycle state the VM is moving to, such as
// "InService" or "Terminated". It is empty for a VM outside an Auto Scaling group.
func (m *IMDS) TargetLifecycleState(ctx context.Context) (string, error) {
	body, err := m.Get(ctx, "meta-data/autoscaling/target-lifecycle-state")
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return strings.TrimSpace(body), err
}

// Retrieve returns the VM role's credentials, so IMDS is a Provider.
func (m *IMDS) Retrieve(ctx context.Context) (Credentials, error) {
	roles, err := m.Get(ctx, "meta-data/iam/security-credentials/")
	if err != nil {
		return Credentials{}, err
	}
	role, _, _ := strings.Cut(strings.TrimSpace(roles), "\n")
	if role == "" {
		return Credentials{}, errors.New("imds: the VM has no role")
	}
	body, err := m.Get(ctx, "meta-data/iam/security-credentials/"+role)
	if err != nil {
		return Credentials{}, err
	}
	var c struct {
		Code            string
		Message         string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      time.Time
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return Credentials{}, fmt.Errorf("imds credentials: %w", err)
	}
	if c.Code != "Success" {
		return Credentials{}, fmt.Errorf("imds credentials: %s: %s", c.Code, c.Message)
	}
	return Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey,
		SessionToken: c.Token, Expires: c.Expiration}, nil
}

// Identity is the instance identity document AWS signs, which the agent shows the
// scheduler to prove which VM it is.
type Identity struct {
	// Document is the raw document.
	Document string
	// Signature is the document's RSA SHA-256 signature, in base64 without newlines.
	Signature  string
	InstanceID string
	Region     string
	Zone       string
	PrivateIP  string
}

func (m *IMDS) Identity(ctx context.Context) (Identity, error) {
	doc, err := m.Get(ctx, "dynamic/instance-identity/document")
	if err != nil {
		return Identity{}, err
	}
	// This reads the plain RSA SHA-256 signature, which, unlike the PKCS7 ones, needs nothing
	// but crypto/rsa and the region's AWS certificate to check.
	sig, err := m.Get(ctx, "dynamic/instance-identity/signature")
	if err != nil {
		return Identity{}, err
	}
	var fields struct {
		InstanceID string `json:"instanceId"`
		Region     string `json:"region"`
		Zone       string `json:"availabilityZone"`
		PrivateIP  string `json:"privateIp"`
	}
	if err := json.Unmarshal([]byte(doc), &fields); err != nil {
		return Identity{}, fmt.Errorf("identity document: %w", err)
	}
	return Identity{
		Document: doc, Signature: strings.Join(strings.Fields(sig), ""),
		InstanceID: fields.InstanceID, Region: fields.Region, Zone: fields.Zone, PrivateIP: fields.PrivateIP,
	}, nil
}
