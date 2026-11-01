using System.Globalization;
using Aspire.Hosting.ApplicationModel;
using Aspire.Hosting.Pipelines;
using ProtoStrap.Hosting;
using ProtoStrap.Hosting.Spec;

namespace Aspire.Hosting;

/// <summary>
/// These extension methods add the ProtoStrap environment to an AppHost, and set what becomes
/// the top of protostrap.yaml: the app, the ProtoStrap version, the hosts, the pools and the edge.
/// </summary>
public static class ProtoStrapEnvironmentExtensions
{
    /// <summary>
    /// AddProtoStrapEnvironment adds the ProtoStrap environment. In publish mode it is the compute
    /// environment of every compute resource not assigned to another, and its protostrap-spec
    /// step, which `aspire publish` runs and `aspire do protostrap-spec` runs alone, writes
    /// protostrap.yaml. Under `aspire run` the environment is dev, and every process a component
    /// runs reads dev's secret, &lt;app&gt;/dev, as a replica reads its environment's
    /// (docs/secrets.md#dev).
    /// </summary>
    /// <param name="builder">It is the AppHost's builder.</param>
    /// <param name="name">It is the environment's resource name.</param>
    /// <param name="app">It is the application's name: the spec's app.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> AddProtoStrapEnvironment(
        this IDistributedApplicationBuilder builder, string name, string app)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var environment = new ProtoStrapEnvironmentResource(name, app) { AppHostDirectory = builder.AppHostDirectory };
        if (!builder.ExecutionContext.IsPublishMode)
        {
            var configuration = builder.Configuration;
            builder.Eventing.Subscribe<BeforeStartEvent>((e, _) =>
            {
                var profile = configuration["AWS_PROFILE"] is { Length: > 0 } named ? named : "developer";
                AddSecretVariables(e.Model.Resources, app + "/dev", profile, LocalStack.Region(configuration));
                return Task.CompletedTask;
            });
            return builder.CreateResourceBuilder(environment);
        }

        return builder.AddResource(environment)
            .ExcludeFromManifest()
            .WithPipelineStepFactory(_ => SpecStep.Create(environment));
    }

    /// <summary>
    /// WithVersion sets the ProtoStrap version the application runs, which defaults to
    /// ProtoStrap.Hosting's own.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithVersion(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string version)
    {
        builder.Resource.Version = version;
        return builder;
    }

    /// <summary>
    /// WithHost adds a host the application answers on (docs/protostrap.md, "Hosts"). Routes
    /// refer to it by its name, such as "admin/", so they mean the same in every account; an
    /// account's Terraform config may give the name another hostname. Every replica learns the
    /// hostname as HOST_&lt;NAME&gt; and its origin as ORIGIN_&lt;NAME&gt;, and so does every
    /// resource under `aspire run`, with the local hostname.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="name">It is the host's name: lowercase letters, digits and dashes, such as admin.</param>
    /// <param name="hostname">It is the spec's hostname, such as admin.example.com.</param>
    /// <param name="local">It is the hostname under `aspire run`, such as admin.dev.localhost.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithHost(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string name, string hostname, string local)
    {
        var first = builder.Resource.HostSettings.Count == 0;
        builder.Resource.HostSettings.Add(new(name, hostname, local));
        if (first && !builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
        {
            var environment = builder.Resource;
            builder.ApplicationBuilder.Eventing.Subscribe<BeforeStartEvent>((e, _) =>
            {
                AddHostVariables(e.Model.Resources, environment);
                return Task.CompletedTask;
            });
            if (environment.LocalEdge is null)
                LocalEdge.Add(builder.ApplicationBuilder, environment);
        }
        return builder;
    }

    /// <summary>
    /// AddSecretVariables gives every process a ProtoStrap component runs under `aspire run` the
    /// variables ProtoStrap gives each replica for its environment's secret: its id, the
    /// component's prefix (its secret_prefix, or its name in PascalCase and _), and the region,
    /// the AppHost's, which LocalStack uses too. AWS_PROFILE, the AppHost's or developer, the
    /// Developer permission set's, stands in for the replica's role. Containers have no sign-in,
    /// so they get none: an image that takes secret files in an account takes Aspire's
    /// parameters locally.
    /// </summary>
    internal static void AddSecretVariables(IEnumerable<IResource> resources, string secret, string profile, string region)
    {
        foreach (var resource in resources.Where(r => r is IResourceWithEnvironment and not ContainerResource).ToList())
        {
            if (!resource.TryGetLastAnnotation<ProtoStrapComponentAnnotation>(out var component))
                continue;
            var prefix = component.Configure().SecretPrefix ?? string.Concat(resource.Name.Split('-', StringSplitOptions.RemoveEmptyEntries)
                .Select(part => char.ToUpperInvariant(part[0]) + part[1..])) + "_";
            resource.Annotations.Add(new EnvironmentCallbackAnnotation(context =>
            {
                context.EnvironmentVariables["SECRET_ID"] = secret;
                context.EnvironmentVariables["SECRET_PREFIX"] = prefix;
                context.EnvironmentVariables["AWS_PROFILE"] = profile;
                context.EnvironmentVariables["AWS_REGION"] = region;
            }));
        }
    }

    /// <summary>
    /// WithLocalEdge sets the port the edge serves the hosts' local hostnames on under `aspire
    /// run`, 20000 by default: browsers open http://&lt;local hostname&gt;:&lt;port&gt;. The edge
    /// is the Envoy ProtoStrap runs in an account, rendered by `protostrap edge render`.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithLocalEdge(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, int port)
    {
        ArgumentOutOfRangeException.ThrowIfLessThan(port, 1);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(port, 65535);
        builder.Resource.LocalEdgePort = port;
        if (builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;
        if (builder.Resource.LocalEdge is { } edge)
            edge.Annotations.OfType<EndpointAnnotation>().Single(e => e.Name == "http").Port = port;
        else
            LocalEdge.Add(builder.ApplicationBuilder, builder.Resource);
        return builder;
    }

    /// <summary>
    /// AddHostVariables gives every resource that takes environment variables each host's
    /// local hostname and origin under `aspire run`, as ProtoStrap gives every replica the
    /// account's.
    /// </summary>
    internal static void AddHostVariables(IEnumerable<IResource> resources, ProtoStrapEnvironmentResource environment)
    {
        foreach (var resource in resources.OfType<IResourceWithEnvironment>().Where(r => r is IComputeResource && r != environment.LocalEdge).ToList())
        {
            resource.Annotations.Add(new EnvironmentCallbackAnnotation(context =>
            {
                var port = environment.LocalEdgePort.ToString(CultureInfo.InvariantCulture);
                foreach (var host in environment.HostSettings)
                {
                    context.EnvironmentVariables[HostVariable("HOST_", host.Name)] = host.Local;
                    context.EnvironmentVariables[HostVariable("ORIGIN_", host.Name)] = $"http://{host.Local}:{port}";
                }
            }));
        }
    }

    private static string HostVariable(string prefix, string name) =>
        prefix + name.Replace('-', '_').ToUpper(CultureInfo.InvariantCulture);

    /// <summary>
    /// WithIngressPool sets the ingress pool: one VM of this type per zone, running the edge and
    /// the zone's NAT.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="vm">It is the instance type, such as c8g.medium.</param>
    /// <param name="reservedCpu">It is the CPU each VM keeps back from replicas, 200m by default.</param>
    /// <param name="reservedMemory">It is the memory each VM keeps back from replicas, 512Mi by default.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithIngressPool(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string vm, string? reservedCpu = null, string? reservedMemory = null)
    {
        builder.Resource.IngressPool = new(vm, reservedCpu, reservedMemory);
        return builder;
    }

    /// <summary>
    /// WithGeneralPool sets the general pool, where services run: instance types of one shape,
    /// sized by the scheduler.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="vm">These are instance types of one shape. Sizing assumes the first.</param>
    /// <param name="min">It is the fewest VMs, at least 1.</param>
    /// <param name="max">It is the most VMs.</param>
    /// <param name="spare">It is how many VMs are kept free for new replicas, 1 by default.</param>
    /// <param name="target">It is the share of the pool's capacity requests fill, 0.70 by default.</param>
    /// <param name="reservedCpu">It is the CPU each VM keeps back from replicas, 400m by default.</param>
    /// <param name="reservedMemory">It is the memory each VM keeps back from replicas, 1Gi by default.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithGeneralPool(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string[] vm, int min, int max,
        int? spare = null, double? target = null, string? reservedCpu = null, string? reservedMemory = null)
    {
        builder.Resource.GeneralPool = new([.. vm], min, max, spare, target, reservedCpu, reservedMemory);
        return builder;
    }

    /// <summary>
    /// WithComputePool sets the compute pool, where workers run when general VMs have no room:
    /// instance types of one shape, on Spot, sized by the scheduler from zero.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="vm">These are instance types of one shape. Sizing assumes the first.</param>
    /// <param name="min">It is the fewest VMs, usually 0.</param>
    /// <param name="max">It is the most VMs.</param>
    /// <param name="spare">It is how many VMs are kept free for new replicas, 0 by default.</param>
    /// <param name="target">It is the share of the pool's capacity requests fill, 0.85 by default.</param>
    /// <param name="reservedCpu">It is the CPU each VM keeps back from replicas, 400m by default.</param>
    /// <param name="reservedMemory">It is the memory each VM keeps back from replicas, 1Gi by default.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithComputePool(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string[] vm, int min, int max,
        int? spare = null, double? target = null, string? reservedCpu = null, string? reservedMemory = null)
    {
        builder.Resource.ComputePool = new([.. vm], min, max, spare, target, reservedCpu, reservedMemory);
        return builder;
    }

    /// <summary>
    /// WithDataPool sets the data pool: a fixed count of VMs, data-1 to data-n, for stateful
    /// components.
    /// </summary>
    /// <param name="builder">It is the environment.</param>
    /// <param name="vm">It is the instance type, which must keep its data on EBS.</param>
    /// <param name="count">It is how many data VMs there are, 1 to 3.</param>
    /// <param name="reservedCpu">It is the CPU kept back from stateful components, 0 by default.</param>
    /// <param name="reservedMemory">It is the memory kept back from stateful components, 1Gi by default.</param>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithDataPool(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string vm, int count = 1, string? reservedCpu = null, string? reservedMemory = null)
    {
        builder.Resource.DataPool = new(vm, count, reservedCpu, reservedMemory);
        return builder;
    }

    /// <summary>
    /// WithEdge sets what the edge, cloudflared and Envoy on each ingress VM, reserves. The
    /// reservation must fit the ingress VM, and defaults to 500m and 256Mi.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithEdge(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string cpu, string memory)
    {
        builder.Resource.Edge = new(cpu, memory);
        return builder;
    }

    /// <summary>
    /// WithSpecPath sets where protostrap.yaml goes, relative to the AppHost's directory. The path
    /// defaults to the root of the git repository the AppHost is in. Paths in the spec are
    /// relative to the spec's directory, so everything a component is built from must be inside it.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithSpecPath(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string path)
    {
        builder.Resource.SpecPath = path;
        return builder;
    }

    /// <summary>
    /// WithCheck sets the protostrap binary that runs `protostrap check` on the spec once it is
    /// written: a name on PATH or a path. It defaults to protostrap, and the check is skipped if
    /// the binary isn't found. With null, the spec is never checked. The local edge renders with
    /// a binary this names; for the default, protostrap-build builds protostrap from the ProtoStrap
    /// checkout this package is built from, and only without the checkout or Go is protostrap on
    /// PATH used. Both warn when a binary they find isn't the AppHost's version.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> WithCheck(
        this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string? command)
    {
        builder.Resource.CheckCommand = command;
        return builder;
    }
}
