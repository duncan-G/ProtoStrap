package reporter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// These tests cover reading the components' errors from telemetry's Loki, through Grafana's
// data source proxy on its socket (docs/protostrap.md#reporter).

func TestLokiReadsTheComponentsErrorLines(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"success","data":{"resultType":"streams","result":[
			{"stream":{"service_name":"api","service_instance_id":"api-k3f9"},"values":[
				["1761390000000000000","fail: boom"],
				["1761389940000000000","Unhandled exception. System.InvalidOperationException"]]},
			{"stream":{"service_name":"web","log_iostream":"stderr"},"values":[
				["1761389880000000000","Error: ECONNREFUSED"]]}]}}`))
	}))
	defer srv.Close()

	l := &Loki{Client: srv.Client(), Base: srv.URL}
	to := time.Unix(1761390060, 0).UTC()
	from := to.Add(-24 * time.Hour)
	lines, err := l.Errors(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/api/datasources/proxy/uid/loki/loki/api/v1/query_range" {
		t.Errorf("%s %s: Grafana's proxy to the loki data source", got.Method, got.URL.Path)
	}
	q := got.URL.Query()
	query := q.Get("query")
	for _, want := range []string{`service_name=~".+"`, "(?i)", "error", "exception", "panic", "fatal", "traceback", "unhandled", "critical"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q lacks %q", query, want)
		}
	}
	if q.Get("start") != "1761303660000000000" || q.Get("end") != "1761390060000000000" {
		t.Errorf("window %s to %s, in nanoseconds", q.Get("start"), q.Get("end"))
	}
	if q.Get("limit") != "5000" || q.Get("direction") != "backward" {
		t.Errorf("limit %s direction %s: the newest 5,000 lines", q.Get("limit"), q.Get("direction"))
	}
	want := []LogLine{
		{Time: time.Unix(0, 1761390000000000000).UTC(), Component: "api", Message: "fail: boom"},
		{Time: time.Unix(0, 1761389940000000000).UTC(), Component: "api", Message: "Unhandled exception. System.InvalidOperationException"},
		{Time: time.Unix(0, 1761389880000000000).UTC(), Component: "web", Message: "Error: ECONNREFUSED"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines %+v", lines)
	}
	for i := range want {
		if !lines[i].Time.Equal(want[i].Time) || lines[i].Component != want[i].Component || lines[i].Message != want[i].Message {
			t.Errorf("line %d: %+v, want %+v", i, lines[i], want[i])
		}
	}
}

func TestLokiSaysWhyItFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "parse error at line 1", http.StatusBadRequest)
	}))
	defer srv.Close()
	_, err := (&Loki{Client: srv.Client(), Base: srv.URL}).Errors(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("err %v", err)
	}
}

func TestLokiWhenTelemetryIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	if _, err := (&Loki{Client: http.DefaultClient, Base: base}).Errors(context.Background(), time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Error("no error from a telemetry that isn't there")
	}
}
