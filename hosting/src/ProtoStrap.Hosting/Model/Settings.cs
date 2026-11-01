using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;

namespace ProtoStrap.Hosting;

// These types hold what the environment and the PublishAsProtoStrap* callbacks were given, as
// given: the generator checks them and writes them the spec's way.

internal sealed record ResourceSettings(string Cpu, string Memory);

internal sealed record HostSettings(string Name, string Hostname, string Local);

internal sealed record IngressPoolSettings(string Vm, string? ReservedCpu, string? ReservedMemory);

internal sealed record ScalingPoolSettings(IReadOnlyList<string> Vm, int Min, int Max, int? Spare, double? Target, string? ReservedCpu, string? ReservedMemory);

internal sealed record DataPoolSettings(string Vm, int Count, string? ReservedCpu, string? ReservedMemory);

internal enum ComponentKind
{
    Service,
    Worker,
    Stateful,
    Task,
}

internal sealed class ComponentSettings(ComponentKind kind)
{
    public ComponentKind Kind { get; } = kind;
    public Pool? Pool { get; set; }
    public string? Vm { get; set; }
    public ResourceSettings? Request { get; set; }
    public Readiness? Ready { get; set; }
    public MetricsSettings? Metrics { get; set; }
    public TimeSpan? Stop { get; set; }
    public ScaleSettings? Scale { get; set; }
    public VolumeSettings? Volume { get; set; }
    public IReadOnlyList<string>? Backup { get; set; }
    public IReadOnlyList<string>? Command { get; set; }
    public bool SecretFiles { get; set; }
    public SecretChange? OnSecretChange { get; set; }
    public string? SecretPrefix { get; set; }
    public ConfigChange? OnConfigChange { get; set; }
}

/// <summary>UpstreamSettings are what WithUpstream gave the gateway for a resource.</summary>
internal sealed class UpstreamSettings(IResource resource)
{
    public IResource Resource { get; } = resource;
    public List<RouteSettings> Routes { get; } = [];
    public GatewayProtocol? Protocol { get; set; }
    public AuthorizeSettings? Authorize { get; set; }
}

internal sealed record RouteSettings(string Route, double? Rate);

internal sealed record AuthorizeSettings(
    IResource By, string? Path, IReadOnlyList<string> Headers, IReadOnlyList<string> Forward, IReadOnlyList<string> Respond,
    bool Open, TimeSpan? Timeout, IReadOnlyList<string> Skip);

internal sealed record MetricsSettings(int Port, string Path);

internal sealed record VolumeSettings(string Size, string? Path, int? Iops, int? Throughput);

internal sealed record ScaleSettings(int Min, int Max, IReadOnlyList<SignalSettings> Demand, IReadOnlyList<LimitSettings> Limits)
{
    public static ScaleSettings Create(int min, int max, Action<ProtoStrapDemand>? demand, Action<ProtoStrapLimits>? limits)
    {
        var signals = new ProtoStrapDemand();
        demand?.Invoke(signals);
        var caps = new ProtoStrapLimits();
        limits?.Invoke(caps);
        return new(min, max, signals.Signals, caps.Limits);
    }
}

internal abstract record SignalSettings;

internal sealed record CpuSignal(double Target) : SignalSettings;

internal sealed record RequestsSignal(double PerSecond) : SignalSettings;

internal sealed record QueueSignal(ProtoStrapQueueResource Queue, double Rate, TimeSpan Drain) : SignalSettings;

internal sealed record MetricSignal(string Name, double Target) : SignalSettings;

internal sealed record LimitSettings(int? Replicas, string? Metric);
