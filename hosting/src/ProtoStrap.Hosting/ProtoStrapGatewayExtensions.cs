using System.Globalization;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting;

namespace Aspire.Hosting;

/// <summary>
/// These extension methods add ProtoStrap's own components to the environment, each a section of
/// protostrap.yaml that runs only when it is added (docs/protostrap-aspire.md, "The tunnel, the
/// gateway and telemetry"), and configure each where it is added: the gateway's hosts and
/// upstreams, and telemetry's size and retention.
/// </summary>
public static class ProtoStrapGatewayExtensions
{
    /// <summary>
    /// AddEnvoyGateway adds the gateway (docs/protostrap.md#gateway): Envoy on each ingress VM,
    /// which answers the hosts WithHost adds and sends their requests to the services WithUpstream
    /// registers. Under `aspire run` it is the same Envoy in a container, serving each host's
    /// local hostname on one port, and every resource gets each host's HOST_ and ORIGIN_.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="name">It is the gateway's resource name, and its container's under `aspire run`.</param>
    public static IResourceBuilder<ProtoStrapGatewayResource> AddEnvoyGateway(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string name)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var environment = builder.Resource;
        if (environment.Gateway is { } existing)
            throw new InvalidOperationException($"{environment.Name} already has a gateway, {existing.Name}: add hosts and upstreams to it");
        var gateway = new ProtoStrapGatewayResource(name, environment);
        environment.Gateway = gateway;
        var app = builder.ApplicationBuilder;
        if (app.ExecutionContext.IsPublishMode)
            return app.CreateResourceBuilder(gateway);

        var local = app.AddResource(gateway);
        LocalEdge.Add(app, local);
        app.Eventing.Subscribe<BeforeStartEvent>((e, _) =>
        {
            AddHostVariables(e.Model.Resources, gateway);
            return Task.CompletedTask;
        });
        return local;
    }

    /// <summary>
    /// AddCloudflareTunnel adds the tunnel (docs/protostrap.md#tunnel): cloudflared beside the
    /// gateway's Envoy, which brings the requests for the gateway's hosts in from Cloudflare.
    /// Without it nothing reaches the gateway from the internet. It runs only in an account.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="name">It is the tunnel's resource name.</param>
    /// <param name="gateway">It is the environment's gateway, which the tunnel sends every request to.</param>
    public static IResourceBuilder<ProtoStrapTunnelResource> AddCloudflareTunnel(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string name, IResourceBuilder<ProtoStrapGatewayResource> gateway)
    {
        ArgumentNullException.ThrowIfNull(builder);
        ArgumentNullException.ThrowIfNull(gateway);
        var environment = builder.Resource;
        if (environment.Tunnel is { } existing)
            throw new InvalidOperationException($"{environment.Name} already has a tunnel, {existing.Name}");
        if (gateway.Resource.Environment != environment)
            throw new InvalidOperationException($"{gateway.Resource.Name} isn't this environment's gateway: {environment.Name}'s tunnel sends requests to its own");
        var tunnel = new ProtoStrapTunnelResource(name, environment, gateway.Resource);
        environment.Tunnel = tunnel;
        return builder.ApplicationBuilder.CreateResourceBuilder(tunnel);
    }

    /// <summary>
    /// AddTelemetry adds telemetry (docs/telemetry.md): the collector, Loki, Tempo, Prometheus and
    /// Grafana on data-1, which keep the application's logs, traces and metrics, and which every
    /// replica sends its telemetry and output to. Without it, replicas' output goes to CloudWatch
    /// Logs. It runs only in an account: under `aspire run` the dashboard has the telemetry.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="name">It is telemetry's resource name.</param>
    public static IResourceBuilder<ProtoStrapTelemetryResource> AddTelemetry(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string name)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var environment = builder.Resource;
        if (environment.Telemetry is { } existing)
            throw new InvalidOperationException($"{environment.Name} already has telemetry, {existing.Name}");
        var telemetry = new ProtoStrapTelemetryResource(name, environment);
        environment.Telemetry = telemetry;
        return builder.ApplicationBuilder.CreateResourceBuilder(telemetry);
    }

    /// <summary>
    /// WithHost adds a host the application answers on (docs/protostrap.md, "Hosts"). Routes
    /// refer to it by its name, such as "admin/", so they mean the same in every account; an
    /// account's Terraform config may give the name another hostname. Every replica learns the
    /// hostname as HOST_&lt;NAME&gt; and its origin as ORIGIN_&lt;NAME&gt;, and so does every
    /// resource under `aspire run`, with the local hostname.
    /// </summary>
    /// <param name="builder">It is the gateway.</param>
    /// <param name="name">It is the host's name: lowercase letters, digits and dashes, such as admin.</param>
    /// <param name="hostname">It is the spec's hostname, such as admin.example.com.</param>
    /// <param name="local">It is the hostname under `aspire run`, such as admin.dev.localhost.</param>
    public static IResourceBuilder<ProtoStrapGatewayResource> WithHost(
        this IResourceBuilder<ProtoStrapGatewayResource> builder, string name, string hostname, string local)
    {
        builder.Resource.HostSettings.Add(new(name, hostname, local));
        return builder;
    }

    /// <summary>
    /// WithUpstream registers a service the gateway sends requests to, and configures how: its
    /// routes, what the gateway speaks to it, and who authorizes its requests. Registering one
    /// again configures it further. The service is published with PublishAsProtoStrapService.
    /// </summary>
    /// <param name="builder">It is the gateway.</param>
    /// <param name="service">It is the service.</param>
    /// <param name="configure">It sets the upstream's routes, protocol and authorizer.</param>
    public static IResourceBuilder<ProtoStrapGatewayResource> WithUpstream<T>(
        this IResourceBuilder<ProtoStrapGatewayResource> builder, IResourceBuilder<T> service, Action<ProtoStrapUpstream> configure)
        where T : IResource
    {
        ArgumentNullException.ThrowIfNull(service);
        ArgumentNullException.ThrowIfNull(configure);
        configure(new ProtoStrapUpstream(builder.Resource.Upstream(service.Resource)));
        return builder;
    }

    /// <summary>
    /// WithRequest sets what the gateway's container reserves on each ingress VM, the tunnel's
    /// cloudflared included: 500m and 256Mi by default. It must fit the ingress VM.
    /// </summary>
    public static IResourceBuilder<ProtoStrapGatewayResource> WithRequest(
        this IResourceBuilder<ProtoStrapGatewayResource> builder, string cpu, string memory)
    {
        builder.Resource.Request = new(cpu, memory);
        return builder;
    }

    /// <summary>
    /// WithLocalPort sets the port the gateway serves the hosts' local hostnames on under `aspire
    /// run`, 20000 by default: browsers open http://&lt;local hostname&gt;:&lt;port&gt;.
    /// </summary>
    public static IResourceBuilder<ProtoStrapGatewayResource> WithLocalPort(
        this IResourceBuilder<ProtoStrapGatewayResource> builder, int port)
    {
        ArgumentOutOfRangeException.ThrowIfLessThan(port, 1);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(port, 65535);
        builder.Resource.LocalPort = port;
        if (builder.Resource.Annotations.OfType<EndpointAnnotation>().FirstOrDefault(e => e.Name == "http") is { } http)
            http.Port = port;
        return builder;
    }

    /// <summary>
    /// WithRequest sets what the telemetry replica reserves on data-1: 500m and 1536Mi by default,
    /// at least 1Gi for its five programs.
    /// </summary>
    public static IResourceBuilder<ProtoStrapTelemetryResource> WithRequest(
        this IResourceBuilder<ProtoStrapTelemetryResource> builder, string cpu, string memory)
    {
        builder.Resource.Request = new(cpu, memory);
        return builder;
    }

    /// <summary>
    /// WithVolume sets the size of telemetry's volume on data-1, for Prometheus's metrics and
    /// Loki's and Tempo's write-ahead logs: 20Gi by default, at least 5Gi.
    /// </summary>
    public static IResourceBuilder<ProtoStrapTelemetryResource> WithVolume(
        this IResourceBuilder<ProtoStrapTelemetryResource> builder, string size)
    {
        builder.Resource.Volume = size;
        return builder;
    }

    /// <summary>
    /// WithRetention sets how long telemetry keeps each kind of record, in whole days: by default
    /// 30 days of logs and traces, and 15 of metrics. Those not given keep their default.
    /// </summary>
    public static IResourceBuilder<ProtoStrapTelemetryResource> WithRetention(
        this IResourceBuilder<ProtoStrapTelemetryResource> builder, TimeSpan? logs = null, TimeSpan? traces = null, TimeSpan? metrics = null)
    {
        builder.Resource.Logs = logs ?? builder.Resource.Logs;
        builder.Resource.Traces = traces ?? builder.Resource.Traces;
        builder.Resource.Metrics = metrics ?? builder.Resource.Metrics;
        return builder;
    }

    /// <summary>
    /// AddHostVariables gives every resource that takes environment variables each of the
    /// gateway's hosts' local hostname and origin under `aspire run`, as ProtoStrap gives every
    /// replica the account's.
    /// </summary>
    internal static void AddHostVariables(IEnumerable<IResource> resources, ProtoStrapGatewayResource gateway)
    {
        foreach (var resource in resources.OfType<IResourceWithEnvironment>().Where(r => r is IComputeResource && r != gateway).ToList())
        {
            resource.Annotations.Add(new EnvironmentCallbackAnnotation(context =>
            {
                var port = gateway.LocalPort.ToString(CultureInfo.InvariantCulture);
                foreach (var host in gateway.HostSettings)
                {
                    context.EnvironmentVariables[HostVariable("HOST_", host.Name)] = host.Local;
                    context.EnvironmentVariables[HostVariable("ORIGIN_", host.Name)] = $"http://{host.Local}:{port}";
                }
            }));
        }
    }

    private static string HostVariable(string prefix, string name) =>
        prefix + name.Replace('-', '_').ToUpper(CultureInfo.InvariantCulture);
}
