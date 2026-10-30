// Package credentials serves each replica its component role's credentials, the way
// Amazon ECS does, so the AWS SDKs find them with no code changes
// (docs/identity.md#how-replicas-get-credentials).
//
// Each replica gets an unguessable token in AWS_CONTAINER_CREDENTIALS_RELATIVE_URI. The
// firewall redirects its requests for 169.254.170.2 to this server, which answers only
// that token, and only from the replica's own address once the agent knows it.
package credentials

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
)

// Entry is one replica allowed credentials.
type Entry struct {
	Token     string
	Replica   string
	Component string
	Role      string // It is the component role's ARN.
	// Addrs are the replica's addresses. They are empty until the agent learns them.
	Addrs []string
}

// Assumer takes on a role.
type Assumer interface {
	AssumeRole(ctx context.Context, role, session string) (aws.Credentials, error)
}

type Options struct {
	// Prefix is the URL path in front of a token. It defaults to /v1/credentials/.
	Prefix string
	// Refresh is how far ahead of their expiry credentials are refreshed. It defaults to 15m.
	Refresh time.Duration
	// Now is the clock. It defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Server serves credentials.
type Server struct {
	assumer Assumer
	opts    Options

	mu      sync.Mutex
	entries map[string]Entry      // It is keyed by replica.
	roles   map[string]*roleCache // It is keyed by role ARN.
}

// roleCache caches one role's credentials. Its lock is held while assuming, so concurrent
// requests share one call to STS.
type roleCache struct {
	mu    sync.Mutex
	creds aws.Credentials
}

func NewServer(assumer Assumer, opts Options) *Server {
	if opts.Prefix == "" {
		opts.Prefix = "/v1/credentials/"
	}
	if opts.Refresh == 0 {
		opts.Refresh = 15 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{assumer: assumer, opts: opts, entries: map[string]Entry{}, roles: map[string]*roleCache{}}
}

// NewToken returns a random token for a replica.
func NewToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Put adds a replica, or replaces it.
func (s *Server) Put(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[e.Replica] = e
	if _, ok := s.roles[e.Role]; !ok {
		s.roles[e.Role] = &roleCache{}
	}
}

// Remove stops serving a replica.
func (s *Server) Remove(replica string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, replica)
}

// Replicas lists the replicas served, sorted.
func (s *Server) Replicas() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.entries))
	for r := range s.entries {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

func (s *Server) lookup(token string) (Entry, *roleCache, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if subtle.ConstantTimeCompare([]byte(e.Token), []byte(token)) == 1 {
			return e, s.roles[e.Role], true
		}
	}
	return Entry{}, nil, false
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.URL.Path, s.opts.Prefix)
	if !ok || token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	e, rl, ok := s.lookup(token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if len(e.Addrs) > 0 {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || !slices.Contains(e.Addrs, ip) {
			s.opts.Logger.Warn("credentials asked for from the wrong address", "replica", e.Replica, "from", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	creds, err := s.credentials(r.Context(), e.Role, e.Component, rl)
	if err != nil {
		s.opts.Logger.Error("assuming a component role failed", "replica", e.Replica, "role", e.Role, "err", err)
		http.Error(w, "credentials unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"AccessKeyId":     creds.AccessKeyID,
		"SecretAccessKey": creds.SecretAccessKey,
		"Token":           creds.SessionToken,
		"Expiration":      creds.Expires.UTC().Format(time.RFC3339),
		"RoleArn":         e.Role,
	})
}

// RoleCredentials returns a component role's credentials for the agent itself, which reads
// the component's secret files with them. It shares the cache its replicas are served from.
func (s *Server) RoleCredentials(ctx context.Context, role, component string) (aws.Credentials, error) {
	s.mu.Lock()
	rl, ok := s.roles[role]
	if !ok {
		rl = &roleCache{}
		s.roles[role] = rl
	}
	s.mu.Unlock()
	return s.credentials(ctx, role, component, rl)
}

// credentials returns the role's credentials, assuming it again when they are close to
// expiring. Credentials that haven't expired survive a failed refresh.
func (s *Server) credentials(ctx context.Context, role, component string, rl *roleCache) (aws.Credentials, error) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := s.opts.Now()
	if !rl.creds.ExpiresWithin(now, s.opts.Refresh) {
		return rl.creds, nil
	}
	fresh, err := s.assumer.AssumeRole(ctx, role, component)
	if err != nil {
		if !rl.creds.ExpiresWithin(now, 0) {
			return rl.creds, nil
		}
		return aws.Credentials{}, err
	}
	rl.creds = fresh
	return fresh, nil
}
