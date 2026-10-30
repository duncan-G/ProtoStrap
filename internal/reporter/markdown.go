package reporter

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// maxEvents is how many VM events the report lists; the rest are counted.
const maxEvents = 40

// Markdown writes the facts as the report's last section.
func (f *Facts) Markdown() string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("## Facts\n\nFrom %s to %s UTC: %d signal snapshots, a minute apart.\n\n", stamp(f.From), stamp(f.To), f.Snapshots)

	p("### Flags\n\n")
	list(&b, f.Flags, "Nothing stood out.")

	p("### Releases\n\n")
	list(&b, f.Releases, "None.")

	p("### Components\n\n")
	if len(f.Components) == 0 {
		p("None seen.\n\n")
	} else {
		p("| Component | Kind | Min–max | Desired today | Changes | At max | CPU avg / peak | Requests avg / peak | Queue peak | Waiting peak | Failures |\n")
		p("|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, c := range f.Components {
			p("| %s | %s | %s | %s | %d | %s | %s | %s | %s | %s | %d |\n",
				c.Name, or(c.Kind, "—"), minMax(c.Min, c.Max), rangeOf(c.seen, c.Low, c.High), c.Changes,
				share(c.AtMax, c.Snapshots, c.Max > 0), stat(c.CPU, "%.0f%%"), stat(c.Requests, "%.1f/s"),
				queues(c.QueuePeak), dash(c.PendingPeak), c.Failed)
		}
		p("\n")
		var notes []string
		for _, c := range f.Components {
			for _, t := range c.Limited {
				notes = append(notes, fmt.Sprintf("**%s** held back: %s (×%d, last %s)", c.Name, t.Text, t.N, stamp(t.Last)))
			}
			for _, t := range c.Failures {
				notes = append(notes, fmt.Sprintf("**%s** replica failed: %s (×%d, last %s)", c.Name, t.Text, t.N, stamp(t.Last)))
			}
		}
		if len(notes) > 0 {
			list(&b, notes, "")
		}
	}

	p("### VMs\n\n")
	if len(f.Pools) > 0 {
		p("| Pool | Max | VMs today | Wanted peak | At max | Short | Added | Removed |\n")
		p("|---|---|---|---|---|---|---|---|\n")
		for _, pl := range f.Pools {
			p("| %s | %s | %s | %d | %s | %s | %d | %d |\n", pl.Name, dash(pl.Max), rangeOf(pl.seen, pl.Low, pl.High), pl.WantedPeak,
				share(pl.AtMax, pl.Snapshots, pl.Max > 0), share(pl.Short, pl.Snapshots, true), pl.Added, pl.Removed)
		}
		p("\n")
	}
	events := f.VMEvents
	if len(events) > maxEvents {
		events = append(slices.Clip(events[:maxEvents]), fmt.Sprintf("…and %d more", len(f.VMEvents)-maxEvents))
	}
	list(&b, events, "No VM events.")

	p("### Scheduler warnings\n\n")
	var warnings []string
	for _, t := range f.Warnings {
		warnings = append(warnings, fmt.Sprintf("%s (×%d, last %s)", t.Text, t.N, stamp(t.Last)))
	}
	list(&b, warnings, "None.")

	p("### Agent warnings\n\n")
	var agents []string
	for _, t := range f.AgentWarnings {
		agents = append(agents, fmt.Sprintf("%s (×%d, last %s)", t.Text, t.N, stamp(t.Last)))
	}
	list(&b, agents, "None.")

	p("### Errors in component logs\n\n")
	switch {
	case f.ErrorsUnread != "":
		p("_The components' errors couldn't be read from telemetry: %s._\n\n", f.ErrorsUnread)
	case len(f.Errors) == 0:
		p("None.\n\n")
	}
	for _, e := range f.Errors {
		p("- **%s**: %d lines\n", e.Component, e.Lines)
		for _, t := range e.Top {
			p("  - ×%d `%s`\n", t.N, strings.ReplaceAll(t.Text, "`", "'"))
		}
	}
	if len(f.Errors) > 0 {
		p("\n")
	}
	return b.String()
}

func list(b *strings.Builder, items []string, empty string) {
	if len(items) == 0 {
		if empty != "" {
			b.WriteString(empty + "\n\n")
		}
		return
	}
	for _, it := range items {
		b.WriteString("- " + cell(it) + "\n")
	}
	b.WriteString("\n")
}

// cell keeps a list item on one line.
func cell(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func dash(n int) string {
	if n == 0 {
		return "—"
	}
	return fmt.Sprint(n)
}

func minMax(lo, hi int) string {
	if hi == 0 {
		return "—"
	}
	return fmt.Sprintf("%d–%d", lo, hi)
}

func rangeOf(seen bool, lo, hi int) string {
	switch {
	case !seen:
		return "—"
	case lo == hi:
		return fmt.Sprint(lo)
	default:
		return fmt.Sprintf("%d–%d", lo, hi)
	}
}

func share(n, of int, known bool) string {
	if !known || of == 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", n*100/of)
}

func stat(s Stat, format string) string {
	if s.N == 0 {
		return "—"
	}
	return fmt.Sprintf(format+" / "+format, s.Avg(), s.Peak)
}

func queues(m map[string]int) string {
	if len(m) == 0 {
		return "—"
	}
	var parts []string
	for _, q := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%s %d", q, m[q]))
	}
	return strings.Join(parts, ", ")
}
