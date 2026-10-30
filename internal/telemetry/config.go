package telemetry

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// Each program's config is written as JSON, which every one of them reads as YAML, so it is
// built from Go values and never from text.

// Where each program listens inside the container. Only the collector's receivers listen
// beyond loopback.
const (
	lokiHTTP       = "127.0.0.1:3100"
	lokiGRPC       = 9095
	tempoHTTP      = "127.0.0.1:3200"
	tempoGRPC      = 9096
	tempoOTLP      = "127.0.0.1:4417" // It is apart from the collector's 4317.
	prometheusHTTP = "127.0.0.1:9090"
	collectorCheck = "127.0.0.1:13134"
)

type obj = map[string]any

func render(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func hours(days int) string { return fmt.Sprintf("%dh", days*24) }

// CollectorConfig is the OpenTelemetry collector's config: OTLP from services and Docker's
// output from every VM in, and each signal out to its store.
func CollectorConfig(c Config) string {
	limit := Shares(c.MemoryMiB)["collector"] * 4 / 5
	limiter := obj{"check_interval": "1s", "limit_mib": limit, "spike_limit_mib": limit / 5}
	pipeline := func(receiver, exporter string, middle ...string) obj {
		procs := append(append([]string{"memory_limiter"}, middle...), "batch")
		return obj{"receivers": []string{receiver}, "processors": procs, "exporters": []string{exporter}}
	}
	// Docker's fluentd driver sends each line with the container's labels and its stream as
	// attributes. They become the resource attributes OTLP records carry, so one query finds
	// a component's lines from both.
	move := func(from, to string) string {
		return fmt.Sprintf(`set(attributes["%s"], attributes["%s"]) where attributes["%s"] != nil`, to, from, from)
	}
	stdout := []string{
		move("protostrap.component", "service.name"),
		move("protostrap.replica", "service.instance.id"),
		move("protostrap.release", "service.version"),
		move("source", "log.iostream"),
		`delete_key(attributes, "protostrap.component")`,
		`delete_key(attributes, "protostrap.replica")`,
		`delete_key(attributes, "protostrap.release")`,
		`delete_key(attributes, "source")`,
		`delete_key(attributes, "container_id")`,
		`delete_key(attributes, "container_name")`,
		`delete_key(attributes, "fluent.tag")`,
	}
	return render(obj{
		"extensions": obj{"health_check": obj{"endpoint": collectorCheck}},
		"receivers": obj{
			"otlp": obj{"protocols": obj{
				"grpc": obj{"endpoint": fmt.Sprintf("0.0.0.0:%d", OTLPGRPCPort)},
				"http": obj{"endpoint": fmt.Sprintf("0.0.0.0:%d", OTLPHTTPPort)},
			}},
			"fluentforward": obj{"endpoint": fmt.Sprintf("0.0.0.0:%d", ForwardPort)},
		},
		"processors": obj{
			"memory_limiter": limiter,
			"batch":          obj{},
			"transform/stdout": obj{"error_mode": "ignore", "log_statements": []obj{
				{"context": "log", "statements": stdout},
			}},
			"groupbyattrs/stdout": obj{"keys": []string{"service.name", "service.instance.id", "service.version"}},
		},
		"exporters": obj{
			"otlphttp/loki":       obj{"endpoint": "http://" + lokiHTTP + "/otlp"},
			"otlp/tempo":          obj{"endpoint": tempoOTLP, "tls": obj{"insecure": true}},
			"otlphttp/prometheus": obj{"endpoint": "http://" + prometheusHTTP + "/api/v1/otlp"},
		},
		"service": obj{
			"extensions": []string{"health_check"},
			"pipelines": obj{
				"logs/otlp":   pipeline("otlp", "otlphttp/loki"),
				"logs/stdout": pipeline("fluentforward", "otlphttp/loki", "transform/stdout", "groupbyattrs/stdout"),
				"traces":      pipeline("otlp", "otlp/tempo"),
				"metrics":     pipeline("otlp", "otlphttp/prometheus"),
			},
			// The collector's own metrics would listen on every address, the overlay's too.
			"telemetry": obj{"metrics": obj{"level": "none"}, "logs": obj{"level": "warn"}},
		},
	})
}

func hostPort(addr string) (string, int) {
	host, port, _ := strings.Cut(addr, ":")
	var n int
	fmt.Sscan(port, &n)
	return host, n
}

// LokiConfig is Loki's config: one tenant, its chunks and index in the bucket, and its
// write-ahead log and caches on the volume.
func LokiConfig(c Config) string {
	dir := filepath.Join(c.DataDir, "loki")
	host, port := hostPort(lokiHTTP)
	return render(obj{
		"auth_enabled": false,
		"server": obj{
			"http_listen_address": host, "http_listen_port": port,
			"grpc_listen_address": host, "grpc_listen_port": lokiGRPC,
			"log_level": "warn",
		},
		"common": obj{
			"path_prefix":        dir,
			"replication_factor": 1,
			"ring":               obj{"instance_addr": host, "kvstore": obj{"store": "inmemory"}},
			"storage":            obj{"s3": obj{"bucketnames": c.Bucket, "region": c.Region}},
		},
		"schema_config": obj{"configs": []obj{{
			"from": "2026-01-01", "store": "tsdb", "object_store": "s3", "schema": "v13",
			"index": obj{"prefix": "loki_index_", "period": "24h"},
		}}},
		"storage_config": obj{"tsdb_shipper": obj{
			"active_index_directory": filepath.Join(dir, "index"),
			"cache_location":         filepath.Join(dir, "index-cache"),
		}},
		"ingester": obj{"wal": obj{"dir": filepath.Join(dir, "wal")}},
		"compactor": obj{
			"working_directory":    filepath.Join(dir, "compactor"),
			"retention_enabled":    true,
			"delete_request_store": "s3",
		},
		"limits_config": obj{
			"retention_period":          hours(c.Retention.Logs),
			"allow_structured_metadata": true,
			"volume_enabled":            true,
			// The replica is a label, so one replica's output is a stream of its own.
			"otlp_config": obj{"resource_attributes": obj{"attributes_config": []obj{
				{"action": "index_label", "attributes": []string{"service.instance.id"}},
			}}},
		},
		"analytics": obj{"reporting_enabled": false},
	})
}

// TempoConfig is Tempo 3's config, in monolithic mode: traces in the bucket under tempo/,
// beside Loki's objects, and its write-ahead logs on the volume. Retention is enforced by
// the backend worker, and the scheduler and the per-tenant default must agree with it.
func TempoConfig(c Config) string {
	dir := filepath.Join(c.DataDir, "tempo")
	host, port := hostPort(tempoHTTP)
	retention := hours(c.Retention.Traces)
	return render(obj{
		"server": obj{
			"http_listen_address": host, "http_listen_port": port,
			"grpc_listen_address": host, "grpc_listen_port": tempoGRPC,
			"log_level": "warn",
		},
		// Its ring would otherwise gossip on every address, the overlay's too.
		"memberlist":  obj{"bind_addr": []string{host}, "advertise_addr": host},
		"distributor": obj{"receivers": obj{"otlp": obj{"protocols": obj{"grpc": obj{"endpoint": tempoOTLP}}}}},
		"live_store": obj{
			"wal":                 obj{"path": filepath.Join(dir, "live-store", "traces")},
			"shutdown_marker_dir": filepath.Join(dir, "live-store", "shutdown-marker"),
		},
		"block_builder": obj{"wal": obj{"path": filepath.Join(dir, "block-builder", "traces")}},
		"backend_scheduler": obj{
			"local_work_path": filepath.Join(dir, "scheduler"),
			"provider":        obj{"compaction": obj{"compaction": obj{"block_retention": retention}}},
		},
		"backend_worker": obj{"compaction": obj{"block_retention": retention}},
		"overrides":      obj{"defaults": obj{"compaction": obj{"block_retention": retention}}},
		"storage": obj{"trace": obj{
			"backend": "s3",
			"s3": obj{
				"bucket": c.Bucket, "region": c.Region, "endpoint": "s3." + c.Region + ".amazonaws.com",
				"prefix": "tempo",
			},
			"wal":   obj{"path": filepath.Join(dir, "wal")},
			"local": obj{"path": filepath.Join(dir, "blocks")},
		}},
		"usage_report": obj{"reporting_enabled": false},
	})
}

// PrometheusConfig is Prometheus's config: every replica of each target, found by its
// component's name on the overlay, which resolves to every replica's address.
func PrometheusConfig(c Config, targets []Target) string {
	jobs := []obj{}
	for _, t := range targets {
		jobs = append(jobs, obj{
			"job_name":     t.Component,
			"metrics_path": t.Path,
			"dns_sd_configs": []obj{{
				"names": []string{t.Component}, "type": "A", "port": t.Port, "refresh_interval": "30s",
			}},
			"relabel_configs": []obj{{"target_label": "service_name", "replacement": t.Component}},
		})
	}
	return render(obj{
		"global":         obj{"scrape_interval": "30s", "evaluation_interval": "30s"},
		"otlp":           obj{"promote_resource_attributes": []string{"service.name", "service.instance.id", "service.version", "deployment.environment.name"}},
		"scrape_configs": jobs,
	})
}

// GrafanaINI is Grafana's config: its socket alone, the same role for everyone who reaches
// it, and nothing loaded from elsewhere. That role is Editor, since Grafana lets no Viewer
// explore. Grafana's own database is in the container, so whatever someone saves lasts until
// the replica restarts: the data sources come from files, uneditable, and no one can add a
// user.
func GrafanaINI(c Config) string {
	return strings.Join([]string{
		"[paths]",
		"data = " + grafanaData(c),
		"provisioning = " + filepath.Join(c.ConfigDir, "grafana", "provisioning"),
		"",
		"[server]",
		"protocol = socket",
		"socket = " + filepath.Join(c.SocketDir, SocketName),
		"socket_mode = 0660",
		"root_url = http://localhost:7482/",
		"enforce_domain = false",
		"",
		"[security]",
		"disable_gravatar = true",
		"disable_initial_admin_creation = true",
		"",
		"[auth]",
		"disable_login_form = true",
		"disable_signout_menu = true",
		"",
		"[auth.anonymous]",
		"enabled = true",
		"org_role = Editor",
		"",
		"[auth.basic]",
		"enabled = false",
		"",
		"[users]",
		"allow_sign_up = false",
		"",
		"[public_dashboards]",
		"enabled = false",
		"",
		"[analytics]",
		"reporting_enabled = false",
		"check_for_updates = false",
		"check_for_plugin_updates = false",
		"feedback_links_enabled = false",
		"",
		"[news]",
		"news_feed_enabled = false",
		"",
		"[plugins]",
		"plugin_admin_enabled = false",
		"",
		"[snapshots]",
		"external_enabled = false",
		"",
		"[unified_alerting]",
		"enabled = false",
		"",
		"[log]",
		"mode = console",
		"level = warn",
		"",
	}, "\n")
}

// GrafanaDatasources are Grafana's data sources, linked: a log line's trace ID opens the
// trace, and a span opens its component's logs.
func GrafanaDatasources() string {
	return render(obj{
		"apiVersion": 1,
		"datasources": []obj{
			{
				"name": "Loki", "type": "loki", "uid": "loki", "url": "http://" + lokiHTTP, "access": "proxy",
				"isDefault": true, "editable": false,
				"jsonData": obj{"maxLines": 1000, "derivedFields": []obj{{
					"name": "TraceID", "matcherType": "label", "matcherRegex": "trace_id",
					"datasourceUid": "tempo", "url": "$${__value.raw}",
				}}},
			},
			{
				"name": "Tempo", "type": "tempo", "uid": "tempo", "url": "http://" + tempoHTTP, "access": "proxy",
				"editable": false,
				"jsonData": obj{
					"tracesToLogsV2": obj{
						"datasourceUid": "loki", "spanStartTimeShift": "-5m", "spanEndTimeShift": "5m",
						"filterByTraceID": true, "tags": []obj{{"key": "service.name", "value": "service_name"}},
					},
					"tracesToMetrics": obj{"datasourceUid": "prometheus"},
					"nodeGraph":       obj{"enabled": true},
				},
			},
			{
				"name": "Prometheus", "type": "prometheus", "uid": "prometheus", "url": "http://" + prometheusHTTP,
				"access": "proxy", "editable": false,
				"jsonData": obj{"exemplarTraceIdDestinations": []obj{{"name": "trace_id", "datasourceUid": "tempo"}}},
			},
		},
	})
}

// grafanaData is Grafana's own data, its database included, which lives with the
// container rather than on the volume.
func grafanaData(c Config) string { return filepath.Join(c.ConfigDir, "grafana", "data") }
