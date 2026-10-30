package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These tests cover what `protostrap telemetry` writes for its five programs
// (docs/telemetry.md): above all, that only the collector's receivers listen beyond the
// container's loopback, since Loki, Tempo, Prometheus and Grafana have no sign-in.

func testConfig() Config {
	return Config{
		Region: "eu-west-1", Bucket: "protostrap-123456789012-eu-west-1-telemetry",
		Retention: Retention{Logs: 30, Traces: 14, Metrics: 15}, MemoryMiB: 1536,
		DataDir: "/var/lib/telemetry", FilesDir: "/etc/protostrap", SocketDir: "/run/protostrap/telemetry",
		ConfigDir: "/tmp/telemetry",
	}
}

// decode parses a rendered config: each is JSON, which every program reads as YAML.
func decode(t *testing.T, name, text string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatalf("%s isn't JSON: %v\n%s", name, err, text)
	}
	return v
}

// at walks a decoded config by keys.
func at(t *testing.T, v any, keys ...string) any {
	t.Helper()
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("no %s in %v (at %v)", k, v, keys)
		}
		v = m[k]
	}
	return v
}

// addresses lists every value under a key that names where something listens or sends.
func addresses(v any, path string, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			p := path + "." + k
			if s, ok := child.(string); ok && (k == "endpoint" || strings.HasSuffix(k, "_address") || k == "instance_addr") {
				out[p] = s
			}
			addresses(child, p, out)
		}
	case []any:
		for i, child := range x {
			addresses(child, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

func TestOnlyTheCollectorsReceiversListenBeyondLoopback(t *testing.T) {
	c := testConfig()
	for name, text := range map[string]string{
		"collector": CollectorConfig(c), "loki": LokiConfig(c), "tempo": TempoConfig(c),
	} {
		found := map[string]string{}
		addresses(decode(t, name, text), name, found)
		if len(found) == 0 {
			t.Errorf("%s names no address", name)
		}
		for p, addr := range found {
			open := map[string]bool{
				"collector.receivers.otlp.protocols.grpc.endpoint": true,
				"collector.receivers.otlp.protocols.http.endpoint": true,
				"collector.receivers.fluentforward.endpoint":       true,
			}[p]
			loopback := strings.HasPrefix(addr, "127.0.0.1") || strings.HasPrefix(addr, "http://127.0.0.1") || strings.HasPrefix(addr, "s3.")
			if !open && !loopback {
				t.Errorf("%s = %q: only the collector's receivers may listen beyond loopback", p, addr)
			}
		}
	}
	// Each program listens on loopback, where an address isn't an endpoint.
	for name, text := range map[string]string{"loki": LokiConfig(c), "tempo": TempoConfig(c)} {
		server := at(t, decode(t, name, text), "server").(map[string]any)
		if server["http_listen_address"] != "127.0.0.1" || server["grpc_listen_address"] != "127.0.0.1" {
			t.Errorf("%s server %v: on loopback", name, server)
		}
	}
}

func TestCollectorReceivesOTLPAndDockersOutput(t *testing.T) {
	v := decode(t, "collector", CollectorConfig(testConfig()))
	if got := at(t, v, "receivers", "otlp", "protocols", "grpc", "endpoint"); got != "0.0.0.0:4317" {
		t.Errorf("OTLP gRPC on %v", got)
	}
	if got := at(t, v, "receivers", "otlp", "protocols", "http", "endpoint"); got != "0.0.0.0:4318" {
		t.Errorf("OTLP HTTP on %v", got)
	}
	if got := at(t, v, "receivers", "fluentforward", "endpoint"); got != "0.0.0.0:24224" {
		t.Errorf("Docker's output on %v", got)
	}
	pipelines := at(t, v, "service", "pipelines").(map[string]any)
	want := map[string]struct{ receivers, exporters []string }{
		"logs/otlp":   {[]string{"otlp"}, []string{"otlphttp/loki"}},
		"logs/stdout": {[]string{"fluentforward"}, []string{"otlphttp/loki"}},
		"traces":      {[]string{"otlp"}, []string{"otlp/tempo"}},
		"metrics":     {[]string{"otlp"}, []string{"otlphttp/prometheus"}},
	}
	for name, w := range want {
		p, ok := pipelines[name].(map[string]any)
		if !ok {
			t.Errorf("no pipeline %s in %v", name, pipelines)
			continue
		}
		if got := strs(p["receivers"]); !slices.Equal(got, w.receivers) {
			t.Errorf("%s receivers %v", name, got)
		}
		if got := strs(p["exporters"]); !slices.Equal(got, w.exporters) {
			t.Errorf("%s exporters %v", name, got)
		}
		procs := strs(p["processors"])
		if len(procs) == 0 || procs[0] != "memory_limiter" || procs[len(procs)-1] != "batch" {
			t.Errorf("%s processors %v: the memory limiter first, batching last", name, procs)
		}
	}
	if len(pipelines) != len(want) {
		t.Errorf("pipelines %v", pipelines)
	}
	exporters := at(t, v, "exporters").(map[string]any)
	if got := at(t, exporters, "otlphttp/loki", "endpoint"); got != "http://127.0.0.1:3100/otlp" {
		t.Errorf("loki at %v", got)
	}
	if got := at(t, exporters, "otlp/tempo", "endpoint"); got != "127.0.0.1:4417" {
		t.Errorf("tempo at %v", got)
	}
	if got := at(t, exporters, "otlphttp/prometheus", "endpoint"); got != "http://127.0.0.1:9090/api/v1/otlp" {
		t.Errorf("prometheus at %v", got)
	}
	// The collector's own metrics would listen on every address: they're off.
	if got := at(t, v, "service", "telemetry", "metrics", "level"); got != "none" {
		t.Errorf("the collector's own metrics level %v", got)
	}
}

// TestStdoutLinesNameTheirComponentAndReplica checks the processing of Docker's output: its
// labels become the resource attributes OTLP records carry, so one query finds a
// component's lines from both.
func TestStdoutLinesNameTheirComponentAndReplica(t *testing.T) {
	text := CollectorConfig(testConfig())
	v := decode(t, "collector", text)
	procs := strs(at(t, v, "service", "pipelines", "logs/stdout", "processors"))
	if !slices.Contains(procs, "transform/stdout") || !slices.Contains(procs, "groupbyattrs/stdout") ||
		slices.Index(procs, "transform/stdout") > slices.Index(procs, "groupbyattrs/stdout") {
		t.Fatalf("processors %v: transform the labels, then group by them", procs)
	}
	transform := fmt.Sprint(at(t, v, "processors", "transform/stdout"))
	for _, want := range []string{`attributes["protostrap.component"]`, `attributes["protostrap.replica"]`, `attributes["protostrap.release"]`,
		`"service.name"`, `"service.instance.id"`, `"service.version"`, `"log.iostream"`, `attributes["source"]`} {
		if !strings.Contains(transform, want) {
			t.Errorf("transform/stdout lacks %s:\n%s", want, transform)
		}
	}
	keys := strs(at(t, v, "processors", "groupbyattrs/stdout", "keys"))
	if !slices.Equal(keys, []string{"service.name", "service.instance.id", "service.version"}) {
		t.Errorf("groupbyattrs keys %v", keys)
	}
}

func TestCollectorRefusesDataOverItsShareOfMemory(t *testing.T) {
	v := decode(t, "collector", CollectorConfig(testConfig()))
	limit := at(t, v, "processors", "memory_limiter", "limit_mib").(float64)
	share := float64(Shares(1536)["collector"])
	if limit <= 0 || limit > share {
		t.Errorf("limit %v MiB, share %v MiB", limit, share)
	}
}

func TestLokiKeepsLogsInTheBucketForTheirRetention(t *testing.T) {
	v := decode(t, "loki", LokiConfig(testConfig()))
	if got := at(t, v, "auth_enabled"); got != false {
		t.Errorf("auth_enabled %v: one tenant", got)
	}
	if got := at(t, v, "server", "http_listen_port"); got != float64(3100) {
		t.Errorf("port %v", got)
	}
	s3 := at(t, v, "common", "storage", "s3").(map[string]any)
	if s3["bucketnames"] != "protostrap-123456789012-eu-west-1-telemetry" || s3["region"] != "eu-west-1" {
		t.Errorf("s3 %v", s3)
	}
	if got := at(t, v, "limits_config", "retention_period"); got != "720h" {
		t.Errorf("retention %v, want 30 days", got)
	}
	if got := at(t, v, "compactor", "retention_enabled"); got != true {
		t.Errorf("the compactor must delete expired lines: %v", got)
	}
	if got := at(t, v, "compactor", "delete_request_store"); got != "s3" {
		t.Errorf("delete_request_store %v", got)
	}
	if got := at(t, v, "common", "path_prefix"); got != "/var/lib/telemetry/loki" {
		t.Errorf("path prefix %v: on the volume", got)
	}
	if got := at(t, v, "analytics", "reporting_enabled"); got != false {
		t.Errorf("analytics %v: nothing is sent elsewhere", got)
	}
	// The replica is indexed, so one replica's output is a stream of its own.
	attrs := fmt.Sprint(at(t, v, "limits_config", "otlp_config"))
	if !strings.Contains(attrs, "service.instance.id") || !strings.Contains(attrs, "index_label") {
		t.Errorf("otlp_config %s", attrs)
	}
}

func TestTempoKeepsTracesInTheBucketUnderItsPrefix(t *testing.T) {
	v := decode(t, "tempo", TempoConfig(testConfig()))
	s3 := at(t, v, "storage", "trace", "s3").(map[string]any)
	if s3["bucket"] != "protostrap-123456789012-eu-west-1-telemetry" || s3["region"] != "eu-west-1" ||
		s3["endpoint"] != "s3.eu-west-1.amazonaws.com" || s3["prefix"] != "tempo" {
		t.Errorf("s3 %v: beside Loki's objects, under tempo/", s3)
	}
	if got := at(t, v, "storage", "trace", "backend"); got != "s3" {
		t.Errorf("backend %v", got)
	}
	// Tempo 3 enforces retention in its backend worker; its scheduler and the per-tenant
	// default say the same.
	for _, path := range [][]string{
		{"backend_worker", "compaction", "block_retention"},
		{"backend_scheduler", "provider", "compaction", "compaction", "block_retention"},
		{"overrides", "defaults", "compaction", "block_retention"},
	} {
		if got := at(t, v, path...); got != "336h" {
			t.Errorf("%v = %v, want 14 days", path, got)
		}
	}
	if _, ok := v["compactor"]; ok {
		t.Error("Tempo 3 refuses a compactor block")
	}
	// Every path it writes is on the volume: its defaults are under /var/tempo, which its
	// user can't write.
	paths := map[string]string{}
	addresses(v, "tempo", paths)
	text := TempoConfig(testConfig())
	if strings.Contains(text, "/var/tempo") {
		t.Errorf("a path under /var/tempo:\n%s", text)
	}
	for _, p := range []any{
		at(t, v, "live_store", "wal", "path"), at(t, v, "live_store", "shutdown_marker_dir"),
		at(t, v, "block_builder", "wal", "path"), at(t, v, "backend_scheduler", "local_work_path"),
	} {
		if !strings.HasPrefix(fmt.Sprint(p), "/var/lib/telemetry/tempo/") {
			t.Errorf("path %v isn't on the volume", p)
		}
	}
	if got := fmt.Sprint(at(t, v, "memberlist", "bind_addr")); got != "[127.0.0.1]" {
		t.Errorf("memberlist binds %s: its gossip must stay on loopback", got)
	}
	if got := at(t, v, "distributor", "receivers", "otlp", "protocols", "grpc", "endpoint"); got != "127.0.0.1:4417" {
		t.Errorf("Tempo's own OTLP receiver %v: on loopback, apart from the collector's 4317", got)
	}
	if got := at(t, v, "storage", "trace", "wal", "path"); got != "/var/lib/telemetry/tempo/wal" {
		t.Errorf("wal %v", got)
	}
	if got := at(t, v, "usage_report", "reporting_enabled"); got != false {
		t.Errorf("usage report %v", got)
	}
}

func TestPrometheusScrapesEveryReplicaOfEachTarget(t *testing.T) {
	targets := []Target{{Component: "api", Port: 9090, Path: "/metrics"}, {Component: "edge", Port: 9902, Path: "/metrics"}}
	v := decode(t, "prometheus", PrometheusConfig(testConfig(), targets))
	if got := at(t, v, "global", "scrape_interval"); got != "30s" {
		t.Errorf("interval %v", got)
	}
	jobs, _ := at(t, v, "scrape_configs").([]any)
	if len(jobs) != 2 {
		t.Fatalf("jobs %v", jobs)
	}
	api := jobs[0].(map[string]any)
	if api["job_name"] != "api" || api["metrics_path"] != "/metrics" {
		t.Errorf("api job %v", api)
	}
	// A name resolves to every replica's address, so each one is a target of its own.
	sd := api["dns_sd_configs"].([]any)[0].(map[string]any)
	if fmt.Sprint(sd["names"]) != "[api]" || sd["type"] != "A" || sd["port"] != float64(9090) {
		t.Errorf("api discovery %v", sd)
	}
	if !strings.Contains(fmt.Sprint(api["relabel_configs"]), "service_name") {
		t.Errorf("api's series aren't labelled with its component: %v", api["relabel_configs"])
	}
	// OTLP metrics keep their component and replica as labels.
	promoted := strs(at(t, v, "otlp", "promote_resource_attributes"))
	for _, want := range []string{"service.name", "service.instance.id"} {
		if !slices.Contains(promoted, want) {
			t.Errorf("promoted %v lacks %s", promoted, want)
		}
	}
	if empty := decode(t, "prometheus", PrometheusConfig(testConfig(), nil)); at(t, empty, "scrape_configs") == nil {
		t.Errorf("no targets is an empty list, not null")
	}
}

func TestGrafanaHasNoSignInAndLoadsNothingFromElsewhere(t *testing.T) {
	ini := GrafanaINI(testConfig())
	for _, want := range []string{
		"protocol = socket", "socket = /run/protostrap/telemetry/grafana.sock",
		"root_url = http://localhost:7482/",
		"[auth.anonymous]\nenabled = true\norg_role = Editor",
		"disable_login_form = true", "[auth.basic]\nenabled = false",
		"allow_sign_up = false", "[public_dashboards]\nenabled = false",
		"reporting_enabled = false", "check_for_updates = false", "check_for_plugin_updates = false",
		"news_feed_enabled = false", "disable_gravatar = true", "external_enabled = false",
		"plugin_admin_enabled = false",
		// Grafana's own database lives with the container, so what someone saves goes with it.
		"data = /tmp/telemetry/grafana/data", "provisioning = /tmp/telemetry/grafana/provisioning",
	} {
		if !strings.Contains(ini, want) {
			t.Errorf("grafana.ini lacks %q:\n%s", want, ini)
		}
	}
	if strings.Contains(ini, "http_addr") || strings.Contains(ini, "http_port") {
		t.Errorf("grafana listens on its socket only:\n%s", ini)
	}
}

func TestGrafanasDataSourcesAreLinked(t *testing.T) {
	v := decode(t, "datasources", GrafanaDatasources())
	sources, _ := v["datasources"].([]any)
	byUID := map[string]map[string]any{}
	for _, s := range sources {
		m := s.(map[string]any)
		byUID[fmt.Sprint(m["uid"])] = m
	}
	want := map[string][2]string{"loki": {"loki", "http://127.0.0.1:3100"}, "tempo": {"tempo", "http://127.0.0.1:3200"}, "prometheus": {"prometheus", "http://127.0.0.1:9090"}}
	for uid, w := range want {
		s := byUID[uid]
		if s == nil || s["type"] != w[0] || s["url"] != w[1] || s["editable"] != false {
			t.Errorf("%s: %v", uid, s)
		}
	}
	if fmt.Sprint(byUID["loki"]["isDefault"]) != "true" {
		t.Error("Loki is the default: most visits are for logs")
	}
	if s := fmt.Sprint(byUID["loki"]["jsonData"]); !strings.Contains(s, "trace_id") || !strings.Contains(s, "datasourceUid:tempo") {
		t.Errorf("a log line's trace ID doesn't open Tempo: %s", s)
	}
	if s := fmt.Sprint(byUID["tempo"]["jsonData"]); !strings.Contains(s, "tracesToLogsV2") || !strings.Contains(s, "datasourceUid:loki") {
		t.Errorf("a span doesn't open its logs: %s", s)
	}
}

func TestEachProgramGetsItsShareOfMemory(t *testing.T) {
	s := Shares(1536)
	if s["loki"] != 384 || s["tempo"] != 384 || s["prometheus"] != 384 {
		t.Errorf("shares %v: Loki, Tempo and Prometheus a quarter each", s)
	}
	if s["collector"] <= s["grafana"] || s["collector"]+s["grafana"] > 384 {
		t.Errorf("shares %v: the collector and Grafana split the rest", s)
	}
	for _, p := range Programs(testConfig()) {
		var limit string
		for _, kv := range p.Env {
			if v, ok := strings.CutPrefix(kv, "GOMEMLIMIT="); ok {
				limit = v
			}
		}
		if limit != fmt.Sprintf("%dMiB", s[p.Name]) {
			t.Errorf("%s GOMEMLIMIT %q, want its share %dMiB", p.Name, limit, s[p.Name])
		}
	}
}

func TestProgramsAndTheirArguments(t *testing.T) {
	c := testConfig()
	c.Bin = Binaries{Collector: "/usr/local/bin/otelcol", Loki: "/usr/local/bin/loki", Tempo: "/usr/local/bin/tempo",
		Prometheus: "/usr/local/bin/prometheus", Grafana: "/usr/share/grafana/bin/grafana", GrafanaHome: "/usr/share/grafana"}
	byName := map[string]Program{}
	var order []string
	for _, p := range Programs(c) {
		byName[p.Name] = p
		order = append(order, p.Name)
	}
	// The stores start before the collector, so intake has somewhere to go.
	if !slices.Equal(order, []string{"loki", "tempo", "prometheus", "grafana", "collector"}) {
		t.Errorf("order %v", order)
	}
	check := func(name, path string, args ...string) {
		t.Helper()
		p := byName[name]
		if p.Path != path {
			t.Errorf("%s path %q", name, p.Path)
		}
		for _, a := range args {
			if !slices.Contains(p.Args, a) {
				t.Errorf("%s args %v lack %q", name, p.Args, a)
			}
		}
	}
	check("collector", "/usr/local/bin/otelcol", "--config=/tmp/telemetry/collector.json")
	check("loki", "/usr/local/bin/loki", "-config.file=/tmp/telemetry/loki.json")
	check("tempo", "/usr/local/bin/tempo", "-config.file=/tmp/telemetry/tempo.json")
	check("prometheus", "/usr/local/bin/prometheus", "--config.file=/tmp/telemetry/prometheus.json",
		"--storage.tsdb.path=/var/lib/telemetry/prometheus", "--storage.tsdb.retention.time=15d",
		"--web.listen-address=127.0.0.1:9090", "--web.enable-otlp-receiver")
	check("grafana", "/usr/share/grafana/bin/grafana", "server", "--homepath=/usr/share/grafana", "--config=/tmp/telemetry/grafana.ini")
}

// TestGrafanasPathsWinOverItsImages checks Grafana's environment: its image sets GF_PATHS_*,
// which win over grafana.ini, so the supervisor sets them to the same paths.
func TestGrafanasPathsWinOverItsImages(t *testing.T) {
	for _, p := range Programs(testConfig()) {
		if p.Name != "grafana" {
			continue
		}
		for _, want := range []string{
			"GF_PATHS_DATA=/tmp/telemetry/grafana/data",
			"GF_PATHS_PLUGINS=/tmp/telemetry/grafana/data/plugins",
			"GF_PATHS_LOGS=/tmp/telemetry/grafana/data/log",
			"GF_PATHS_PROVISIONING=/tmp/telemetry/grafana/provisioning",
			"GF_PATHS_CONFIG=/tmp/telemetry/grafana.ini",
		} {
			if !slices.Contains(p.Env, want) {
				t.Errorf("grafana's env %v lacks %s", p.Env, want)
			}
		}
		return
	}
	t.Fatal("no grafana")
}

func TestEnvConfig(t *testing.T) {
	env := map[string]string{
		"AWS_REGION": "eu-west-1", "TELEMETRY_BUCKET": "protostrap-1-eu-west-1-telemetry",
		"TELEMETRY_LOGS_DAYS": "30", "TELEMETRY_TRACES_DAYS": "14", "TELEMETRY_METRICS_DAYS": "15",
	}
	c, err := EnvConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Region != "eu-west-1" || c.Bucket != "protostrap-1-eu-west-1-telemetry" || c.Retention != (Retention{30, 14, 15}) {
		t.Errorf("config %+v", c)
	}
	for _, key := range []string{"AWS_REGION", "TELEMETRY_BUCKET", "TELEMETRY_LOGS_DAYS"} {
		broken := map[string]string{}
		for k, v := range env {
			broken[k] = v
		}
		delete(broken, key)
		if _, err := EnvConfig(func(k string) string { return broken[k] }); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("without %s: %v", key, err)
		}
	}
	env["TELEMETRY_TRACES_DAYS"] = "0"
	if _, err := EnvConfig(func(k string) string { return env[k] }); err == nil {
		t.Error("0 days of traces accepted")
	}
}

func TestCgroupMemory(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "memory.max")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	if got, err := CgroupMemoryMiB(write("1610612736\n")); err != nil || got != 1536 {
		t.Errorf("got %d %v", got, err)
	}
	if got, err := CgroupMemoryMiB(write("max\n")); err != nil || got != 0 {
		t.Errorf("no limit: got %d %v", got, err)
	}
	if _, err := CgroupMemoryMiB(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing file is an error")
	}
}

func TestTargets(t *testing.T) {
	ts := []Target{{Component: "web", Port: 9100, Path: "/stats"}, {Component: "api", Port: 9090, Path: "/metrics"}}
	text := TargetsFile(ts)
	back, err := ParseTargets([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(back, []Target{ts[1], ts[0]}) {
		t.Errorf("targets %v: sorted by component, so the file is stable", back)
	}
	for _, bad := range []string{
		`[{"component":"API","port":9090,"path":"/metrics"}]`,
		`[{"component":"api","port":0,"path":"/metrics"}]`,
		`[{"component":"api","port":9090,"path":"metrics"}]`,
		`not json`,
	} {
		if _, err := ParseTargets([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if got, err := ParseTargets(nil); err != nil || len(got) != 0 {
		t.Errorf("no file is no targets: %v %v", got, err)
	}
}

func strs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, fmt.Sprint(x))
	}
	return out
}
