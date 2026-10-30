package infralog

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// Outbox holds an agent's log records, info and above, until a sync carries them to the
// scheduler. While the scheduler is unreachable it keeps the newest, and says how many it
// dropped. Its methods are safe to call at once.
type Outbox struct {
	// Limit is the most records it holds. It defaults to 5,000.
	Limit int

	mu      sync.Mutex
	records []api.LogRecord
	dropped int
}

// Handler holds each record at info and above, and passes every record on to next, the
// agent's own log.
func (o *Outbox) Handler(next slog.Handler) slog.Handler {
	return &handler{next: next, add: func(r slog.Record, msg string, attrs []api.LogAttr) {
		at := r.Time
		if at.IsZero() {
			at = time.Now()
		}
		o.mu.Lock()
		defer o.mu.Unlock()
		o.records = append(o.records, api.LogRecord{Time: at.UTC(), Level: levelName(r.Level), Msg: msg, Attrs: attrs})
		o.trim()
	}}
}

// trim drops the oldest records beyond the limit. The caller holds mu.
func (o *Outbox) trim() {
	if over := len(o.records) - cmp.Or(o.Limit, 5000); over > 0 {
		o.dropped += over
		o.records = slices.Delete(o.records, 0, over)
	}
}

// Take removes and returns the oldest n records, for a report.
func (o *Outbox) Take(n int) []api.LogRecord {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.records) == 0 && o.dropped == 0 {
		return nil
	}
	var out []api.LogRecord
	if o.dropped > 0 {
		s := "s"
		if o.dropped == 1 {
			s = ""
		}
		out = append(out, api.LogRecord{Time: time.Now().UTC(), Level: "WARN",
			Msg: fmt.Sprintf("%d log record%s dropped while the scheduler was unreachable", o.dropped, s)})
		o.dropped = 0
	}
	k := min(max(n-len(out), 0), len(o.records))
	out = append(out, o.records[:k]...)
	o.records = slices.Clone(o.records[k:])
	return out
}

// Return puts back records a failed sync didn't deliver, before those logged since.
func (o *Outbox) Return(records []api.LogRecord) {
	if len(records) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.records = append(slices.Clone(records), o.records...)
	o.trim()
}
