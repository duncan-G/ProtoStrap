package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/duncan-G/protostrap/internal/infralog"
)

// logParams are the parameters GET /api/logs takes. Any other is refused, so a misspelt
// one isn't quietly ignored.
var logParams = []string{"component", "source", "vm", "level", "q", "since", "until", "after", "limit", "snapshots", "format"}

// defaultSince is how far back a query looks without since.
const defaultSince = time.Hour

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	f, text, err := s.filter(r)
	if err != nil {
		fail(w, http.StatusBadRequest, fmt.Errorf("%w. See /api/help", err))
		return
	}
	res := infralog.Result{Records: []infralog.Record{}}
	if s.Logs != nil {
		timeout := s.LogTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		res, err = s.Logs.Query(ctx, f)
		switch {
		case errors.Is(err, infralog.ErrRange):
			fail(w, http.StatusBadRequest, fmt.Errorf("%w. See /api/help", err))
			return
		case err != nil:
			s.log().Warn("querying the infra log", "err", err)
			fail(w, http.StatusBadGateway, err)
			return
		}
	}
	if text {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		infralog.WriteText(w, res)
		return
	}
	reply(w, res)
}

// filter reads a logs query's parameters, and whether it wants text.
func (s *Server) filter(r *http.Request) (infralog.Filter, bool, error) {
	q := r.URL.Query()
	for k, v := range q {
		if !slices.Contains(logParams, k) {
			return infralog.Filter{}, false, fmt.Errorf("unknown parameter %q: want one of %s", k, strings.Join(logParams, ", "))
		}
		if len(v) > 1 {
			return infralog.Filter{}, false, fmt.Errorf("%s: given %d times, want it once", k, len(v))
		}
	}
	now := s.now()
	f := infralog.Filter{Component: q.Get("component"), VM: q.Get("vm"), Contains: q.Get("q"), Limit: infralog.DefaultLimit}
	if v := q.Get("source"); v != "" {
		if v != infralog.SourceScheduler && v != infralog.SourceAgent {
			return f, false, fmt.Errorf("source %q: want scheduler or agent", v)
		}
		f.Source = v
	}
	if v := q.Get("level"); v != "" {
		switch strings.ToLower(v) {
		case "debug", "info", "warn", "warning", "error":
			f.Level = v
		default:
			return f, false, fmt.Errorf("level %q: want debug, info, warn or error", v)
		}
	}
	var err error
	if v := q.Get("after"); v != "" {
		if f.After, err = strconv.ParseInt(v, 10, 64); err != nil || f.After < 0 {
			return f, false, fmt.Errorf("after %q: want the seq of a record, such as a response's latest", v)
		}
	}
	if v := q.Get("limit"); v != "" {
		if f.Limit, err = strconv.Atoi(v); err != nil || f.Limit < 1 || f.Limit > infralog.MaxLimit {
			return f, false, fmt.Errorf("limit %q: want 1 to %d", v, infralog.MaxLimit)
		}
	}
	if v := q.Get("snapshots"); v != "" {
		if f.Snapshots, err = strconv.ParseBool(v); err != nil {
			return f, false, fmt.Errorf("snapshots %q: want true or false", v)
		}
	}
	switch v := q.Get("since"); {
	case v != "":
		if f.Since, err = when(v, now); err != nil {
			return f, false, fmt.Errorf("since %q: %w", v, err)
		}
	case f.After == 0:
		// A tail follows the log wherever it is; a query looks back an hour.
		f.Since = now.Add(-defaultSince)
	}
	if v := q.Get("until"); v != "" {
		if f.Until, err = when(v, now); err != nil {
			return f, false, fmt.Errorf("until %q: %w", v, err)
		}
		if !f.Since.IsZero() && !f.Until.After(f.Since) {
			return f, false, fmt.Errorf("until %q is before since", v)
		}
	}
	var text bool
	switch v := q.Get("format"); v {
	case "text":
		text = true
	case "json":
	case "":
		accept := r.Header.Get("Accept")
		text = strings.Contains(accept, "text/plain") && !strings.Contains(accept, "application/json")
	default:
		return f, false, fmt.Errorf("format %q: want json or text", v)
	}
	return f, text, nil
}

var ago = regexp.MustCompile(`^(\d+)d$`)

// when reads a time: a duration back from now, such as 30m, 2h or 3d, or a time such as
// 2026-10-29T08:00:00Z.
func when(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	var d time.Duration
	if m := ago.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		d = time.Duration(n) * 24 * time.Hour
	} else if p, err := time.ParseDuration(v); err == nil {
		d = p
	} else {
		return time.Time{}, errors.New("want a duration back from now, such as 30m, 2h or 3d, or a time such as 2026-10-29T08:00:00Z")
	}
	if d <= 0 {
		return time.Time{}, errors.New("want a duration greater than 0")
	}
	return now.Add(-d), nil
}

func (s *Server) help(w http.ResponseWriter, r *http.Request) {
	st := s.Status()
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	helpPage.Execute(w, map[string]any{
		"Base": (&url.URL{Scheme: "http", Host: r.Host}).String(),
		"App":  st.App, "Environment": st.Environment, "Role": s.role(),
		"Limit": infralog.DefaultLimit, "MaxLimit": infralog.MaxLimit,
	})
}

// helpPage is GET /api/help: enough for an agent, such as Claude, to query the infra log on
// its own.
var helpPage = template.Must(template.New("help").Parse(`# ProtoStrap operator API: {{.App}} {{.Environment}}

This is ProtoStrap's operator UI on data-1, reached through an SSM port forward that your
AWS sign-in allowed, as {{.Role}}. It is read-only: it answers GET only, and changes
nothing. Times are UTC. Errors are JSON, {"error": "..."}: 400 for a bad parameter, which
the message names, and 502 when the deploy bucket can't be read.

## GET /api/logs

The infra log: ProtoStrap's own records, not the services'. They are the scheduler's
decisions, each with its reason, and every agent's records of its VM and of the replicas
it starts, stops and backs up.

A service's own output, traces and metrics aren't here: they are in telemetry's Grafana.
Its forward is scripts/telemetry.sh, a separate port, and its API is described at
http://localhost:7482/protostrap/help. A replica named here, such as api-x7k2qa, is
service_instance_id there. Replicas started before the environment had telemetry log to
CloudWatch Logs instead, in /protostrap/{{.App}}/<component>.

Every parameter is optional:

| Parameter | Meaning | Default |
| --------- | ------- | ------- |
| component | Records about one component, from the scheduler and the agents | Every component |
| source    | scheduler or agent | Both |
| vm        | A VM's name, such as general-3 or data-1, or its instance ID | Every VM |
| level     | The lowest level: debug, info, warn or error | Every level |
| q         | Text in the message or an attribute's value, in any case | |
| since     | How far back: a duration such as 30m, 2h or 3d, or a time such as 2026-10-29T08:00:00Z | 1h |
| until     | Where to stop, the same way | Now |
| limit     | The most records, 1 to {{.MaxLimit}}. The newest are returned, oldest first | {{.Limit}} |
| after     | Only the records newer than this seq, oldest first: to follow the log, pass the last response's latest | |
| snapshots | true adds the scheduler's signal snapshots, one a minute; with component, only its part | false |
| format    | json, or text for one line per record | json |

The last day comes from memory. Up to 7 days back comes from the deploy bucket, which is
slower; a query may cover at most 7 days.

Examples:

    curl -s '{{.Base}}/api/logs?component=api&since=2h&format=text'
    curl -s '{{.Base}}/api/logs?component=api&level=warn&since=3d&format=text'
    curl -s '{{.Base}}/api/logs?vm=general-3&since=30m&format=text'
    curl -s '{{.Base}}/api/logs?component=api&snapshots=true&since=1h&format=text'
    curl -s '{{.Base}}/api/logs?source=scheduler&q=release&since=1d&format=text'

A text line is the time, level, source, VM, component (or -), message and attributes:

    2026-10-29T08:30:00.000Z ERROR agent     general-3 api replica failed replica=api-x7k2qa reason="exited with code 137"

A last line starting with # says more records matched than were shown: narrow the window
or the filters, or raise limit.

JSON is {"records": [...], "latest": <seq>, "more": <bool>, "from": <time>, "to": <time>},
each record {"seq", "time", "level", "source", "vm", "vm_name", "component", "msg",
"attrs": [{"key", "value"}]}.

Messages worth knowing:

- desired replicas: the scheduler changed a component's replica count; reason says why
  (a signal, or limited by a limit).
- replica placed, stopping a replica: where replicas go, and why they stop.
- replica failed: reason says how. Exit code 137 is usually out of memory.
- release deployed, release finished, release failed: rolling back.
- adding VMs, removing a VM: moving its replicas off first, VM is silent: placing its
  replicas elsewhere, VM gone.
- signals: a snapshot of every component's signals, with snapshots=true.
- From agents: starting a replica, stopping a replica, backing up, backup failed, and
  their errors.

## Other routes

- GET /api/status: what runs where. Each component's desired and ready replicas, its
  signals and limits, the pools and VMs, every replica and the release, as JSON.
- GET /api/reports: the reporter's daily reports. GET /api/reports/<date> is one, as
  markdown.
- GET /api/whoami: the role this port serves, platform_admin or org_admin.
`))
