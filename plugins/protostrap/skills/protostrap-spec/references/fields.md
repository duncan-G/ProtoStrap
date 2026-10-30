# Every field

The spec's fields as `internal/spec` reads them. A key not listed here is an error.

## Formats

| Kind of value    | Form                                   | Examples                    |
| ---------------- | -------------------------------------- | --------------------------- |
| CPU              | Millicores, or cores                   | `250m`, `1000m`, `2`        |
| Memory           | A binary unit: Ki, Mi, Gi, Ti          | `256Mi`, `2Gi`              |
| Volume `size`    | Whole GiB, in Gi or Ti                 | `20Gi`, `1Ti` (not `512Mi`) |
| Percent          | A percentage                           | `70%`                       |
| Duration         | Seconds, minutes, hours, days          | `30s`, `2m`, `4d`           |
| Bucket durations | Whole days                             | `7d`, `30d`                 |
| Rate             | Per second, minute or hour             | `2/s`, `30/m`, `100/h`      |

## Top level

```yaml
app: shop                      # up to 30 characters; names the secret, shop/<environment>, and the log groups
protostrap: 0.1.0              # the ProtoStrap version; must match the workflows' @v and the roots' ?ref=
hosts:                         # optional; each host's name, which routes use (shop/api/), and hostname
  shop: shop.example.com       # an account's Terraform root may give every name another hostname
pools: { ... }
edge: { request: { cpu: 500m, memory: 256Mi } }
telemetry:                     # optional; ProtoStrap's logs, traces and metrics on data-1 (docs/telemetry.md)
  request: { cpu: 500m, memory: 1536Mi }   # must fit data-1 beside its stateful components; at least 1Gi
  volume: 20Gi                 # Prometheus's metrics; at least 5Gi, only grows
  retention: { logs: 30d, traces: 30d, metrics: 15d }   # whole days
queues: { ... }
buckets: { ... }
components: { ... }
```

## Pools

```yaml
pools:
  ingress: { vm: c8g.medium }              # one VM per zone, the edge and NAT only. Also: reserved
  general:
    vm: [m7g.large, m6g.large, m8g.large, m7gd.large]   # one shape; sizing assumes the first
    min: 1
    max: 4                                  # also the most it can cost
    spare: 1                                # VMs kept free so a new replica places at once
    target: 70%                             # how full sizing aims to keep each VM
    reserved: { cpu: 400m, memory: 1Gi }    # left for the OS and agent on each VM
  compute:                                  # optional; Spot, with On-Demand as a fallback
    vm: [c7g.large, c6g.large, c8g.large, c7gd.large]
    min: 0
    max: 4
    spare: 0
    target: 85%
    reserved: { cpu: 400m, memory: 1Gi }
  data: { vm: m7g.large, count: 1 }         # data-1 .. data-3 at most, fixed. Also: reserved
```

VM types are Graviton (arm64): any of them, from the burstable `t4g` up; x86 types such as
`m7i` or `t3` are refused. Only Graviton3 and later (`c7g`, `m7g`, `r7g`, the `8g` families)
encrypt traffic between VMs in hardware. A request must fit one VM of its pool after
`reserved`.

## Queues

```yaml
queues:
  orders: {}                                     # all defaults
  outcomes: { fifo: true, visibility: 5m, retention: 4d, max_receives: 5 }
```

| Field          | Default | Meaning                                                    |
| -------------- | ------- | ---------------------------------------------------------- |
| `visibility`   | `2m`    | How long a received job stays hidden. Longer than a job    |
| `retention`    | `4d`    | How long a job waits before SQS drops it                   |
| `max_receives` | `5`     | Receives before a job goes to the dead-letter queue        |
| `fifo`         | false   | A FIFO queue                                               |

## Buckets

```yaml
buckets:
  documents: { browser: true, versions: 7d, lock: true }
  reports: { expire: 30d }
```

| Field      | Meaning                                                                          |
| ---------- | -------------------------------------------------------------------------------- |
| `browser`  | Browsers on the hosts and the account's `cors_origins` may use signed URLs       |
| `versions` | Versioning on; earlier versions kept this long. Components can't delete them     |
| `lock`     | Object lock, for GOVERNANCE retention. Needs `versions`. Can't be turned off     |
| `expire`   | Objects are deleted this long after they're written                              |

Every bucket is private, encrypted, TLS-only, and drops abandoned multipart uploads after 7 days.

## Email

```yaml
email:
  domains: [shop.example.com]
```

The domains the application sends mail from with SES, each in its Cloudflare zone, no
wildcards. A component with `email` in its `uses` may send from any address in them. No queue
or bucket may be called `email`. How to send is the protostrap-email skill.

## Components

| Field              | Kinds                     | Meaning                                                             |
| ------------------ | ------------------------- | ------------------------------------------------------------------- |
| `kind`             | all                       | `service`, `worker`, `stateful` or `task`                           |
| `build`            | service, worker, task     | Directory with a file named `Dockerfile`                            |
| `project`          | service, worker, task     | Instead of `build`: a `.csproj`, built with the SDK's container publish |
| `context`          | service, worker, task     | The build context, if not `build`                                   |
| `sources`          | service, worker, task     | More files, directories and git pathspecs the image is built from   |
| `image`            | stateful                  | Upstream image with a pinned tag, such as `postgres:18.3`           |
| `command`          | all                       | Replaces the image's command, as a list                             |
| `pool`             | service, worker, task     | `general` or `compute`                                              |
| `vm`               | stateful                  | The data VM, such as `data-1`                                       |
| `request`          | all                       | `{ cpu, memory }` one replica reserves                              |
| `port`             | service, stateful         | The port it listens on                                              |
| `ready`            | service                   | `grpc`, or an HTTP path such as `/healthz`                          |
| `routes`           | service                   | A host's name and a path prefix (`shop/api/`), or a path for every host (`/api/`), or `{ route, rate }` |
| `authorize`        | service                   | `{ by, path, headers, forward }`: a service the edge asks about each request to this one's routes first |
| `metrics`          | all                       | `<port>/<path>` of a Prometheus endpoint, such as `9090/metrics`    |
| `depends_on`       | all                       | Components with a ready replica before this one starts              |
| `uses`             | all                       | Queues, buckets, `files`, `backups`, `email`. Builds its IAM role   |
| `env`              | all                       | Plain variables. Never secrets                                      |
| `secrets`          | all                       | Key names it reads from the environment's secret, without prefix    |
| `secret_prefix`    | all                       | Its own keys' prefix, if not its name's, such as `Api_`             |
| `secret_files`     | all                       | Write each of `secrets` to `/run/secrets/<KEY>`, for images that can't read AWS |
| `on_secret_change` | service, worker, stateful | `restart` (default), `reload`, or a command list run in the replica |
| `config`           | all                       | `{ from, path }`: a repository directory whose tracked files are mounted read-only at `path`. No secrets in them; at most 1 MiB, 256 files |
| `on_config_change` | service, worker, stateful | `restart` (default), `reload`, or a command list run in the replica once new files are swapped in. Needs `config` |
| `configure`        | service, worker, stateful | A task's name: run after the component is ready, and again when the task, its keys or the component change |
| `migrate`          | service, worker           | A task's name, or a command list in this component's image          |
| `stop`             | all                       | SIGTERM to SIGKILL. Default 10 s, at most 1 h                       |
| `volume`           | stateful                  | `{ path, size }`, and optionally `iops`, `throughput`               |
| `backup`           | stateful                  | Command whose stdout is the daily backup                            |
| `scale`            | service, worker           | `{ min, max, demand, limits }`. Without it, exactly one replica     |

## Signals and limits

A component's demand is the highest of its signals. Each limit is a replica cap, and the
lowest one applies. Then:

```
desired replicas = clamp(min(demand, lowest limit), min, max)
```

| Signal                                 | Measured by                      | Demand                                   |
| -------------------------------------- | -------------------------------- | ---------------------------------------- |
| `{ cpu: 70% }`                         | Agents, from Docker              | ceil(ready replicas × average CPU ÷ 70%), CPU as a share of the request |
| `{ requests: 50/s }`                   | The edge's Envoy; needs `routes` | ceil(requests per second ÷ 50)           |
| `{ queue: orders, rate: 2/s, drain: 60s }` | The scheduler, from SQS      | ceil((waiting + in flight) ÷ (2 × 60))   |
| `{ metric: <name>, target: n }`        | Replicas' `metrics` endpoints    | ceil(ready replicas × average ÷ n)       |

| Limit              | Value                                                                         |
| ------------------ | ----------------------------------------------------------------------------- |
| `{ replicas: n }`  | A fixed cap, such as a third-party rate limit ÷ what one replica uses        |
| `{ metric: <name> }` | A gauge any replica of any component exports: a replica cap, or −1 for none. No value for 5 minutes means no cap |
