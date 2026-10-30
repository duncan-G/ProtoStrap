package metrics

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const exposition = `# HELP llm_budget_cap Replicas the LLM budget allows, -1 for no cap.
# TYPE llm_budget_cap gauge
llm_budget_cap -1
# TYPE envoy_cluster_upstream_rq_total counter
envoy_cluster_upstream_rq_total{envoy_cluster_name="api"} 1027 1729760000000
envoy_cluster_upstream_rq_total{envoy_cluster_name="orders",zone="a"} 3
envoy_cluster_upstream_rq_total{envoy_cluster_name="orders",zone="b"} 4
queue_wait_seconds{quantile="0.5"} 0.25
weird_value NaN
plus_inf +Inf
escaped{path="a \"quoted\" \\ value\nnext"} 7
spaced { a = "1" , b="2" , } 9
`

func TestParse(t *testing.T) {
	samples, err := Parse(strings.NewReader(exposition))
	if err != nil {
		t.Fatal(err)
	}
	want := []Sample{
		{Name: "llm_budget_cap", Value: -1},
		{Name: "envoy_cluster_upstream_rq_total", Labels: map[string]string{"envoy_cluster_name": "api"}, Value: 1027},
		{Name: "envoy_cluster_upstream_rq_total", Labels: map[string]string{"envoy_cluster_name": "orders", "zone": "a"}, Value: 3},
		{Name: "envoy_cluster_upstream_rq_total", Labels: map[string]string{"envoy_cluster_name": "orders", "zone": "b"}, Value: 4},
		{Name: "queue_wait_seconds", Labels: map[string]string{"quantile": "0.5"}, Value: 0.25},
		{Name: "weird_value", Value: math.NaN()},
		{Name: "plus_inf", Value: math.Inf(1)},
		{Name: "escaped", Labels: map[string]string{"path": "a \"quoted\" \\ value\nnext"}, Value: 7},
		{Name: "spaced", Labels: map[string]string{"a": "1", "b": "2"}, Value: 9},
	}
	if len(samples) != len(want) {
		t.Fatalf("got %d samples: %+v", len(samples), samples)
	}
	for i := range want {
		g, w := samples[i], want[i]
		if g.Name != w.Name || !reflect.DeepEqual(g.Labels, w.Labels) && !(len(g.Labels) == 0 && len(w.Labels) == 0) {
			t.Errorf("sample %d: got %+v want %+v", i, g, w)
		}
		if math.IsNaN(w.Value) != math.IsNaN(g.Value) || (!math.IsNaN(w.Value) && g.Value != w.Value) {
			t.Errorf("sample %d value: got %v want %v", i, g.Value, w.Value)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, bad := range []string{
		"no_value\n",
		"name{unterminated=\"x} 1\n",
		"name 12abc\n",
		"name{a=1} 1\n",
		"{a=\"1\"} 1\n",
	} {
		if _, err := Parse(strings.NewReader(bad)); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestParseSelector(t *testing.T) {
	s, err := ParseSelector(`envoy_cluster_upstream_rq_total{envoy_cluster_name="api"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "envoy_cluster_upstream_rq_total" || !reflect.DeepEqual(s.Labels, map[string]string{"envoy_cluster_name": "api"}) {
		t.Fatalf("got %+v", s)
	}
	if _, err := ParseSelector(`bad name`); err == nil {
		t.Fatal("expected an error")
	}
}

func TestSelect(t *testing.T) {
	samples, _ := Parse(strings.NewReader(exposition))
	got := Select(samples, []string{
		"llm_budget_cap",
		`envoy_cluster_upstream_rq_total{envoy_cluster_name="api"}`,
		`envoy_cluster_upstream_rq_total{envoy_cluster_name="orders"}`, // This selector sums two series.
		"missing_metric",
		"weird_value", // NaN can't be sent as JSON.
		"plus_inf",
		"bad selector{",
	})
	want := map[string]float64{
		"llm_budget_cap": -1,
		`envoy_cluster_upstream_rq_total{envoy_cluster_name="api"}`:    1027,
		`envoy_cluster_upstream_rq_total{envoy_cluster_name="orders"}`: 7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("selected values must be valid JSON: %v", err)
	}
}

func TestSelectPrefersTheUnlabelledSeries(t *testing.T) {
	samples, _ := Parse(strings.NewReader("x 5\nx{a=\"1\"} 2\n"))
	if got := Select(samples, []string{"x"}); got["x"] != 5 {
		t.Fatalf("got %v", got)
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := map[string]struct {
		port int
		path string
	}{
		"9090/metrics":    {9090, "/metrics"},
		"9090":            {9090, "/metrics"},
		"9901/stats/prom": {9901, "/stats/prom"},
	}
	for in, want := range cases {
		port, path, err := ParseEndpoint(in)
		if err != nil || port != want.port || path != want.path {
			t.Errorf("ParseEndpoint(%q) = %d %q %v", in, port, path, err)
		}
	}
	for _, bad := range []string{"", "metrics", "0/metrics", "99999/x", "/metrics"} {
		if _, _, err := ParseEndpoint(bad); err == nil {
			t.Errorf("ParseEndpoint(%q) should fail", bad)
		}
	}
}

func TestScrape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(exposition))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	s := NewScraper(time.Second)
	got, err := s.Scrape(t.Context(), host, port+"/metrics", []string{"llm_budget_cap"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]float64{"llm_budget_cap": -1}) {
		t.Fatalf("got %v", got)
	}
}

func TestScrapeFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if _, err := NewScraper(time.Second).Scrape(t.Context(), host, port+"/metrics", []string{"x"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestScrapeBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("x 1\n"))
		w.Write([]byte(strings.Repeat("# padding\n", 2<<20)))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if _, err := NewScraper(time.Second).Scrape(t.Context(), host, port+"/metrics", []string{"x"}); err == nil {
		t.Fatal("an endpoint larger than the limit is an error")
	}
}
