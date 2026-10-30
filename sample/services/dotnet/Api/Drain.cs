using Grpc.Health.V1;
using Grpc.HealthCheck;
using Sample.Shared;

namespace Sample.Api;

/// <summary>
/// Drain owns the gRPC health status that the readiness probe and Envoy check, for the
/// service named after the component and for "" (the whole server).
/// The status is SERVING once Kestrel listens. On SIGTERM it turns NOT_SERVING at once, then
/// requests are still served for DRAIN_SECONDS so the gateway stops sending them, and only
/// then does Kestrel stop.
/// </summary>
/// <remarks>
/// This waits in StoppingAsync, not StopAsync: StoppingAsync runs before any hosted service
/// stops, whereas WebApplicationBuilder registers Kestrel's hosted service last, so in the
/// reverse-order StopAsync pass Kestrel would stop first.
/// </remarks>
public sealed class Drain(HealthServiceImpl health, Identity me, ILogger<Drain> log) : IHostedLifecycleService
{
    public Task StartingAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StartAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StartedAsync(CancellationToken cancellationToken)
    {
        SetStatus(HealthCheckResponse.Types.ServingStatus.Serving);
        log.LogInformation("Serving");
        return Task.CompletedTask;
    }

    public async Task StoppingAsync(CancellationToken cancellationToken)
    {
        SetStatus(HealthCheckResponse.Types.ServingStatus.NotServing);
        var seconds = Env.Int("DRAIN_SECONDS", 10);
        log.LogInformation("Draining for {Seconds} s", seconds);
        try
        {
            await Task.Delay(TimeSpan.FromSeconds(seconds), cancellationToken);
        }
        catch (OperationCanceledException)
        {
            // The shutdown timeout ran out first.
        }
    }

    public Task StopAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    public Task StoppedAsync(CancellationToken cancellationToken) => Task.CompletedTask;

    private void SetStatus(HealthCheckResponse.Types.ServingStatus status)
    {
        health.SetStatus("", status);
        health.SetStatus(me.Component, status);
    }
}
