using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Hosting;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;

namespace ProtoStrap.Hosting;

/// <summary>ProtoStrapBuildResource is protostrap-build on the dashboard: the protostrap the local gateway renders with.</summary>
internal sealed class ProtoStrapBuildResource(string name) : Resource(name);

/// <summary>
/// LocalBuild is the protostrap the local gateway renders with under `aspire run`
/// (docs/protostrap-aspire.md, "The local gateway"). With the ProtoStrap checkout ProtoStrap.Hosting
/// is built from, and Go, it builds protostrap from the checkout as the AppHost starts, again
/// whenever the gateway starts, and on its Rebuild command, which then restarts the gateway. Go's build
/// cache makes a build a no-op when nothing changed. Every few seconds it checks whether a file
/// that goes into protostrap changed after the last build started, and says it's behind if one did.
/// </summary>
internal sealed class LocalBuild
{
    /// <summary>Name is the build's resource name.</summary>
    public const string Name = "protostrap-build";

    /// <summary>RebuildCommand is the name of its dashboard command.</summary>
    public const string RebuildCommand = "rebuild-protostrap";

    internal const string UpToDate = "Up to date";
    internal const string Behind = "Behind";
    internal const string Failed = "Build failed";
    internal const string NoGo = "Go not found";

    // Hidden is the state of a build the gateway doesn't use, which the dashboard hides.
    internal const string Hidden = "Not used";

    // Executable is the file name protostrap is built as.
    private static readonly string Executable = OperatingSystem.IsWindows() ? "protostrap.exe" : "protostrap";

    private readonly ProtoStrapEnvironmentResource _environment;
    private readonly string _output;
    private readonly SemaphoreSlim _building = new(1, 1);

    // _built is when the last build that succeeded started: a file changed after it may not be in it.
    private DateTime? _built;

    private LocalBuild(ProtoStrapEnvironmentResource environment, string output)
    {
        _environment = environment;
        _output = output;
    }

    /// <summary>Resource is the build on the dashboard.</summary>
    public ProtoStrapBuildResource Resource { get; } = new(Name);

    /// <summary>State is the build's state on the dashboard.</summary>
    internal string State { get; private set; } = KnownResourceStates.NotStarted;

    /// <summary>Logger writes to the build's console, once the AppHost starts. Tests replace it.</summary>
    internal ILogger Logger { get; set; } = NullLogger.Instance;

    private ResourceNotificationService? Notifications { get; set; }

    /// <summary>
    /// Add adds the build under the gateway, with its Rebuild command. It starts building as the
    /// AppHost starts it.
    /// </summary>
    public static LocalBuild Add(IDistributedApplicationBuilder builder, ProtoStrapEnvironmentResource environment, IResourceBuilder<ContainerResource> gateway)
    {
        var build = new LocalBuild(environment, Path.Combine(builder.AppHostDirectory, "obj", "protostrap", Executable));
        builder.AddResource(build.Resource)
            .WithParentRelationship(gateway)
            .WithInitialState(new CustomResourceSnapshot
            {
                ResourceType = "Build",
                State = KnownResourceStates.NotStarted,
                Properties = [new("protostrap.binary", build._output)],
            })
            .WithCommand(RebuildCommand, "Rebuild", context => build.ExecuteRebuildAsync(context, gateway.Resource), new CommandOptions
            {
                Description = "Builds protostrap from the ProtoStrap checkout, then restarts the gateway with it.",
                IconName = "ArrowSync",
                IsHighlighted = true,
                UpdateState = context =>
                    context.ResourceSnapshot.IsHidden ? ResourceCommandState.Hidden
                    : context.ResourceSnapshot.State?.Text == KnownResourceStates.NotStarted || context.ResourceSnapshot.State?.Text == KnownResourceStates.Building
                        ? ResourceCommandState.Disabled
                        : ResourceCommandState.Enabled,
            })
            .ExcludeFromManifest()
            .OnInitializeResource((_, e, _) =>
            {
                build.Start(e);
                return Task.CompletedTask;
            });
        return build;
    }

    /// <summary>
    /// EnsureAsync returns the protostrap the gateway renders with, built again if the build is its
    /// own. Without its own build, as for a binary WithCheck names, it is hidden, and the gateway's
    /// logger hears whether the binary found isn't the AppHost's ProtoStrap.
    /// </summary>
    public async Task<string> EnsureAsync(ILogger gatewayLogger, CancellationToken cancellationToken)
    {
        var binary = _environment.Binary;
        var command = _environment.CheckCommand ?? ProtoStrapBinary.DefaultCommand;
        var checkout = command == ProtoStrapBinary.DefaultCommand ? binary.ProtoStrapCheckout() : null;
        if (checkout is null)
        {
            await PublishAsync(Hidden, null);
            return await binary.FindOrBuildAsync(command, _output, gatewayLogger, cancellationToken);
        }
        if (binary.FindGo() is null)
        {
            if (State != NoGo && binary.Find(command) is { } found)
                Logger.LogWarning("Go isn't installed, so protostrap isn't built from {Checkout}: the gateway renders with {Binary}.", checkout, found);
            await PublishAsync(NoGo, KnownResourceStateStyles.Warn);
            return await binary.FindOrBuildAsync(command, _output, gatewayLogger, cancellationToken);
        }

        await _building.WaitAsync(cancellationToken);
        try
        {
            await PublishAsync(KnownResourceStates.Building, KnownResourceStateStyles.Info);
            var started = DateTime.UtcNow;
            string path;
            try
            {
                path = await binary.FindOrBuildAsync(command, _output, Logger, cancellationToken);
            }
            catch (InvalidOperationException e)
            {
                Logger.LogError("{Problem}", e.Message);
                await PublishAsync(Failed, KnownResourceStateStyles.Error);
                throw;
            }
            _built = started;
            Logger.LogInformation("protostrap is up to date with {Checkout}, in {Seconds:0.0}s", checkout, (DateTime.UtcNow - started).TotalSeconds);
            await PublishAsync(UpToDate, KnownResourceStateStyles.Success);
            return path;
        }
        finally
        {
            _building.Release();
        }
    }

    /// <summary>
    /// CheckAsync says the build is behind once a file that goes into protostrap changed after the
    /// last build started: the gateway still renders with the protostrap built before it.
    /// </summary>
    internal async Task CheckAsync()
    {
        if (State != UpToDate || _built is not { } built || _environment.Binary.ProtoStrapCheckout() is not { } checkout)
            return;
        if (ProtoStrapBinary.NewestSource(checkout) is not { } newest || newest.Changed <= built)
            return;
        Logger.LogWarning(
            "{File} changed in {Checkout} at {Changed}, after protostrap was built: the gateway still renders with the ProtoStrap from before. Rebuild it with protostrap-build's Rebuild, or by restarting the gateway.",
            newest.Path, checkout, newest.Changed.ToLocalTime().ToString("HH:mm:ss", System.Globalization.CultureInfo.InvariantCulture));
        await PublishAsync(Behind, KnownResourceStateStyles.Warn);
    }

    /// <summary>
    /// RebuildAsync builds protostrap, then restarts the gateway, which renders with it, and returns
    /// why it couldn't, or null. A failed build leaves the gateway as it is.
    /// </summary>
    internal async Task<string?> RebuildAsync(Func<CancellationToken, Task<string?>> restartEdge, CancellationToken cancellationToken)
    {
        try
        {
            await EnsureAsync(Logger, cancellationToken);
        }
        catch (InvalidOperationException e)
        {
            return e.Message;
        }
        if (State != UpToDate)
            return $"protostrap isn't built from the checkout: {State}.";
        return await restartEdge(cancellationToken);
    }

    private async Task<ExecuteCommandResult> ExecuteRebuildAsync(ExecuteCommandContext context, IResource gateway)
    {
        var commands = context.Services.GetRequiredService<ResourceCommandService>();
        var problem = await RebuildAsync(async cancellationToken =>
        {
            var restarted = await commands.ExecuteCommandAsync(gateway, KnownResourceCommands.RestartCommand, cancellationToken);
            return restarted.Success ? null : restarted.Message ?? "the gateway didn't restart";
        }, context.CancellationToken);
        return problem is null ? CommandResults.Success() : CommandResults.Failure(problem);
    }

    /// <summary>
    /// Start builds protostrap as the AppHost starts, before the gateway asks for it, then checks
    /// every 5 seconds whether the checkout moved on, until the AppHost stops.
    /// </summary>
    private void Start(InitializeResourceEvent e)
    {
        Notifications = e.Notifications;
        Logger = e.Logger;
        var stopping = e.Services.GetRequiredService<IHostApplicationLifetime>().ApplicationStopping;
        _ = Task.Run(async () =>
        {
            try
            {
                await EnsureAsync(NullLogger.Instance, stopping);
            }
            catch (InvalidOperationException)
            {
                // A failed build is in the build's state and console, and the gateway says it too.
            }
            using var timer = new PeriodicTimer(TimeSpan.FromSeconds(5));
            try
            {
                while (await timer.WaitForNextTickAsync(stopping))
                {
                    try
                    {
                        await CheckAsync();
                    }
                    catch (Exception ex) when (ex is IOException or UnauthorizedAccessException)
                    {
                        // A file that went away mid-check is checked again at the next tick.
                    }
                }
            }
            catch (OperationCanceledException)
            {
            }
        }, stopping);
    }

    private async Task PublishAsync(string state, string? style)
    {
        State = state;
        if (Notifications is { } notifications)
            await notifications.PublishUpdateAsync(Resource, s => s with { State = new ResourceStateSnapshot(state, style), IsHidden = state == Hidden });
    }
}
