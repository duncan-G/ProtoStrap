# Node.js telemetry under ProtoStrap

Packages: `@opentelemetry/api`, `@opentelemetry/sdk-node`,
`@opentelemetry/auto-instrumentations-node`, `@opentelemetry/instrumentation`,
`@opentelemetry/exporter-trace-otlp-proto`, `@opentelemetry/exporter-metrics-otlp-proto`,
`@opentelemetry/sdk-metrics`. Pin them in the lock file, which is in the component's
`sources`. ProtoStrap's sample Node service doesn't use OpenTelemetry, so this hasn't run
there: check option names against the installed versions' READMEs.

## Starting the SDK

Load it before the application, so the instrumentations patch modules before they're
imported. For an ES module service (`"type": "module"`), in the Dockerfile:

```dockerfile
CMD ["node", "--import", "./src/telemetry.js", "src/server.js"]
```

```js
// src/telemetry.js
import { register } from 'node:module';
import { NodeSDK } from '@opentelemetry/sdk-node';
import { getNodeAutoInstrumentations } from '@opentelemetry/auto-instrumentations-node';
import { OTLPTraceExporter } from '@opentelemetry/exporter-trace-otlp-proto';
import { OTLPMetricExporter } from '@opentelemetry/exporter-metrics-otlp-proto';
import { PeriodicExportingMetricReader } from '@opentelemetry/sdk-metrics';

// ES modules, Express among them, are instrumented only through this hook.
register('@opentelemetry/instrumentation/hook.mjs', import.meta.url);

// ProtoStrap with telemetry and `aspire run` set the endpoint. Without it, export nothing.
export const sdk = process.env.OTEL_EXPORTER_OTLP_ENDPOINT
  ? new NodeSDK({
      // The exporters read the endpoint and add /v1/traces and /v1/metrics; the SDK reads
      // OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES.
      traceExporter: new OTLPTraceExporter(),
      metricReader: new PeriodicExportingMetricReader({ exporter: new OTLPMetricExporter() }),
      instrumentations: [getNodeAutoInstrumentations({
        '@opentelemetry/instrumentation-fs': { enabled: false },   // a span per file read
        '@opentelemetry/instrumentation-http': {
          ignoreIncomingRequestHook: (req) => req.url === '/healthz' || req.url === '/metrics',
        },
        // Logs stay on stdout as JSON lines: don't send them over OTLP too.
        '@opentelemetry/instrumentation-pino': { disableLogSending: true },
        '@opentelemetry/instrumentation-winston': { disableLogSending: true },
        '@opentelemetry/instrumentation-bunyan': { disableLogSending: true },
      })],
    })
  : undefined;
sdk?.start();
```

Flush on the way out: in the SIGTERM handler, after the server has closed (protostrap-contract's
`references/node.md`), `await sdk?.shutdown()` before exiting.

## Logs

Keep one JSON object per line on stdout. With pino, its instrumentation adds `trace_id` and
`span_id` to each line inside a span. Without a logging library, add them yourself:

```js
import { trace } from '@opentelemetry/api';
const span = trace.getActiveSpan()?.spanContext();
console.log(JSON.stringify({ level: 'info', msg, trace_id: span?.traceId, span_id: span?.spanId }));
```

## Custom spans and metrics

```js
import { trace, metrics } from '@opentelemetry/api';

const tracer = trace.getTracer('shop-web');
const ordersPlaced = metrics.getMeter('shop-web').createCounter('shop.orders.placed', { description: 'Orders placed' });

await tracer.startActiveSpan('place order', async (span) => {
  try {
    span.setAttribute('order.id', order.id);   // IDs on spans, never on metrics
    ordersPlaced.add(1, { channel: 'web' });
  } finally {
    span.end();
  }
});
```

Node needs no registration: the SDK exports every tracer and meter.

## A trace through a queue

`getNodeAutoInstrumentations` includes the AWS SDK's instrumentation, which injects the
context into a sent message's attributes. Without it, inject by hand:

```js
import { context, propagation } from '@opentelemetry/api';

const carrier = {};
propagation.inject(context.active(), carrier);
await sqs.send(new SendMessageCommand({
  QueueUrl, MessageBody: body,
  MessageAttributes: Object.fromEntries(Object.entries(carrier).map(([k, v]) => [k, { DataType: 'String', StringValue: v }])),
}));
```

The worker extracts it either way:

```js
import { context, propagation, trace, SpanKind } from '@opentelemetry/api';

const tracer = trace.getTracer('orders-worker');
const { Messages = [] } = await sqs.send(new ReceiveMessageCommand({
  QueueUrl, WaitTimeSeconds: 20, MaxNumberOfMessages: 10,
  MessageAttributeNames: ['All'],   // Without this, SQS returns no attributes.
}));
for (const message of Messages) {
  const carrier = Object.fromEntries(Object.entries(message.MessageAttributes ?? {}).map(([k, v]) => [k, v.StringValue]));
  await tracer.startActiveSpan('process order', { kind: SpanKind.CONSUMER }, propagation.extract(context.active(), carrier), async (span) => {
    try {
      // The job, then DeleteMessageCommand.
    } finally {
      span.end();
    }
  });
}
```

## A metric for scaling

A `metric` signal or limit reads Prometheus text from the spec's `metrics` endpoint, not
OTLP. ProtoStrap's `sample/services/web/src/server.js` serves `nodejs_eventloop_lag_seconds`
on a server of its own, port 9464, for the event-loop-lag signal protostrap-spec's
`references/scaling.md` recommends.
