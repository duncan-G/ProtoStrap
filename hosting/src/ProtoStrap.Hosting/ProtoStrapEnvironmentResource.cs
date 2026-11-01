using ProtoStrap.Hosting;
using ProtoStrap.Hosting.Sources;

namespace Aspire.Hosting.ApplicationModel;

/// <summary>
/// ProtoStrapEnvironmentResource is the ProtoStrap environment: an Aspire compute environment
/// whose settings are the top of protostrap.yaml, and whose protostrap-spec pipeline step writes
/// the spec in publish mode. In run mode it isn't added to the model at all. ProtoStrap's own
/// components are added to it: its gateway, tunnel and telemetry, each a section of the spec.
/// </summary>
public sealed class ProtoStrapEnvironmentResource : Resource, IComputeEnvironmentResource
{
    /// <summary>
    /// This constructor creates the environment for the application called <paramref name="app"/>.
    /// </summary>
    public ProtoStrapEnvironmentResource(string name, string app)
        : base(name)
    {
        App = app;
    }

    /// <summary>App is the application's name: the spec's app.</summary>
    public string App { get; }

    /// <summary>
    /// Version is the ProtoStrap version the application runs, or null for
    /// ProtoStrap.Hosting's own.
    /// </summary>
    public string? Version { get; internal set; }

    /// <summary>Cdn is the CDN AddCloudflareCdn added, or null: then Cloudflare has ProtoStrap's settings alone.</summary>
    public ProtoStrapCdnResource? Cdn { get; internal set; }

    /// <summary>Gateway is the gateway AddEnvoyGateway added, or null: then nothing is routed.</summary>
    public ProtoStrapGatewayResource? Gateway { get; internal set; }

    /// <summary>Tunnel is the tunnel AddCloudflareTunnel added, or null: then nothing comes in from the internet.</summary>
    public ProtoStrapTunnelResource? Tunnel { get; internal set; }

    /// <summary>Telemetry is the telemetry AddTelemetry added, or null: then replicas log to CloudWatch.</summary>
    public ProtoStrapTelemetryResource? Telemetry { get; internal set; }

    internal IngressPoolSettings? IngressPool { get; set; }

    internal ScalingPoolSettings? GeneralPool { get; set; }

    internal ScalingPoolSettings? ComputePool { get; set; }

    internal DataPoolSettings? DataPool { get; set; }

    /// <summary>
    /// SpecPath is where protostrap.yaml goes, as given to WithSpecPath, or null for the
    /// repository's root.
    /// </summary>
    internal string? SpecPath { get; set; }

    internal string AppHostDirectory { get; init; } = "";

    /// <summary>SourceTracerFactory makes the source tracer for the spec's directory. Tests replace it.</summary>
    internal Func<string, ISourceTracer> SourceTracerFactory { get; set; } = root => new MSBuildSourceTracer(root);

    /// <summary>
    /// CheckCommand is the protostrap binary that checks the spec once it is written, or null
    /// not to check.
    /// </summary>
    internal string? CheckCommand { get; set; } = ProtoStrapBinary.DefaultCommand;

    /// <summary>Binary finds the protostrap binary, and builds it for the local gateway. Tests replace it.</summary>
    internal ProtoStrapBinary Binary { get; set; } = new();

    /// <summary>
    /// A component is reached by its name on the overlay network, from any VM.
    /// </summary>
    public ReferenceExpression GetHostAddressExpression(EndpointReference endpointReference) =>
        ReferenceExpression.Create($"{endpointReference.Resource.Name}");

    /// <summary>
    /// GetEndpointPropertyExpression gives an endpoint's properties once deployed: the component's
    /// name, and its target port, which is what it listens on in every replica. An endpoint
    /// without a target port gets 8080, where a project listens by default.
    /// </summary>
    public ReferenceExpression GetEndpointPropertyExpression(EndpointReferenceExpression endpointReferenceExpression)
    {
        var endpoint = endpointReferenceExpression.Endpoint;
        var host = endpoint.Resource.Name;
        var port = endpoint.EndpointAnnotation.TargetPort?.ToString(System.Globalization.CultureInfo.InvariantCulture) ?? "8080";
        var scheme = endpoint.EndpointAnnotation.UriScheme;
        var value = endpointReferenceExpression.Property switch
        {
            EndpointProperty.Host or EndpointProperty.IPV4Host => host,
            EndpointProperty.Port or EndpointProperty.TargetPort => port,
            EndpointProperty.HostAndPort => $"{host}:{port}",
            EndpointProperty.Scheme => scheme,
            EndpointProperty.TlsEnabled => endpoint.TlsEnabled ? "true" : "false",
            _ => $"{scheme}://{host}:{port}",
        };
        return ReferenceExpression.Create($"{value}");
    }
}
