package infralog

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// These bound what an agent's sync may add.
const (
	maxPerSync = 1000
	maxMsg     = 4096
	maxValue   = 2048
	maxKey     = 128
	maxAttrs   = 32
)

// These bound a query.
const (
	DefaultLimit = 200
	MaxLimit     = 5000
)

// ErrRange is a query over a longer window than the log reads.
var ErrRange = errors.New("a query covers at most 7 days")

// Log is the scheduler's infra log: the last day of records in memory, by hour, and, with
// an archive, every hour also in the deploy bucket. Its methods are safe to call at once.
type Log struct {
	// Archive is where records outlive the scheduler. Without one, they live only in memory.
	Archive *Archive
	// Window is how long records are kept in memory. It defaults to a day.
	Window time.Duration
	// HourLimit is the most records kept for one hour, so a crash loop can't push everything
	// else out of memory. It defaults to 20,000.
	HourLimit int
	// Limit is the most records kept in memory in all, since data-1 runs stateful
	// components too. Past it, the oldest hours already in the archive leave memory early,
	// and queries read them from there. It defaults to 100,000.
	Limit int
	// MaxRange is the longest window a query may cover. It defaults to 7 days.
	MaxRange time.Duration
	// Every is how often Run writes to the archive. It defaults to a minute.
	Every time.Duration
	// Now is the clock. It defaults to time.Now.
	Now func() time.Time
	// Logger hears when the archive can't be written. It is optional.
	Logger *slog.Logger

	mu    sync.Mutex
	ready bool
	seq   int64
	hours map[time.Time]*hour
	// complete is when memory starts holding every record: when the log started, or as far
	// back as Load read, and later as hours leave memory.
	complete time.Time
}

// hour is one hour's records in memory. own are this run's, which it writes to its own
// archive object; loaded came from the archive, written by earlier runs.
type hour struct {
	start  time.Time
	own    []Record
	loaded []Record
	dirty  bool
	full   bool
}

func (l *Log) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *Log) window() time.Duration { return cmp.Or(l.Window, 24*time.Hour) }

func (l *Log) maxRange() time.Duration { return cmp.Or(l.MaxRange, 7*24*time.Hour) }

func (l *Log) hourLimit() int { return cmp.Or(l.HourLimit, 20000) }

func (l *Log) limit() int { return cmp.Or(l.Limit, 100000) }

// setup readies the log on first use. The caller holds mu.
func (l *Log) setup(now time.Time) {
	if !l.ready {
		l.hours = map[time.Time]*hour{}
		l.complete = now
		l.ready = true
	}
}

// Handler records each record at info and above from source on the VM vm, named name, and
// passes every record on to next.
func (l *Log) Handler(next slog.Handler, source, vm, name string) slog.Handler {
	return &handler{next: next, add: func(r slog.Record, msg string, attrs []api.LogAttr) {
		l.mu.Lock()
		defer l.mu.Unlock()
		now := l.now()
		l.setup(now)
		l.add(now, Record{Time: now, Level: levelName(r.Level), Source: source, VM: vm, VMName: name, Msg: msg, Attrs: attrs}, false)
	}}
}

// AddAgent adds the records an agent sent with its report. vm is the instance ID the agent
// proved, and name the VM's name. Each record is kept within bounds, and stamped when it
// arrives if it has no time or a time from a fast clock.
func (l *Log) AddAgent(vm, name string, records []api.LogRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.setup(now)
	for i, rec := range records {
		if i == maxPerSync {
			break
		}
		at := rec.Time.UTC()
		if rec.Time.IsZero() || at.After(now.Add(time.Minute)) {
			at = now
		}
		r := Record{Time: at, Level: normalLevel(rec.Level), Source: SourceAgent, VM: vm, VMName: name, Msg: clip(rec.Msg, maxMsg)}
		for j, a := range rec.Attrs {
			if j == maxAttrs {
				break
			}
			r.Attrs = append(r.Attrs, api.LogAttr{Key: clip(a.Key, maxKey), Value: clip(a.Value, maxValue)})
		}
		l.add(now, r, false)
	}
}

// add keeps a record, unless it is older than the window or its hour is full. The caller
// holds mu.
func (l *Log) add(now time.Time, r Record, loaded bool) {
	if r.Time.Before(now.Add(-l.window())) {
		return
	}
	start := r.Time.Truncate(time.Hour)
	h := l.hours[start]
	if h == nil {
		h = &hour{start: start}
		l.hours[start] = h
	}
	if !loaded && len(h.own)+len(h.loaded) >= l.hourLimit() {
		if h.full {
			return
		}
		h.full = true
		r = Record{Time: r.Time, Level: "WARN", Source: SourceScheduler, Msg: "infra log hour full: dropping its records until the next hour",
			Attrs: []api.LogAttr{{Key: "hour", Value: start.Format(time.RFC3339)}, {Key: "limit", Value: fmt.Sprint(l.hourLimit())}}}
	}
	l.seq++
	r.Seq = l.seq
	if r.Component == "" {
		r.Component = componentOf(r.Attrs)
	}
	if loaded {
		h.loaded = append(h.loaded, r)
	} else {
		h.own = append(h.own, r)
		h.dirty = l.Archive != nil
	}
	l.evict(now)
}

// evict drops the hours older than the window, and then, while memory holds more than its
// limit, the oldest hours but the newest. An hour not yet written to the archive waits for
// it, for up to another window. The caller holds mu.
func (l *Log) evict(now time.Time) {
	cut := now.Add(-l.window())
	held := 0
	for start, h := range l.hours {
		end := start.Add(time.Hour)
		if end.After(cut) || (h.dirty && end.After(cut.Add(-l.window()))) {
			held += len(h.own) + len(h.loaded)
			continue
		}
		l.drop(start)
	}
	if held <= l.limit() {
		return
	}
	starts := slices.SortedFunc(maps.Keys(l.hours), time.Time.Compare)
	for _, start := range starts[:len(starts)-1] {
		if held <= l.limit() {
			break
		}
		if h := l.hours[start]; !h.dirty {
			held -= len(h.own) + len(h.loaded)
			l.drop(start)
		}
	}
}

// drop removes an hour from memory: queries before its end read the archive. The caller
// holds mu.
func (l *Log) drop(start time.Time) {
	delete(l.hours, start)
	if end := start.Add(time.Hour); end.After(l.complete) {
		l.complete = end
	}
}

// Filter says which records a query wants. Its zero value is every record of the last day
// but signal snapshots, up to DefaultLimit.
type Filter struct {
	// Component matches records about that component, from the scheduler and the agents.
	Component string
	// Source is SourceScheduler or SourceAgent.
	Source string
	// VM matches a VM's instance ID or name: its agent's records, and the scheduler's about it.
	VM string
	// Level is the lowest level wanted: debug, info, warn or error, in any case.
	Level string
	// Contains matches a message or attribute value, in any case.
	Contains string
	// Since and Until bound the records' times. Since defaults to the memory's window, Until
	// to now.
	Since, Until time.Time
	// After asks for the records the log got after that sequence number, oldest first, from
	// memory: a tail.
	After int64
	// Limit is the most records returned, up to MaxLimit. It defaults to DefaultLimit.
	Limit int
	// Snapshots adds the scheduler's signal snapshots, one a minute. With a component, each
	// shows only that component's part.
	Snapshots bool
}

// Result is a query's records, in time order.
type Result struct {
	Records []Record `json:"records"`
	// Latest is the log's latest sequence number, for a tail to ask after next.
	Latest int64 `json:"latest"`
	// More says more records matched than Limit allowed. A query returns the newest of them;
	// a tail, the oldest.
	More bool `json:"more"`
	// From and To are the window the query covered.
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// Query returns the records the filter matches. Records older than memory holds come from
// the archive.
func (l *Log) Query(ctx context.Context, f Filter) (Result, error) {
	lowest := slog.LevelDebug
	if f.Level != "" {
		lv, ok := levels[strings.ToUpper(f.Level)]
		if !ok {
			return Result{}, fmt.Errorf("level %q: want debug, info, warn or error", f.Level)
		}
		lowest = lv
	}
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	now := l.now()
	until, since := f.Until, f.Since
	if until.IsZero() {
		until = now
	}
	if since.IsZero() {
		since = now.Add(-l.window())
	}
	if d := until.Sub(since); d > l.maxRange() {
		return Result{}, fmt.Errorf("%w: this one covers %s", ErrRange, d.Round(time.Hour))
	}

	l.mu.Lock()
	l.setup(now)
	latest, complete := l.seq, l.complete
	fromArchive := l.Archive != nil && f.After == 0 && since.Before(complete)
	memSince := since
	if fromArchive {
		memSince = complete
	}
	var out []Record
	for start, h := range l.hours {
		if !start.Add(time.Hour).After(memSince) || start.After(until) {
			continue
		}
		for _, rs := range [][]Record{h.own, h.loaded} {
			for i := range rs {
				r := &rs[i]
				if r.Seq <= f.After || r.Time.Before(memSince) || r.Time.After(until) {
					continue
				}
				if m, ok := f.match(r, lowest); ok {
					out = append(out, m)
				}
			}
		}
	}
	l.mu.Unlock()

	if fromArchive {
		recs, err := l.Archive.Read(ctx, since, complete)
		if err != nil {
			return Result{}, fmt.Errorf("reading the infra log's archive: %w", err)
		}
		for i := range recs {
			if m, ok := f.match(&recs[i], lowest); ok {
				out = append(out, m)
			}
		}
	}
	sortRecords(out)
	res := Result{Latest: latest, From: since, To: until, More: len(out) > limit}
	switch {
	case !res.More:
	case f.After > 0:
		out = out[:limit]
	default:
		out = out[len(out)-limit:]
	}
	res.Records = append([]Record{}, out...)
	return res, nil
}

func sortRecords(rs []Record) {
	slices.SortStableFunc(rs, func(a, b Record) int {
		return cmp.Or(a.Time.Compare(b.Time), cmp.Compare(a.Seq, b.Seq))
	})
}

// isSnapshot reports whether a record is one of the scheduler's signal snapshots.
func isSnapshot(r *Record) bool { return r.Source == SourceScheduler && r.Msg == "signals" }

// match reports whether the filter wants r, and the record to return for it: r itself, or a
// snapshot cut to the filter's component.
func (f Filter) match(r *Record, lowest slog.Level) (Record, bool) {
	if f.Source != "" && r.Source != f.Source {
		return Record{}, false
	}
	if rank(r.Level) < lowest {
		return Record{}, false
	}
	if f.VM != "" && r.VM != f.VM && r.VMName != f.VM && r.attr("vm") != f.VM {
		return Record{}, false
	}
	m := *r
	switch {
	case isSnapshot(r):
		if !f.Snapshots {
			return Record{}, false
		}
		if f.Component != "" {
			var ok bool
			if m, ok = snapshotOf(*r, f.Component); !ok {
				return Record{}, false
			}
		}
	case f.Component != "" && r.Component != f.Component:
		return Record{}, false
	}
	if f.Contains != "" && !contains(m, strings.ToLower(f.Contains)) {
		return Record{}, false
	}
	return m, true
}

func contains(r Record, q string) bool {
	if strings.Contains(strings.ToLower(r.Msg), q) {
		return true
	}
	for _, a := range r.Attrs {
		if strings.Contains(strings.ToLower(a.Value), q) {
			return true
		}
	}
	return false
}

// snapshotOf cuts a signal snapshot to one component's part: its signals, and how many of
// its replicas wait for a VM. It reads the snapshot the scheduler logs:
//
//	components: api desired=2 ready=2 cpu=40%; jobs desired=1 ...
//	waiting:    api=1 jobs=1
func snapshotOf(r Record, component string) (Record, bool) {
	var part string
	for _, p := range strings.Split(r.attr("components"), ";") {
		p = strings.TrimSpace(p)
		if name, _, _ := strings.Cut(p, " "); name == component {
			part = p
		}
	}
	if part == "" {
		return Record{}, false
	}
	out := r
	out.Component = component
	out.Attrs = []api.LogAttr{{Key: "components", Value: part}}
	for _, w := range strings.Fields(r.attr("waiting")) {
		if name, _, _ := strings.Cut(w, "="); name == component {
			out.Attrs = append(out.Attrs, api.LogAttr{Key: "waiting", Value: w})
		}
	}
	return out, true
}
