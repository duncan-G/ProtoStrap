package reporter

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/spec"
)

// event is one of the scheduler's records.
type event struct {
	Time  time.Time
	Level string
	Msg   string
	Attrs map[string]string
}

// schedulerEvents are the scheduler's records, in time order.
func schedulerEvents(records []infralog.Record) []event {
	var out []event
	for _, r := range records {
		if r.Source != infralog.SourceScheduler {
			continue
		}
		e := event{Time: r.Time, Level: r.Level, Msg: r.Msg, Attrs: map[string]string{}}
		for _, a := range r.Attrs {
			e.Attrs[a.Key] = a.Value
		}
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b event) int { return a.Time.Compare(b.Time) })
	return out
}

// agentWarnings tallies the agents' warnings and errors, by message and component.
func agentWarnings(records []infralog.Record) []Tally {
	m := map[string]*Tally{}
	for _, r := range records {
		if r.Source != infralog.SourceAgent || (r.Level != "WARN" && r.Level != "ERROR") {
			continue
		}
		text := r.Msg
		if r.Component != "" {
			text += " (" + r.Component + ")"
		}
		t := m[text]
		if t == nil {
			t = &Tally{Text: text}
			m[text] = t
		}
		t.N++
		if r.Time.After(t.Last) {
			t.Last = r.Time
		}
	}
	return tallies(m)
}

// Facts is what the reporter found in a day of logs: the part of the report that isn't
// the LLM's, and what the LLM is given.
type Facts struct {
	From, To   time.Time
	Releases   []string
	Components []*ComponentFacts
	Pools      []*PoolFacts
	VMEvents   []string
	// Warnings tallies the scheduler's other warnings and errors.
	Warnings []Tally
	// AgentWarnings tallies the agents' warnings and errors.
	AgentWarnings []Tally
	Errors        []*LogErrors
	// ErrorsUnread says why the components' errors couldn't be read, when they couldn't.
	ErrorsUnread string
	// Flags note what looks worth changing.
	Flags     []string
	Snapshots int
}

// Tally is a message and how often it appeared.
type Tally struct {
	Text string
	N    int
	Last time.Time
}

// Stat is a signal over the day's snapshots.
type Stat struct {
	Sum  float64
	N    int
	Peak float64
}

func (s *Stat) add(v float64) {
	s.Sum += v
	s.N++
	s.Peak = max(s.Peak, v)
}

func (s Stat) Avg() float64 {
	if s.N == 0 {
		return 0
	}
	return s.Sum / float64(s.N)
}

// ComponentFacts is one component's day.
type ComponentFacts struct {
	Name string
	Kind string
	// Min and Max come from the spec, or are 0 if the component is unknown or doesn't scale.
	Min, Max int
	// Low and High are the fewest and the most desired replicas over the day.
	Low, High int
	seen      bool
	// Changes counts changes to desired replicas.
	Changes   int
	Snapshots int
	// AtMax counts snapshots at Max.
	AtMax int
	// CPU is the share of the CPU request used, in percent.
	CPU Stat
	// Requests is the request rate, per second.
	Requests         Stat
	QueuePeak        map[string]int
	PendingPeak      int
	PendingSnapshots int
	// Limited tallies decisions where a limit set desired replicas.
	Limited  []Tally
	Failed   int
	Failures []Tally
}

func (c *ComponentFacts) desired(n int) {
	if !c.seen {
		c.Low, c.High, c.seen = n, n, true
		return
	}
	c.Low, c.High = min(c.Low, n), max(c.High, n)
}

// PoolFacts is one pool's day.
type PoolFacts struct {
	Name string
	// Max comes from the spec.
	Max int
	// Low and High are the fewest and the most VMs over the day.
	Low, High  int
	seen       bool
	Snapshots  int
	AtMax      int
	WantedPeak int
	// Short counts snapshots in which the pool wanted more VMs than it had or was booting.
	Short          int
	Added, Removed int
}

// LogErrors is one component's error lines.
type LogErrors struct {
	Component string
	Lines     int
	Top       []Tally
}

func (f *Facts) component(name string) *ComponentFacts {
	for _, c := range f.Components {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (f *Facts) pool(name string) *PoolFacts {
	for _, p := range f.Pools {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// releaseMsgs and vmMsgs are messages the scheduler logs, by where each goes in the report.
var (
	releaseMsgs = map[string]bool{
		"release deployed": true, "release finished": true, "release superseded": true,
		"release failed: rolling back": true, "release failed, with no release to roll back to: running nothing": true,
	}
	vmMsgs = map[string]bool{
		"adding VMs": true, "removing a VM: moving its replicas off first": true, "VM drained: terminating it": true,
		"VM gone": true, "VM is silent: placing its replicas elsewhere": true,
		"marking a silent VM unhealthy, so its group replaces it": true, "cancelled launches that couldn't be filled": true,
		"removing a gone VM's swarm node": true, "scheduler started": true,
	}
)

// The thresholds for flags are long enough not to be set off by a VM booting or a burst.
const (
	atMaxShare      = 0.25 // It is a share of the day's snapshots.
	waitingFlag     = 10   // It counts snapshots, which are a minute apart.
	shortFlag       = 10
	failuresFlag    = 3
	cpuLowPeak      = 20.0 // It is a percent of the CPU request, at its peak.
	cpuHighAvg      = 100.0
	cpuMinSnapshots = 30
)

// gather builds the facts from the infra log's records, the error lines of every
// component's logs, and the spec, which may be nil.
func gather(records []infralog.Record, errLines []LogLine, sp *spec.Spec, from, to time.Time) *Facts {
	f := &Facts{From: from, To: to, AgentWarnings: agentWarnings(records)}
	events := schedulerEvents(records)
	comps := map[string]*ComponentFacts{}
	comp := func(name string) *ComponentFacts {
		if c, ok := comps[name]; ok {
			return c
		}
		c := &ComponentFacts{Name: name, QueuePeak: map[string]int{}}
		if sp != nil {
			if sc := sp.Components[name]; sc != nil {
				c.Kind = string(sc.Kind)
				if sc.Scale != nil {
					c.Min, c.Max = sc.Scale.Min, sc.Scale.Max
				}
			}
		}
		comps[name] = c
		return c
	}
	pools := map[string]*PoolFacts{}
	pool := func(name string) *PoolFacts {
		if p, ok := pools[name]; ok {
			return p
		}
		p := &PoolFacts{Name: name}
		if sp != nil {
			if sh := sp.Pool(name); sh != nil {
				p.Max = sh.Max
			}
		}
		pools[name] = p
		return p
	}
	limited := map[string]map[string]*Tally{}
	failures := map[string]map[string]*Tally{}
	warnings := map[string]*Tally{}
	silent := 0

	for _, e := range events {
		switch {
		case e.Msg == "signals":
			f.Snapshots++
			snapshot(e, comp, pool)
		case e.Msg == "desired replicas":
			c := comp(e.Attrs["component"])
			if from, err := strconv.Atoi(e.Attrs["from"]); err == nil && from != 0 {
				c.desired(from)
			}
			n, _ := strconv.Atoi(e.Attrs["to"])
			c.desired(n)
			c.Changes++
			if reason := e.Attrs["reason"]; strings.HasPrefix(reason, "limited") {
				count(limited, c.Name, reason, e.Time)
			}
		case e.Msg == "replica failed":
			c := comp(e.Attrs["component"])
			c.Failed++
			count(failures, c.Name, e.Attrs["reason"], e.Time)
		case releaseMsgs[e.Msg]:
			f.Releases = append(f.Releases, releaseLine(e))
			if strings.HasPrefix(e.Msg, "release failed") {
				f.Flags = append(f.Flags, fmt.Sprintf("Release %s failed: %s.", e.Attrs["release"], e.Attrs["reason"]))
			}
		case vmMsgs[e.Msg]:
			f.VMEvents = append(f.VMEvents, eventLine(e))
			switch e.Msg {
			case "adding VMs":
				pool(e.Attrs["pool"]).Added++
			case "removing a VM: moving its replicas off first":
				pool(e.Attrs["pool"]).Removed++
			case "VM is silent: placing its replicas elsewhere":
				silent++
			}
		case e.Level == "WARN" || e.Level == "ERROR":
			t := warnings[e.Msg]
			if t == nil {
				t = &Tally{Text: e.Msg}
				warnings[e.Msg] = t
			}
			t.N++
			t.Last = e.Time
		}
	}
	for name, byReason := range limited {
		comp(name).Limited = tallies(byReason)
	}
	for name, byReason := range failures {
		comp(name).Failures = tallies(byReason)
	}
	f.Warnings = tallies(warnings)
	f.Errors = logErrors(errLines)
	for _, name := range slices.Sorted(maps.Keys(comps)) {
		f.Components = append(f.Components, comps[name])
	}
	for _, name := range slices.Sorted(maps.Keys(pools)) {
		f.Pools = append(f.Pools, pools[name])
	}
	f.flag(silent)
	return f
}

// snapshot reads one signals line:
//
//	components: api desired=2 ready=2 cpu=40% requests=12.5/s queue[jobs]=5+2 limit=4; ...
//	pools:      general vms=2 booting=0 wanted=2 cpu=1500m; ...
//	waiting:    postgres=1 api=2
func snapshot(e event, comp func(string) *ComponentFacts, pool func(string) *PoolFacts) {
	for _, part := range split(e.Attrs["components"], ";") {
		fields := strings.Fields(part)
		c := comp(fields[0])
		c.Snapshots++
		for _, kv := range fields[1:] {
			k, v, _ := strings.Cut(kv, "=")
			switch {
			case k == "desired":
				n, _ := strconv.Atoi(v)
				c.desired(n)
				if c.Max > 0 && n >= c.Max {
					c.AtMax++
				}
			case k == "cpu":
				if x, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64); err == nil {
					c.CPU.add(x)
				}
			case k == "requests":
				if x, err := strconv.ParseFloat(strings.TrimSuffix(v, "/s"), 64); err == nil {
					c.Requests.add(x)
				}
			case strings.HasPrefix(k, "queue[") && strings.HasSuffix(k, "]"):
				q := k[len("queue[") : len(k)-1]
				visible, inFlight, _ := strings.Cut(v, "+")
				a, _ := strconv.Atoi(visible)
				b, _ := strconv.Atoi(inFlight)
				c.QueuePeak[q] = max(c.QueuePeak[q], a+b)
			}
		}
	}
	for _, part := range split(e.Attrs["pools"], ";") {
		fields := strings.Fields(part)
		p := pool(fields[0])
		p.Snapshots++
		v := map[string]int{}
		for _, kv := range fields[1:] {
			k, val, _ := strings.Cut(kv, "=")
			v[k], _ = strconv.Atoi(strings.TrimSuffix(val, "m"))
		}
		if !p.seen {
			p.Low, p.High, p.seen = v["vms"], v["vms"], true
		}
		p.Low, p.High = min(p.Low, v["vms"]), max(p.High, v["vms"])
		p.WantedPeak = max(p.WantedPeak, v["wanted"])
		if p.Max > 0 && v["vms"] >= p.Max {
			p.AtMax++
		}
		if v["wanted"] > v["vms"]+v["booting"] {
			p.Short++
		}
	}
	for _, kv := range strings.Fields(e.Attrs["waiting"]) {
		name, val, _ := strings.Cut(kv, "=")
		n, _ := strconv.Atoi(val)
		c := comp(name)
		c.PendingPeak = max(c.PendingPeak, n)
		c.PendingSnapshots++
	}
}

func split(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// flag notes what looks worth changing.
func (f *Facts) flag(silent int) {
	for _, c := range f.Components {
		if c.Max > 0 && c.Snapshots > 0 && float64(c.AtMax) >= atMaxShare*float64(c.Snapshots) {
			f.Flags = append(f.Flags, fmt.Sprintf("%s was at its maximum of %d replicas in %d%% of the day's snapshots: demand may be above its max.",
				c.Name, c.Max, c.AtMax*100/c.Snapshots))
		}
		if n := sumTallies(c.Limited); n > 0 {
			f.Flags = append(f.Flags, fmt.Sprintf("%s was held back by a limit %d times (%s).", c.Name, n, c.Limited[0].Text))
		}
		if c.PendingSnapshots >= waitingFlag {
			what := "its pool may be at its maximum"
			if c.Kind == string(api.KindStateful) {
				what = "its data VM is running short"
			}
			f.Flags = append(f.Flags, fmt.Sprintf("%s waited for a VM in %d snapshots, up to %d replicas: %s.", c.Name, c.PendingSnapshots, c.PendingPeak, what))
		}
		if c.Failed >= failuresFlag {
			f.Flags = append(f.Flags, fmt.Sprintf("%s had %d replica failures, most often %q.", c.Name, c.Failed, c.Failures[0].Text))
		}
		for _, t := range c.Failures {
			if strings.Contains(t.Text, "code 137") {
				f.Flags = append(f.Flags, fmt.Sprintf("%s was killed with exit code 137 %d times, usually out of memory: its memory request may be too small.", c.Name, t.N))
			}
		}
		if c.CPU.N >= cpuMinSnapshots {
			switch {
			case c.CPU.Avg() >= cpuHighAvg:
				f.Flags = append(f.Flags, fmt.Sprintf("%s averaged %.0f%% of its CPU request: the request may be too small.", c.Name, c.CPU.Avg()))
			case c.CPU.Peak < cpuLowPeak:
				f.Flags = append(f.Flags, fmt.Sprintf("%s used at most %.0f%% of its CPU request: the request may be too large.", c.Name, c.CPU.Peak))
			}
		}
	}
	for _, p := range f.Pools {
		if p.Max > 0 && p.AtMax > 0 {
			f.Flags = append(f.Flags, fmt.Sprintf("The %s pool was at its maximum of %d VMs in %d snapshots.", p.Name, p.Max, p.AtMax))
		}
		if p.Short >= shortFlag {
			f.Flags = append(f.Flags, fmt.Sprintf("The %s pool wanted more VMs than it had in %d snapshots.", p.Name, p.Short))
		}
	}
	if silent > 0 {
		f.Flags = append(f.Flags, fmt.Sprintf("%d VM went silent and was replaced.", silent))
		if silent > 1 {
			f.Flags[len(f.Flags)-1] = fmt.Sprintf("%d VMs went silent and were replaced.", silent)
		}
	}
}

func count(m map[string]map[string]*Tally, name, text string, at time.Time) {
	if m[name] == nil {
		m[name] = map[string]*Tally{}
	}
	t := m[name][text]
	if t == nil {
		t = &Tally{Text: text}
		m[name][text] = t
	}
	t.N++
	t.Last = at
}

// tallies lists the tallies, the most frequent first.
func tallies(m map[string]*Tally) []Tally {
	var out []Tally
	for _, t := range m {
		out = append(out, *t)
	}
	slices.SortFunc(out, func(a, b Tally) int {
		return cmp.Or(cmp.Compare(b.N, a.N), strings.Compare(a.Text, b.Text))
	})
	return out
}

func sumTallies(ts []Tally) int {
	n := 0
	for _, t := range ts {
		n += t.N
	}
	return n
}

func stamp(t time.Time) string { return t.UTC().Format("Jan 2 15:04") }

func releaseLine(e event) string {
	r := e.Attrs["release"]
	switch e.Msg {
	case "release deployed":
		return fmt.Sprintf("%s %s deployed (ProtoStrap %s)", stamp(e.Time), r, e.Attrs["version"])
	case "release finished":
		return fmt.Sprintf("%s %s finished in %s", stamp(e.Time), r, e.Attrs["took"])
	case "release superseded":
		return fmt.Sprintf("%s %s superseded by %s", stamp(e.Time), r, e.Attrs["by"])
	case "release failed: rolling back":
		return fmt.Sprintf("%s %s failed, rolled back to %s: %s", stamp(e.Time), r, e.Attrs["to"], e.Attrs["reason"])
	default:
		return fmt.Sprintf("%s %s failed, with nothing to roll back to: %s", stamp(e.Time), r, e.Attrs["reason"])
	}
}

func eventLine(e event) string {
	var b strings.Builder
	b.WriteString(stamp(e.Time) + " " + e.Msg)
	for _, k := range slices.Sorted(maps.Keys(e.Attrs)) {
		fmt.Fprintf(&b, " %s=%s", k, e.Attrs[k])
	}
	return b.String()
}

// Error lines differ in their numbers, addresses and IDs; without them, the same error is
// one tally.
var (
	uuidPattern   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	stampPattern  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`)
	hexPattern    = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-f]{12,}\b`)
	numberPattern = regexp.MustCompile(`\d+`)
)

func normalize(msg string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	first = uuidPattern.ReplaceAllString(first, "<id>")
	first = stampPattern.ReplaceAllString(first, "<time>")
	first = hexPattern.ReplaceAllString(first, "<hex>")
	first = numberPattern.ReplaceAllString(first, "N")
	if len(first) > 200 {
		first = first[:200] + "…"
	}
	return first
}

// logErrors groups error lines by component, each component's commonest first.
func logErrors(rows []LogLine) []*LogErrors {
	byGroup := map[string]map[string]*Tally{}
	lines := map[string]int{}
	for _, row := range rows {
		name := row.Component
		count(byGroup, name, normalize(row.Message), row.Time)
		lines[name]++
	}
	var out []*LogErrors
	for name, m := range byGroup {
		top := tallies(m)
		out = append(out, &LogErrors{Component: name, Lines: lines[name], Top: top[:min(len(top), 5)]})
	}
	slices.SortFunc(out, func(a, b *LogErrors) int {
		return cmp.Or(cmp.Compare(b.Lines, a.Lines), strings.Compare(a.Component, b.Component))
	})
	return out
}
