using Amazon.SQS;
using Grpc.HealthCheck;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Server.Kestrel.Core;
using Npgsql;
using Sample.Api;
using Sample.Shared;
using StackExchange.Redis;

const int AppPort = 8080;
const int MetricsPort = 9090;

var builder = WebApplication.CreateBuilder(args);
var me = builder.AddProtoStrap("api");

builder.WebHost.ConfigureKestrel(kestrel =>
{
    // Envoy and the readiness probe send everything, gRPC or not, as cleartext HTTP/2 with
    // prior knowledge. Without TLS, Kestrel only takes that on an HTTP/2-only endpoint.
    kestrel.ListenAnyIP(AppPort, listen => listen.Protocols = HttpProtocols.Http2);
    kestrel.ListenAnyIP(MetricsPort, listen => listen.Protocols = HttpProtocols.Http1);
});
// The 25 s shutdown timeout covers draining (10 s) and then finishing requests, inside the
// spec's 30 s stop.
builder.Services.Configure<HostOptions>(options => options.ShutdownTimeout = TimeSpan.FromSeconds(25));

builder.Services.AddGrpc();
builder.Services.AddSingleton<HealthServiceImpl>();
builder.Services.AddHostedService<Drain>();
builder.Services.AddSingleton<ApiMetrics>();
builder.Services.AddSingleton<JobQueue>();
// Connections are made on first use, so the api starts (and says hello) without them.
builder.Services.AddSingleton(services =>
    NpgsqlDataSource.Create(services.GetRequiredService<IConfiguration>().RequiredConnectionString("postgres")));
builder.Services.AddSingleton<IConnectionMultiplexer>(services =>
{
    var options = StackExchange.Redis.ConfigurationOptions.Parse(
        services.GetRequiredService<IConfiguration>().RequiredConnectionString("redis"));
    options.AbortOnConnectFail = false; // The multiplexer keeps retrying while Redis starts.
    return ConnectionMultiplexer.Connect(options);
});
builder.Services.AddSingleton<IAmazonSQS>(_ => new AmazonSQSClient());

var app = builder.Build();
var metrics = app.Services.GetRequiredService<ApiMetrics>();

app.Use(async (context, next) =>
{
    // /metrics is only on the metrics port, and everything else only on the app port.
    var onMetricsPort = context.Connection.LocalPort == MetricsPort;
    if (onMetricsPort != (context.Request.Path.Value == "/metrics"))
    {
        context.Response.StatusCode = StatusCodes.Status404NotFound;
        return;
    }
    if (!onMetricsPort && !context.Request.Path.StartsWithSegments("/grpc.health.v1.Health"))
        metrics.CountRequest();
    await next(context);
});

app.MapGrpcService<ApiService>();
app.MapGrpcService<HealthServiceImpl>();

app.MapGet("/api/hello", () => new { me.App, me.Component, me.Replica, me.Release, time = DateTimeOffset.UtcNow });

app.MapGet("/api/burn", (int? ms) =>
{
    var elapsed = Cpu.BurnCpuTime(Math.Clamp(ms ?? 100, 0, JobQueue.MaxBurnMs));
    return new { elapsed_ms = (int)elapsed.TotalMilliseconds, me.Component, me.Replica, me.Release };
});

app.MapPost("/api/jobs", async (JobQueue queue, int? count, [FromQuery(Name = "work_ms")] int? workMs, CancellationToken cancellationToken) =>
{
    if (!queue.Configured)
        return Results.Json(new { error = "QUEUE_JOBS_URL is not set" }, statusCode: StatusCodes.Status503ServiceUnavailable);
    var sent = await queue.EnqueueAsync(count ?? 100, workMs ?? Jobs.DefaultWorkMs, cancellationToken);
    return Results.Ok(new { sent, me.Replica });
});

app.MapGet("/api/jobs/stats", async (NpgsqlDataSource db, IConnectionMultiplexer redis, CancellationToken cancellationToken) =>
{
    await using var command = db.CreateCommand("SELECT count(*) FROM jobs_done");
    var done = (long)(await command.ExecuteScalarAsync(cancellationToken))!;
    var month = DateTime.UtcNow;
    var spent = (long?)await redis.GetDatabase().StringGetAsync(Budget.SpentKey(month)) ?? 0;
    return new { done, spent, budget = Budget.Limit(), month = month.ToString("yyyy-MM"), me.Replica };
});

app.MapGet("/metrics", async (HttpContext context) =>
{
    context.Response.ContentType = "text/plain; version=0.0.4; charset=utf-8";
    await context.Response.WriteAsync(await metrics.RenderAsync());
});

app.Run();
