using System.Diagnostics;
using System.Globalization;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Aspire.Hosting.Eventing;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using ProtoStrap.Hosting.Spec;

namespace ProtoStrap.Hosting;

/// <summary>
/// LocalEdge is the gateway under `aspire run` (docs/protostrap-aspire.md, "The local
/// gateway"): the Envoy an account's edge runs, in a container, configured by the same renderer,
/// `protostrap edge render`, from the same hosts, upstreams and authorizers. It serves each
/// host's local hostname on one port, and reaches each service where the AppHost runs it.
/// </summary>
internal static class LocalEdge
{
    /// <summary>DefaultPort is the port browsers reach the gateway on, on the machine.</summary>
    public const int DefaultPort = 20000;

    /// <summary>ListenPort is where Envoy listens in its container.</summary>
    public const int ListenPort = 10000;

    // EnvoyImage and EnvoyTag are internal/edge's EnvoyImage: the edge's own Envoy.
    private const string EnvoyImage = "envoyproxy/envoy";
    private const string EnvoyTag = "v1.40.2";

    /// <summary>
    /// Add makes the gateway Envoy's container. Its files are rendered as Aspire resolves its
    /// config argument, once every service it reaches has its address on the container network.
    /// </summary>
    public static void Add(IDistributedApplicationBuilder builder, IResourceBuilder<ProtoStrapGatewayResource> gateway)
    {
        var resource = gateway.Resource;
        var dir = Path.Combine(builder.AppHostDirectory, "obj", resource.Name);
        var config = new Config(resource, dir);
        gateway.WithImage(EnvoyImage, EnvoyTag)
            .WithBindMount(dir, "/etc/protostrap", isReadOnly: true)
            .WithArgs("-c", config, "--log-level", "warn")
            .WithHttpEndpoint(port: resource.LocalPort, targetPort: ListenPort, name: "http", isProxied: false)
            // A container reaches the AppHost's projects at host.docker.internal, which native
            // Docker on Linux doesn't define by itself.
            .WithContainerRuntimeArgs("--add-host=host.docker.internal:host-gateway")
            .WithUrls(context => LinkHosts(context, resource))
            // Envoy reads only OTEL_RESOURCE_ATTRIBUTES, the instance the dashboard knows it by.
            // The rest is rendered into its config (DashboardAsync).
            .WithOtlpExporter(OtlpProtocol.Grpc)
            .ExcludeFromManifest();
        resource.LocalBuild = LocalBuild.Add(builder, resource.Environment, gateway);
        builder.Eventing.Subscribe<BeforeStartEvent>((e, _) =>
        {
            config.AppServices = e.Services;
            ReferenceReachedServices(resource);
            return Task.CompletedTask;
        });
    }

    /// <summary>
    /// LinkHosts gives the gateway a link on the dashboard to each host's local hostname, by the
    /// host's name, first. The endpoint's own URL, on localhost, matches no host, so it is left
    /// to the details.
    /// </summary>
    private static void LinkHosts(ResourceUrlsCallbackContext context, ProtoStrapGatewayResource gateway)
    {
        var hosts = gateway.HostSettings.GroupBy(h => h.Name).Select(g => g.Last()).ToList();
        if (hosts.Count == 0)
            return;
        foreach (var url in context.Urls.Where(u => u.Endpoint?.EndpointName == "http"))
            url.DisplayLocation = UrlDisplayLocation.DetailsOnly;
        var endpoint = context.GetEndpoint("http");
        var port = gateway.LocalPort.ToString(CultureInfo.InvariantCulture);
        context.Urls.InsertRange(0, hosts.Select(h => new ResourceUrlAnnotation { Url = $"http://{h.Local}:{port}", DisplayText = h.Name, Endpoint = endpoint }));
    }

    /// <summary>
    /// ReferenceReachedServices has the gateway reference each service it reaches, once the model
    /// is complete, so the dashboard draws it in front of them. When a service's settings fail,
    /// none is: the gateway reports that when it renders.
    /// </summary>
    internal static void ReferenceReachedServices(ProtoStrapGatewayResource gateway)
    {
        List<(IResource Resource, ComponentSettings Settings, UpstreamSettings? Upstream)> reached;
        try
        {
            reached = ReachedServices(gateway);
        }
        catch (InvalidOperationException)
        {
            return;
        }
        foreach (var (service, _, _) in reached)
            gateway.Annotations.Add(new ResourceRelationshipAnnotation(service, "Reference"));
    }

    /// <summary>
    /// Config is the gateway's -c argument: Envoy's config, rendered when Aspire resolves it.
    /// Aspire resolves a container's arguments once each host resource they reference has its
    /// address on the container network, which under its container tunnel is only once the
    /// container is being created, after its start event. So Config references the services the
    /// gateway reaches.
    /// </summary>
    internal sealed class Config(ProtoStrapGatewayResource gateway, string dir) : IValueProvider, IValueWithReferences
    {
        private readonly SemaphoreSlim _rendering = new(1, 1);

        /// <summary>AppServices are the AppHost's services, once it starts.</summary>
        internal IServiceProvider? AppServices { get; set; }

        public IEnumerable<object> References => ReachedServices(gateway).Select(s => s.Resource);

        // Aspire logs that the gateway is waiting for this.
        public override string ToString() => "the gateway's config, from protostrap edge render";

        public async ValueTask<string?> GetValueAsync(CancellationToken cancellationToken = default) =>
            await RenderAsync((resource, endpoint) => ResolveAsync(resource, endpoint, cancellationToken), cancellationToken);

        /// <summary>
        /// RenderAsync writes the routing, with each service where resolve says the edge reaches
        /// it, and renders Envoy's files beside it. It returns where the container reads them.
        /// </summary>
        internal async Task<string> RenderAsync(Func<IResource, EndpointAnnotation, Task<string>> resolve, CancellationToken cancellationToken)
        {
            await _rendering.WaitAsync(cancellationToken);
            try
            {
                var logger = (AppServices?.GetService(typeof(ResourceLoggerService)) as ResourceLoggerService) is { } loggers
                    ? loggers.GetLogger(gateway)
                    : NullLogger.Instance;
                var renderer = await gateway.LocalBuild!.EnsureAsync(logger, cancellationToken);
                Directory.CreateDirectory(dir);
                var telemetry = AppServices?.GetService<DistributedApplicationExecutionContext>() is { } executionContext
                    ? await DashboardAsync(gateway, executionContext, logger, cancellationToken)
                    : null;
                var routing = await RoutingAsync(gateway, ListenPort, resolve, cancellationToken, telemetry);
                var routingPath = Path.Combine(dir, "routing.json");
                await File.WriteAllTextAsync(routingPath, routing, cancellationToken);
                await LocalEdge.RenderAsync(renderer, routingPath, dir, cancellationToken);
                return "/etc/protostrap/envoy.json";
            }
            finally
            {
                _rendering.Release();
            }
        }
    }

    /// <summary>
    /// DashboardAsync is where the gateway sends each request's log record and span: the OTLP
    /// endpoint and headers Aspire gives the gateway's container, which Envoy reads only from its
    /// config. It is null when the dashboard takes no gRPC, or there is none.
    /// </summary>
    internal static async Task<RoutedTelemetry?> DashboardAsync(
        ProtoStrapGatewayResource gateway, DistributedApplicationExecutionContext executionContext, ILogger logger, CancellationToken cancellationToken)
    {
        var env = new Dictionary<string, object>();
        var callbackContext = new EnvironmentCallbackContext(executionContext, gateway, env, cancellationToken) { Logger = logger };
        foreach (var callback in gateway.Annotations.OfType<EnvironmentCallbackAnnotation>())
            await callback.Callback(callbackContext);
        if (!env.TryGetValue("OTEL_EXPORTER_OTLP_ENDPOINT", out var endpoint) || env.GetValueOrDefault("OTEL_EXPORTER_OTLP_PROTOCOL") as string != "grpc")
            return null;
        var context = new ValueProviderContext { ExecutionContext = executionContext, Caller = gateway, Network = KnownNetworkIdentifiers.DefaultAspireContainerNetwork };
        var url = endpoint switch
        {
            IValueProvider provider => await provider.GetValueAsync(context, cancellationToken),
            string text => text,
            _ => null,
        };
        if (!Uri.TryCreate(url, UriKind.Absolute, out var uri))
            return null;
        // OTEL_EXPORTER_OTLP_HEADERS is key=value pairs, comma-separated.
        var headers = (env.GetValueOrDefault("OTEL_EXPORTER_OTLP_HEADERS") as string ?? "")
            .Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
            .Select(h => h.Split('=', 2))
            .Where(kv => kv.Length == 2)
            .ToDictionary(kv => kv[0], kv => kv[1], StringComparer.Ordinal);
        return new RoutedTelemetry(
            uri.Host + ":" + uri.Port.ToString(CultureInfo.InvariantCulture),
            uri.Scheme == Uri.UriSchemeHttps ? true : null,
            headers.Count > 0 ? headers : null,
            gateway.Name);
    }

    private static async Task RenderAsync(string renderer, string routing, string dir, CancellationToken cancellationToken)
    {
        var start = new ProcessStartInfo(renderer)
        {
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        foreach (var arg in new[] { "edge", "render", "--routing", routing, "--out", dir })
            start.ArgumentList.Add(arg);
        using var process = Process.Start(start)!;
        var output = process.StandardOutput.ReadToEndAsync(cancellationToken);
        var error = process.StandardError.ReadToEndAsync(cancellationToken);
        await process.WaitForExitAsync(cancellationToken);
        if (process.ExitCode != 0)
            throw new InvalidOperationException($"protostrap edge render failed: {(await output + await error).Trim()}");
    }

    /// <summary>
    /// ResolveAsync is where a container on the AppHost's network reaches an endpoint: a
    /// project on the host, a container by its name.
    /// </summary>
    private static async Task<string> ResolveAsync(IResource resource, EndpointAnnotation endpoint, CancellationToken cancellationToken)
    {
        var reference = new EndpointReference((IResourceWithEndpoints)resource, endpoint, KnownNetworkIdentifiers.DefaultAspireContainerNetwork);
        return await reference.Property(EndpointProperty.HostAndPort).GetValueAsync(cancellationToken) ?? "";
    }

    /// <summary>
    /// RoutingAsync is the routing `protostrap edge render` reads, as JSON: the services the
    /// gateway sends requests to and asks about them, where it reaches each, and each host's
    /// local hostname.
    /// </summary>
    public static async Task<string> RoutingAsync(
        ProtoStrapGatewayResource gateway, int listenPort,
        Func<IResource, EndpointAnnotation, Task<string>> resolve, CancellationToken cancellationToken,
        RoutedTelemetry? telemetry = null)
    {
        var routed = new List<RoutedService>();
        foreach (var (resource, settings, upstream) in ReachedServices(gateway))
        {
            cancellationToken.ThrowIfCancellationRequested();
            var endpoints = resource.Annotations.OfType<EndpointAnnotation>().ToList();
            var primary = endpoints.FirstOrDefault(e => e.Name == "http") ?? endpoints.FirstOrDefault(e => e.UriScheme == "http")
                ?? endpoints.FirstOrDefault(e => e.UriScheme != "https") ?? endpoints.FirstOrDefault();
            if (primary is null)
                continue;
            var address = await resolve(resource, primary);
            var port = int.Parse(address[(address.LastIndexOf(':') + 1)..], CultureInfo.InvariantCulture);

            var (ready, checkEndpoint) = CheckOf(resource, settings, endpoints, primary);
            int? healthPort = null;
            if (checkEndpoint is not null && checkEndpoint != primary)
            {
                var at = await resolve(resource, checkEndpoint);
                healthPort = int.Parse(at[(at.LastIndexOf(':') + 1)..], CultureInfo.InvariantCulture);
            }
            else if (ready?.Port is { } checkPort)
            {
                healthPort = checkPort;
            }
            var protocol = upstream?.Protocol ?? (ready is { Kind: "grpc", Port: null } ? GatewayProtocol.Grpc : GatewayProtocol.Http1);

            routed.Add(new RoutedService(
                resource.Name, port, ready?.Value ?? "tcp", protocol.ToString().ToLowerInvariant(),
                upstream is not { Routes.Count: > 0 } ? null : upstream.Routes.Select(Route).ToList(),
                upstream?.Authorize is { } a
                    ? new RoutedAuthorize(a.By.Name, a.Path is { Length: > 0 } p && p != "/" ? p : null, a.Headers.Count > 0 ? a.Headers : null,
                        a.Forward.Count > 0 ? a.Forward : null, a.Respond.Count > 0 ? a.Respond : null,
                        a.Open ? true : null, a.Timeout is { } t ? Quantity.Duration(t) : null, a.Skip.Count > 0 ? a.Skip : null)
                    : null,
                address, healthPort));
        }

        var routing = new Routing(
            routed,
            gateway.HostSettings.Count == 0 ? null : gateway.HostSettings.GroupBy(h => h.Name).ToDictionary(g => g.Key, g => g.Last().Local, StringComparer.Ordinal),
            "0.0.0.0:" + listenPort.ToString(CultureInfo.InvariantCulture),
            Local: true,
            telemetry);
        return JsonSerializer.Serialize(routing, JsonOptions);
    }

    /// <summary>
    /// ReachedServices are the services the gateway reaches, in name order: each upstream, and
    /// each authorizer of one, with its settings, and its upstream if it is one.
    /// </summary>
    private static List<(IResource Resource, ComponentSettings Settings, UpstreamSettings? Upstream)> ReachedServices(ProtoStrapGatewayResource gateway)
    {
        var reached = new Dictionary<IResource, UpstreamSettings?>();
        foreach (var upstream in gateway.Upstreams)
        {
            reached[upstream.Resource] = upstream;
            if (upstream.Authorize is { } authorize)
                reached.TryAdd(authorize.By, null);
        }
        var services = new List<(IResource Resource, ComponentSettings Settings, UpstreamSettings? Upstream)>();
        foreach (var (resource, upstream) in reached.OrderBy(r => r.Key.Name, StringComparer.Ordinal))
        {
            if (!resource.TryGetLastAnnotation<ProtoStrapComponentAnnotation>(out var annotation))
                throw new InvalidOperationException($"{resource.Name} isn't a ProtoStrap component, so the gateway can't route to it: call PublishAsProtoStrapService(...)");
            ComponentSettings settings;
            try
            {
                settings = annotation.Configure();
            }
            catch (Exception e)
            {
                throw new InvalidOperationException($"{resource.Name}: {annotation.Method} failed, so the gateway can't route to it: {e.Message}", e);
            }
            if (settings.Kind != ComponentKind.Service)
                throw new InvalidOperationException($"{resource.Name} is a {settings.Kind.ToString().ToLowerInvariant()}: only services get requests from the gateway");
            services.Add((resource, settings, upstream));
        }
        return services;
    }

    private static RoutedRoute Route(RouteSettings route)
    {
        var text = route.Route;
        if (text.StartsWith('/'))
            return new(null, text, route.Rate);
        var slash = text.IndexOf('/');
        return slash < 0 ? new(text, "/", route.Rate) : new(text[..slash], text[slash..], route.Rate);
    }

    /// <summary>
    /// CheckOf is the service's check as the spec writes it, and the endpoint it runs on when
    /// that is known: its own, or another the AppHost gives it.
    /// </summary>
    private static (Readiness? Ready, EndpointAnnotation? Endpoint) CheckOf(
        IResource resource, ComponentSettings settings, List<EndpointAnnotation> endpoints, EndpointAnnotation primary)
    {
        if (settings.Ready is { } ready)
        {
            var on = ready.Port is { } p ? endpoints.FirstOrDefault(e => e.TargetPort == p) : primary;
            return (ready, on);
        }
        string? path = null, endpointName = null;
        if (resource.Annotations.OfType<EndpointProbeAnnotation>().FirstOrDefault(p => p.Type == ProbeType.Readiness) is { } probe)
        {
            (path, endpointName) = (probe.Path, probe.EndpointReference?.EndpointName);
        }
        else
        {
            var pattern = new Regex("^" + Regex.Escape(resource.Name) + "_(?<endpoint>[^/]+?)_(?<path>/.*)_(?<status>[0-9]{3})_check$");
            if (resource.Annotations.OfType<HealthCheckAnnotation>().Select(h => pattern.Match(h.Key)).FirstOrDefault(m => m.Success) is { } match)
                (path, endpointName) = (match.Groups["path"].Value, match.Groups["endpoint"].Value);
        }
        if (path is null)
            return (null, null);
        var endpoint = endpoints.FirstOrDefault(e => e.Name == endpointName) ?? primary;
        return endpoint == primary || endpoint.TargetPort is not { } port
            ? (Readiness.Http(path), endpoint)
            : (Readiness.Http(path).On(port), endpoint);
    }

    private static readonly JsonSerializerOptions JsonOptions = new()
    {
        WriteIndented = true,
        PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
        DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
        Encoder = System.Text.Encodings.Web.JavaScriptEncoder.UnsafeRelaxedJsonEscaping,
    };

    // These records are internal/edge's Routing, Service, Route, Authorize and Telemetry, as JSON.

    private sealed record Routing(
        IReadOnlyList<RoutedService> Services, IReadOnlyDictionary<string, string>? Hosts, string Listen, bool Local, RoutedTelemetry? Telemetry);

    private sealed record RoutedService(
        string Name, int Port, string Ready, string Protocol, IReadOnlyList<RoutedRoute>? Routes, RoutedAuthorize? Authorize,
        string Address, int? HealthPort);

    private sealed record RoutedRoute(string? Host, string Path, double? Rate);

    internal sealed record RoutedTelemetry(string Address, bool? Tls, IReadOnlyDictionary<string, string>? Headers, string Service);

    private sealed record RoutedAuthorize(
        string By, string? Path, IReadOnlyList<string>? Headers, IReadOnlyList<string>? Forward, IReadOnlyList<string>? Respond,
        bool? Open, string? Timeout, IReadOnlyList<string>? Skip);
}
