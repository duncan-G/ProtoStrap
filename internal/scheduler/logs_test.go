package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/duncan-G/protostrap/internal/api"
)

// Agents send their log records with their reports, and the scheduler keeps them in the
// infra log, attributed to the VM that proved who it is.

type agentLogs struct {
	vm, name string
	records  []api.LogRecord
}

type fakeLogSink struct{ got []agentLogs }

func (f *fakeLogSink) AddAgent(vm, name string, records []api.LogRecord) {
	f.got = append(f.got, agentLogs{vm, name, records})
}

func TestASyncCarriesTheAgentsLogRecords(t *testing.T) {
	h := newHarness(t)
	h.settle()
	srv, _ := server(h)
	sink := &fakeLogSink{}
	srv.Logs = sink
	a := h.vms("general")[0]
	doc := identityDoc(a.id, a.ip, testAccount, testRegion)
	recs := []api.LogRecord{{Time: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), Level: "ERROR", Msg: "replica failed",
		Attrs: []api.LogAttr{{Key: "replica", Value: "api-aaaaaa"}}}}

	report, _ := json.Marshal(api.Report{VM: a.id, Name: "general-3", Pool: "general", Version: "1.4.0", Capacity: a.capacity, Logs: recs})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), doc, "s", a.ip+":5000"))
	if w.Code != 200 {
		t.Fatalf("sync: %d %s", w.Code, w.Body)
	}
	if len(sink.got) != 1 || sink.got[0].vm != a.id || sink.got[0].name != "general-3" || len(sink.got[0].records) != 1 || sink.got[0].records[0].Msg != "replica failed" {
		t.Errorf("logs %+v", sink.got)
	}

	// A report without records adds nothing.
	report, _ = json.Marshal(api.Report{VM: a.id, Name: "general-3", Pool: "general", Version: "1.4.0", Capacity: a.capacity})
	srv.ServeHTTP(httptest.NewRecorder(), signedRequest(t, "POST", "/v1/sync", string(report), doc, "s", a.ip+":5000"))
	if len(sink.got) != 1 {
		t.Errorf("logs %+v", sink.got)
	}
}

func TestARefusedSyncKeepsNoRecords(t *testing.T) {
	// The agent sends them again with its next report, so keeping them now would keep them
	// twice.
	recs := []api.LogRecord{{Time: time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC), Level: "INFO", Msg: "m"}}
	t.Run("another VM's report", func(t *testing.T) {
		h := newHarness(t)
		h.settle()
		srv, _ := server(h)
		sink := &fakeLogSink{}
		srv.Logs = sink
		a := h.vms("general")[0]
		report, _ := json.Marshal(api.Report{VM: "i-other", Pool: "general", Logs: recs})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), identityDoc(a.id, a.ip, testAccount, testRegion), "s", a.ip+":5000"))
		if w.Code != http.StatusForbidden || len(sink.got) != 0 {
			t.Errorf("%d, logs %+v", w.Code, sink.got)
		}
	})
	t.Run("warming up", func(t *testing.T) {
		h := newHarness(t)
		h.start()
		srv, _ := server(h)
		sink := &fakeLogSink{}
		srv.Logs = sink
		a := h.vms("general")[0]
		report, _ := json.Marshal(api.Report{VM: a.id, Pool: "general", Logs: recs})
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, signedRequest(t, "POST", "/v1/sync", string(report), identityDoc(a.id, a.ip, testAccount, testRegion), "s", a.ip+":1"))
		if w.Code != http.StatusServiceUnavailable || len(sink.got) != 0 {
			t.Errorf("%d, logs %+v", w.Code, sink.got)
		}
	})
}
