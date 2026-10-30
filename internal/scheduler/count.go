package scheduler

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/edge"
	"github.com/duncan-G/protostrap/internal/spec"
)

// EdgeRequestsMetric is what the scheduler asks the edge for, per routed service, for
// requests signals.
func EdgeRequestsMetric(service string) string { return edge.RequestsMetric(service) }

// count works out each service's and worker's desired replicas (README.md#scaling-replicas):
//
//	desired = clamp(min(demand, limit cap), min, max)
//
// Demand is the highest of the component's signals. It scales up on the last minute's
// average, but down only to the highest demand of the last few minutes, and ignores CPU
// within 10% of its target. Limits apply at once.
func (s *Scheduler) count(now time.Time) {
	tgt := s.st.target
	if tgt == nil {
		s.st.desired = map[string]int{}
		s.st.why = nil
		return
	}
	s.sampleRequests(now)
	s.sampleCPU(now)
	holding := now.Sub(s.st.started) < s.cfg.StartupHold
	next := map[string]int{}
	why := map[string]*ScaleStatus{}
	for _, c := range tgt.spec.Sorted() {
		if c.Kind != api.KindService && c.Kind != api.KindWorker {
			continue
		}
		cur, known := s.st.desired[c.Name]
		d, reason, signals := s.demand(c, cur, known, now)

		hist := appendSample(s.st.demand[c.Name], sample{now, float64(d)}, now, s.cfg.PeakWindow)
		s.st.demand[c.Name] = hist
		if known && d < cur {
			peak := 0
			for _, x := range hist {
				peak = max(peak, int(x.v))
			}
			if peak > d {
				d = min(cur, peak)
				reason += fmt.Sprintf("; held at the peak of the last %v, %d", s.cfg.PeakWindow, peak)
			}
		}
		if holding {
			floor := max(s.st.lastDesired[c.Name], s.liveWithFingerprint(c.Name, s.fingerprint(tgt, c.Name)))
			if d < floor {
				d = floor
				reason = "the scheduler started recently: scaling nothing down until it has 5 minutes of signals"
			}
		}
		// Limits hold whatever the signals say, a start included: they are facts, not
		// missing data.
		limit, limitReason, limits := s.limitCap(c, now)
		if limit >= 0 && d > limit {
			d, reason = limit, limitReason
		}
		clamped := min(max(d, c.Scale.Min), c.Scale.Max)
		next[c.Name] = clamped
		if clamped != cur || !known {
			s.log.Info("desired replicas", "component", c.Name, "from", cur, "to", clamped, "reason", reason)
		}
		// The status names the clamp too; the log keeps the reason the reporter reads.
		switch {
		case clamped > d:
			reason += fmt.Sprintf("; raised to its minimum, %d", clamped)
		case clamped < d:
			reason += fmt.Sprintf("; capped at its maximum, %d", clamped)
		}
		why[c.Name] = &ScaleStatus{Min: c.Scale.Min, Max: c.Scale.Max, Reason: reason, Signals: signals, Limits: limits}
	}
	s.st.desired = next
	s.st.why = why
}

func (s *Scheduler) liveWithFingerprint(component, fp string) int {
	n := 0
	for _, r := range s.st.replicas {
		if r.Component == component && r.Kind != api.KindTask && r.live() && r.Fingerprint == fp {
			n++
		}
	}
	return n
}

// ceil rounds up, forgiving float error: 2 × 0.7 / 0.7 is 2, not 3.
func ceil(x float64) int { return int(math.Ceil(x - 1e-9)) }

// demand is the highest of a component's signals, why, and each signal's reading.
func (s *Scheduler) demand(c *spec.Component, cur int, known bool, now time.Time) (int, string, []SignalStatus) {
	var ready []*Replica
	for _, r := range s.st.replicas {
		if r.Component == c.Name && r.Kind != api.KindTask && r.live() && r.State == api.StateReady {
			if vm := s.st.vms[r.VM]; vm != nil && !vm.Lost {
				ready = append(ready, r)
			}
		}
	}
	slices.SortFunc(ready, func(a, b *Replica) int { return strings.Compare(a.ID, b.ID) })
	best, reason := -1, ""
	var signals []SignalStatus
	consider := func(sig spec.Signal, d int, why string) {
		signals = append(signals, SignalStatus{Signal: signalText(sig), Demand: &d, Detail: why})
		if d > best {
			best, reason = d, why
		}
	}
	unread := func(sig spec.Signal, why string) {
		signals = append(signals, SignalStatus{Signal: signalText(sig), Detail: why})
	}
	for _, sig := range c.Scale.Demand {
		switch sig.Kind() {
		case spec.SignalCPU:
			// Demand is the component's total CPU over the last minute, divided by what one
			// replica should carry. That equals replicas × average usage ÷ target when every
			// replica requests the same, but it doesn't overshoot while new replicas take load
			// from old ones whose averages still remember that load (docs/scaling-examples.md).
			total, ok := average(s.st.cpuTotal[c.Name], now, s.cfg.CPUWindow)
			if !ok || len(ready) == 0 || c.Request.CPU <= 0 {
				unread(sig, "no ready replica has reported its CPU")
				continue
			}
			target := float64(sig.CPU)
			usage := total / float64(int64(len(ready))*int64(c.Request.CPU))
			if math.Abs(usage-target)/target <= s.cfg.Hysteresis && known {
				consider(sig, cur, fmt.Sprintf("cpu %.0f%% is within %.0f%% of its %.0f%% target", usage*100, s.cfg.Hysteresis*100, target*100))
				continue
			}
			perReplica := float64(c.Request.CPU) * target
			consider(sig, ceil(total/perReplica),
				fmt.Sprintf("cpu %.0fm used, %.0f%% of request, against %.0f%%: ceil(%.0fm / %.0fm)", total, usage*100, target*100, total, perReplica))
		case spec.SignalRequests:
			rate, ok := s.requestRate(c.Name, now)
			if !ok {
				unread(sig, "no request counts from the edge yet")
				continue
			}
			consider(sig, ceil(rate/float64(sig.Requests)), fmt.Sprintf("%.1f requests/s at %.1f per replica", rate, float64(sig.Requests)))
		case spec.SignalQueue:
			d, ok := s.st.depths[sig.Queue]
			if !ok {
				unread(sig, fmt.Sprintf("queue %s hasn't been read yet", sig.Queue))
				continue
			}
			work := d.Visible + d.InFlight
			per := float64(sig.Rate) * sig.Drain.Seconds()
			consider(sig, ceil(float64(work)/per), fmt.Sprintf("queue %s: %d waiting + %d in flight, %.0f per replica within %s", sig.Queue, d.Visible, d.InFlight, per, durationText(sig.Drain.Duration)))
		case spec.SignalMetric:
			var sum float64
			var n int
			for _, r := range ready {
				if avg, ok := average(r.metrics[sig.Metric], now, s.cfg.CPUWindow); ok {
					sum += avg
					n++
				}
			}
			if n == 0 {
				unread(sig, fmt.Sprintf("no ready replica has reported %s", sig.Metric))
				continue
			}
			avg := sum / float64(n)
			consider(sig, ceil(float64(len(ready))*avg/sig.Target), fmt.Sprintf("%s averages %.3g against %.3g: ceil(%d × %.3g / %.3g)", sig.Metric, avg, sig.Target, len(ready), avg, sig.Target))
		}
	}
	if best < 0 {
		if !known {
			return c.Scale.Min, "no signal yet: the minimum", signals
		}
		return cur, "no signal: unchanged", signals
	}
	return best, reason, signals
}

// signalText is a demand signal as the spec writes it.
func signalText(sig spec.Signal) string {
	switch sig.Kind() {
	case spec.SignalCPU:
		return fmt.Sprintf("cpu: %s", percentText(float64(sig.CPU)))
	case spec.SignalRequests:
		return fmt.Sprintf("requests: %g/s", float64(sig.Requests))
	case spec.SignalQueue:
		return fmt.Sprintf("queue: %s, rate: %g/s, drain: %s", sig.Queue, float64(sig.Rate), durationText(sig.Drain.Duration))
	case spec.SignalMetric:
		return fmt.Sprintf("metric: %s, target: %g", sig.Metric, sig.Target)
	}
	return sig.Kind()
}

func percentText(f float64) string {
	return strconv.FormatFloat(math.Round(f*1e4)/1e2, 'f', -1, 64) + "%"
}

// durationText writes a duration as a spec would: whole seconds under 10 minutes (60s,
// 300s), then whole hours or minutes (2h, 15m), and Go's own form otherwise (1h30m0s).
func durationText(d time.Duration) string {
	switch {
	case d < 10*time.Minute && d%time.Second == 0:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0 && d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.String()
}

// average is the mean of the samples within window of now.
func average(list []sample, now time.Time, window time.Duration) (float64, bool) {
	var sum float64
	var n int
	for _, x := range list {
		if now.Sub(x.at) <= window {
			sum += x.v
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// limitCap is the lowest of a component's limits, or -1 when none applies, why, and each
// limit's cap.
func (s *Scheduler) limitCap(c *spec.Component, now time.Time) (int, string, []LimitStatus) {
	limit, reason := -1, ""
	var limits []LimitStatus
	for _, l := range c.Scale.Limits {
		n, why := -1, ""
		switch {
		case l.Replicas != nil:
			n, why = *l.Replicas, fmt.Sprintf("limited to %d replicas", *l.Replicas)
			limits = append(limits, LimitStatus{Limit: fmt.Sprintf("replicas: %d", n), Cap: &n, Detail: why})
		case l.Metric != "":
			st := LimitStatus{Limit: "metric: " + l.Metric}
			v, ok := s.st.limits[l.Metric]
			switch {
			case !ok:
				st.Detail = "no value yet: no cap"
			case now.Sub(v.at) > s.cfg.LimitTTL:
				st.Detail = fmt.Sprintf("no value in the last %s: no cap", durationText(s.cfg.LimitTTL))
			case v.v < 0:
				st.Detail = fmt.Sprintf("%s is %g: no cap", l.Metric, v.v)
			default:
				n, why = int(math.Floor(v.v)), fmt.Sprintf("limited by %s to %d", l.Metric, int(math.Floor(v.v)))
				st.Cap, st.Detail = &n, why
			}
			limits = append(limits, st)
		}
		if n >= 0 && (limit < 0 || n < limit) {
			limit, reason = n, why
		}
	}
	return limit, reason, limits
}

// sampleRequests sums each routed component's request count across the edges, for its
// rate.
func (s *Scheduler) sampleRequests(now time.Time) {
	tgt := s.st.target
	for _, c := range tgt.spec.Sorted() {
		if len(c.Routes) == 0 {
			continue
		}
		name := edge.RequestsMetric(c.Name)
		var total float64
		var found bool
		for id, counts := range s.st.edgeCounts {
			if r := s.st.replicas[id]; r == nil || !r.live() {
				continue
			}
			if v, ok := counts[name]; ok {
				total += v
				found = true
			}
		}
		if !found {
			continue
		}
		hist := s.st.requests[c.Name]
		if n := len(hist); n > 0 && total < hist[n-1].v {
			hist = nil // An edge restarted, and its count started over.
		}
		s.st.requests[c.Name] = appendSample(hist, sample{now, total}, now, s.cfg.CPUWindow)
	}
}

// requestRate is the requests per second the edges sent a component over the last minute.
func (s *Scheduler) requestRate(component string, now time.Time) (float64, bool) {
	hist := s.st.requests[component]
	if len(hist) < 2 {
		return 0, false
	}
	first, last := hist[0], hist[len(hist)-1]
	dt := last.at.Sub(first.at).Seconds()
	if dt < 5 {
		return 0, false
	}
	return (last.v - first.v) / dt, true
}

// sampleCPU records each component's total CPU across its ready replicas, from each one's
// latest report.
func (s *Scheduler) sampleCPU(now time.Time) {
	totals := map[string]float64{}
	seen := map[string]bool{}
	for _, r := range s.st.replicas {
		if !r.live() || r.State != api.StateReady || len(r.cpu) == 0 {
			continue
		}
		if vm := s.st.vms[r.VM]; vm == nil || vm.Lost {
			continue
		}
		last := r.cpu[len(r.cpu)-1]
		if now.Sub(last.at) > 2*s.cfg.Cycle {
			continue
		}
		totals[r.Component] += last.v
		seen[r.Component] = true
	}
	for c := range seen {
		s.st.cpuTotal[c] = appendSample(s.st.cpuTotal[c], sample{now, totals[c]}, now, s.cfg.SignalWindow)
	}
}
