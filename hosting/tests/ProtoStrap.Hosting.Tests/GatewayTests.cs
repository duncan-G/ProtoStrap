using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// ProtoStrap's own components are resources the AppHost adds to the environment, each runs only
/// when it is added, and each is configured where it is added: the tunnel, which brings requests
/// in from Cloudflare; the gateway, which has the hosts and sends their requests to its
/// upstreams; and telemetry (docs/protostrap-aspire.md, "The tunnel, the gateway and telemetry").
/// </summary>
public class GatewayTests
{
    private static IResourceBuilder<ProjectResource> AddApi(IDistributedApplicationBuilder builder, FixtureRepo repo) =>
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

    private static IResourceBuilder<ProjectResource> AddWeb(IDistributedApplicationBuilder builder, FixtureRepo repo, string name = "web", int port = 4000) =>
        builder.AddProject(name, repo.Project($"services/{name}/{name}.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: port, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));

    [Fact]
    public async Task EachIsASectionOfTheSpec()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment();
        var gateway = protostrap.AddEnvoyGateway("gateway")
            .WithHost("app", "protofast.dev", local: "app.dev.localhost")
            .WithHost("admin", "admin.protofast.dev", local: "admin.dev.localhost")
            .WithRequest(cpu: "400m", memory: "320Mi");
        protostrap.AddCloudflareTunnel("tunnel", gateway);
        protostrap.AddTelemetry("telemetry");
        var api = AddApi(builder, repo);
        var web = AddWeb(builder, repo);
        gateway.WithUpstream(web, u => u.Routes("app/", "admin/")).WithUpstream(api, u => u.Route("app/api/", rate: 50));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("tunnel: {}\n", SpecText.Section(yaml, "tunnel"));
        Assert.Equal("""
            gateway:
              request: { cpu: 400m, memory: 320Mi }
              hosts:
                admin: admin.protofast.dev
                app: protofast.dev
              upstreams:
                api:
                  routes: [{ route: app/api/, rate: 50/s }]
                web:
                  routes: [app/, admin/]

            """, SpecText.Section(yaml, "gateway"));
        Assert.Equal("telemetry: {}\n", SpecText.Section(yaml, "telemetry"));
        // The sections come after the pools, in this order, and the components carry no routes.
        Assert.True(yaml.IndexOf("\npools:", StringComparison.Ordinal) < yaml.IndexOf("\ntunnel:", StringComparison.Ordinal)
            && yaml.IndexOf("\ntunnel:", StringComparison.Ordinal) < yaml.IndexOf("\ngateway:", StringComparison.Ordinal)
            && yaml.IndexOf("\ngateway:", StringComparison.Ordinal) < yaml.IndexOf("\ntelemetry:", StringComparison.Ordinal)
            && yaml.IndexOf("\ntelemetry:", StringComparison.Ordinal) < yaml.IndexOf("\ncomponents:", StringComparison.Ordinal), yaml);
        Assert.DoesNotContain("routes", SpecText.Component(yaml, "api"));
        Assert.DoesNotContain("routes", SpecText.Component(yaml, "web"));
    }

    [Fact]
    public async Task WithoutThemTheSpecHasNone()
    {
        // An application that adds none runs none: no edge, no tunnel, no telemetry.
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        foreach (var section in new[] { "tunnel:", "gateway:", "telemetry:", "edge:", "hosts:" })
            Assert.DoesNotContain("\n" + section, yaml);
    }

    [Fact]
    public async Task TheGatewayDefaultsToProtoStrapsRequest()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().AddEnvoyGateway("gateway");
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            gateway:
              request: { cpu: 500m, memory: 256Mi }

            """, SpecText.Section(yaml, "gateway"));
    }

    [Fact]
    public async Task TelemetryIsSizedWhereItIsAdded()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().AddTelemetry("telemetry")
            .WithRequest(cpu: "250m", memory: "1Gi")
            .WithVolume("30Gi")
            .WithRetention(logs: TimeSpan.FromDays(7), traces: TimeSpan.FromDays(3), metrics: TimeSpan.FromDays(30));
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            telemetry:
              request: { cpu: 250m, memory: 1Gi }
              volume: 30Gi
              retention: { logs: 7d, traces: 3d, metrics: 30d }

            """, SpecText.Section(yaml, "telemetry"));
    }

    [Fact]
    public async Task TelemetryWritesOnlyWhatIsSet()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().AddTelemetry("telemetry").WithRetention(logs: TimeSpan.FromDays(14));
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            telemetry:
              retention: { logs: 14d }

            """, SpecText.Section(yaml, "telemetry"));
    }

    [Fact]
    public async Task AnUpstreamSaysHowTheGatewayReachesIt()
    {
        // Its protocol, routes and authorizer, in the spec's order; registering it again adds to
        // what it has.
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var auth = AddWeb(builder, repo, "auth", 8080);
        var api = AddApi(builder, repo);
        gateway.WithUpstream(api, u => u.Protocol(GatewayProtocol.Http2).Routes("app/api/"))
            .WithUpstream(api, u => u.Routes("/v2/").Authorize(auth, headers: ["x-user"]));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            gateway:
              request: { cpu: 500m, memory: 256Mi }
              hosts:
                app: protofast.dev
              upstreams:
                api:
                  protocol: http2
                  routes: [app/api/, /v2/]
                  authorize: { by: auth, headers: [x-user] }

            """, SpecText.Section(yaml, "gateway"));
        // An authorizer needn't be an upstream: the gateway asks it on its own.
        Assert.DoesNotContain("\n    auth:", yaml);
    }

    [Fact]
    public async Task WhatTheGatewayCantSendToIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var worker = builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));
        var redis = builder.AddRedis("redis").PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "250m", memory: "512Mi").Volume(size: "5Gi"));
        var smtp = builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3.8").ExcludeFromManifest();
        var plain = builder.AddContainer("plain", "nginx", "1.27").WithHttpEndpoint(targetPort: 80);
        var api = AddApi(builder, repo);
        var web = AddWeb(builder, repo);
        gateway.WithUpstream(worker, u => u.Routes("app/w/"))
            .WithUpstream(redis, u => u.Routes("app/r/"))
            .WithUpstream(smtp, u => u.Routes("app/mail/"))
            .WithUpstream(plain, u => u.Routes("app/plain/"))
            .WithUpstream(api, _ => { })
            .WithUpstream(web, u => u.Routes("other/", "protofast.dev/x/", ""));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("worker: WithUpstream: worker is a worker: only services get requests from the gateway", problems);
        Assert.Contains("redis: WithUpstream: redis is a stateful component: only services get requests from the gateway", problems);
        Assert.Contains("smtp4dev: WithUpstream: smtp4dev is excluded from publish", problems);
        Assert.Contains("plain: WithUpstream: plain isn't a ProtoStrap component: call PublishAsProtoStrapService(...)", problems);
        Assert.Contains("api: WithUpstream: no routes: call Routes(...) or Route(...)", problems);
        Assert.Contains("web: Routes: no host other: call WithHost(\"other\", ...) on gateway", problems);
        Assert.Contains("web: Routes: a route names its host by its name, such as app/x/, not protofast.dev", problems);
        Assert.Contains("web: Routes: an empty route", problems);
    }

    [Fact]
    public async Task WhatTheSectionsCantBeIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment();
        protostrap.AddEnvoyGateway("gateway").WithRequest(cpu: "0", memory: "lots")
            .WithHost("Bad.Name", "protofast.dev", local: "x.localhost")
            .WithHost("app", "not a host", local: "app.localhost")
            .WithHost("www", "protofast.dev", local: "www.localhost");
        protostrap.AddTelemetry("telemetry").WithRequest(cpu: "500m", memory: "512Mi").WithVolume("2Gi")
            .WithRetention(logs: TimeSpan.FromHours(36), traces: TimeSpan.Zero);
        AddApi(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("gateway: WithRequest: cpu must be above 0", problems);
        Assert.Contains("gateway: WithRequest: memory \"lots\" isn't memory, like \"512Mi\" or \"1Gi\"", problems);
        Assert.Contains("gateway: WithHost: \"Bad.Name\" isn't a host's name: lowercase letters, digits and dashes", problems);
        Assert.Contains("gateway: WithHost: \"not a host\" isn't a host name", problems);
        Assert.Contains("gateway: WithHost: www has protofast.dev too, which Bad.Name has: each host has a hostname of its own", problems);
        Assert.Contains("telemetry: WithRequest: 512Mi is too little memory for its five programs: at least 1Gi", problems);
        Assert.Contains("telemetry: WithVolume: 2Gi is too small: at least 5Gi", problems);
        Assert.Contains("telemetry: WithRetention: logs must be a whole number of days, at least 1 day", problems);
        Assert.Contains("telemetry: WithRetention: traces must be a whole number of days, at least 1 day", problems);
    }

    [Fact]
    public async Task WhatOnlyAnUpstreamCanHaveIsReported()
    {
        // The gateway counts requests and checks readiness only for its upstreams.
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc).Scale(min: 1, max: 4, d => d.Requests(20)));
        var web = builder.AddProject("web", repo.Project("services/web/Web.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 4000, name: "http")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Command("test", "-f", "/tmp/ready")));
        var grpc = builder.AddProject("grpc", repo.Project("services/grpc/Grpc.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Grpc));
        gateway.WithUpstream(web, u => u.Routes("app/")).WithUpstream(grpc, u => u.Protocol(GatewayProtocol.Http1).Routes("app/grpc/"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("api: Scale: a requests signal is counted by the gateway, so only one of its upstreams has one", problems);
        Assert.Contains("web: Ready: the gateway checks its upstreams too, and can't run a command: use Readiness.Grpc, Readiness.Tcp or an HTTP path", problems);
        Assert.Contains("grpc: Ready: the gateway checks gRPC over its HTTP/2 connections to the service: set Protocol(GatewayProtocol.Http2) or Protocol(GatewayProtocol.Grpc) on its upstream", problems);
    }

    [Fact]
    public void EachIsAddedOnce()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment();
        var gateway = protostrap.AddEnvoyGateway("gateway");
        protostrap.AddCloudflareTunnel("tunnel", gateway);
        protostrap.AddTelemetry("telemetry");

        Assert.Contains("already has a gateway, gateway", Assert.Throws<InvalidOperationException>(() => protostrap.AddEnvoyGateway("gateway2")).Message);
        Assert.Contains("already has a tunnel, tunnel", Assert.Throws<InvalidOperationException>(() => protostrap.AddCloudflareTunnel("tunnel2", gateway)).Message);
        Assert.Contains("already has telemetry, telemetry", Assert.Throws<InvalidOperationException>(() => protostrap.AddTelemetry("telemetry2")).Message);

        // A tunnel sends requests to its own environment's gateway.
        var other = TestApp.Publish(repo).AddProtoStrapEnvironment("other", app: "other").AddEnvoyGateway("gateway");
        var fresh = TestApp.Publish(repo).AddEnvironment();
        Assert.Contains("isn't this environment's", Assert.Throws<InvalidOperationException>(() => fresh.AddCloudflareTunnel("tunnel", other)).Message);
    }

    [Fact]
    public void EachIsTheEnvironments()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment();
        var gateway = protostrap.AddEnvoyGateway("gateway");
        var tunnel = protostrap.AddCloudflareTunnel("tunnel", gateway);
        var telemetry = protostrap.AddTelemetry("telemetry");

        Assert.Same(gateway.Resource, protostrap.Resource.Gateway);
        Assert.Same(tunnel.Resource, protostrap.Resource.Tunnel);
        Assert.Same(telemetry.Resource, protostrap.Resource.Telemetry);
        Assert.Same(protostrap.Resource, gateway.Resource.Environment);
        Assert.Same(protostrap.Resource, tunnel.Resource.Parent);
        Assert.Same(protostrap.Resource, telemetry.Resource.Parent);
        Assert.Same(gateway.Resource, tunnel.Resource.Gateway);
        // None is a component of its own: the spec has a section for each.
        Assert.DoesNotContain(builder.Resources, r => r is ProtoStrapGatewayResource or ProtoStrapTunnelResource or ProtoStrapTelemetryResource);
    }

    [Fact]
    public void RunModeRunsTheGatewayAloneAsTheLocalEnvoy()
    {
        // Under `aspire run` the gateway is the Envoy an account runs, in a container; Cloudflare
        // and telemetry stay in the account, and the dashboard shows the services' telemetry.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment();
        var gateway = protostrap.AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        protostrap.AddCloudflareTunnel("tunnel", gateway);
        protostrap.AddTelemetry("telemetry");

        var container = Assert.Single(builder.Resources.OfType<ProtoStrapGatewayResource>());
        Assert.Same(gateway.Resource, container);
        Assert.Equal("gateway", container.Name);
        var image = Assert.Single(container.Annotations.OfType<ContainerImageAnnotation>());
        Assert.Equal(("envoyproxy/envoy", "v1.40.2"), (image.Image, image.Tag));
        var http = Assert.Single(container.Annotations.OfType<EndpointAnnotation>());
        Assert.Equal((20000, 10000, false), (http.Port, http.TargetPort, http.IsProxied));
        Assert.DoesNotContain(builder.Resources, r => r is ProtoStrapTunnelResource or ProtoStrapTelemetryResource);
    }
}
