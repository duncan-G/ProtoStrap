# Node.js under ProtoStrap

A working Express service is ProtoStrap's `sample/services/web/src/server.js`. Node runs one
JavaScript thread, so a component scales out on CPU and event loop lag, with a request of a
core or less.

## Keys

```js
import { SecretsManagerClient, GetSecretValueCommand } from '@aws-sdk/client-secrets-manager';

// loadSettings returns the component's keys: from the environment's secret when SECRET_ID is
// set, else from the environment, as in a local run.
export async function loadSettings() {
  const secretId = process.env.SECRET_ID;
  if (!secretId) return { ...process.env };
  const client = new SecretsManagerClient({}); // Default chain: the agent's credentials, AWS_REGION.
  const { SecretString } = await client.send(new GetSecretValueCommand({ SecretId: secretId }));
  const values = JSON.parse(SecretString);
  const prefix = process.env.SECRET_PREFIX;
  const keys = {};
  for (const [k, v] of Object.entries(values)) if (k.startsWith('Shared_')) keys[k.slice(7)] = v;
  if (prefix) for (const [k, v] of Object.entries(values)) if (k.startsWith(prefix)) keys[k.slice(prefix.length)] = v;
  return keys; // Its own keys win.
}
```

Load it before the server listens (top-level `await` in an ES module).

## Readiness, draining and keep-alive

```js
const component = process.env.PROTOSTRAP_COMPONENT;
const DRAIN_SECONDS = Number(process.env.DRAIN_SECONDS ?? 10);
let draining = false;

// Ready only when asked for this service by name, and not draining: overlay addresses are
// reused by other services, so a check that reaches the wrong replica must fail.
app.get('/healthz', (req, res) => {
  const host = (req.headers.host ?? '').replace(/:\d+$/, '');
  if (draining || (component && host !== component)) return res.status(503).json({ ready: false });
  res.json({ ready: true });
});

const server = app.listen(3000);
// Longer than Envoy keeps an idle connection (60 s). Node's default, 5 s, makes it reuse
// connections this server is closing.
server.keepAliveTimeout = 65_000;
server.headersTimeout = 66_000;

function shutdown() {
  if (draining) process.exit(0); // A second signal stops now.
  draining = true;
  setTimeout(() => {
    server.close(() => process.exit(0));
    server.closeIdleConnections();
    setTimeout(() => process.exit(0), 5000).unref(); // Inside the spec's stop.
  }, DRAIN_SECONDS * 1000);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);
```

The spec's `stop` must exceed `DRAIN_SECONDS` plus the 5 s grace: 20 s in the sample.

## Event loop lag, for scaling

```js
let lagSum = 0, lagTicks = 0, lagSeconds = 0, expected = Date.now() + 10;
setInterval(() => { const now = Date.now(); lagSum += Math.max(0, now - expected); lagTicks++; expected = now + 10; }, 10).unref();
setInterval(() => { lagSeconds = lagTicks ? lagSum / lagTicks / 1000 : 0; lagSum = 0; lagTicks = 0; }, 5000).unref();
```

Serve `nodejs_eventloop_lag_seconds ${lagSeconds}` as a Prometheus gauge on a port of its
own, named in the spec's `metrics`, with the signal
`{ metric: nodejs_eventloop_lag_seconds, target: 0.05 }`.

## Other services, queues and buckets

- Call others by name: `process.env.API_URL ?? 'http://api:8080'`, set in the spec's `env`.
  Use an HTTP agent whose sockets are recycled about every minute, and retry `ECONNREFUSED`.
- `new SQSClient({})`, `new S3Client({ forcePathStyle: !!process.env.AWS_ENDPOINT_URL_S3 })`:
  SDK v3 reads `AWS_ENDPOINT_URL_SQS` and `AWS_ENDPOINT_URL_S3` itself.
- Queue URLs from `process.env.QUEUE_<NAME>_URL`, buckets from `process.env.BUCKET_<NAME>`
  and `FILES_BUCKET`.

## Logs

One JSON object per line on stdout, with `PROTOSTRAP_COMPONENT`, `PROTOSTRAP_REPLICA` and
`PROTOSTRAP_RELEASE`. No line per request under load.

Logs stay on stdout; don't also export them over OTLP. Traces, metrics and trace IDs in
each line: protostrap-telemetry's `references/node.md`.
