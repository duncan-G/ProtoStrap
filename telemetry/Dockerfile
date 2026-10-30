# Telemetry (docs/telemetry.md) runs the OpenTelemetry collector, Loki, Tempo, Prometheus and
# Grafana in one container on data-1, as the edge runs cloudflared and Envoy. `protostrap
# telemetry` writes their configs, runs all five, and answers the agent's readiness check.
#
# The deploy workflow builds this image once per ProtoStrap version, for linux/arm64, with
# that version's protostrap binary copied in beside this file, and pushes it to
# protostrap/telemetry:<version>. Each program's version is pinned here.
#
# It starts as root only to give its directories to the user its programs run as, 10001,
# which `protostrap telemetry` drops to. The four Go programs are static, so they run on
# Grafana's base image as they are.
FROM otel/opentelemetry-collector-contrib:0.164.0 AS collector
FROM grafana/loki:3.7.9 AS loki
FROM grafana/tempo:3.1.1 AS tempo
FROM prom/prometheus:v3.15.1 AS prometheus

FROM grafana/grafana:13.3.0
COPY --from=collector /otelcol-contrib /usr/local/bin/otelcol-contrib
COPY --from=loki /usr/bin/loki /usr/local/bin/loki
COPY --from=tempo /tempo /usr/local/bin/tempo
COPY --from=prometheus /bin/prometheus /usr/local/bin/prometheus
COPY protostrap /usr/local/bin/protostrap
USER root
ENTRYPOINT ["/usr/local/bin/protostrap", "telemetry"]
