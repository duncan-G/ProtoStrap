import http from 'node:http';
import os from 'node:os';
import express from 'express';
import { ApiClient } from './api-client.js';

const identity = {
  app: process.env.PROTOSTRAP_APP || 'sample',
  component: process.env.PROTOSTRAP_COMPONENT || 'web',
  replica: process.env.PROTOSTRAP_REPLICA || os.hostname(),
  release: process.env.PROTOSTRAP_RELEASE || 'local',
};
const PORT = 3000;
const METRICS_PORT = 9464;
const DRAIN_SECONDS = Number(process.env.DRAIN_SECONDS || 10);
const MAX_BLOCK_MS = 5000;

function log(level, msg, fields = {}) {
  const { component, replica, release } = identity;
  process.stdout.write(`${JSON.stringify({ time: new Date().toISOString(), level, msg, component, replica, release, ...fields })}\n`);
}

function intParam(value, fallback, max) {
  const n = Number.parseInt(value, 10);
  return Number.isFinite(n) ? Math.min(Math.max(n, 0), max) : fallback;
}

const api = new ApiClient(process.env.API_URL || 'http://api:8080', log);
let draining = false;
let requests = 0;

const app = express();
app.disable('x-powered-by');

// While draining, close each connection after its response, so the gateway's connections
// close too.
app.use((req, res, next) => {
  if (draining) res.setHeader('Connection', 'close');
  next();
});

// The replica is ready only when asked for this service by name, and not draining:
// overlay addresses are reused by other services, so a check that reaches the wrong
// replica must fail.
app.get('/healthz', (req, res) => {
  const host = (req.headers.host ?? '').replace(/:\d+$/, '');
  if (draining || host !== identity.component) {
    res.status(503).json({ ready: false, draining });
    return;
  }
  res.json({ ready: true, ...identity });
});

app.use((req, res, next) => {
  requests++;
  next();
});

app.get('/web/hello', async (req, res) => {
  try {
    const reply = await api.get('/api/hello');
    res.json({ ...identity, time: new Date().toISOString(), api: reply });
  } catch (err) {
    log('warn', 'api call failed', { error: err.message });
    res.status(502).json({ ...identity, error: `api: ${err.message}` });
  }
});

// /web/burn spins the one JavaScript thread, which raises CPU per process, and event loop
// lag for everything else.
app.get('/web/burn', (req, res) => {
  const ms = intParam(req.query.ms, 50, MAX_BLOCK_MS);
  const start = performance.now();
  while (performance.now() - start < ms) {
    // The loop only keeps the thread busy.
  }
  res.json({ ...identity, elapsed_ms: Math.round(performance.now() - start) });
});

// /web/block blocks the event loop without using CPU, to drive the lag signal on its own.
app.get('/web/block', (req, res) => {
  const ms = intParam(req.query.ms, 50, MAX_BLOCK_MS);
  const start = performance.now();
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
  res.json({ ...identity, elapsed_ms: Math.round(performance.now() - start) });
});

const server = app.listen(PORT, (err) => {
  if (err) throw err;
  log('info', 'listening', { port: PORT });
});
// The keep-alive timeout is longer than the gateway keeps an idle connection, so the
// gateway never sends on one being closed.
server.keepAliveTimeout = 65_000;
server.headersTimeout = 66_000;

// Event loop lag is how late a 10 ms timer fires, averaged over 5 s windows.
const LAG_TICK_MS = 10;
let lagSum = 0;
let lagTicks = 0;
let lagSeconds = 0;
let expected = performance.now() + LAG_TICK_MS;
setInterval(() => {
  const now = performance.now();
  lagSum += Math.max(0, now - expected);
  lagTicks++;
  expected = now + LAG_TICK_MS;
}, LAG_TICK_MS).unref();
setInterval(() => {
  lagSeconds = lagTicks > 0 ? lagSum / lagTicks / 1000 : 0;
  lagSum = 0;
  lagTicks = 0;
}, 5000).unref();

const metrics = http.createServer((req, res) => {
  if (req.url.split('?')[0] !== '/metrics') {
    res.writeHead(404).end();
    return;
  }
  res.writeHead(200, { 'Content-Type': 'text/plain; version=0.0.4; charset=utf-8' });
  res.end(
    [
      '# HELP nodejs_eventloop_lag_seconds Mean lateness of a 10 ms timer over the last 5 s.',
      '# TYPE nodejs_eventloop_lag_seconds gauge',
      `nodejs_eventloop_lag_seconds ${lagSeconds.toFixed(6)}`,
      '# HELP web_requests_total Requests served, health checks left out.',
      '# TYPE web_requests_total counter',
      `web_requests_total ${requests}`,
      '',
    ].join('\n'),
  );
});
metrics.listen(METRICS_PORT);

// On SIGTERM, shutdown fails readiness at once and keeps serving while the gateway notices,
// then stops taking connections, lets requests in flight finish, and exits inside the
// spec's 20 s stop.
function shutdown(signal) {
  if (draining) return;
  draining = true;
  log('info', 'draining', { signal, seconds: DRAIN_SECONDS });
  setTimeout(() => {
    server.close(() => {
      log('info', 'stopped');
      process.exit(0);
    });
    server.closeIdleConnections();
    api.close();
    metrics.close();
    setTimeout(() => {
      log('warn', 'requests still open after 5 s, exiting');
      process.exit(0);
    }, 5000).unref();
  }, DRAIN_SECONDS * 1000);
}
process.on('SIGTERM', shutdown);
process.on('SIGINT', shutdown);
