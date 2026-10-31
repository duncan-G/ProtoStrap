# Python telemetry under ProtoStrap

Packages: `opentelemetry-distro`, `opentelemetry-exporter-otlp-proto-http`, and the
instrumentations for the libraries the service uses, which
`opentelemetry-bootstrap -a requirements` lists. Pin them all in `requirements.txt`, which is
in the component's `sources`. Don't run `opentelemetry-bootstrap -a install` in the
Dockerfile: it installs whatever is newest at build time. ProtoStrap's sample Python service
doesn't use OpenTelemetry, so this hasn't run there: check names against the installed
versions' documentation.

## Starting it

`opentelemetry-instrument` wraps the command, and configures everything from the `OTEL_`
variables:

```dockerfile
CMD ["opentelemetry-instrument", "python", "main.py"]
```

- With `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`, which ProtoStrap sets, it picks the HTTP
  exporter, so `opentelemetry-exporter-otlp-proto-http` must be installed. Without the
  variable the distro picks gRPC, and with only the HTTP exporter installed it starts with no
  telemetry: under `aspire run`, give the container `WithOtlpExporter(OtlpProtocol.HttpProtobuf)`.
- **Keep one process per replica.** Uvicorn's `--workers` and gunicorn fork after the SDK
  starts, and forked workers need extra setup to export. ProtoStrap scales replicas instead.
- **Leave out the readiness path and metrics:**
  `OTEL_PYTHON_FASTAPI_EXCLUDED_URLS=healthz,metrics` in the spec's `env` (each instrumentation
  has its own `..._EXCLUDED_URLS`).
- Locally, without `OTEL_EXPORTER_OTLP_ENDPOINT`, run `python main.py` as before, or set
  `OTEL_TRACES_EXPORTER=none` and `OTEL_METRICS_EXPORTER=none`.

## Logs

Keep the JSON formatter on stdout (protostrap-contract's `references/python.md`), and turn
the OTLP log handler off: **`opentelemetry-instrument` adds one by default** (from
opentelemetry-instrumentation-logging 0.66b, `OTEL_PYTHON_LOG_AUTO_INSTRUMENTATION` defaults
to true), and adds it again after `logging.basicConfig(force=True)`, so every line leaves
twice. Set, in the Dockerfile or the spec's `env`:

```dockerfile
ENV OTEL_PYTHON_LOG_AUTO_INSTRUMENTATION=false OTEL_LOGS_EXPORTER=none OTEL_PYTHON_LOG_CORRELATION=true
```

With log correlation on, the logging instrumentation puts `otelTraceID` and `otelSpanID` on
each record, for the formatter to write:

```python
"trace_id": getattr(record, "otelTraceID", None),
"span_id": getattr(record, "otelSpanID", None),
```

## Custom spans and metrics

```python
from opentelemetry import metrics, trace

tracer = trace.get_tracer("shop.orders")
orders_placed = metrics.get_meter("shop.orders").create_counter("shop.orders.placed", description="Orders placed")

with tracer.start_as_current_span("place order") as span:
    span.set_attribute("order.id", order_id)   # IDs on spans, never on metrics
    orders_placed.add(1, {"channel": "web"})
```

Python needs no registration: the SDK exports every tracer and meter.

## A trace through a queue

`opentelemetry-bootstrap` may add an SQS instrumentation that injects on send already: look
for `traceparent` among a sent message's attributes before injecting by hand.

```python
from opentelemetry import propagate, trace

def send(sqs, queue_url: str, body: str) -> None:
    carrier: dict[str, str] = {}
    propagate.inject(carrier)
    sqs.send_message(
        QueueUrl=queue_url, MessageBody=body,
        MessageAttributes={k: {"DataType": "String", "StringValue": v} for k, v in carrier.items()},
    )
```

```python
tracer = trace.get_tracer("orders-worker")

response = sqs.receive_message(
    QueueUrl=queue_url, WaitTimeSeconds=20, MaxNumberOfMessages=10,
    MessageAttributeNames=["All"],   # Without this, SQS returns no attributes.
)
for message in response.get("Messages", []):
    carrier = {k: v["StringValue"] for k, v in message.get("MessageAttributes", {}).items() if "StringValue" in v}
    with tracer.start_as_current_span("process order", context=propagate.extract(carrier), kind=trace.SpanKind.CONSUMER):
        ...  # The job, then delete_message.
```

## A metric for scaling

A `metric` signal or limit reads Prometheus text from the spec's `metrics` endpoint, not
OTLP. ProtoStrap's `sample/services/py/main.py` serves `py_requests_total` at `/metrics`.
Prefer a port of its own, so the edge's routes and the scrape don't share a server.
