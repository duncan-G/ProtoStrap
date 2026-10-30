// Package ui is the operator UI (docs/protostrap.md#operator-ui): a read-only page that
// shows what the scheduler knows, what it decided and why, the infra log and the reporter's
// reports, and the logs API that operators hand to agents such as Claude.
//
// The scheduler serves it on data-1's loopback only. It has no sign-in of its own:
// operators reach it through an SSM port forward, which their IAM sign-in authorizes, so
// it opens no port and changes nothing in the network. The port is the role: Platform
// Admins' session document forwards one port, Org Admins' another, and each listener serves
// its role. Developers may start neither. It refuses any request that isn't from loopback or isn't for a loopback name, so a
// page elsewhere can't read it through the operator's forward, and it changes nothing:
// deploys and rollbacks stay with the deploy workflow.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/scheduler"
)

// ErrNotFound means the report doesn't exist.
var ErrNotFound = errors.New("not found")

// Report is one of the reporter's daily reports, reports/<date>.md in the deploy bucket.
type Report struct {
	Date     string    `json:"date"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Reports reads the reporter's reports.
type Reports interface {
	Reports(ctx context.Context) ([]Report, error)
	Report(ctx context.Context, date string) ([]byte, error) // It returns ErrNotFound if there is none.
}

// Role is who a listener serves. The UI has no sign-in of its own: the SSM session
// document an operator may start decides which port they reach, and so their role.
type Role string

const (
	RolePlatformAdmin Role = "platform_admin"
	RoleOrgAdmin      Role = "org_admin"
)

// Logs is the infra log.
type Logs interface {
	Query(ctx context.Context, f infralog.Filter) (infralog.Result, error)
}

// Server is the UI's HTTP handler. It serves these routes:
//
//	GET /                    the page, with /app.js, /app.css and /favicon.svg
//	GET /api/status          the scheduler's status, as GET /v1/status gives it
//	GET /api/logs            the infra log, filtered (see /api/help)
//	GET /api/help            the API, for agents, as markdown
//	GET /api/whoami          the listener's role
//	GET /api/reports         the reporter's reports, newest first
//	GET /api/reports/<date>  one report, as markdown
type Server struct {
	Status func() scheduler.Status
	// Role is who reaches this listener. It defaults to RolePlatformAdmin. Every role sees the
	// same read-only view for now.
	Role       Role
	Logs       Logs          // It is optional.
	Reports    Reports       // It is optional.
	Logger     *slog.Logger  // It defaults to slog.Default().
	Timeout    time.Duration // It bounds reading reports, and defaults to 10s.
	LogTimeout time.Duration // It bounds a logs query, and defaults to 30s.
	Now        func() time.Time
}

//go:embed static
var static embed.FS

var assets = map[string]string{
	"/":            "text/html; charset=utf-8",
	"/app.js":      "text/javascript; charset=utf-8",
	"/app.css":     "text/css; charset=utf-8",
	"/favicon.svg": "image/svg+xml",
}

// Everything comes from the scheduler: the page runs no inline script and loads nothing from
// elsewhere.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

var reportDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")

	switch {
	case !loopbackAddr(r.RemoteAddr):
		s.log().Warn("an operator UI request from beyond loopback", "from", r.RemoteAddr)
		fail(w, http.StatusForbidden, errors.New("the operator UI is for loopback only: use an SSM port forward"))
		return
	case !loopbackName(r.Host):
		fail(w, http.StatusForbidden, errors.New("open the operator UI as localhost"))
		return
	case r.Header.Get("Sec-Fetch-Site") == "cross-site":
		fail(w, http.StatusForbidden, errors.New("no cross-site requests"))
		return
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		h.Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, errors.New("the operator UI is read-only"))
		return
	}

	path := r.URL.Path
	if typ, ok := assets[path]; ok {
		name := "static" + path
		if path == "/" {
			name = "static/index.html"
		}
		body, err := static.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		h.Set("Content-Type", typ)
		w.Write(body)
		return
	}
	switch {
	case path == "/api/status":
		reply(w, s.Status())
	case path == "/api/logs":
		s.logs(w, r)
	case path == "/api/help":
		s.help(w, r)
	case path == "/api/whoami":
		reply(w, map[string]Role{"role": s.role()})
	case path == "/api/reports":
		s.reports(w, r)
	case strings.HasPrefix(path, "/api/reports/"):
		s.report(w, r, strings.TrimPrefix(path, "/api/reports/"))
	default:
		fail(w, http.StatusNotFound, errors.New("no such page"))
	}
}

func (s *Server) context(r *http.Request) (context.Context, context.CancelFunc) {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(r.Context(), timeout)
}

func (s *Server) role() Role {
	if s.Role == "" {
		return RolePlatformAdmin
	}
	return s.Role
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Server) reports(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Reports []Report `json:"reports"`
	}{Reports: []Report{}}
	if s.Reports != nil {
		ctx, cancel := s.context(r)
		defer cancel()
		list, err := s.Reports.Reports(ctx)
		if err != nil {
			s.log().Warn("listing reports", "err", err)
			fail(w, http.StatusBadGateway, err)
			return
		}
		out.Reports = append(out.Reports, list...)
		slices.SortFunc(out.Reports, func(a, b Report) int { return strings.Compare(b.Date, a.Date) })
	}
	reply(w, out)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, date string) {
	if _, err := time.Parse(time.DateOnly, date); err != nil || !reportDate.MatchString(date) {
		fail(w, http.StatusBadRequest, errors.New("want a report's date, such as 2026-10-25"))
		return
	}
	if s.Reports == nil {
		fail(w, http.StatusNotFound, ErrNotFound)
		return
	}
	ctx, cancel := s.context(r)
	defer cancel()
	body, err := s.Reports.Report(ctx, date)
	switch {
	case errors.Is(err, ErrNotFound):
		fail(w, http.StatusNotFound, errors.New("no report for "+date))
	case err != nil:
		s.log().Warn("reading a report", "date", date, "err", err)
		fail(w, http.StatusBadGateway, err)
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write(body)
	}
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

// loopbackName reports whether a Host header names loopback: localhost or a loopback address
// such as 127.0.0.1 or [::1], with any port, since the operator picks the forward's local port.
func loopbackName(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
