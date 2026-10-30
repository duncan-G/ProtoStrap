using System.Diagnostics;
using System.Globalization;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.RegularExpressions;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Aspire.Hosting.Eventing;

namespace ProtoStrap.Hosting;

/// <summary>
/// LocalEdge is the edge under `aspire run` (docs/protostrap-aspire.md, "The local edge"):
/// the Envoy an account's edge runs, in a container, configured by the same renderer,
/// `protostrap edge render`, from the same routes, hosts and authorizers. It serves each
/// host's local hostname on one port, and reaches each service where the AppHost runs it.
/// </summary>
internal static class LocalEdge
{
    /// <summary>Name is the edge's resource name.</summary>
    public const string Name = "protostrap-edge";

    /// <summary>DefaultPort is the port browsers reach the edge on, on the machine.</summary>
    public const int DefaultPort = 20000;

    /// <summary>ListenPort is where Envoy listens in its container.</summary>
    public const int ListenPort = 10000;

    // EnvoyImage and EnvoyTag are internal/edge's EnvoyImage: the edge's own Envoy.
    private const string EnvoyImage = "envoyproxy/envoy";
    private const string EnvoyTag = "v1.40.2";

    /// <summary>
    /// Add adds the edge's container. Its files are rendered just before it starts, once the
    /// AppHost has given every service its endpoints.
    /// </summary>
    public static void Add(IDistributedApplicationBuilder builder, ProtoStrapEnvironmentResource environment)
    {
        var dir = Path.Combine(builder.AppHostDirectory, "obj", Name);
        var edge = builder.AddContainer(Name, EnvoyImage, EnvoyTag)
            .WithBindMount(dir, "/etc/protostrap", isReadOnly: true)
            .WithArgs("-c", "/etc/protostrap/envoy.json", "--log-level", "warn")
            .WithHttpEndpoint(port: environment.LocalEdgePort, targetPort: ListenPort, name: "http", isProxied: false)
            // A container reaches the AppHost's projects at host.docker.internal, which native
            // Docker on Linux doesn't define by itself.
            .WithContainerRuntimeArgs("--add-host=host.docker.internal:host-gateway")
            .ExcludeFromManifest();
        environment.LocalEdge = edge.Resource;
        builder.Eventing.Subscribe<BeforeResourceStartedEvent>(edge.Resource, async (e, cancellationToken) =>
        {
            var renderer = Renderer(environment.CheckCommand ?? "protostrap");
            Directory.CreateDirectory(dir);
            var model = e.Services.GetService(typeof(DistributedApplicationModel)) as DistributedApplicationModel;
            var routing = await RoutingAsync(model?.Resources ?? builder.Resources, environment, ListenPort, ResolveAsync, cancellationToken);
            var routingPath = Path.Combine(dir, "routing.json");
            await File.WriteAllTextAsync(routingPath, routing, cancellationToken);
            await RenderAsync(renderer, routingPath, dir, cancellationToken);
        });
    }

    /// <summary>
    /// Renderer finds the protostrap binary that renders the edge: a path, or a name on PATH.
    /// </summary>
    public static string Renderer(string command)
    {
        if (command.Contains(Path.DirectorySeparatorChar) || command.Contains(Path.AltDirectorySeparatorChar))
        {
            if (File.Exists(command))
                return command;
        }
        else
        {
            var names = OperatingSystem.IsWindows() ? new[] { command + ".exe", command } : [command];
            var found = (Environment.GetEnvironmentVariable("PATH") ?? "")
                .Split(Path.PathSeparator, StringSplitOptions.RemoveEmptyEntries)
                .SelectMany(directory => names.Select(name => Path.Combine(directory, name)))
                .FirstOrDefault(File.Exists);
            if (found is not null)
                return found;
        }
        throw new InvalidOperationException(
            $"The local edge is rendered by `protostrap edge render`, and {command} isn't installed. Build it from the ProtoStrap checkout at the AppHost's version, " +
            "`go build -o ~/.local/bin/protostrap ./cmd/protostrap`, and put it on PATH, or name it with WithCheck(...).");
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
    private static async Task<string> ResolveAsync(IResource resource, EndpointAnnotation endpoint)
    {
        var reference = new EndpointReference((IResourceWithEndpoints)resource, endpoint, KnownNetworkIdentifiers.DefaultAspireContainerNetwork);
        return await reference.Property(EndpointProperty.HostAndPort).GetValueAsync(CancellationToken.None) ?? "";
    }

    /// <summary>
    /// RoutingAsync is the routing `protostrap edge render` reads, as JSON: the services the edge
    /// sends requests to and asks about them, where it reaches each, and each host's local
    /// hostname.
    /// </summary>
    public static async Task<string> RoutingAsync(
        IEnumerable<IResource> resources, ProtoStrapEnvironmentResource environment, int listenPort,
        Func<IResource, EndpointAnnotation, Task<string>> resolve, CancellationToken cancellationToken)
    {
        var services = new List<(IResource Resource, ComponentSettings Settings)>();
        foreach (var resource in resources.Where(r => r is IResourceWithEndpoints && !r.IsExcludedFromPublish()).OrderBy(r => r.Name, StringComparer.Ordinal))
        {
            if (!resource.TryGetLastAnnotation<ProtoStrapComponentAnnotation>(out var annotation))
                continue;
            ComponentSettings settings;
            try
            {
                settings = annotation.Configure();
            }
            catch (Exception e)
            {
                throw new InvalidOperationException($"{resource.Name}: {annotation.Method} failed, so the local edge can't route to it: {e.Message}", e);
            }
            if (settings.Kind == ComponentKind.Service)
                services.Add((resource, settings));
        }
        var authorizers = services.Where(s => s.Settings.Routes.Count > 0 && s.Settings.Authorize is not null)
            .Select(s => s.Settings.Authorize!.By).ToHashSet();

        var routed = new List<RoutedService>();
        foreach (var (resource, settings) in services)
        {
            cancellationToken.ThrowIfCancellationRequested();
            if (settings.Routes.Count == 0 && !authorizers.Contains(resource))
                continue;
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
            var protocol = settings.Protocol ?? (ready is { Kind: "grpc", Port: null } ? GatewayProtocol.Grpc : GatewayProtocol.Http1);

            routed.Add(new RoutedService(
                resource.Name, port, ready?.Value ?? "tcp", protocol.ToString().ToLowerInvariant(),
                settings.Routes.Count == 0 ? null : settings.Routes.Select(Route).ToList(),
                settings.Authorize is { } a
                    ? new RoutedAuthorize(a.By.Name, a.Path is { Length: > 0 } p && p != "/" ? p : null, a.Headers.Count > 0 ? a.Headers : null, a.Forward.Count > 0 ? a.Forward : null)
                    : null,
                address, healthPort));
        }

        var routing = new Routing(
            routed,
            environment.HostSettings.Count == 0 ? null : environment.HostSettings.GroupBy(h => h.Name).ToDictionary(g => g.Key, g => g.Last().Local, StringComparer.Ordinal),
            "0.0.0.0:" + listenPort.ToString(CultureInfo.InvariantCulture),
            Local: true);
        return JsonSerializer.Serialize(routing, JsonOptions);
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

    // These records are internal/edge's Routing, Service, Route and Authorize, as JSON.

    private sealed record Routing(IReadOnlyList<RoutedService> Services, IReadOnlyDictionary<string, string>? Hosts, string Listen, bool Local);

    private sealed record RoutedService(
        string Name, int Port, string Ready, string Protocol, IReadOnlyList<RoutedRoute>? Routes, RoutedAuthorize? Authorize,
        string Address, int? HealthPort);

    private sealed record RoutedRoute(string? Host, string Path, double? Rate);

    private sealed record RoutedAuthorize(string By, string? Path, IReadOnlyList<string>? Headers, IReadOnlyList<string>? Forward);
}
