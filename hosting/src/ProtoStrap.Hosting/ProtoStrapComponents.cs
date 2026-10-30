using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting;

namespace Aspire.Hosting;

/// <summary>Pool is the scaling pool that a service, worker or task runs in.</summary>
public enum Pool
{
    /// <summary>General is the pool of general VMs, where services run.</summary>
    General,

    /// <summary>Compute is the pool of compute VMs, on Spot and sized from zero, where workers run.</summary>
    Compute,
}

/// <summary>
/// Readiness is how ProtoStrap knows a replica can serve: a gRPC, TCP or HTTP check on the
/// component's port, or on another with On, or a command run in the replica
/// (docs/protostrap.md#readiness-checks).
/// </summary>
public sealed class Readiness
{
    private Readiness(string kind, int? port, string? path, IReadOnlyList<string>? command)
    {
        Kind = kind;
        Port = port;
        Path = path;
        CommandLine = command;
    }

    internal string Kind { get; }
    internal int? Port { get; }
    internal string? Path { get; }
    internal IReadOnlyList<string>? CommandLine { get; }

    /// <summary>Value is the check as the spec writes it, such as "grpc:9001" or "9000/healthz"; null for a command.</summary>
    internal string? Value => Kind switch
    {
        "command" => null,
        "http" => Port is { } port ? port.ToString(System.Globalization.CultureInfo.InvariantCulture) + Path : Path,
        _ => Port is { } port ? Kind + ":" + port.ToString(System.Globalization.CultureInfo.InvariantCulture) : Kind,
    };

    /// <summary>Grpc is the gRPC health checking protocol, on the component's port.</summary>
    public static Readiness Grpc { get; } = new("grpc", null, null, null);

    /// <summary>Tcp passes once the component's port accepts a connection.</summary>
    public static Readiness Tcp { get; } = new("tcp", null, null, null);

    /// <summary>Http is an HTTP GET of the path, such as /healthz, which must answer 2xx or 3xx.</summary>
    public static Readiness Http(string path) => new("http", null, path, null);

    /// <summary>
    /// Command runs in the replica, and passes when it exits 0, such as pg_isready. The edge
    /// can't run it, so a service with routes can't use one.
    /// </summary>
    public static Readiness Command(params string[] command) => new("command", null, null, [.. command]);

    /// <summary>On runs the check on another port, such as a management port.</summary>
    public Readiness On(int port)
    {
        if (Kind == "command")
            throw new InvalidOperationException("a command runs in the replica, on no port");
        ArgumentOutOfRangeException.ThrowIfLessThan(port, 1);
        ArgumentOutOfRangeException.ThrowIfGreaterThan(port, 65535);
        return new(Kind, port, Path, null);
    }
}

/// <summary>GatewayProtocol is what the gateway speaks to a service.</summary>
public enum GatewayProtocol
{
    /// <summary>HTTP/1.1.</summary>
    Http1,

    /// <summary>Cleartext HTTP/2.</summary>
    Http2,

    /// <summary>Cleartext HTTP/2, with browsers' gRPC-Web translated on the service's routes.</summary>
    Grpc,
}

/// <summary>
/// SecretChange is what a new value of a component's keys does to its replicas, once the agent
/// has written the new secret files. Without one, the replicas are replaced.
/// </summary>
public sealed class SecretChange
{
    private SecretChange(string? mode, IReadOnlyList<string>? command) => (Mode, Command) = (mode, command);

    internal string? Mode { get; }

    internal IReadOnlyList<string>? Command { get; }

    /// <summary>Reload rewrites the files in place, all at once, and the replica reads them again itself.</summary>
    public static SecretChange Reload { get; } = new("reload", null);

    /// <summary>
    /// Run rewrites the files, then runs the command in the replica, which keeps running. The
    /// command must be safe to run twice; the previous values are in /run/secrets/previous.
    /// </summary>
    public static SecretChange Run(params string[] command) => new(null, [.. command]);
}

/// <summary>
/// ConfigChange is what new config files do to a component's replicas, once the agent has
/// swapped them in (docs/config.md#taking-new-files). Without one, the replicas are replaced.
/// </summary>
public sealed class ConfigChange
{
    private ConfigChange(string? mode, IReadOnlyList<string>? command) => (Mode, Command) = (mode, command);

    internal string? Mode { get; }

    internal IReadOnlyList<string>? Command { get; }

    /// <summary>Reload swaps the new files in, and the replica reads them again itself, such as Envoy watching its directory.</summary>
    public static ConfigChange Reload { get; } = new("reload", null);

    /// <summary>
    /// Run swaps the new files in, then runs the command in the replica, which keeps running. A
    /// command that checks the files before it reloads them fails a release whose files are bad.
    /// </summary>
    public static ConfigChange Run(params string[] command) => new(null, [.. command]);
}

/// <summary>ProtoStrapService configures a service, which gets requests from the gateway on its routes.</summary>
public sealed class ProtoStrapService
{
    internal ComponentSettings Settings { get; } = new(ComponentKind.Service);

    /// <summary>Pool sets the pool the service runs in, which defaults to general.</summary>
    public ProtoStrapService Pool(Pool pool) { Settings.Pool = pool; return this; }

    /// <summary>Request sets the CPU and memory one replica reserves, such as "500m" and "512Mi".</summary>
    public ProtoStrapService Request(string cpu, string memory) { Settings.Request = new(cpu, memory); return this; }

    /// <summary>
    /// Routes adds host and path prefixes the gateway sends to the service, such as
    /// "protofast.dev/api/". A path alone, such as "/api/", matches every host.
    /// </summary>
    public ProtoStrapService Routes(params string[] routes)
    {
        Settings.Routes.AddRange(routes.Select(r => new RouteSettings(r, null)));
        return this;
    }

    /// <summary>Route adds a route with a rate limit, in requests per second across every replica.</summary>
    public ProtoStrapService Route(string route, double rate) { Settings.Routes.Add(new(route, rate)); return this; }

    /// <summary>
    /// Authorize has the edge ask another service about each request to this one's routes
    /// before sending it on (docs/protostrap.md, "Authorizing requests"). The edge sends the
    /// request's method, path and headers, without its body, to the authorizer at path followed
    /// by the request's own path. A 2xx lets the request through with the authorizer's headers
    /// added, after removing the client's own copies of them; anything else is the response.
    /// </summary>
    /// <param name="by">It is the service that answers.</param>
    /// <param name="path">It is the prefix of the path the edge asks for, such as /auth/check; by default none.</param>
    /// <param name="headers">They are the authorizer's response headers added to the request, such as x-user.</param>
    /// <param name="forward">They are the request's headers sent to the authorizer besides Authorization, such as cookie.</param>
    public ProtoStrapService Authorize<T>(IResourceBuilder<T> by, string? path = null, string[]? headers = null, string[]? forward = null)
        where T : IResource
    {
        Settings.Authorize = new(by.Resource, path, headers ?? [], forward ?? []);
        return this;
    }

    /// <summary>
    /// Ready sets the service's readiness check, which defaults to the path of a readiness probe,
    /// otherwise the path of WithHttpHealthCheck, on the port of its endpoint.
    /// </summary>
    public ProtoStrapService Ready(Readiness readiness) { Settings.Ready = readiness; return this; }

    /// <summary>Ready sets the service's readiness check to an HTTP path, such as "/healthz".</summary>
    public ProtoStrapService Ready(string path) { Settings.Ready = Readiness.Http(path); return this; }

    /// <summary>
    /// Protocol sets what the gateway speaks to the service, which defaults to Grpc with
    /// Ready(Readiness.Grpc), otherwise Http1.
    /// </summary>
    public ProtoStrapService Protocol(GatewayProtocol protocol) { Settings.Protocol = protocol; return this; }

    /// <summary>Metrics sets the service's Prometheus endpoint, for metric signals and limits.</summary>
    public ProtoStrapService Metrics(int port, string path = "/metrics") { Settings.Metrics = new(port, path); return this; }

    /// <summary>Stop sets the time allowed between SIGTERM and SIGKILL, which defaults to 10 seconds.</summary>
    public ProtoStrapService Stop(TimeSpan stop) { Settings.Stop = stop; return this; }

    /// <summary>
    /// SecretPrefix sets the prefix of its own keys in the environment's secret, if not its
    /// name's, such as "Idp_" for a configure task that reads the keys of the component it
    /// configures.
    /// </summary>
    public ProtoStrapService SecretPrefix(string prefix) { Settings.SecretPrefix = prefix; return this; }

    /// <summary>
    /// OnConfigChange says what new config files from WithProtoStrapConfig do to its replicas;
    /// by default they are replaced.
    /// </summary>
    public ProtoStrapService OnConfigChange(ConfigChange onChange) { Settings.OnConfigChange = onChange; return this; }

    /// <summary>
    /// SecretFiles has the agent write the keys in the component's secrets to files in
    /// /run/secrets, such as /run/secrets/SMTP_PASSWORD, for an image that can't read the
    /// environment's secret itself. onChange says what a new value does; by default the
    /// replicas are replaced.
    /// </summary>
    public ProtoStrapService SecretFiles(SecretChange? onChange = null)
    {
        Settings.SecretFiles = true;
        Settings.OnSecretChange = onChange;
        return this;
    }

    /// <summary>
    /// Scale sets how many replicas the service runs: between min and max, as many as its demand
    /// signals ask for, capped by its limits. Without Scale it runs one.
    /// </summary>
    public ProtoStrapService Scale(int min, int max, Action<ProtoStrapDemand>? demand = null, Action<ProtoStrapLimits>? limits = null)
    {
        Settings.Scale = ScaleSettings.Create(min, max, demand, limits);
        return this;
    }
}

/// <summary>ProtoStrapWorker configures a worker, which takes jobs from a queue and takes no requests.</summary>
public sealed class ProtoStrapWorker
{
    internal ComponentSettings Settings { get; } = new(ComponentKind.Worker);

    /// <summary>
    /// Pool sets the pool the worker runs in, which defaults to general; workers usually run
    /// in compute.
    /// </summary>
    public ProtoStrapWorker Pool(Pool pool) { Settings.Pool = pool; return this; }

    /// <summary>Request sets the CPU and memory one replica reserves, such as "1000m" and "1Gi".</summary>
    public ProtoStrapWorker Request(string cpu, string memory) { Settings.Request = new(cpu, memory); return this; }

    /// <summary>
    /// Ready sets the worker's readiness check. A worker has no port, so the check names one with
    /// On, or is a command. Without one, the worker is ready once it runs.
    /// </summary>
    public ProtoStrapWorker Ready(Readiness readiness) { Settings.Ready = readiness; return this; }

    /// <summary>Metrics sets the worker's Prometheus endpoint, for metric signals and limits.</summary>
    public ProtoStrapWorker Metrics(int port, string path = "/metrics") { Settings.Metrics = new(port, path); return this; }

    /// <summary>Stop sets the time allowed between SIGTERM and SIGKILL, which defaults to 10 seconds.</summary>
    public ProtoStrapWorker Stop(TimeSpan stop) { Settings.Stop = stop; return this; }

    /// <summary>
    /// SecretPrefix sets the prefix of its own keys in the environment's secret, if not its
    /// name's, such as "Idp_" for a configure task that reads the keys of the component it
    /// configures.
    /// </summary>
    public ProtoStrapWorker SecretPrefix(string prefix) { Settings.SecretPrefix = prefix; return this; }

    /// <summary>
    /// OnConfigChange says what new config files from WithProtoStrapConfig do to its replicas;
    /// by default they are replaced.
    /// </summary>
    public ProtoStrapWorker OnConfigChange(ConfigChange onChange) { Settings.OnConfigChange = onChange; return this; }

    /// <summary>
    /// SecretFiles has the agent write the keys in the component's secrets to files in
    /// /run/secrets, such as /run/secrets/SMTP_PASSWORD, for an image that can't read the
    /// environment's secret itself. onChange says what a new value does; by default the
    /// replicas are replaced.
    /// </summary>
    public ProtoStrapWorker SecretFiles(SecretChange? onChange = null)
    {
        Settings.SecretFiles = true;
        Settings.OnSecretChange = onChange;
        return this;
    }

    /// <summary>
    /// Scale sets how many replicas the worker runs: between min and max, as many as its demand
    /// signals ask for, capped by its limits. Without Scale it runs one.
    /// </summary>
    public ProtoStrapWorker Scale(int min, int max, Action<ProtoStrapDemand>? demand = null, Action<ProtoStrapLimits>? limits = null)
    {
        Settings.Scale = ScaleSettings.Create(min, max, demand, limits);
        return this;
    }
}

/// <summary>
/// ProtoStrapStateful configures a stateful component: an upstream image pinned to a data VM,
/// with its own volume.
/// </summary>
public sealed class ProtoStrapStateful
{
    internal ComponentSettings Settings { get; } = new(ComponentKind.Stateful);

    /// <summary>Vm sets the data VM the component is pinned to, such as "data-1".</summary>
    public ProtoStrapStateful Vm(string vm) { Settings.Vm = vm; return this; }

    /// <summary>Request sets the CPU and memory the component reserves on its data VM.</summary>
    public ProtoStrapStateful Request(string cpu, string memory) { Settings.Request = new(cpu, memory); return this; }

    /// <summary>
    /// Volume sets the component's EBS volume: the size, such as "100Gi", and the path its data
    /// lives at in the container. The path defaults to WithDataVolume's, or what PostgreSQL and
    /// Redis use.
    /// </summary>
    public ProtoStrapStateful Volume(string size, string? path = null, int? iops = null, int? throughput = null)
    {
        Settings.Volume = new(size, path, iops, throughput);
        return this;
    }

    /// <summary>
    /// Backup sets the command whose output is the daily backup. PostgreSQL and Redis have one
    /// already.
    /// </summary>
    public ProtoStrapStateful Backup(params string[] command) { Settings.Backup = [.. command]; return this; }

    /// <summary>Command replaces the image's command, and the AppHost's arguments.</summary>
    public ProtoStrapStateful Command(params string[] command) { Settings.Command = [.. command]; return this; }

    /// <summary>
    /// Ready sets the component's readiness check, such as Readiness.Command("pg_isready").
    /// Without one, it is ready once its port accepts a connection.
    /// </summary>
    public ProtoStrapStateful Ready(Readiness readiness) { Settings.Ready = readiness; return this; }

    /// <summary>Metrics sets the component's Prometheus endpoint.</summary>
    public ProtoStrapStateful Metrics(int port, string path = "/metrics") { Settings.Metrics = new(port, path); return this; }

    /// <summary>Stop sets the time allowed between SIGTERM and SIGKILL, which defaults to 10 seconds.</summary>
    public ProtoStrapStateful Stop(TimeSpan stop) { Settings.Stop = stop; return this; }

    /// <summary>
    /// SecretPrefix sets the prefix of its own keys in the environment's secret, if not its
    /// name's, such as "Idp_" for a configure task that reads the keys of the component it
    /// configures.
    /// </summary>
    public ProtoStrapStateful SecretPrefix(string prefix) { Settings.SecretPrefix = prefix; return this; }

    /// <summary>
    /// OnConfigChange says what new config files from WithProtoStrapConfig do to its replicas;
    /// by default they are replaced.
    /// </summary>
    public ProtoStrapStateful OnConfigChange(ConfigChange onChange) { Settings.OnConfigChange = onChange; return this; }

    /// <summary>
    /// SecretFiles has the agent write the keys in the component's secrets to files in
    /// /run/secrets, such as /run/secrets/SMTP_PASSWORD, for an image that can't read the
    /// environment's secret itself. onChange says what a new value does; by default the
    /// replicas are replaced.
    /// </summary>
    public ProtoStrapStateful SecretFiles(SecretChange? onChange = null)
    {
        Settings.SecretFiles = true;
        Settings.OnSecretChange = onChange;
        return this;
    }
}

/// <summary>
/// ProtoStrapTask configures a task, which runs to completion once in each release in which its
/// image is new, before any new replica of a component that waits for it with WaitForCompletion.
/// </summary>
public sealed class ProtoStrapTask
{
    internal ComponentSettings Settings { get; } = new(ComponentKind.Task);

    /// <summary>Pool sets the pool the task runs in, which defaults to general.</summary>
    public ProtoStrapTask Pool(Pool pool) { Settings.Pool = pool; return this; }

    /// <summary>Request sets the CPU and memory the task reserves while it runs.</summary>
    public ProtoStrapTask Request(string cpu, string memory) { Settings.Request = new(cpu, memory); return this; }

    /// <summary>Stop sets the time allowed between SIGTERM and SIGKILL, which defaults to 10 seconds.</summary>
    public ProtoStrapTask Stop(TimeSpan stop) { Settings.Stop = stop; return this; }

    /// <summary>
    /// SecretPrefix sets the prefix of its own keys in the environment's secret, if not its
    /// name's, such as "Idp_" for a configure task that reads the keys of the component it
    /// configures.
    /// </summary>
    public ProtoStrapTask SecretPrefix(string prefix) { Settings.SecretPrefix = prefix; return this; }

    /// <summary>
    /// SecretFiles has the agent write the keys in the task's secrets to files in /run/secrets,
    /// such as /run/secrets/ADMIN_CLIENT_SECRET, for an image that can't read the environment's
    /// secret itself. A task reads them each time it runs.
    /// </summary>
    public ProtoStrapTask SecretFiles() { Settings.SecretFiles = true; return this; }
}

/// <summary>ProtoStrapDemand holds a component's demand signals, the highest of which is its demand.</summary>
public sealed class ProtoStrapDemand
{
    internal List<SignalSettings> Signals { get; } = [];

    /// <summary>
    /// Cpu adds a signal that keeps each replica's CPU near target, a share of its request
    /// such as 0.70.
    /// </summary>
    public ProtoStrapDemand Cpu(double target) { Signals.Add(new CpuSignal(target)); return this; }

    /// <summary>
    /// Requests adds a signal for the requests per second one replica handles, which the
    /// gateway counts.
    /// </summary>
    public ProtoStrapDemand Requests(double perSecond) { Signals.Add(new RequestsSignal(perSecond)); return this; }

    /// <summary>
    /// Queue adds a signal that clears the queue within drain, when one replica handles rate jobs
    /// per second. The component may receive from the queue, so it needn't also reference it.
    /// </summary>
    public ProtoStrapDemand Queue(IResourceBuilder<ProtoStrapQueueResource> queue, double rate, TimeSpan drain)
    {
        Signals.Add(new QueueSignal(queue.Resource, rate, drain));
        return this;
    }

    /// <summary>
    /// Metric adds a signal from a gauge the replicas export, of which one replica should
    /// carry target.
    /// </summary>
    public ProtoStrapDemand Metric(string name, double target) { Signals.Add(new MetricSignal(name, target)); return this; }
}

/// <summary>
/// ProtoStrapLimits holds a component's limits: caps on its replicas, of which the lowest applies.
/// </summary>
public sealed class ProtoStrapLimits
{
    internal List<LimitSettings> Limits { get; } = [];

    /// <summary>Replicas adds a fixed cap.</summary>
    public ProtoStrapLimits Replicas(int replicas) { Limits.Add(new(replicas, null)); return this; }

    /// <summary>Metric adds a cap from a gauge some replica exports: a replica count, or -1 for no cap.</summary>
    public ProtoStrapLimits Metric(string name) { Limits.Add(new(null, name)); return this; }
}
