using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Every component but a task may have a readiness check, on its own port or another, or a
/// command run in it; a service also says what the gateway speaks to it
/// (docs/protostrap.md#readiness-checks).
/// </summary>
public class ReadinessTests
{
    [Theory]
    [InlineData("tcp", "tcp")]
    [InlineData("grpc-on", "grpc:9001")]
    [InlineData("tcp-on", "tcp:9001")]
    [InlineData("http-on", "9000/health/ready")]
    public async Task EveryCheckHasItsForm(string name, string want)
    {
        var readiness = name switch
        {
            "tcp" => Readiness.Tcp,
            "grpc-on" => Readiness.Grpc.On(9001),
            "tcp-on" => Readiness.Tcp.On(9001),
            _ => Readiness.Http("/health/ready").On(9000),
        };
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(readiness));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains($"\n  ready: {want}\n", api);
    }

    /// <summary>
    /// A health check on another endpoint, such as the management port Aspire's Keycloak
    /// integration checks, names that endpoint's port.
    /// </summary>
    [Fact]
    public async Task AHealthCheckOnAnotherEndpointNamesItsPort()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpEndpoint(targetPort: 9000, name: "management")
            .WithHttpHealthCheck("/health/ready", endpointName: "management")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi"));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("\n  port: 8080\n", api);
        Assert.Contains("\n  ready: 9000/health/ready\n", api);
    }

    [Fact]
    public async Task AnUpstreamSaysWhatTheGatewaySpeaksToIt()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway");
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready("/healthz"));
        var web = builder.AddProject("web", repo.Project("services/web/Web.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready("/healthz"));
        gateway.WithUpstream(api, u => u.Protocol(GatewayProtocol.Http2).Routes("/api/")).WithUpstream(web, u => u.Routes("/web/"));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  protocol: http2\n", SpecText.Upstream(yaml, "api"));
        Assert.DoesNotContain("protocol:", SpecText.Upstream(yaml, "web"));
        Assert.DoesNotContain("protocol:", SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task AWorkerAndAStatefulComponentMayHaveAChecks()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Http("/healthz").On(8081)));
        builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi")
                .Ready(Readiness.Command("pg_isready", "-U", "postgres")));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  ready: 8081/healthz\n", SpecText.Component(yaml, "worker"));
        Assert.Contains("\n  ready: [pg_isready, -U, postgres]\n", SpecText.Component(yaml, "postgres"));
    }

    [Fact]
    public async Task ChecksThatCantWorkAreReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Command("true")));
        var web = builder.AddProject("web", repo.Project("services/web/Web.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc.On(9001)));
        gateway.WithUpstream(api, u => u.Routes("/api/")).WithUpstream(web, u => u.Routes("/web/"));
        builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Http("/healthz")));
        builder.AddProject("other", repo.Project("services/other/Other.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Command()));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: Ready: the gateway checks its upstreams too, and can't run a command: use Readiness.Grpc, Readiness.Tcp or an HTTP path",
            "other: Ready: a command can't be empty",
            "web: Ready: the gateway checks gRPC over its HTTP/2 connections to the service: set Protocol(GatewayProtocol.Http2) or Protocol(GatewayProtocol.Grpc) on its upstream",
            "worker: Ready: a worker has no port, so its check names one, such as Readiness.Http(\"/healthz\").On(8081)",
        ], problems.Order(StringComparer.Ordinal));
    }

    [Fact]
    public void APortMustBeAPort()
    {
        Assert.Throws<ArgumentOutOfRangeException>(() => Readiness.Grpc.On(0));
        Assert.Throws<ArgumentOutOfRangeException>(() => Readiness.Tcp.On(70000));
        Assert.Throws<InvalidOperationException>(() => Readiness.Command("x").On(9000));
    }
}
