using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Each compute resource says what kind of component it is with a PublishAsProtoStrap* method,
/// whose settings become its entry (docs/protostrap-aspire.md, "Component kinds").
/// </summary>
public class ComponentKindTests
{
    [Fact]
    public async Task AProjectServiceHasItsProjectSourcesAndSettings()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/src/ProtoFast.Api/ProtoFast.Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s
                .Request(cpu: "500m", memory: "512Mi")
                .Ready(Readiness.Grpc)
                .Stop(TimeSpan.FromSeconds(30))
                .Scale(min: 2, max: 10, demand => demand.Cpu(0.70)));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            api:
              kind: service
              project: services/api/src/ProtoFast.Api/ProtoFast.Api.csproj
              sources:
                - global.json
                - services/api/src/ProtoFast.Api
              pool: general
              request: { cpu: 500m, memory: 512Mi }
              port: 8080
              ready: grpc
              env:
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
              stop: 30s
              scale: { min: 2, max: 10, demand: [{ cpu: 70% }] }

            """, SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task EveryServiceSettingHasItsField()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .PublishAsProtoStrapService(s => s
                .Pool(Pool.Compute)
                .Request(cpu: "1", memory: "1Gi")
                .Ready("/ready")
                .Metrics(port: 9090, path: "/metrics")
                .Stop(TimeSpan.FromMinutes(2))
                .Scale(min: 1, max: 8,
                    demand => demand.Requests(20).Cpu(0.7).Metric("nodejs_eventloop_lag_seconds", target: 0.05),
                    limits => limits.Replicas(6).Metric("llm_budget_cap")));
        // A requests signal is for one of the gateway's upstreams.
        gateway.WithUpstream(api, u => u.Routes("app/api/", "/healthz/").Route("/py/", rate: 200));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            api:
              kind: service
              project: services/api/Api.csproj
              sources:
                - global.json
                - services/api
              pool: compute
              request: { cpu: 1000m, memory: 1Gi }
              port: 8080
              ready: /ready
              metrics: 9090/metrics
              env:
                ASPNETCORE_FORWARDEDHEADERS_ENABLED: "true"
                HTTP_PORTS: "8080"
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
              stop: 2m
              scale:
                min: 1
                max: 8
                demand: [{ requests: 20/s }, { cpu: 70% }, { metric: nodejs_eventloop_lag_seconds, target: 0.05 }]
                limits: [{ replicas: 6 }, { metric: llm_budget_cap }]

            """, SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task ThePortIsTheHttpEndpointsTargetPort()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 5000, name: "grpc")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("\n  port: 5000\n", api);
        Assert.Contains("\n    HTTP_PORTS: \"5000\"\n", api);
    }

    [Fact]
    public async Task ReadyComesFromTheHttpHealthCheck()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi"));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("\n  ready: /healthz\n", api);
    }

    [Fact]
    public async Task ReadyComesFromAReadinessProbe()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpProbe(ProbeType.Liveness, "/alive")
            .WithHttpProbe(ProbeType.Readiness, "/ready")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi"));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("\n  ready: /ready\n", api);
    }

    [Fact]
    public async Task ReadySetInTheCallbackWins()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("\n  ready: grpc\n", api);
    }

    [Fact]
    public async Task AWorkerHasNoPortOrReadiness()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("document-import", repo.Project("services/worker/Worker.csproj", "Microsoft.NET.Sdk.Worker"))
            .WithHttpEndpoint(name: "http")
            .PublishAsProtoStrapWorker(w => w
                .Pool(Pool.Compute)
                .Request(cpu: "1000m", memory: "1Gi")
                .Metrics(port: 9464)
                .Stop(TimeSpan.FromSeconds(120))
                .Scale(min: 0, max: 20, demand => demand.Cpu(0.8)));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            document-import:
              kind: worker
              project: services/worker/Worker.csproj
              sources:
                - global.json
                - services/worker
              pool: compute
              request: { cpu: 1000m, memory: 1Gi }
              metrics: 9464/metrics
              env:
                ASPNETCORE_FORWARDEDHEADERS_ENABLED: "true"
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
              stop: 2m
              scale: { min: 0, max: 20, demand: [{ cpu: 80% }] }

            """, SpecText.Component(yaml, "document-import"));
    }

    [Fact]
    public async Task AComponentWithoutScaleRunsOneReplica()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.DoesNotContain("scale", api);
    }

    [Fact]
    public async Task ATaskRunsInAPoolAndWaitingForItIsMigrate()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var migrations = builder.AddProject("protofast-db-migrations", repo.Project("services/api/src/ProtoFast.SchemaMigrations/ProtoFast.SchemaMigrations.csproj"))
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/src/ProtoFast.Api/ProtoFast.Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WaitForCompletion(migrations)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            protofast-db-migrations:
              kind: task
              project: services/api/src/ProtoFast.SchemaMigrations/ProtoFast.SchemaMigrations.csproj
              sources:
                - global.json
                - services/api/src/ProtoFast.SchemaMigrations
              pool: general
              request: { cpu: 250m, memory: 256Mi }
              env:
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory

            """, SpecText.Component(yaml, "protofast-db-migrations"));
        Assert.Contains("\n  migrate: protofast-db-migrations\n", SpecText.Component(yaml, "api"));
        Assert.DoesNotContain("depends_on", SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task WaitingForCompletionMustBeOfOneTask()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var one = builder.AddProject("one", repo.Project("services/one/One.csproj"))
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));
        var two = builder.AddProject("two", repo.Project("services/two/Two.csproj"))
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));
        var worker = builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WaitForCompletion(one)
            .WaitForCompletion(two)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        builder.AddProject("other", repo.Project("services/other/Other.csproj", "Microsoft.NET.Sdk.Web"))
            .WaitForCompletion(worker)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        builder.AddProject("three", repo.Project("services/three/Three.csproj"))
            .WaitForCompletion(one)
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: waits for the completion of one and two: ProtoStrap runs one task as a component's migrate",
            "other: waits for the completion of worker, which is a worker: only a task runs to completion",
            "three: waits for the completion of one: only services and workers run a task as migrate",
        ], problems);
    }

    [Fact]
    public void ACallbackRunsOnlyWhenItsSettingsAreRead()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();

        // Under `aspire run` the local gateway reads a service's settings, so the call is kept, but
        // nothing runs it until then, and sources mean nothing.
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(_ => throw new InvalidOperationException("ran in run mode"))
            .WithProtoStrapSources("protos/**");

        Assert.Single(api.Resource.Annotations, a => a.GetType().Name == "ProtoStrapComponentAnnotation");
        Assert.DoesNotContain(api.Resource.Annotations, a => a.GetType().Name == "ProtoStrapSourcesAnnotation");
    }

    [Fact]
    public async Task ACallbackCanThrowToReportAProblem()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(_ => throw new InvalidOperationException("no request yet"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["api: PublishAsProtoStrapService failed: no request yet"], problems);
    }

    [Fact]
    public async Task SettingsThatArentValidAreReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.dev.localhost");
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s
                .Request(cpu: "lots", memory: "0Mi")
                .Ready("healthz")
                .Metrics(port: 0)
                .Stop(TimeSpan.FromHours(2))
                .Scale(min: 3, max: 2, demand => demand.Cpu(1.5).Requests(0).Metric("bad name", target: 0)));
        builder.AddProject("worker", repo.Project("services/worker/Worker.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi")
                .Scale(min: -1, max: 0, demand => demand.Cpu(0.5), limits => limits.Replicas(-1).Metric("")));
        gateway.WithUpstream(api, u => u.Routes("auth/", "", "protofast.dev/api/"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: Request: cpu \"lots\" isn't CPU, like \"500m\" or \"2\"",
            "api: Request: memory must be above 0",
            "api: Ready: \"healthz\" must be Readiness.Grpc, Readiness.Tcp or an HTTP path such as \"/healthz\"",
            "api: Metrics: port 0 must be 1 to 65535",
            "api: Stop: 2h is more than 1h",
            "api: Scale: max 2 is below min 3",
            "api: Scale: cpu 1.5 must be above 0 and at most 1, such as 0.70",
            "api: Scale: requests must be above 0 per second",
            "api: Scale: \"bad name\" isn't a metric name",
            "api: Scale: the metric's target must be above 0",
            "worker: Scale: min -1 is negative",
            "worker: Scale: max 0 must be at least 1",
            "worker: Scale: a replicas limit of -1 is negative",
            "worker: Scale: \"\" isn't a metric name",
            "api: Routes: no host auth: call WithHost(\"auth\", ...) on gateway",
            "api: Routes: an empty route",
            "api: Routes: a route names its host by its name, such as app/api/, not protofast.dev",
        ], problems);
    }

    [Fact]
    public async Task MissingSettingsAreReportedWithTheCallToMake()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(_ => { });
        builder.AddProject("jobs", repo.Project("services/jobs/Jobs.csproj"))
            .PublishAsProtoStrapWorker(w => w.Scale(min: 0, max: 2, demand => demand.Requests(5)));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: no request: call Request(cpu: ..., memory: ...)",
            "api: no readiness check: call Ready(Readiness.Grpc) or Ready(\"/healthz\"), or WithHttpHealthCheck(\"/healthz\")",
            "jobs: no request: call Request(cpu: ..., memory: ...)",
            "jobs: Scale: a requests signal is counted by the gateway, so only one of its upstreams has one",
        ], problems);
    }

    [Fact]
    public async Task ComponentNamesFollowProtoStrapsRules()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("Api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        builder.AddProject("edge", repo.Project("services/edge/Edge.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        builder.AddProject("telemetry", repo.Project("services/telemetry/Telemetry.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));
        builder.AddProject("a-component-name-that-is-longer-than-forty", repo.Project("services/long/Long.csproj"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "Api: a component's name is lowercase letters, digits and dashes",
            "a-component-name-that-is-longer-than-forty: a component's name is at most 40 characters",
            "edge: \"edge\" is reserved for ProtoStrap",
            "telemetry: \"telemetry\" is reserved for ProtoStrap",
        ], problems);
    }

    [Fact]
    public async Task AComputeResourceWithoutAKindIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"));
        builder.AddContainer("cache", "redis", "8.6");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: no PublishAsProtoStrap* call: say whether it is a service, worker, stateful component or task",
            "cache: no PublishAsProtoStrap* call: say whether it is a service, worker, stateful component or task",
        ], problems);
    }

    [Fact]
    public async Task ResourcesExcludedFromPublishAreLeftOut()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        builder.AddContainer("smtp4dev", "rnwood/smtp4dev")
            .WithBindMount("../data", "/data")
            .ExcludeFromManifest();
        builder.AddExecutable("seed", "bash", "../scripts", "seed.sh").ExcludeFromManifest();

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.False(SpecText.HasComponent(yaml, "smtp4dev"));
        Assert.False(SpecText.HasComponent(yaml, "seed"));
    }

    [Fact]
    public async Task ComponentsAreSortedByName()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        foreach (var name in new[] { "zeta", "alpha", "mid" })
        {
            builder.AddProject(name, repo.Project($"services/{name}/{name}.csproj"))
                .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));
        }

        var yaml = await TestApp.GenerateYamlAsync(builder);

        var order = new[] { "alpha", "mid", "zeta" }.Select(n => yaml.IndexOf("\n  " + n + ":\n", StringComparison.Ordinal)).ToList();
        Assert.All(order, i => Assert.True(i > 0));
        Assert.Equal(order.Order().ToList(), order);
    }
}
