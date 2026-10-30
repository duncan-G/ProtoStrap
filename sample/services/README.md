# Sample services

The components of the sample application in [`../protostrap.yaml`](../protostrap.yaml). Each
one keeps to the [application contract](../../docs/protostrap.md#application-contract) and
has a knob that makes load, so that deploys, replica scaling, VM scaling and scaling from
zero can be tested end to end. [`../load`](../load) is a load generator to turn the knobs.

| Component | Code                         | Kind    | Port | Ready      | Metrics        | Scales on                         |
| --------- | ---------------------------- | ------- | ---- | ---------- | -------------- | --------------------------------- |
| api       | [dotnet/Api](dotnet/Api)     | service | 8080 | `grpc`     | `9090/metrics` | CPU                               |
| web       | [web](web)                   | service | 3000 | `/healthz` | `9464/metrics` | CPU, event loop lag               |
| py        | [py](py)                     | service | 8000 | `/healthz` | `8000/metrics` | requests per second, CPU          |
| jobs      | [dotnet/Worker](dotnet/Worker) | worker | —   | —          | —              | queue depth, capped by the budget |
| migrate   | [dotnet/Migrate](dotnet/Migrate) | task | —   | —          | —              | —                                 |

What they all do:

- **Readiness is specific to the service.** `/healthz` answers 200 only when the request's
  `Host` (without a port) is the component's own name and the replica isn't draining, and
  503 otherwise. The api's gRPC health check reports `SERVING` for the service `api` and for
  `""`, and `NOT_FOUND` for any other name.
- **Stopping.** On SIGTERM readiness fails at once, requests are still served for
  `DRAIN_SECONDS` (default 10) while the gateway notices, then the server stops taking
  connections, finishes what it has and exits, inside the spec's `stop`.
- **Secrets.** With `SECRET_ID` set, the api, jobs, migrate and py read that secret from
  Secrets Manager at startup, a JSON object such as
  `{"ConnectionStrings__postgres": "Host=postgres;Port=5432;Username=postgres;Database=postgres", "ConnectionStrings__redis": "redis:6379"}`.
  Without it (a local run), the same keys come from environment variables. web has no secret.
- **Identity.** Responses and log lines carry `PROTOSTRAP_COMPONENT`, `PROTOSTRAP_REPLICA` and
  `PROTOSTRAP_RELEASE`. Logs are one JSON object per line on stdout.
- **Burn knobs** take `ms`. On the api and py, `ms` is CPU time: each request costs the
  same CPU however busy the replica is, and the `elapsed_ms` they return (wall-clock) grows
  when the replica is short of CPU. On web, the loop holds the one JavaScript thread for
  `ms` of wall-clock time.

## api

.NET 10, gRPC and minimal-API HTTP on one port. Port 8080 speaks only cleartext HTTP/2
(h2c, prior knowledge), which is what Envoy and the readiness probe use for every request;
call it directly with `curl --http2-prior-knowledge` or `load ... -h2c`. Port 9090 serves
`/metrics` over HTTP/1.1.

| Endpoint                                   | Does                                                                                    |
| ------------------------------------------ | --------------------------------------------------------------------------------------- |
| `GET /api/hello`                           | Who answered: app, component, replica, release, time                                    |
| `GET /api/burn?ms=100`                     | Uses `ms` of CPU (at most 5000) and returns `elapsed_ms`                                |
| `POST /api/jobs?count=100&work_ms=1000`    | Puts `count` jobs (at most 100000) on the jobs queue, 10 per `SendMessageBatch`; returns `sent` |
| `GET /api/jobs/stats`                      | Rows in `jobs_done`, this month's job count in Redis, and the budget                    |
| `sample.Api/Hello`, `/Burn`, `/Enqueue`    | The same over gRPC and gRPC-Web ([proto](dotnet/Api/Protos/sample.proto))               |
| `grpc.health.v1.Health/Check`              | Readiness, for `api` and `""`                                                           |
| `:9090/metrics`                            | `jobs_budget_cap`, `api_requests_total`, `dotnet_threadpool_queue_length`, `dotnet_threadpool_threads` |

`jobs_budget_cap` is the jobs worker's replica cap: `0` once the Redis counter
`jobs:spent:<yyyy-MM>` (UTC) reaches `JOBS_BUDGET`, otherwise `-1` (no cap), and also `-1`
when `JOBS_BUDGET` is unset or Redis can't be reached. The api exports it because it always
runs, so the cap is known while the worker is at zero.

Environment: `JOBS_BUDGET`, `DRAIN_SECONDS`, and from ProtoStrap `QUEUE_JOBS_URL` (without
it, `POST /api/jobs` answers 503).

## jobs

.NET 10 worker. Long-polls `QUEUE_JOBS_URL` (20 s) and runs `JOBS_CONCURRENCY` jobs at once
(default 2). A job is `{"id": "...", "work_ms": 1000}`; it keeps a CPU busy for `work_ms` of
wall-clock time (at most 10000), so one replica does about two 1-second jobs a second, the
`rate: 2/s` of the spec's queue signal. Then it inserts the id into `jobs_done` (`ON CONFLICT
DO NOTHING`) and deletes the message. A job already in `jobs_done` is deleted without being
run again.

Before each job it adds 1 to `jobs:spent:<yyyy-MM>`. If `JOBS_BUDGET` is set on the worker
too and that would pass it, the job goes back to the queue for 5 minutes, so the budget holds
while the api's cap scales the workers down.

On SIGTERM it stops receiving, lets the jobs in flight finish, and sets the visibility of
any message it received but hadn't started to 0, so another replica gets it at once. Without
`QUEUE_JOBS_URL` (a local run) it logs a warning and idles.

## migrate

.NET 10 task, run once in each release that changes it, before any new api or jobs replica.
It waits up to 2 minutes for PostgreSQL, takes an advisory lock, applies the steps in
[`Migrations.cs`](dotnet/Migrate/Migrations.cs) that `schema_migrations` doesn't list yet,
each in a transaction, and exits 0 (or 1 if it failed). Step 1 creates `jobs_done`, step 2
creates `hits`.

## web

Node.js 24 and Express 5. Single-threaded, so it scales out: on CPU per process, and on
event loop lag.

| Endpoint                | Does                                                                                         |
| ----------------------- | -------------------------------------------------------------------------------------------- |
| `GET /healthz`          | Readiness: 200 only for `Host: web` while not draining                                       |
| `GET /web/hello`        | Its own identity, plus the api's `/api/hello`                                                |
| `GET /web/burn?ms=50`   | Spins the event loop for `ms` (at most 5000): CPU, and lag for every other request           |
| `GET /web/block?ms=50`  | Blocks the event loop for `ms` without using CPU, to drive the lag signal on its own         |
| `:9464/metrics`         | `nodejs_eventloop_lag_seconds` (mean lateness of a 10 ms timer over the last 5 s), `web_requests_total` |

It calls the api at `API_URL` straight across the overlay, not through the gateway, over
h2c (all the api speaks). It balances over the api's replicas itself: a session per
address, the name looked up again every 30 s, each session closed after about a minute so
new replicas get traffic, and a request that was refused retried on another replica.

## py

Python 3.13, FastAPI on uvicorn, one process per replica.

| Endpoint              | Does                                                                                         |
| --------------------- | -------------------------------------------------------------------------------------------- |
| `GET /healthz`        | Readiness: 200 only for `Host: py` while not draining                                        |
| `GET /py/hello`       | Its identity and the time                                                                    |
| `GET /py/burn?ms=50`  | Uses `ms` of CPU (at most 5000) on a worker thread. The GIL runs one at a time, so a replica does at most 1000/`ms` a second |
| `GET /py/count`       | `INCR py:count` in Redis, and the new value                                                  |
| `GET /metrics`        | `python_eventloop_lag_seconds` (mean lateness of a 50 ms sleep over the last 5 s), `py_requests_total` |

## Load

`go run ./sample/load` from the repository root (Go standard library only):

```sh
# A steady rate, open loop: requests go out at the rate whatever comes back.
go run ./sample/load http -url https://HOST/py/hello -rps 50 -duration 2m [-concurrency 64] [-ramp 30s]
# A staircase of rates, to watch scaling out and back in.
go run ./sample/load steps -url https://HOST/py/hello -steps 10,50,100,20 -step 3m [-ramp 30s]
# Jobs, through the api, in POSTs of at most 1000.
go run ./sample/load jobs -url https://HOST/api/jobs -count 1000 -work-ms 1000
```

Every 10 seconds it prints the rate, requests sent, answered ok, in flight, dropped (beyond
`-concurrency`), errors by HTTP status or kind, and p50/p90/p99 latency; totals at the end
or on Ctrl+C. Add `-h2c` to call the api directly over cleartext HTTP/2. Cloudflare may
challenge or rate-limit a lot of traffic from one address: allow it, or run the load from
inside the account.

## Exercising each scaling path

The numbers assume the spec as it is: the general pool is 2-vCPU VMs with 400m reserved.

- **CPU on the api.** Each `/api/burn?ms=100` costs 0.1 CPU-second, and a replica requests
  500m with a target of 70%, so 10 requests a second need about 3 replicas and 20 need
  about 6. Three api replicas fill a general VM, so this also adds VMs.
  `load steps -url https://HOST/api/burn?ms=100 -steps 5,10,20,5 -step 5m`.
  gRPC works too: `grpcurl -import-path sample/services/dotnet/Api/Protos -proto sample.proto -d '{"ms":100}' HOST:443 sample.Api/Burn`.
  `dotnet_threadpool_queue_length` shows requests waiting for threads once CPU runs short.
- **CPU on web.** A replica requests 250m, so 70% is 175m: `/web/burn?ms=50` at 5, 10 and
  20 a second needs about 2, 3 and 6 replicas. One replica can't use more than one CPU, so
  it saturates at 20 a second, and latency climbs if scaling lags.
  `load steps -url https://HOST/web/burn?ms=50 -steps 5,10,20,5 -step 5m`.
- **Event loop lag on web.** `/web/block?ms=50` holds the event loop without CPU, so only
  the lag signal sees it. The lag stays low until a replica is nearly busy (20 a second of
  these) and then climbs fast, so expect a step up rather than a smooth curve.
  `load http -url https://HOST/web/block?ms=50 -rps 40 -duration 10m`.
- **Requests per second on py.** Demand is requests per second over 20, counted at the
  edge: `load steps -url https://HOST/py/hello -steps 20,60,120,40 -step 5m` goes 1, 3, 6
  and back to 2. Above the route's `rate: 200/s` the edge rejects the excess (429).
  `/py/burn?ms=50` caps a replica at 20 a second, so the same rates there also show latency
  when scaling lags.
- **A worker from zero.** `load jobs -url https://HOST/api/jobs -count 1200 -work-ms 1000`:
  1200 jobs at 2 a second per replica, drained in 60 s, need 10 workers, started from zero.
  `GET /api/jobs/stats` counts them done. Once the queue is empty the workers go back to 0.
  Running a job twice (a replica lost mid-job) still leaves one row in `jobs_done`.
- **The budget cap.** Set `JOBS_BUDGET` low on the api (and on jobs, to enforce it per job),
  say `"1500"`, deploy, and enqueue 2000 jobs. Once 1500 have started, `jobs_budget_cap`
  turns 0 and the workers scale to zero while the rest wait in the queue. To start again,
  raise the budget or delete this month's counter: `redis-cli DEL "jobs:spent:$(date -u +%Y-%m)"`.
- **Migrations.** Add a step to `Steps` in [`Migrations.cs`](dotnet/Migrate/Migrations.cs),
  such as `(3, "ALTER TABLE hits ADD COLUMN IF NOT EXISTS path text")`, and deploy: the
  release runs migrate before any new api or jobs replica. A step that fails (bad SQL) fails
  the task, and so the release. Keep steps additive, since the previous release may run
  against the new schema after a rollback.
- **Deploys without errors.** Keep a `load http` running against each service while you
  deploy a release that changes it: replicas fail readiness, drain and stop, so errors should
  stay at 0.

## Running locally

[`../compose.yaml`](../compose.yaml) runs everything but the queue, without AWS:
`docker compose -f sample/compose.yaml up --build` (this downloads base images and packages).
The jobs worker idles without a queue. Then, for example:

```sh
curl --http2-prior-knowledge localhost:8080/api/hello
curl localhost:3000/web/hello
curl -H 'Host: py' localhost:8000/healthz
go run ./sample/load http -url 'http://localhost:8080/api/burn?ms=50' -rps 20 -duration 1m -h2c
```
