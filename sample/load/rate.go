package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const reportEvery = 10 * time.Second

// phase is one rate held for a while, reached over the runner's ramp.
type phase struct {
	rps      float64
	duration time.Duration
}

// runner sends GETs at a set rate, whatever the answers: an open loop, so a slow service
// sees the same load as a fast one. A send beyond the concurrency limit is dropped and
// counted, not queued.
type runner struct {
	flags   rateFlags
	client  *http.Client
	slots   chan struct{}
	rate    atomic.Uint64 // The current rate is stored as math.Float64bits.
	wg      sync.WaitGroup
	period  stats // These stats cover the time since the last report.
	overall stats // These stats cover the time since the start.
}

func newRunner(f rateFlags) *runner {
	transport := newTransport(f.h2c)
	transport.MaxIdleConns = f.concurrency
	transport.MaxIdleConnsPerHost = f.concurrency
	return &runner{
		flags:  f,
		client: &http.Client{Transport: transport, Timeout: f.timeout},
		slots:  make(chan struct{}, f.concurrency),
	}
}

// run goes through the phases, then waits for the requests in flight and prints totals.
// Cancelling ctx (Ctrl+C) cuts the run short, but it still prints the totals.
func (r *runner) run(ctx context.Context, phases []phase) {
	start := time.Now()
	label := func() string { return fmt.Sprintf("%6s", since(start)) }

	stopReports := make(chan struct{})
	reportsDone := make(chan struct{})
	go func() {
		defer close(reportsDone)
		ticker := time.NewTicker(reportEvery)
		defer ticker.Stop()
		for {
			select {
			case <-stopReports:
				return
			case <-ticker.C:
				r.report(label(), &r.period, true)
			}
		}
	}()

	from := 0.0
	for _, p := range phases {
		if ctx.Err() != nil {
			break
		}
		fmt.Printf("%s  rate %s/s for %s\n", label(), formatRate(p.rps), p.duration)
		r.pace(ctx, from, p)
		from = p.rps
	}
	r.wg.Wait()
	close(stopReports)
	<-reportsDone
	r.report(label(), &r.period, true)
	r.report(" total", &r.overall, false)
}

// pace sends at the phase's rate for its whole duration, moving there from the previous
// rate over the ramp. Each moment's rate adds credit; each whole credit is a request.
func (r *runner) pace(ctx context.Context, from float64, p phase) {
	start := time.Now()
	end := start.Add(p.duration)
	last := start
	credit := 0.0
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		now := time.Now()
		if !now.Before(end) {
			return
		}
		rate := p.rps
		if elapsed := now.Sub(start); r.flags.ramp > 0 && elapsed < r.flags.ramp {
			rate = from + (p.rps-from)*float64(elapsed)/float64(r.flags.ramp)
		}
		r.rate.Store(math.Float64bits(rate))

		// After a stall, carry on at the rate rather than make up for it in a burst.
		credit = min(credit+rate*now.Sub(last).Seconds(), max(rate, 1))
		last = now
		for ; credit >= 1; credit-- {
			r.send(ctx)
		}

		// Wake when the next request is due, and at least every 50 ms, for the ramp.
		wait := 50 * time.Millisecond
		if rate > 0 {
			wait = min(wait, time.Duration((1-credit)/rate*float64(time.Second)))
		}
		timer.Reset(min(wait, time.Until(end)))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}

func (r *runner) send(ctx context.Context) {
	select {
	case r.slots <- struct{}{}:
	default:
		r.period.drop()
		r.overall.drop()
		return
	}
	r.period.send()
	r.overall.send()
	r.wg.Go(func() {
		defer func() { <-r.slots }()
		outcome, latency := r.get(ctx)
		if outcome == "" {
			return // The request was cancelled.
		}
		r.period.record(outcome, latency)
		r.overall.record(outcome, latency)
	})
}

// get returns "ok", the HTTP status of a failure, or a kind of error, and the latency of
// an answer. It returns "" if ctx was cancelled.
func (r *runner) get(ctx context.Context) (string, time.Duration) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.flags.url, nil)
	if err != nil {
		return "bad-url", 0
	}
	start := time.Now()
	resp, err := r.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", 0
		}
		return errorKind(err), 0
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	latency := time.Since(start)
	switch {
	case err != nil && ctx.Err() != nil:
		return "", 0
	case err != nil:
		return errorKind(err), 0
	case resp.StatusCode < 400:
		return "ok", latency
	default:
		return strconv.Itoa(resp.StatusCode), latency
	}
}

func errorKind(err error) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	default:
		return "error"
	}
}

// report prints a line of counts and latency percentiles; for a period it also shows the
// rate and the requests in flight, and starts the next period.
func (r *runner) report(label string, s *stats, period bool) {
	snap := s.take(period)
	line := fmt.Sprintf("%s  sent %d  ok %d", label, snap.sent, snap.ok)
	if period {
		rate := math.Float64frombits(r.rate.Load())
		line = fmt.Sprintf("%s  rate %s/s  sent %d  ok %d  in flight %d",
			label, formatRate(rate), snap.sent, snap.ok, len(r.slots))
	}
	if snap.dropped > 0 {
		line += fmt.Sprintf("  dropped %d", snap.dropped)
	}
	if len(snap.failed) > 0 {
		var parts []string
		for _, kind := range slices.Sorted(maps.Keys(snap.failed)) {
			parts = append(parts, fmt.Sprintf("%s:%d", kind, snap.failed[kind]))
		}
		line += "  errors " + strings.Join(parts, " ")
	}
	if len(snap.latencies) > 0 {
		slices.Sort(snap.latencies)
		line += fmt.Sprintf("  p50 %s  p90 %s  p99 %s",
			percentile(snap.latencies, 0.50), percentile(snap.latencies, 0.90), percentile(snap.latencies, 0.99))
	}
	fmt.Println(line)
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)].Round(100 * time.Microsecond)
}

func formatRate(rps float64) string {
	return strconv.FormatFloat(rps, 'f', -1, 64)
}

// stats counts what happened to requests. Latencies are of answers, whatever their status.
type stats struct {
	mu sync.Mutex
	snapshot
}

type snapshot struct {
	sent      int
	ok        int
	dropped   int
	failed    map[string]int // It counts failures by HTTP status or kind of error.
	latencies []time.Duration
}

func (s *stats) send() {
	s.mu.Lock()
	s.sent++
	s.mu.Unlock()
}

func (s *stats) drop() {
	s.mu.Lock()
	s.dropped++
	s.mu.Unlock()
}

func (s *stats) record(outcome string, latency time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if outcome == "ok" {
		s.ok++
	} else {
		if s.failed == nil {
			s.failed = map[string]int{}
		}
		s.failed[outcome]++
	}
	if latency > 0 {
		s.latencies = append(s.latencies, latency)
	}
}

// take returns the counts so far, and with reset, starts again from zero.
func (s *stats) take(reset bool) snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snapshot
	if reset {
		s.snapshot = snapshot{}
	} else {
		snap.failed = maps.Clone(s.failed)
		snap.latencies = slices.Clone(s.latencies)
	}
	return snap
}
