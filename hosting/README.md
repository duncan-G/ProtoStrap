# ProtoStrap.Hosting

ProtoStrap.Hosting lets a .NET application describe its ProtoStrap components in its
[Aspire](https://aspire.dev) AppHost. `aspire run` runs the application as usual, and
`aspire do protostrap-spec` writes its `protostrap.yaml`, so the spec is derived from the
AppHost and the MSBuild project graph instead of written by hand. The design is in
[ProtoStrap with Aspire](../docs/protostrap-aspire.md).

| Path                                                                   | What it is                                                |
| ---------------------------------------------------------------------- | --------------------------------------------------------- |
| [`src/ProtoStrap.Hosting`](src/ProtoStrap.Hosting)                     | The package, for Aspire 13.6 and .NET 10                  |
| [`tests/ProtoStrap.Hosting.Tests`](tests/ProtoStrap.Hosting.Tests)     | Its tests, scenario by scenario                           |
| [`tests/.../Golden/protofast.yaml`](tests/ProtoStrap.Hosting.Tests/Golden/protofast.yaml) | The spec the AppHost in the design generates, which `internal/spec`'s Go tests also check |


## An AppHost

Reference the package from the AppHost project (until it is published, as a project:
`<ProjectReference Include=".../ProtoStrap.Hosting.csproj" IsAspireProjectResource="false" />`),
add the ProtoStrap environment, and say what kind of component each compute resource is:

```csharp
var builder = DistributedApplication.CreateBuilder(args);

builder.AddProtoStrapEnvironment("protostrap", app: "shop")
    .WithHost("shop", "shop.example.com", local: "shop.localhost")
    .WithIngressPool(vm: "c8g.medium")
    .WithGeneralPool(vm: ["m7g.large", "m6g.large"], min: 1, max: 6)
    .WithComputePool(vm: ["c7g.xlarge", "c6g.xlarge"], min: 0, max: 4)
    .WithDataPool(vm: "m7g.xlarge");

var postgres = builder.AddPostgres("postgres")
    .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "2000m", memory: "8Gi").Volume(size: "100Gi"));
var shopDb = postgres.AddDatabase("shop-db", databaseName: "shop");

var files = builder.AddProtoStrapFiles("files");
var receipts = builder.AddProtoStrapBucket("receipts", versions: TimeSpan.FromDays(30));
var orders = builder.AddProtoStrapQueue("orders");

var migrations = builder.AddProject<Projects.Shop_Migrations>("shop-migrations")
    .WithReference(shopDb, connectionName: "shop").WaitFor(shopDb)
    .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));

builder.AddProject<Projects.Shop_Api>("api")
    .WithReference(shopDb, connectionName: "shop").WaitFor(shopDb)
    .WithReference(files)
    .WithReference(receipts)
    .WithReference(orders)
    .WaitForCompletion(migrations)
    .PublishAsProtoStrapService(s => s
        .Request(cpu: "500m", memory: "512Mi")
        .Routes("shop/api/")
        .Ready(Readiness.Grpc)
        .Scale(min: 2, max: 10, demand => demand.Cpu(0.70)));

builder.AddProject<Projects.Shop_OrdersWorker>("orders-worker")
    .WithReference(shopDb, connectionName: "shop").WaitFor(shopDb)
    .PublishAsProtoStrapWorker(w => w
        .Pool(Pool.Compute)
        .Request(cpu: "1000m", memory: "1Gi")
        .Scale(min: 0, max: 20, demand => demand.Queue(orders, rate: 2, drain: TimeSpan.FromSeconds(60))));

builder.Build().Run();
```

Every `PublishAsProtoStrap*` callback runs only in publish mode, and the environment isn't
added to the model in run mode, so `aspire run` is unchanged.

| Call                                          | Becomes                                                                 |
| --------------------------------------------- | ----------------------------------------------------------------------- |
| `AddProtoStrapEnvironment(name, app)`         | `app`, and with `WithVersion`, `WithHost`, `With*Pool` and `WithEdge`, the rest of the spec's top |
| `.WithHost(name, hostname, local)`            | An entry in `hosts`, which routes name as `name/`; under `aspire run`, every resource's `HOST_<NAME>` and `ORIGIN_<NAME>` for the local hostname |
| `.WithDevSecret(profile)`                     | Nothing in the spec; under `aspire run`, every parameter a component takes gets its value from dev's secret, `<app>/dev`, by the key it reads in an account, after an AWS SSO sign-in ([docs/secrets.md](../docs/secrets.md#dev)) |
| `PublishAsProtoStrapService(...)`             | `kind: service`: `Pool`, `Request`, `Routes`/`Route`, `Ready`, `Metrics`, `Stop`, `Scale`, `SecretFiles` |
| `PublishAsProtoStrapWorker(...)`              | `kind: worker`: `Pool`, `Request`, `Metrics`, `Stop`, `Scale`, `SecretFiles` |
| `PublishAsProtoStrapStateful(...)`            | `kind: stateful`: `Vm`, `Request`, `Volume`, `Backup`, `Command`, `Metrics`, `Stop`, `SecretFiles` |
| `PublishAsProtoStrapTask(...)`                | `kind: task`: `Pool`, `Request`, `Stop`, `SecretFiles`                  |
| `.SecretFiles(onChange)`                      | `secret_files: true`, and `on_secret_change` from `SecretChange.Reload` or `SecretChange.Run(...)`: the keys as files in `/run/secrets`, for an image that can't read the secret |
| `AddProtoStrapQueue(name, fifo: ...)`         | An entry under `queues`                                                 |
| `AddProtoStrapBucket(name, browser: ..., versions: ..., locked: ..., expire: ...)` | An entry under `buckets` |
| `AddProtoStrapFiles(name)`                    | The files bucket                                                        |
| `AddProtoStrapEmail(name, domains)`           | The `email` section: the domains the application sends mail from with SES ([Email](../docs/email.md)) |
| `.WithReference(queue)`, `.WithReference(bucket)`, `.WithReference(files)`, `.WithReference(email)` | `uses`, without the queue the component's own `Queue(...)` signal reads |
| `.Authorize(auth, path, headers, forward, respond)` | `authorize`: the service the edge asks about each request to this one's routes first ([Authorizing requests](../docs/protostrap.md#authorizing-requests)) |
| `.WithProtoStrapSources(pathspecs)`           | More `sources`, for inputs the build can't show, such as generated protobuf code |
| `.WithProtoStrapConfig(source, target)`       | `config: { from, path }`: a directory of the repository mounted read-only in each replica, and a read-only bind mount of it under `aspire run` ([Config files](../docs/config.md)) |
| `.OnConfigChange(ConfigChange.Reload)`, `ConfigChange.Run(...)` | `on_config_change`: how a service, worker or stateful component takes new config files |
| `.WithProtoStrapConfigure(task)`              | `configure`: the task that configures the component through its API once it is ready, which waits for it; under `aspire run`, whatever waits for the component waits for the task too |
| `.SecretPrefix("Idp_")`                       | `secret_prefix`: the prefix of its own keys, such as a configure task reading the keys of the component it configures |
| `WaitFor(x)`, `WaitForCompletion(task)`       | `depends_on`, and `migrate`                                              |

The rest comes from the app model: a project's `project` and its `sources`, traced with
MSBuild; a Dockerfile's `build`, `context` and `sources`, without what `.dockerignore`
leaves out; a container's `image`; the `port` from its endpoint, and `ready` from
`WithHttpHealthCheck` or a readiness probe, on its endpoint's port; and its `env` and `secrets`. The full mapping is
in [From the app model to the spec](../docs/protostrap-aspire.md#from-the-app-model-to-the-spec).


## Writing the spec

```bash
aspire do protostrap-spec
```

writes `protostrap.yaml` at the root of the git repository the AppHost is in, or where
`WithSpecPath(...)` says; paths in it are relative to its directory. `aspire publish` runs the
same step. Without the Aspire CLI, run the AppHost in publish mode:

```bash
dotnet run --project apphost -- --operation publish --step protostrap-spec
```

The spec is written only if it changed. Then, if `protostrap` is on the `PATH`, the step runs
`protostrap check` on it (`WithCheck(...)` names another binary, or `null` none), warning first
if that binary isn't the AppHost's version: not built from the checkout's commit and its latest
changes, or, for a package, not ProtoStrap.Hosting's version. If the
AppHost describes something ProtoStrap can't run, nothing is written, every problem is listed
with the resource at fault, and the AppHost exits with 1:

```
protostrap.yaml wasn't written: 2 problems
  keycloak: bind mount at /opt/keycloak/themes: a replica can't mount the repository, so copy the files into the image, or make them config files with WithProtoStrapConfig
  smtp4dev: no PublishAsProtoStrap* call: say whether it is a service, worker, stateful component or task
```

Resources that only make sense locally are left out with `ExcludeFromManifest()`, or not
added in publish mode. What else the generator rejects is in
[What the generator rejects](../docs/protostrap-aspire.md#what-the-generator-rejects).

In CI, the reusable `plan` workflow regenerates the spec when its `apphost` input names the
AppHost project, and fails if the committed one isn't what the AppHost writes.

### Secrets and connection details

A value is written into `env` only if nothing in it is secret. Anything holding a parameter,
secret or not, a connection string, or the endpoint of a resource with a connection string
goes into `secrets` by name, and its value lives only in the environment's secret, under the
component's prefix or `Shared_`, where an Org Admin sets it ([Secrets](../docs/secrets.md)). Aspire 13's `WithReference` adds each connection property beside the
connection string, such as `SHOP_HOST` and `SHOP_DATABASENAME`, and each of those goes to the
secret too. A component that reads only its connection strings can say so:

```csharp
.WithReferenceEnvironment(ReferenceEnvironmentInjectionFlags.ConnectionString | ReferenceEnvironmentInjectionFlags.ServiceDiscovery)
```

PostgreSQL's and Redis's integrations pass a generated password in a secret parameter, which
their upstream images can't read from a secret. So each gets its password as a
[secret file](../docs/secrets.md#secret-files), `POSTGRES_PASSWORD` or `REDIS_PASSWORD`,
and a hook that applies a new one without a restart. Set both in the environment's secret
before the first deploy, as `Postgres_POSTGRES_PASSWORD` and `Redis_REDIS_PASSWORD`.

### Floating inputs

The step warns about inputs that move a tag without a commit saying why: projects whose
`ContainerBaseImage` isn't pinned by digest. The SDK can't float: a project without a
`global.json` at or above it that sets `sdk.version` fails the step, since ProtoStrap never
picks a .NET SDK.


## Run mode

With hosts, `aspire run` runs the edge's Envoy as `protostrap-edge`, serving each host's
local hostname on port 20000 (`WithLocalEdge(port: ...)`), routed and authorized by the same
`PublishAsProtoStrapService` settings and rendered by `protostrap edge render`. The
`protostrap-build` resource, under the edge, builds that `protostrap` with Go from this checkout
as the AppHost starts and whenever the edge starts (a no-op when nothing changed), turns
`Behind` when the checkout's Go changes after a build, and has a Rebuild command that rebuilds
and restarts the edge ([The local edge](../docs/protostrap-aspire.md#the-local-edge)). Without
the checkout or Go, the edge uses `protostrap` on `PATH`, with the same warning when it isn't the
AppHost's version. On the dashboard the edge
links each host, references the services it reaches, and logs each request it routes. Every
resource gets `HOST_<NAME>` and `ORIGIN_<NAME>` for the local hostnames.

In run mode, `AddProtoStrapQueue`, `AddProtoStrapBucket` and `AddProtoStrapFiles` add one
LocalStack container, `protostrap-localstack`, which creates each queue and bucket, as
`protostrap-<name>`, and the files bucket, `protostrap-files`, before any component that
references them starts. A bucket is versioned and locked as the spec says; LocalStack keeps
every object and answers every origin, so `expire` and `browser` only apply in an account.
The reference sets what ProtoStrap sets in every replica, pointed at LocalStack:

| Variable                                   | Value                                         |
| ------------------------------------------ | --------------------------------------------- |
| `QUEUE_<NAME>_URL`                         | The queue's URL in LocalStack                 |
| `BUCKET_<NAME>`                            | `protostrap-<name>`                           |
| `FILES_BUCKET`                             | `protostrap-files`                            |
| `AWS_REGION`                               | `AWS_REGION` from the AppHost's configuration, or `us-east-1` |
| `AWS_ENDPOINT_URL_SQS`, `AWS_ENDPOINT_URL_S3` | LocalStack, for the AWS SDK's SQS and S3 clients only |

Credentials aren't set: LocalStack takes any, so the developer's own still reach the AWS
services LocalStack doesn't emulate. S3 clients need path-style addressing against it.


## Working on it

```bash
dotnet test hosting/ProtoStrap.Hosting.slnx
```

The tests build AppHosts in publish mode over repositories they write to a temporary
directory, and trace real projects with `dotnet msbuild`, so they need the .NET 10 SDK but no
network once packages are restored. `ProtoFastScenarioTests` generates the spec of the AppHost
in the design and compares it with `Golden/protofast.yaml`; after a deliberate change to the
format, rewrite it with `PROTOSTRAP_UPDATE_GOLDEN=1 dotnet test hosting/ProtoStrap.Hosting.slnx`
and run the Go tests, which check the same file.

Aspire's pipeline and compute environment APIs are experimental in 13.x and change between
minor versions, so the package pins Aspire 13.6.2. The spec's format, not the Aspire API, is
the contract.
