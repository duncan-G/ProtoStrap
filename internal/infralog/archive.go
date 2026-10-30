package infralog

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
)

// Prefix is where the archive lives in the deploy bucket.
const Prefix = "logs/"

// Bucket is the deploy bucket.
type Bucket interface {
	List(ctx context.Context, bucket, prefix string) ([]aws.Object, error)
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	Put(ctx context.Context, bucket, key string, body []byte) error
}

// Archive is the infra log in the deploy bucket: for each hour, one object per scheduler
// run, logs/<date>/<hour>/<writer>.ndjson.gz, gzipped JSON lines in UTC. A run rewrites
// only its own objects, so a restart never loses the run before it, and the bucket's
// lifecycle expires them.
type Archive struct {
	Bucket Bucket
	// Name is the deploy bucket's name.
	Name string
	// Writer names this run's objects. Only the scheduler writes; the reporter only reads.
	Writer string
}

// readers is how many hours Read fetches at once.
const readers = 8

func hourPrefix(t time.Time) string { return Prefix + t.UTC().Format("2006-01-02/15") + "/" }

func (a *Archive) key(start time.Time) string { return hourPrefix(start) + a.Writer + ".ndjson.gz" }

// write puts one hour's records, replacing this run's object for the hour.
func (a *Archive) write(ctx context.Context, start time.Time, records []Record) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, r := range records {
		r.Seq = 0
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return a.Bucket.Put(ctx, a.Name, a.key(start), buf.Bytes())
}

// Read returns the records from from up to to, of every run, in time order. A broken object
// or line is passed over.
func (a *Archive) Read(ctx context.Context, from, to time.Time) ([]Record, error) {
	return a.read(ctx, from, to, "")
}

// read is Read, leaving out the writer skip's objects.
func (a *Archive) read(ctx context.Context, from, to time.Time, skip string) ([]Record, error) {
	var hours []time.Time
	for h := from.UTC().Truncate(time.Hour); h.Before(to); h = h.Add(time.Hour) {
		hours = append(hours, h)
	}
	var (
		mu   sync.Mutex
		out  []Record
		errs []error
		wg   sync.WaitGroup
		work = make(chan time.Time)
	)
	for range min(readers, len(hours)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range work {
				recs, err := a.readHour(ctx, h, from, to, skip)
				mu.Lock()
				out = append(out, recs...)
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
			}
		}()
	}
	for _, h := range hours {
		work <- h
	}
	close(work)
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b Record) int { return a.Time.Compare(b.Time) })
	return out, nil
}

func (a *Archive) readHour(ctx context.Context, start, from, to time.Time, skip string) ([]Record, error) {
	objects, err := a.Bucket.List(ctx, a.Name, hourPrefix(start))
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", hourPrefix(start), err)
	}
	var out []Record
	for _, o := range objects {
		name, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, hourPrefix(start)), ".ndjson.gz")
		if !ok || (skip != "" && name == skip) {
			continue
		}
		body, err := a.Bucket.Get(ctx, a.Name, o.Key)
		if errors.Is(err, aws.ErrNotFound) {
			continue // It expired after the listing.
		}
		if err != nil {
			return out, fmt.Errorf("reading %s: %w", o.Key, err)
		}
		out = append(out, decode(body, from, to)...)
		body.Close()
	}
	return out, nil
}

// decode reads an object's records from from up to to.
func decode(body io.Reader, from, to time.Time) []Record {
	zr, err := gzip.NewReader(body)
	if err != nil {
		return nil
	}
	defer zr.Close()
	var out []Record
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.Time.Before(from) || !r.Time.Before(to) {
			continue
		}
		r.Seq = 0
		out = append(out, r)
	}
	return out
}

// Load reads the last window of records, written by earlier runs, into memory, so a
// restarted scheduler shows what the run before it saw.
func (l *Log) Load(ctx context.Context) error {
	if l.Archive == nil {
		return nil
	}
	now := l.now()
	from := now.Add(-l.window())
	recs, err := l.Archive.read(ctx, from, now.Add(time.Minute), l.Archive.Writer)
	if err != nil {
		return fmt.Errorf("loading the infra log: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setup(now)
	for _, r := range recs {
		l.add(now, r, true)
	}
	if from.Before(l.complete) {
		l.complete = from
	}
	return nil
}

// Flush writes every hour with records this run hasn't written yet to the archive. An hour
// that can't be written is tried again next time.
func (l *Log) Flush(ctx context.Context) error {
	if l.Archive == nil {
		return nil
	}
	type pending struct {
		h       *hour
		records []Record
	}
	l.mu.Lock()
	now := l.now()
	l.setup(now)
	var todo []pending
	for _, h := range l.hours {
		if h.dirty {
			h.dirty = false
			recs := slices.Clone(h.own)
			sortRecords(recs)
			todo = append(todo, pending{h, recs})
		}
	}
	l.mu.Unlock()
	slices.SortFunc(todo, func(a, b pending) int { return a.h.start.Compare(b.h.start) })

	var errs []error
	for _, p := range todo {
		if err := l.Archive.write(ctx, p.h.start, p.records); err != nil {
			l.mu.Lock()
			p.h.dirty = true
			l.mu.Unlock()
			errs = append(errs, fmt.Errorf("writing %s: %w", l.Archive.key(p.h.start), err))
		}
	}
	l.mu.Lock()
	l.evict(now)
	l.mu.Unlock()
	return errors.Join(errs...)
}

// Run writes to the archive every Every until ctx is done, then once more.
func (l *Log) Run(ctx context.Context) {
	t := time.NewTicker(cmp.Or(l.Every, time.Minute))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			last, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			l.flush(last)
			cancel()
			return
		case <-t.C:
			l.flush(ctx)
		}
	}
}

func (l *Log) flush(ctx context.Context) {
	if err := l.Flush(ctx); err != nil && l.Logger != nil {
		l.Logger.Warn("writing the infra log to the deploy bucket", "err", err)
	}
}
