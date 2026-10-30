// Package infralog is the infra log (docs/protostrap.md#infra-log): ProtoStrap's own log
// records, the scheduler's and every agent's, apart from the services' own logs. The
// scheduler keeps the last day of them in memory, for the operator UI and its logs API, and
// writes them to the deploy bucket, where they outlive the scheduler and its VMs and where
// the reporter reads them.
package infralog

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/duncan-G/protostrap/internal/api"
)

// These are where records come from.
const (
	SourceScheduler = "scheduler"
	SourceAgent     = "agent"
)

// Record is one record of the infra log.
type Record struct {
	// Seq is the order the scheduler got the record in, for tailing the log. Records read
	// from the archive have none.
	Seq   int64     `json:"seq,omitempty"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	// Source is SourceScheduler or SourceAgent.
	Source string `json:"source"`
	// VM is the instance ID of the VM the record came from, and VMName its name, such as
	// general-3 or data-1.
	VM     string `json:"vm,omitempty"`
	VMName string `json:"vm_name,omitempty"`
	// Component is the component the record is about, if any.
	Component string        `json:"component,omitempty"`
	Msg       string        `json:"msg"`
	Attrs     []api.LogAttr `json:"attrs,omitempty"`
}

func (r Record) attr(key string) string {
	for _, a := range r.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// componentOf is the component a record is about: its component attribute, or else the
// component of the replica it names. A replica's ID is its component's name, a dash and six
// characters (scheduler.randomID).
func componentOf(attrs []api.LogAttr) string {
	var replica string
	for _, a := range attrs {
		switch a.Key {
		case "component":
			return a.Value
		case "replica":
			replica = a.Value
		}
	}
	i := strings.LastIndexByte(replica, '-')
	if i <= 0 || len(replica)-i-1 != 6 {
		return ""
	}
	for _, c := range replica[i+1:] {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return replica[:i]
}

// levelName is a level as records carry it.
func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}

// levels ranks the level names, and the names a filter may use for them.
var levels = map[string]slog.Level{
	"DEBUG": slog.LevelDebug, "INFO": slog.LevelInfo, "WARN": slog.LevelWarn, "WARNING": slog.LevelWarn, "ERROR": slog.LevelError,
}

// normalLevel is an agent's level as one of the four names; anything else is INFO.
func normalLevel(s string) string {
	if l, ok := levels[strings.ToUpper(s)]; ok {
		return levelName(l)
	}
	return "INFO"
}

func rank(name string) slog.Level {
	if l, ok := levels[name]; ok {
		return l
	}
	return slog.LevelInfo
}

// clip cuts s to at most n bytes, on a character boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// handler is a slog handler that passes each record at info or above to add, as well as
// every record to next.
type handler struct {
	add  func(r slog.Record, msg string, attrs []api.LogAttr)
	next slog.Handler
	// attrs come from WithAttrs and are already prefixed.
	attrs []api.LogAttr
	// prefix has "group." for each group that WithGroup opened.
	prefix string
}

func (h *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.next.Enabled(ctx, level)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		attrs := append([]api.LogAttr(nil), h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			attrs = flatten(attrs, h.prefix, a)
			return true
		})
		h.add(r, r.Message, attrs)
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append([]api.LogAttr(nil), h.attrs...)
	for _, a := range attrs {
		c.attrs = flatten(c.attrs, h.prefix, a)
	}
	c.next = h.next.WithAttrs(attrs)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.prefix = h.prefix + name + "."
	c.next = h.next.WithGroup(name)
	return &c
}

func flatten(out []api.LogAttr, prefix string, a slog.Attr) []api.LogAttr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, g := range v.Group() {
			out = flatten(out, p, g)
		}
		return out
	}
	if a.Key == "" {
		return out
	}
	return append(out, api.LogAttr{Key: prefix + a.Key, Value: v.String()})
}
