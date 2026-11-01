using ProtoStrap.Hosting;

namespace Aspire.Hosting.ApplicationModel;

/// <summary>
/// ProtoStrapGatewayResource is the gateway (docs/protostrap.md#gateway): Envoy on each
/// ingress VM, which answers the hosts and sends their requests to its upstreams. It is the
/// spec's gateway section. Under `aspire run` it is the same Envoy in a container, rendered by
/// `protostrap edge render`; in publish mode it is in no model, and the environment holds it.
/// </summary>
public sealed class ProtoStrapGatewayResource : ContainerResource
{
    /// <summary>This constructor creates the gateway of <paramref name="environment"/>.</summary>
    public ProtoStrapGatewayResource(string name, ProtoStrapEnvironmentResource environment)
        : base(name)
    {
        Environment = environment;
    }

    /// <summary>Environment is the ProtoStrap environment the gateway is part of.</summary>
    public ProtoStrapEnvironmentResource Environment { get; }

    /// <summary>
    /// Hosts are the application's hosts, by the name routes refer to them by, with their
    /// hostnames: the spec's gateway.hosts.
    /// </summary>
    public IReadOnlyDictionary<string, string> Hosts =>
        HostSettings.GroupBy(h => h.Name).ToDictionary(g => g.Key, g => g.Last().Hostname, StringComparer.Ordinal);

    /// <summary>HostSettings are the hosts as WithHost was given them, in order.</summary>
    internal List<HostSettings> HostSettings { get; } = [];

    /// <summary>Upstreams are the services the gateway sends requests to, in the order WithUpstream first named them.</summary>
    internal List<UpstreamSettings> Upstreams { get; } = [];

    /// <summary>Request is what the gateway's container reserves, the tunnel's included, as WithRequest gave it.</summary>
    internal ResourceSettings? Request { get; set; }

    /// <summary>LocalPort is the port the gateway serves the local hostnames on under `aspire run`.</summary>
    internal int LocalPort { get; set; } = LocalEdge.DefaultPort;

    /// <summary>LocalBuild is the protostrap the gateway renders with under `aspire run`, added with it.</summary>
    internal LocalBuild? LocalBuild { get; set; }

    /// <summary>Upstream is the upstream of a resource, made the first time it is asked for.</summary>
    internal UpstreamSettings Upstream(IResource resource)
    {
        if (Upstreams.FirstOrDefault(u => u.Resource == resource) is { } upstream)
            return upstream;
        upstream = new UpstreamSettings(resource);
        Upstreams.Add(upstream);
        return upstream;
    }

    /// <summary>UpstreamOf is a resource's upstream, or null if the gateway doesn't send it requests.</summary>
    internal UpstreamSettings? UpstreamOf(IResource resource) => Upstreams.FirstOrDefault(u => u.Resource == resource);
}

/// <summary>
/// ProtoStrapTunnelResource is the tunnel (docs/protostrap.md#tunnel): cloudflared, which brings
/// the requests for the gateway's hosts in from Cloudflare, running beside the gateway's Envoy.
/// It is the spec's tunnel section, and runs only in an account.
/// </summary>
public sealed class ProtoStrapTunnelResource(string name, ProtoStrapEnvironmentResource environment, ProtoStrapGatewayResource gateway)
    : Resource(name), IResourceWithParent<ProtoStrapEnvironmentResource>
{
    /// <summary>Parent is the ProtoStrap environment the tunnel is part of.</summary>
    public ProtoStrapEnvironmentResource Parent { get; } = environment;

    /// <summary>Gateway is the gateway the tunnel sends every request to.</summary>
    public ProtoStrapGatewayResource Gateway { get; } = gateway;
}

/// <summary>
/// ProtoStrapTelemetryResource is telemetry (docs/telemetry.md): ProtoStrap's collector, Loki,
/// Tempo, Prometheus and Grafana on data-1, which keep the application's logs, traces and
/// metrics. It is the spec's telemetry section, and runs only in an account: under `aspire run`
/// the dashboard has the services' telemetry.
/// </summary>
public sealed class ProtoStrapTelemetryResource(string name, ProtoStrapEnvironmentResource environment)
    : Resource(name), IResourceWithParent<ProtoStrapEnvironmentResource>
{
    /// <summary>Parent is the ProtoStrap environment telemetry is part of.</summary>
    public ProtoStrapEnvironmentResource Parent { get; } = environment;

    /// <summary>Request is what the replica reserves, as WithRequest gave it.</summary>
    internal ResourceSettings? Request { get; set; }

    /// <summary>Volume is its volume's size, as WithVolume gave it.</summary>
    internal string? Volume { get; set; }

    internal TimeSpan? Logs { get; set; }

    internal TimeSpan? Traces { get; set; }

    internal TimeSpan? Metrics { get; set; }
}
