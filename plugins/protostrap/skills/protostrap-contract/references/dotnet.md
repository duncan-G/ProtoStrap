# .NET under ProtoStrap

Working versions of all of this are in ProtoStrap's `sample/services/dotnet` (`Shared/ProtoStrapHosting.cs`,
`Api/Drain.cs`, `Worker/JobRunner.cs`). Packages: `AWSSDK.SecretsManager`, `AWSSDK.SQS`,
`AWSSDK.S3`, `AWSSDK.SSO` and `AWSSDK.SSOOIDC` for `aspire run`'s `AWS_PROFILE`, and
`Grpc.HealthCheck` for gRPC readiness.

## Keys into configuration

Call it first thing, before anything reads configuration:

```csharp
builder.Configuration.AddProtoStrapSecret(
    Environment.GetEnvironmentVariable("SECRET_ID"),
    Environment.GetEnvironmentVariable("SECRET_PREFIX"));
```

```csharp
public static IConfigurationBuilder AddProtoStrapSecret(this IConfigurationBuilder configuration, string? secretId, string? prefix)
{
    if (string.IsNullOrEmpty(secretId))
        return configuration; // A local run: the same keys are environment variables.

    using var client = new AmazonSecretsManagerClient(); // Default chain: the agent's credentials, AWS_REGION.
    var secret = client.GetSecretValueAsync(new GetSecretValueRequest { SecretId = secretId }).GetAwaiter().GetResult();
    var values = JsonSerializer.Deserialize<Dictionary<string, string>>(secret.SecretString)
        ?? throw new InvalidOperationException($"Secret {secretId} isn't a JSON object");

    var keys = new Dictionary<string, string>();
    foreach (var (key, value) in values)
        if (key.StartsWith("Shared_", StringComparison.Ordinal))
            keys[key["Shared_".Length..]] = value;
    if (!string.IsNullOrEmpty(prefix))
        foreach (var (key, value) in values)
            if (key.StartsWith(prefix, StringComparison.Ordinal))
                keys[key[prefix.Length..]] = value; // Its own keys win.

    return configuration.AddInMemoryCollection(keys.Select(pair => KeyValuePair.Create(
        pair.Key.Replace("__", ConfigurationPath.KeyDelimiter, StringComparison.Ordinal), (string?)pair.Value)));
}
```

`ConnectionStrings__postgres` then reads as `configuration.GetConnectionString("postgres")`,
the same name Aspire's `WithReference` sets locally.

## Readiness and draining (HTTP)

```csharp
public sealed class DrainState { public volatile bool Draining; }

public sealed class Drain(DrainState state, ILogger<Drain> log) : IHostedLifecycleService
{
    // StoppingAsync runs before any hosted service stops, so Kestrel still serves meanwhile.
    public async Task StoppingAsync(CancellationToken token)
    {
        state.Draining = true;
        var seconds = int.TryParse(Environment.GetEnvironmentVariable("DRAIN_SECONDS"), out var s) ? s
            : Environment.GetEnvironmentVariable("PROTOSTRAP_REPLICA") is { Length: > 0 } ? 10 : 0;
        log.LogInformation("Stopping: not ready, serving on for {Seconds} s", seconds);
        try { await Task.Delay(TimeSpan.FromSeconds(seconds), token); } catch (OperationCanceledException) { }
    }
    public Task StartingAsync(CancellationToken token) => Task.CompletedTask;
    public Task StartAsync(CancellationToken token) => Task.CompletedTask;
    public Task StartedAsync(CancellationToken token) => Task.CompletedTask;
    public Task StopAsync(CancellationToken token) => Task.CompletedTask;
    public Task StoppedAsync(CancellationToken token) => Task.CompletedTask;
}
```

```csharp
builder.Services.AddSingleton<DrainState>();
builder.Services.AddHostedService<Drain>();
// The drain delay plus the longest request, inside the spec's stop.
builder.Services.Configure<HostOptions>(o => o.ShutdownTimeout = TimeSpan.FromSeconds(25));

var app = builder.Build();
var component = Environment.GetEnvironmentVariable("PROTOSTRAP_COMPONENT");

// The spec's ready: /healthz. Only for this component's own name, and not while draining.
app.MapGet("/healthz", (HttpContext context, DrainState drain) =>
    drain.Draining || (component is { Length: > 0 } && context.Request.Host.Host != component)
        ? Results.StatusCode(StatusCodes.Status503ServiceUnavailable)
        : Results.Ok());
```

With ASP.NET health checks instead, add a check that is unhealthy while draining, and put the
host test in front of `MapHealthChecks("/ready")` in a small middleware.

Kestrel's default keep-alive timeout (130 s) is already longer than Envoy's 60 s.

## Readiness (gRPC)

`ready: grpc` means Envoy and the probe speak h2c to the port. Kestrel only takes h2c on an
HTTP/2-only endpoint:

```csharp
builder.WebHost.ConfigureKestrel(k => k.ListenAnyIP(8080, l => l.Protocols = HttpProtocols.Http2));
builder.Services.AddGrpc();
builder.Services.AddSingleton<HealthServiceImpl>();   // Grpc.HealthCheck: NOT_FOUND for names it wasn't given
// ...
app.MapGrpcService<HealthServiceImpl>();
```

Set `SERVING` for `""` and `PROTOSTRAP_COMPONENT` when started, and `NOT_SERVING` for both in
`StoppingAsync`, before the drain delay. For gRPC-Web from browsers the edge translates;
nothing changes in the service. Serve metrics on a separate HTTP/1.1 port.

## A worker

```csharp
var queueUrl = builder.Configuration["QUEUE_ORDERS_URL"]
    ?? throw new InvalidOperationException("QUEUE_ORDERS_URL is not set");
```

The loop, in a `BackgroundService`:

- `ReceiveMessageAsync` with `WaitTimeSeconds = 20` and `MaxNumberOfMessages` up to the free
  slots; run each job; `DeleteMessageAsync` only after it succeeds.
- Before the work, skip a job whose id is already recorded as done; record it in the same
  transaction as the work (`INSERT … ON CONFLICT (id) DO NOTHING`).
- When `stoppingToken` fires: stop receiving, give back messages received but not started with
  `ChangeMessageVisibilityBatchAsync(VisibilityTimeout = 0)`, and await the running jobs.
- Set `HostOptions.ShutdownTimeout` inside the spec's `stop`.

## S3 and SQS clients

```csharp
var s3 = new AmazonS3Config();
if (builder.Configuration["AWS_ENDPOINT_URL_S3"] is { Length: > 0 } endpoint)
{
    s3.ServiceURL = endpoint;
    s3.ForcePathStyle = true; // LocalStack serves buckets by path.
}
```

A presigned URL from the SDK is `https` unless `Protocol` is set; against LocalStack's `http`
endpoint, set `Protocol = endpoint.StartsWith("http:") ? Protocol.HTTP : Protocol.HTTPS`.

## Logs

Either one JSON line per entry on stdout, `builder.Logging.AddJsonConsole()` or the sample's
`JsonLineFormatter`, **or** logs over OTLP with the console off under ProtoStrap. Never both:
Aspire's ServiceDefaults exports logs over OTLP whenever ProtoStrap's endpoint is set, so a
JSON console beside it sends every line twice. The code for each is protostrap-telemetry's
`references/dotnet.md`. Raise `Microsoft.AspNetCore` to `Warning` so requests don't each log.
