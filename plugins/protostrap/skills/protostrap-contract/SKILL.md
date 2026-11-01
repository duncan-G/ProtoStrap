---
name: protostrap-contract
description: Make service, worker and task code run correctly under ProtoStrap - read configuration and secrets (SECRET_ID, SECRET_PREFIX), find queues and buckets (QUEUE_<NAME>_URL, BUCKET_<NAME>, FILES_BUCKET), answer readiness checks, drain on SIGTERM, process SQS jobs safely, call other services, handle uploads, what the request path can't carry (WebSockets, long responses), work on a schedule, other AWS APIs, write migrations and configure tasks, and build the image. Use when writing or reviewing code in an application deployed with ProtoStrap, adding a new service or worker, or when a replica fails readiness, drops requests during deploys, loses jobs, or gets AccessDenied. Logs, traces and metrics are protostrap-telemetry.
---

# The application contract

ProtoStrap runs any container that keeps these rules. Breaking one rarely fails locally; it
fails in an account, as a rollout that rolls back, errors during every deploy, or lost jobs.

Language specifics, with code: [.NET](references/dotnet.md), [Node.js](references/node.md),
[Python](references/python.md). The rules are in `docs/protostrap.md#application-contract` at
the spec's `protostrap:` version, and the sample services in `sample/services/` keep them all.

## What every replica gets

| Variable                                    | Value                                                  |
| ------------------------------------------- | ------------------------------------------------------ |
| `PROTOSTRAP_APP`, `PROTOSTRAP_COMPONENT`, `PROTOSTRAP_REPLICA`, `PROTOSTRAP_RELEASE` | Its app, component, replica and release |
| `SECRET_ID`                                 | The environment's secret, `<app>/<environment>`        |
| `SECRET_PREFIX`                             | Its own keys' prefix, such as `Api_`                   |
| `AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`    | Where the AWS SDK gets its role's credentials          |
| `AWS_REGION`                                | The region                                             |
| `QUEUE_<NAME>_URL`                          | Each queue's URL (`<NAME>` upper case, `-` as `_`)     |
| `BUCKET_<NAME>`, `FILES_BUCKET`             | Each bucket's name                                     |
| `HOST_<NAME>`, `ORIGIN_<NAME>`              | Each of the spec's hosts' hostname in this account, and its origin (`https://…`) |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL` | The telemetry collector, `http://telemetry:4318`, `http/protobuf` |
| `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Its component; its replica, release and environment |

Plus the spec's `env`, which wins for any `OTEL_` variable it sets. Treat `PROTOSTRAP_REPLICA` being set as "running under ProtoStrap".
Fail at startup, naming the variable, when one the code needs is missing.

## 1. Read keys from the environment's secret, at startup

- Read `SECRET_ID` from Secrets Manager once, at startup, with the SDK's default credential
  chain (no keys anywhere). It is a JSON object of strings.
- Under `aspire run` with ProtoStrap.Hosting, `SECRET_ID` is dev's secret and `AWS_PROFILE` the
  developer's SSO profile, which the same default chain finds. Don't also set AWS keys in the
  environment: they win over the profile.
- Keep the keys that start with `Shared_`, then those that start with `SECRET_PREFIX`, drop
  the prefixes, and **let its own keys win**. Never use keys with other prefixes: they belong
  to other components.
- **List every key it reads in the spec's `secrets`**, `Shared_` ones above all: a change to
  an unlisted `Shared_` key doesn't replace the replicas, so they keep the old value.
- Keys are in environment-variable form, `ConnectionStrings__postgres`, so a local run sets
  the same names, without prefixes, as plain environment variables. Without `SECRET_ID`, read
  the environment instead.
- When a key changes, ProtoStrap replaces the replicas (`on_secret_change: restart`, the
  default). So read keys once, at startup; don't build in-process reloading. `reload` and
  hooks are for upstream images that take keys as files (protostrap-secrets).
- **Connection details come from keys,** never from `env` or code: `Host=postgres;Port=5432;…`
  in a key means moving PostgreSQL to RDS only changes the secret.
- Never log a value, or a connection string.

Naming keys and getting them set is the protostrap-secrets skill.

## 2. Readiness that answers only for its own name

A stopped replica's overlay address can be given to another service's replica. So a check
must fail if it reaches the wrong service:

- **HTTP (`ready: /healthz`, or `9000/healthz` on another port):** answer 200 only when the
  request's `Host`, without a port, is `PROTOSTRAP_COMPONENT`, and the replica isn't
  draining. Otherwise 503. Any 2xx or 3xx passes, for the agent and the edge alike. Outside
  ProtoStrap, skip the host test so local health checks pass. On the component's own port,
  the check speaks its `protocol`, so an `http2` service gets it over h2c.
- **gRPC (`ready: grpc`, or `grpc:9001`):** implement `grpc.health.v1.Health`, answering
  `SERVING` for the service names `""` and `PROTOSTRAP_COMPONENT`, `NOT_FOUND` for any
  other, and `NOT_SERVING` once draining. The port must speak cleartext HTTP/2 (h2c, prior
  knowledge). ASP.NET's `MapGrpcHealthChecksService()` maps only `""` by default: map the
  component's name too.
- **A command (`ready: [pg_isready, -U, postgres]`),** for an image you don't write: it runs
  in the replica and passes on exit 0, within 5 s. Not for a routed service, since the edge
  can't run it.
- Ready means able to serve: dependencies it can't work without are reachable. A replica not
  ready within 5 minutes fails, and the release rolls back.

## 3. Stop cleanly

On SIGTERM:

1. **Fail readiness at once**, but keep serving for `DRAIN_SECONDS` (default 10), so the edge
   notices before connections close.
2. Stop accepting, finish what's in progress, exit. All of it inside the spec's `stop`, so
   `stop` ≥ 10 s + the longest request. **`stop` defaults to 10 s**, which ends with the
   drain: set it in the spec. A second SIGTERM may exit at once.
3. **A worker** stops receiving, finishes the jobs it has, and sets the visibility of jobs it
   received but won't start to 0, so another replica takes them now. On a Spot interruption,
   `stop` is cut to 90 s.

**Keep-alive.** Envoy closes idle upstream connections after 60 s. A server whose idle
keep-alive timeout is shorter (Node's default is 5 s, uvicorn's 5 s) closes connections Envoy
is about to reuse, and requests fail at random. Set it to 65 s or more.

## 4. Jobs

- Receive from `QUEUE_<NAME>_URL` with long polling. Delete a message **only after** the work
  is done and committed.
- **Handle a job twice safely.** SQS delivers at least once, and a replica can stop mid-job.
  Record a job's id with its result in one transaction, and skip ids already done.
- `visibility` in the spec must exceed the longest job, or extend it while working.
  After `max_receives` failures a job goes to the dead-letter queue.
- A job message carries keys (a database id, an S3 key), never a file.
- A job that can't run now (a budget, a rate limit) goes back with a visibility delay.

## 5. Files and uploads

- Buckets come from `FILES_BUCKET` and `BUCKET_<NAME>`, and the component's spec entry must
  list them in `uses`, or the role refuses it.
- The tunnel takes request bodies up to 100 MB, and the edge gives a response 120 s in all,
  streams included, then cuts it. **Large uploads and downloads go straight to S3 with a
  presigned URL** (the bucket needs `browser: true`, or be `files`). Long work goes on a
  queue, and the client polls.
- Store a file's key in the database, not its URL; sign URLs when they're asked for.
- Locally, against LocalStack, S3 clients need path-style addressing and the endpoint in
  `AWS_ENDPOINT_URL_S3` / `AWS_ENDPOINT_URL_SQS`.

## 6. Calling other services

- Call by component name on the overlay, `http://api:8080`, straight, not through the edge.
- The caller balances: **gRPC** clients round-robin and re-resolve about every 30 s;
  **HTTP** clients cap a pooled connection's lifetime at about a minute. Docker's DNS TTL is
  minutes, so don't trust it.
- Retry a call whose connection was refused: that replica just stopped.
- External clients (browsers included) can't use native gRPC through the tunnel. Use
  gRPC-Web, which the edge translates for `protocol: grpc` services (unary and server
  streaming only, within the 120 s),
  or HTTP/JSON endpoints the service serves itself.

**What the request path can't carry**, so it works locally and fails in an account:

- **WebSockets.** Envoy's routes don't upgrade connections. For updates the client waits
  for, poll, or hold a request open for less than 120 s and have the client ask again.
- **A response longer than 120 s**, server-sent events and gRPC-Web streams included: the
  edge cuts it. A stream ends before then and the client reconnects; long work goes on a
  queue (§4).
- **A request body over 100 MB:** straight to S3 instead (§5).

## 7. Logs, traces and metrics

The protostrap-telemetry skill has the rules and each language's code. In short:

- Logs leave **by one path**: JSON lines on stdout, or OTLP with the console off under
  ProtoStrap. Both means every line twice. Skip a line per request under load.
- OpenTelemetry's SDK, configured from the `OTEL_` variables alone: no endpoint in code.
- Label metrics by route, never by user, ID or raw path.
- For a `metric` signal or limit, serve Prometheus text on the spec's `metrics` port and
  path, ideally a port of its own: the scheduler never sees OTLP metrics. A limit gauge is
  a replica cap, or −1 for none.

## 8. Migrations

- Run as a `task` (or the `migrate` command) once per release in which they changed, before
  any new replica of the components that name it in `migrate`. Exit 0 on success; non-zero
  fails and rolls back the release.
- **Compatible with the previous release**, which keeps running during the rollout and after
  a rollback: add columns and tables, backfill, and drop only in a later release. Make each
  step idempotent.

## 9. Configure tasks

A component that keeps its config in its own database, changed through its API (an
identity provider's realms, a broker's queues), gets it from a `task` named in its
`configure` (protostrap-spec). The task is the application's code or a declarative tool, and
`docs/config.md#writing-a-configure-task` is the contract:

- **Apply the whole config, every run, the same way twice.** It runs again after a failure,
  a rollback or a restart. A script of one-off changes doesn't qualify.
- **Exit 0 only when everything is applied.** Anything else fails the release.
- **Log in with a key from the environment's secret,** read from its secret file: a
  service account it keeps, never one made by hand.
- **Reach the component by name** (`http://idp:8080`), and leave admin APIs unrouted.
- **Log to stdout.**

## An authorizer

A service named in another's `authorize` (protostrap-spec) answers the edge for each request
to that service's routes:

- **Answer every method under its `path`,** fast: the edge waits its `timeout`, 2 seconds
  unless set, then gives the client a 503, or with `open: true` sends the request on as an
  anonymous one. Read the session from the forwarded headers (a cookie), never the body,
  which isn't sent. The path it's asked about is normalised, as the service gets it.
- **200 lets the request through,** with the headers named in `headers` from the answer
  added. Leave them out for an anonymous request the service may still serve; the edge
  has already removed the client's own copies, so the service can trust what arrives.
- **A 200 can carry headers back to the client,** those named in `respond`, such as a
  `set-cookie` when the authorizer renewed the session as it checked it. The edge adds
  each to the service's response as a header of its own, after the service's, so send one
  `set-cookie` for each cookie; of a cookie both set, the browser keeps the authorizer's.
  The client gets no other header of a 200.
- **Anything else is the client's response,** as it is: 401, a 302 to sign in, and a 204
  or any other 2xx too. A 5xx counts as no answer.
- **With `open: true`, a request without `headers` is anonymous,** not refused: the
  service gets one whenever the authorizer is down or answers a 5xx, so it serves or
  refuses it as it would any anonymous request. Requests `skip` names never reach the
  authorizer.
- **The authorized service reads the headers,** and trusts them only from the edge: a
  call from another service on the overlay carries whatever that service sends.

## 10. Work on a schedule

ProtoStrap has no cron and no scheduled tasks. **Tell the user before building around it**;
it may be something they want ProtoStrap to provide. If they choose to do it in the
application meanwhile:

- A component that always runs (`min` ≥ 1) checks the time, and **takes a lock before the
  work**, such as a row or advisory lock in PostgreSQL, since every replica runs the same
  code. Record each run, so a missed one (a deploy, a restart) is caught up, not skipped.
- Make the work a job on a queue (§4) rather than doing it in the timer, so it is retried,
  limited and scaled like any other.

## 11. Other AWS APIs

A component's role allows its secret, what its `uses` names (queues, buckets, `files`,
`backups`, `email`), and nothing else. Calling another AWS API (a model on Bedrock,
DynamoDB, KMS…) gets AccessDenied in an account even when it works locally with a
developer's credentials. The spec can't grant it, and every role is inside a boundary only
an Org Admin widens. **Say so to the user before writing the code,** and don't put AWS
access keys in the secret to get around it.

## 12. The image

- Built by the deploy workflow, `linux/arm64`: every VM is Graviton. A file named
  `Dockerfile`, no build arguments. Its `RUN` steps run under emulation, so a native
  dependency must have an arm64 build.
- **Pin base images by digest** (`FROM …@sha256:…`, or `ContainerBaseImage` for .NET), and
  the .NET SDK with `global.json`. Pin the multi-platform index's digest, which includes
  arm64, not one platform's. An unpinned base lets a later build of the same commit differ
  from the one that was tested.
- No secrets in the image, no bind mounts, no volumes for services and workers.
- Anything the build reads from outside its directory must be in the spec's `sources`.

## Reviewing code against the contract

Check, for each component: secret loading filters by prefix and `Shared_`, and every key it
reads is in `secrets`; `stop` is set and longer than the drain; the readiness
check tests the host or gRPC service name and fails while draining; SIGTERM drains for
`DRAIN_SECONDS`; the keep-alive timeout is over 60 s; workers delete after success and are
idempotent; every queue and bucket the code touches is in `uses`, and it calls no other AWS
API; nothing needs a WebSocket or a response over 120 s; nothing logs a secret. For its
telemetry, protostrap-telemetry's review list.
