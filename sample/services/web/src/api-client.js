import dns from 'node:dns/promises';
import http2 from 'node:http2';
import net from 'node:net';

const LOOKUP_EVERY_MS = 30_000;
const SESSION_MAX_AGE_MS = 60_000;
const TIMEOUT_MS = 5_000;
// These errors mean a request never reached a replica, as when the replica has just stopped.
const NOT_DELIVERED = new Set([
  'ECONNREFUSED',
  'ECONNRESET',
  'EHOSTUNREACH',
  'ERR_HTTP2_GOAWAY_SESSION',
  'ERR_HTTP2_INVALID_SESSION',
  'ERR_HTTP2_STREAM_CANCEL',
]);

/**
 * ApiClient calls the api straight across the overlay, over cleartext HTTP/2 (all the api
 * speaks), balancing requests over its replicas itself: one session per address, the name
 * looked up again every 30 s, and each session closed after about a minute, so new replicas
 * get traffic.
 */
export class ApiClient {
  #url;
  #log;
  #addresses = [];
  #resolvedAt = 0;
  #resolving = null;
  #next = 0;
  #sessions = new Map();

  constructor(url, log) {
    this.#url = new URL(url);
    this.#log = log;
  }

  /**
   * get GETs path and returns the parsed JSON body. It retries on another replica if the
   * request wasn't delivered.
   */
  async get(path) {
    const addresses = await this.#lookup();
    let lastError;
    for (let attempt = 0; attempt < Math.min(addresses.length, 3); attempt++) {
      const address = addresses[this.#next++ % addresses.length];
      try {
        return await this.#request(address, path);
      } catch (err) {
        lastError = err;
        if (err.status) break; // The api answered, so this isn't a connection problem.
        this.#drop(address);
        if (!NOT_DELIVERED.has(err.cause?.code ?? err.code)) break;
      }
    }
    throw lastError;
  }

  close() {
    for (const address of [...this.#sessions.keys()]) this.#drop(address);
  }

  #lookup() {
    if (this.#addresses.length > 0 && Date.now() - this.#resolvedAt < LOOKUP_EVERY_MS) {
      return Promise.resolve(this.#addresses);
    }
    this.#resolving ??= this.#resolve().finally(() => {
      this.#resolving = null;
    });
    return this.#resolving;
  }

  async #resolve() {
    try {
      const found = await dns.lookup(this.#url.hostname, { all: true });
      this.#addresses = found.map((entry) => entry.address);
      this.#resolvedAt = Date.now();
      for (const address of [...this.#sessions.keys()]) {
        if (!this.#addresses.includes(address)) this.#drop(address);
      }
    } catch (err) {
      if (this.#addresses.length === 0) throw err;
      this.#log('warn', 'api lookup failed, using the last addresses', { error: err.message });
    }
    return this.#addresses;
  }

  #session(address) {
    const existing = this.#sessions.get(address);
    if (existing && !existing.closed && !existing.destroyed && Date.now() - existing.openedAt < SESSION_MAX_AGE_MS) {
      return existing;
    }
    if (existing) this.#drop(address);

    const host = net.isIPv6(address) ? `[${address}]` : address;
    const session = http2.connect(`http://${host}:${this.#url.port || 80}`);
    session.openedAt = Date.now();
    const forget = () => {
      if (this.#sessions.get(address) === session) this.#sessions.delete(address);
    };
    session.on('error', (err) => {
      this.#log('warn', 'api session failed', { address, error: err.message });
      forget();
    });
    session.on('goaway', forget);
    session.on('close', forget);
    this.#sessions.set(address, session);
    return session;
  }

  /** #drop stops new requests on the address's session; requests in flight on it still finish. */
  #drop(address) {
    const session = this.#sessions.get(address);
    this.#sessions.delete(address);
    if (session && !session.closed && !session.destroyed) session.close();
  }

  #request(address, path) {
    const session = this.#session(address);
    return new Promise((resolve, reject) => {
      const stream = session.request({
        ':method': 'GET',
        ':path': path,
        ':authority': this.#url.host,
      });
      let status = 0;
      const chunks = [];
      stream.setTimeout(TIMEOUT_MS, () => {
        reject(Object.assign(new Error(`api didn't answer in ${TIMEOUT_MS} ms`), { code: 'ETIMEDOUT' }));
        stream.close(http2.constants.NGHTTP2_CANCEL);
      });
      stream.on('response', (headers) => {
        status = headers[':status'];
      });
      stream.on('data', (chunk) => chunks.push(chunk));
      stream.on('end', () => {
        if (!status) {
          // The stream ended with no response, as when the session never connected.
          reject(Object.assign(new Error('api stream ended without a response'), { code: 'ERR_HTTP2_STREAM_CANCEL' }));
          return;
        }
        const body = Buffer.concat(chunks).toString('utf8');
        if (status < 200 || status >= 300) {
          reject(Object.assign(new Error(`api answered ${status}`), { status }));
          return;
        }
        try {
          resolve(JSON.parse(body));
        } catch (err) {
          reject(err);
        }
      });
      stream.on('error', reject);
      stream.on('close', () => reject(Object.assign(new Error('api stream closed'), { code: 'ERR_HTTP2_STREAM_CANCEL' })));
      stream.end();
    });
  }
}
