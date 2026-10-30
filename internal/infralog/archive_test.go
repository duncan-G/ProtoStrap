package infralog

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
	"github.com/duncan-G/protostrap/internal/aws"
)

// The archive is the deploy bucket's logs/ prefix: one object per hour per scheduler run,
// logs/<date>/<hour>/<writer>.ndjson.gz, gzipped JSON lines. A scheduler rewrites only its
// own objects, so a restart never loses the records of the run before it.

type fakeBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    []string
	lists   int
	putErr  error
	listErr error
}

func newBucket() *fakeBucket { return &fakeBucket{objects: map[string][]byte{}} }

func (b *fakeBucket) List(_ context.Context, bucket, prefix string) ([]aws.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lists++
	if b.listErr != nil {
		return nil, b.listErr
	}
	if bucket != "deploy" {
		return nil, fmt.Errorf("bucket %q", bucket)
	}
	var out []aws.Object
	for k, v := range b.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, aws.Object{Key: k, Size: int64(len(v))})
		}
	}
	slices.SortFunc(out, func(a, b aws.Object) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (b *fakeBucket) Get(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.objects[key]
	if !ok {
		return nil, fmt.Errorf("s3 GET %s: %w", key, aws.ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}

func (b *fakeBucket) Put(_ context.Context, bucket, key string, body []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.putErr != nil {
		return b.putErr
	}
	b.objects[key] = slices.Clone(body)
	b.puts = append(b.puts, key)
	return nil
}

func (b *fakeBucket) takePuts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.puts
	b.puts = nil
	return p
}

// stored decodes one of the bucket's objects.
func stored(t *testing.T, b *fakeBucket, key string) []Record {
	t.Helper()
	b.mu.Lock()
	body, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		t.Fatalf("no object %s", key)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("%s isn't gzipped: %v", key, err)
	}
	var out []Record
	dec := json.NewDecoder(zr)
	for {
		var r Record
		if err := dec.Decode(&r); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		out = append(out, r)
	}
	return out
}

// store writes an object as an earlier scheduler run would have.
func store(t *testing.T, b *fakeBucket, key string, recs ...Record) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, r := range recs {
		line, _ := json.Marshal(r)
		zw.Write(append(line, '\n'))
	}
	zw.Close()
	b.objects[key] = buf.Bytes()
}

const writer = "20261029T080000Z-i-0data1"

func archived(c *clock, b *fakeBucket) *Log {
	return &Log{Now: c.Now, Archive: &Archive{Bucket: b, Name: "deploy", Writer: writer}}
}

func TestFlushWritesOneObjectPerHourWithRecords(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.AddAgent("i-0a1b", "general-3", []api.LogRecord{
		rec(time.Date(2026, 10, 29, 9, 59, 30, 0, time.UTC), "INFO", "starting a replica", "replica", "api-aaaaaa"),
		rec(t0, "ERROR", "replica failed", "replica", "api-aaaaaa", "reason", "exited with code 1"),
	})
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	puts := b.takePuts()
	slices.Sort(puts)
	nine, ten := "logs/2026-10-29/09/"+writer+".ndjson.gz", "logs/2026-10-29/10/"+writer+".ndjson.gz"
	if strings.Join(puts, " ") != nine+" "+ten {
		t.Fatalf("puts %v", puts)
	}
	got := stored(t, b, ten)
	if len(got) != 1 || got[0].Msg != "replica failed" || got[0].Source != SourceAgent || got[0].VMName != "general-3" ||
		got[0].Component != "api" || attr(got[0], "reason") != "exited with code 1" || !got[0].Time.Equal(t0) {
		t.Errorf("hour 10: %+v", got)
	}
	if got[0].Seq != 0 {
		t.Errorf("seq %d: sequence numbers are the memory's alone", got[0].Seq)
	}

	// Nothing new, nothing written.
	if err := l.Flush(context.Background()); err != nil || len(b.takePuts()) != 0 {
		t.Errorf("a flush with nothing new wrote %v, %v", b.puts, err)
	}
	// A new record rewrites its hour's object with everything this run has for the hour.
	c.advance(time.Minute)
	l.AddAgent("i-0a1b", "general-3", []api.LogRecord{rec(c.now, "INFO", "starting a replica", "replica", "api-bbbbbb")})
	l.Flush(context.Background())
	if puts := b.takePuts(); len(puts) != 1 || puts[0] != ten {
		t.Errorf("puts %v", puts)
	}
	if got := stored(t, b, ten); msgs(got) != "replica failed,starting a replica" {
		t.Errorf("hour 10: %s", msgs(got))
	}
}

func TestFlushTriesAgainAfterAFailure(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "kept")})
	b.putErr = errors.New("SlowDown")
	if err := l.Flush(context.Background()); err == nil || !strings.Contains(err.Error(), "SlowDown") {
		t.Errorf("err %v", err)
	}
	b.putErr = nil
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, b, "logs/2026-10-29/10/"+writer+".ndjson.gz"); msgs(got) != "kept" {
		t.Errorf("got %s", msgs(got))
	}
}

func TestALateRecordRewritesItsHour(t *testing.T) {
	// An agent that couldn't reach the scheduler sends its records when it can.
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(-40*time.Minute), "INFO", "on time")})
	l.Flush(context.Background())
	b.takePuts()
	c.advance(20 * time.Minute)
	l.AddAgent("i-2", "general-2", []api.LogRecord{rec(t0.Add(-35*time.Minute), "WARN", "late")})
	l.Flush(context.Background())
	nine := "logs/2026-10-29/09/" + writer + ".ndjson.gz"
	if puts := b.takePuts(); len(puts) != 1 || puts[0] != nine {
		t.Errorf("puts %v", puts)
	}
	if got := stored(t, b, nine); msgs(got) != "on time,late" {
		t.Errorf("got %s", msgs(got))
	}
}

func TestLoadFillsMemoryWithTheLastDay(t *testing.T) {
	// A restarted scheduler shows what the run before it saw.
	c := &clock{now: t0}
	b := newBucket()
	old := "20261028T060000Z-i-0data1"
	store(t, b, "logs/2026-10-28/09/"+old+".ndjson.gz",
		Record{Time: time.Date(2026, 10, 28, 9, 45, 0, 0, time.UTC), Level: "INFO", Source: SourceScheduler, Msg: "more than a day ago"})
	store(t, b, "logs/2026-10-28/11/"+old+".ndjson.gz",
		Record{Time: time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC), Level: "INFO", Source: SourceScheduler, Msg: "yesterday", Component: "api"})
	store(t, b, "logs/2026-10-29/10/"+old+".ndjson.gz",
		Record{Time: time.Date(2026, 10, 29, 10, 5, 0, 0, time.UTC), Level: "WARN", Source: SourceAgent, VMName: "general-3", Msg: "this morning"})
	l := archived(c, b)
	if err := l.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	listed := b.lists
	res := query(t, l, Filter{Since: t0.Add(-24 * time.Hour)})
	if msgs(res.Records) != "yesterday,this morning" || res.Records[0].Seq == 0 || res.Records[0].Component != "api" {
		t.Errorf("records %+v", res.Records)
	}
	if b.lists != listed {
		t.Errorf("a query of the last day read the bucket: it is all in memory")
	}

	// This run's objects hold this run's records alone; the earlier run's are untouched.
	before := slices.Clone(b.objects["logs/2026-10-29/10/"+old+".ndjson.gz"])
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "now")})
	l.Flush(context.Background())
	if puts := b.takePuts(); len(puts) != 1 || puts[0] != "logs/2026-10-29/10/"+writer+".ndjson.gz" {
		t.Errorf("puts %v", puts)
	}
	if got := stored(t, b, "logs/2026-10-29/10/"+writer+".ndjson.gz"); msgs(got) != "now" {
		t.Errorf("this run's object: %s", msgs(got))
	}
	if !bytes.Equal(before, b.objects["logs/2026-10-29/10/"+old+".ndjson.gz"]) {
		t.Error("rewrote the earlier run's object")
	}
}

func TestLoadFailsWhenTheBucketCantBeRead(t *testing.T) {
	b := newBucket()
	b.listErr = errors.New("AccessDenied")
	l := archived(&clock{now: t0}, b)
	if err := l.Load(context.Background()); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Errorf("err %v", err)
	}
}

func TestOlderRecordsAreReadFromTheArchive(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	old := "20261029T000000Z-i-0data1"
	store(t, b, "logs/2026-10-29/05/"+old+".ndjson.gz",
		Record{Time: t0.Add(-5 * time.Hour), Level: "INFO", Source: SourceScheduler, Component: "api", Msg: "five hours ago"})
	store(t, b, "logs/2026-10-29/06/"+old+".ndjson.gz",
		Record{Time: t0.Add(-4 * time.Hour), Level: "INFO", Source: SourceScheduler, Component: "jobs", Msg: "four hours ago"})
	l := archived(c, b)
	l.Window = 2 * time.Hour
	if err := l.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(-30*time.Minute), "INFO", "half an hour ago", "component", "api")})

	listed := b.lists
	if got := msgs(query(t, l, Filter{Since: t0.Add(-time.Hour)}).Records); got != "half an hour ago" || b.lists != listed {
		t.Errorf("got %s, %d lists: the last hour is in memory", got, b.lists-listed)
	}
	res := query(t, l, Filter{Since: t0.Add(-6 * time.Hour), Component: "api"})
	if msgs(res.Records) != "five hours ago,half an hour ago" {
		t.Errorf("got %s: the archive's records go through the same filter", msgs(res.Records))
	}
	if res := query(t, l, Filter{Since: t0.Add(-6 * time.Hour), Limit: 1}); msgs(res.Records) != "half an hour ago" || !res.More {
		t.Errorf("got %s, more %v", msgs(res.Records), res.More)
	}
}

func TestRecordsEvictedFromMemoryAreReadFromTheArchive(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.Window = 2 * time.Hour
	l.Load(context.Background())
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "early")})
	l.Flush(context.Background())
	c.advance(3 * time.Hour)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(c.now, "INFO", "later")})
	if got := msgs(query(t, l, Filter{Since: t0.Add(-time.Hour)}).Records); got != "early,later" {
		t.Errorf("got %s", got)
	}
}

func TestUnwrittenRecordsStayInMemoryUntilWritten(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.Window = 2 * time.Hour
	l.Load(context.Background())
	b.putErr = errors.New("SlowDown")
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "early")})
	l.Flush(context.Background())
	c.advance(3 * time.Hour)
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(c.now, "INFO", "later")})
	b.putErr = nil
	if err := l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, b, "logs/2026-10-29/10/"+writer+".ndjson.gz"); msgs(got) != "early" {
		t.Errorf("got %s: an hour isn't dropped from memory before it is written", msgs(got))
	}
}

func TestArchiveRead(t *testing.T) {
	// The reporter reads the day's records from the archive alone: it runs apart from the
	// scheduler.
	b := newBucket()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, time.UTC) }
	store(t, b, "logs/2026-10-29/09/a.ndjson.gz", Record{Time: at(9, 10), Msg: "before"}, Record{Time: at(9, 50), Msg: "b"})
	store(t, b, "logs/2026-10-29/09/b.ndjson.gz", Record{Time: at(9, 20), Msg: "a"})
	store(t, b, "logs/2026-10-29/10/b.ndjson.gz", Record{Time: at(10, 5), Msg: "c"}, Record{Time: at(10, 40), Msg: "after"})
	b.objects["logs/2026-10-29/10/broken.ndjson.gz"] = []byte("not gzip")
	a := &Archive{Bucket: b, Name: "deploy"}
	got, err := a.Read(context.Background(), at(9, 15), at(10, 30))
	if err != nil {
		t.Fatal(err)
	}
	if msgs(got) != "a,b,c" {
		t.Errorf("got %s: the window's records from every run, in time order, past a broken object", msgs(got))
	}
	b.listErr = errors.New("AccessDenied")
	if _, err := a.Read(context.Background(), at(9, 15), at(10, 30)); err == nil {
		t.Error("no error when the bucket can't be listed")
	}
}

func TestRunFlushesUntilStoppedAndOnceMore(t *testing.T) {
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.Every = 5 * time.Millisecond
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "first")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() { l.Run(ctx); done <- true }()
	key := "logs/2026-10-29/10/" + writer + ".ndjson.gz"
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		_, ok := b.objects[key]
		b.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never flushed")
		}
		time.Sleep(time.Millisecond)
	}
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "last")})
	cancel()
	<-done
	if got := stored(t, b, key); msgs(got) != "first,last" {
		t.Errorf("got %s: Run flushes once more when it stops", msgs(got))
	}
}

func TestMemoryHoldsAtMostItsLimitAndTheArchiveTheRest(t *testing.T) {
	// data-1 runs the stateful components too, so memory is bounded: the oldest hours that
	// are written leave early, and queries read them from the archive.
	c := &clock{now: t0}
	b := newBucket()
	l := archived(c, b)
	l.Limit = 4
	l.Load(context.Background())
	for i := range 3 {
		at := t0.Add(time.Duration(i-2) * time.Hour)
		l.AddAgent("i-1", "general-1", []api.LogRecord{rec(at, "INFO", fmt.Sprint(i, "a")), rec(at, "INFO", fmt.Sprint(i, "b"))})
		l.Flush(context.Background())
	}
	l.mu.Lock()
	held := 0
	for _, h := range l.hours {
		held += len(h.own) + len(h.loaded)
	}
	l.mu.Unlock()
	if held > 4 {
		t.Errorf("memory holds %d records, want at most 4", held)
	}
	if got := msgs(query(t, l, Filter{Since: t0.Add(-3 * time.Hour)}).Records); got != "0a,0b,1a,1b,2a,2b" {
		t.Errorf("got %s", got)
	}
}

func TestWithoutAnArchiveTheLimitDropsTheOldest(t *testing.T) {
	c := &clock{now: t0}
	l := &Log{Now: c.Now, Limit: 2}
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(-2*time.Hour), "INFO", "old")})
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0.Add(-time.Hour), "INFO", "middle")})
	l.AddAgent("i-1", "general-1", []api.LogRecord{rec(t0, "INFO", "new")})
	if got := msgs(query(t, l, Filter{Since: t0.Add(-3 * time.Hour)}).Records); got != "middle,new" {
		t.Errorf("got %s", got)
	}
}
