---
name: protostrap-telemetry
description: Send logs, traces and metrics from an application deployed with ProtoStrap, and check they arrive - OpenTelemetry setup for .NET (Aspire ServiceDefaults included), Node.js and Python from the OTEL_ variables ProtoStrap sets, logs on stdout or over OTLP but never both, custom spans and metrics and registering them, trace context through SQS jobs, sampling, metrics endpoints for scale signals, label cardinality, telemetry from browsers, the spec's telemetry section, and local runs. Use when adding tracing, metrics or structured logging, when log lines appear twice, a custom metric or span never shows in Grafana, a trace stops at a queue, or telemetry's retention or size needs changing. To read telemetry while diagnosing a problem, use protostrap-operate.
---

# Telemetry

Every deployed environment runs one telemetry replica on data-1: an OpenTelemetry collector,
Loki for logs, Tempo for traces, Prometheus for metrics, and Grafana to read them. A service
needs no ProtoStrap code for it: its stdout is collected, and OpenTelemetry's SDKs find the
collector in variables ProtoStrap sets.

The design is `docs/telemetry.md` at the spec's `protostrap:` version
(`.protostrap/docs/telemetry.md` or `../ProtoStrap/docs/telemetry.md` if checked out at
`v<version>`, otherwise on GitHub at the tag). Where it and this skill disagree, it wins.

Code for each language: [.NET](references/dotnet.md), [Node.js](references/node.md),
[Python](references/python.md).

## Rules for you

- **No endpoint, protocol or service name in code or in the spec's `env`.** The SDK reads
  the `OTEL_` variables. `aspire run` sets the same ones, pointing at its dashboard.
- **Never log a secret, a token, a connection string, or a person's data** such as an email
  address. Everyone who can open Grafana in that environment reads every service's logs.
  The same goes for span attributes.
- **Never give a metric a label with unbounded values:** a user, an ID, an email, a raw
  path. Each distinct label set is a series Prometheus keeps in memory, on data-1 beside
  PostgreSQL. Label by route template, status class, queue or outcome. Spans may carry IDs.
- **Never add a vendor's agent or a second backend.** The collector is already there. An
  application that wants its own collector puts it in front of ProtoStrap's (below).

## What arrives where

| Sent as                                  | Reaches                                                      | Kept in    |
| ---------------------------------------- | ------------------------------------------------------------ | ---------- |
| A line on stdout or stderr               | Docker's log driver, then the collector                      | Loki       |
| OTLP logs                                | `telemetry:4318` on the overlay                              | Loki       |
| OTLP traces                              | The same                                                     | Tempo      |
| OTLP metrics                             | The same                                                     | Prometheus |
| The spec's `metrics` endpoint            | Scraped every 30 s from every replica; agents read it for scaling | Prometheus |

Every record is labelled `service_name` (the component), `service_instance_id` (the
replica, as the infra log names it) and `service_version` (the release), whichever way it
came. Stdout lines also carry `log_iostream`, and OTLP log records carry `trace_id`.

ProtoStrap sets, in every replica but its own:

| Variable                      | Value                                                     |
| ----------------------------- | --------------------------------------------------------- |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://telemetry:4318`                                   |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf`                                           |
| `OTEL_SERVICE_NAME`           | The component                                             |
| `OTEL_RESOURCE_ATTRIBUTES`    | `service.instance.id`, `service.version`, `deployment.environment.name` |

The spec's `env` wins for each one it sets, and setting the endpoint or the protocol drops
both of ProtoStrap's. Other `OTEL_` variables, such as the sampler's, are the spec's to set.

## Logs: stdout or OTLP, never both

A component that writes its logs to stdout **and** exports them over OTLP has every line
twice. Pick one, for each component:

- **OTLP, with the console logger off under ProtoStrap.** Usually better: each record
  carries its trace's ID, so a line opens its trace in Grafana. What the program prints
  before its logging starts, and a crash's stack trace, still arrive from stderr.
- **Or one JSON object per line on stdout, and no OTLP log exporter.** Loki parses it with
  `| json`. Put the trace ID in each line to find its trace.

"Under ProtoStrap" means `PROTOSTRAP_REPLICA` is set. Keep the console locally, where
people read it.

**Aspire's ServiceDefaults exports logs over OTLP whenever `OTEL_EXPORTER_OTLP_ENDPOINT` is
set,** which ProtoStrap always does. A ServiceDefaults that also adds a console logger
under ProtoStrap, for JSON lines, sends each line twice. The fix is in
[references/dotnet.md](references/dotnet.md#aspire-servicedefaults).

Log what happened, at `Information`; not one line per request under load. Never log the
readiness check or a metrics scrape.

## Traces

- **Instrument the server framework and the HTTP and gRPC clients** with the SDK's
  instrumentation. That covers most spans, and the clients pass `traceparent` to the
  services they call, so a request's trace crosses services with no code.
- **Leave out the readiness path**, `ready` in the spec. It is checked continually, by the
  agent and by Envoy, and would bury real traces.
- **Custom spans** come from one tracer (an `ActivitySource` in .NET) named after the
  component or its assembly. **In .NET, a source that isn't added with `AddSource` records
  nothing,** silently.
- **Through a queue, carry the trace yourself.** The sender injects the current context
  into the message's attributes (`traceparent`, `tracestate`); the worker receives with
  `MessageAttributeNames: ["All"]`, extracts it, and starts its job's span as a consumer
  with that parent. Each reference has the code. Some AWS SDK instrumentations already inject
  on send (.NET's `AddAWSInstrumentation()` does): then only extract. SQS allows 10
  attributes a message, so leave room.
- **Sampling.** The SDKs keep every trace by default. When traffic makes that too much,
  set it in the spec's `env`, not in code:

  ```yaml
  env:
    OTEL_TRACES_SAMPLER: parentbased_traceidratio
    OTEL_TRACES_SAMPLER_ARG: "0.1"
  ```

## Metrics

Two ways in. Both land in Prometheus, but **only one drives scaling**:

|                 | OTLP metrics                              | The `metrics` endpoint                               |
| --------------- | ----------------------------------------- | ---------------------------------------------------- |
| For             | What people look at in Grafana            | Anything a `metric` scale signal or limit reads      |
| How             | The SDK's OTLP metric exporter            | Prometheus text on a port and path, named in the spec's `metrics: 9090/metrics` |
| Seen by scaling | **Never**                                 | Yes: agents read every replica's endpoint            |

- **A signal or limit must be on the `metrics` endpoint.** A metric sent only over OTLP is
  in Grafana, and the scheduler never sees it: the signal has no value, and a limit with no
  value for 5 minutes doesn't cap. Spec side: protostrap-spec's `references/scaling.md`.
- **Serve the endpoint on a port of its own**, as ProtoStrap's sample .NET and Node services
  do (`sample/services/dotnet/Api/ApiMetrics.cs`, `sample/services/web/src/server.js`).
- **The name in the spec is the name in the text.** Exporters rewrite OpenTelemetry names:
  `shop.queue.backlog` becomes `shop_queue_backlog`, and a counter gains `_total`. Run the
  service and `curl` the endpoint before writing the signal.
- **In .NET, a `Meter` that isn't added with `AddMeter` exports nothing.**
- **The edge counts every routed service's requests** with nothing from the service:
  `envoy_cluster_upstream_rq_total`, by `envoy_cluster_name`.

## Checking it arrives

Locally, in `aspire run`'s dashboard, which shows one run's logs, traces and metrics. In
prod, through Grafana's forward (protostrap-operate says how to open it; Developers open it
with their own sign-in), with `curl -sG` and `--data-urlencode`:

| Check                    | Query                                                                                       |
| ------------------------ | ------------------------------------------------------------------------------------------- |
| Its logs are there       | Loki `{service_name="api"}`, newest first. Each line twice means both log paths are on      |
| Its traces are there     | Tempo search `{ resource.service.name = "api" }`                                            |
| Its own metric is there  | Prometheus `count by (__name__) ({service_name="api"})`, and look for the name             |
| Its scale metric is read | The operator UI's `/api/status`: the component's signal has a reading (Platform or Org Admins) |

`http://localhost:7482/protostrap/help` describes every call.

## Telemetry from browsers

The collector isn't reachable from the internet. To take telemetry from browsers, run a
collector of the application's own as a service: a Dockerfile `FROM` the OpenTelemetry
collector's image, pinned by digest, with its config in the image, a route such as
`/otlp/`, a `rate` on that route, and an exporter to `http://telemetry:4318` over OTLP/HTTP.
Anyone can send to that route, so accept only what the browser code sends.

## A collector of the application's own

For processing ProtoStrap's collector doesn't do, point components at the application's
collector in the spec's `env`, the endpoint and protocol together:

```yaml
env:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector:4318
  OTEL_EXPORTER_OTLP_PROTOCOL: http/protobuf
```

It exports to `http://telemetry:4318`, OTLP over HTTP. It must not change `service.name` or
`service.instance.id`, or its records lose their component and replica.

## The spec's telemetry section

```yaml
telemetry:
  request: { cpu: 500m, memory: 1536Mi }   # on data-1, beside the stateful components; at least 1Gi
  volume: 20Gi                             # Prometheus; at least 5Gi, only grows
  retention: { logs: 30d, traces: 30d, metrics: 15d }
```

- **All of it is optional;** those are the defaults. `protostrap check` refuses a request
  that doesn't fit on data-1 beside its stateful components.
- **A change replaces the replica:** the old stops before the new starts, so nothing is
  taken in for about a minute. Say so in the pull request.
- **Its memory is a hard limit,** like any replica's. If the infra log shows the telemetry
  replica failing with exit code 137, raise `request`'s memory before raising retention.
- **With an Aspire AppHost,** ProtoStrap.Hosting writes no `telemetry` section, so the
  generated spec runs the defaults. Tell the user if the application needs others: the
  generated spec can't be edited by hand.

## Locally

- **`aspire run`:** the dashboard is the collector and viewer. Nothing to change.
- **Without Aspire,** configure exporters only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set, so
  a plain local run doesn't log export errors. To see telemetry, run any OTLP viewer, such
  as Aspire's standalone dashboard image, and set the variable to it.

## Reviewing telemetry code

Check, for each component: no endpoint or service name in code; logs leave by one path under
ProtoStrap; every custom `ActivitySource` and `Meter` is registered (.NET); the readiness path
isn't traced; no metric label takes unbounded values; nothing logs or tags a secret or a
person's data; workers carry the trace through their queue; any metric a scale signal or
limit names is served on the `metrics` endpoint under the name in the spec.
