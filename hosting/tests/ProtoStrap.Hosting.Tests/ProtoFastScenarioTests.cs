using System.Runtime.CompilerServices;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Sources;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// ProtoFastScenarioTests runs the AppHost from docs/protostrap-aspire.md over a repository
/// shaped like ProtoFast's, with sources traced by MSBuild. Its spec is Golden/protofast.yaml,
/// which internal/spec's tests also parse and check, so the C# writer and the Go reader agree on
/// the format.
/// </summary>
public class ProtoFastScenarioTests
{
    [Fact]
    public async Task TheDocsAppHostGeneratesTheGoldenSpec()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        DescribeProtoFast(repo, builder);

        var result = await TestApp.GenerateAsync(builder, new MSBuildSourceTracer(repo.Root));

        Assert.True(result.Problems.Count == 0, string.Join("\n", result.Problems));
        var golden = GoldenPath();
        if (Environment.GetEnvironmentVariable("PROTOSTRAP_UPDATE_GOLDEN") == "1")
            await File.WriteAllTextAsync(golden, result.Yaml, TestContext.Current.CancellationToken);
        Assert.Equal(await File.ReadAllTextAsync(golden, TestContext.Current.CancellationToken), result.Yaml);
    }

    /// <summary>
    /// WriteRepository writes ProtoFast's repository: its services, their shared libraries, and a
    /// test project nothing deploys.
    /// </summary>
    private static void WriteRepository(FixtureRepo repo)
    {
        repo.Write("nuget.config", "<configuration />\n");
        repo.Write("global.json", """{ "sdk": { "version": "10.0.100", "rollForward": "latestFeature" } }""");

        const string shared = "services/shared";
        repo.Project($"{shared}/Database.Abstractions/Database.Abstractions.csproj");
        repo.Project($"{shared}/Database/Database.csproj", references: $"{shared}/Database.Abstractions/Database.Abstractions.csproj");
        repo.Project($"{shared}/Exceptions/Exceptions.csproj");
        repo.Project($"{shared}/Grpc/Grpc.csproj");
        repo.Project($"{shared}/ServiceDefaults/ServiceDefaults.csproj");
        repo.Project($"{shared}/Storage.Abstractions/Storage.Abstractions.csproj");
        repo.Project($"{shared}/Storage/Storage.csproj", references: $"{shared}/Storage.Abstractions/Storage.Abstractions.csproj");

        const string import = "services/document_import/src";
        repo.Project($"{import}/ProtoFast.DocumentImport.Core/ProtoFast.DocumentImport.Core.csproj", references: $"{shared}/Exceptions/Exceptions.csproj");
        repo.Project($"{import}/ProtoFast.DocumentImport.Data/ProtoFast.DocumentImport.Data.csproj", references:
            [$"{import}/ProtoFast.DocumentImport.Core/ProtoFast.DocumentImport.Core.csproj", $"{shared}/Database/Database.csproj"]);
        repo.Project($"{import}/ProtoFast.DocumentImport.Engine/ProtoFast.DocumentImport.Engine.csproj", references: $"{import}/ProtoFast.DocumentImport.Core/ProtoFast.DocumentImport.Core.csproj");
        repo.Project($"{import}/ProtoFast.DocumentImport.Storage/ProtoFast.DocumentImport.Storage.csproj", references: $"{shared}/Storage/Storage.csproj");
        repo.Project($"{import}/ProtoFast.DocumentImport.Worker/ProtoFast.DocumentImport.Worker.csproj", "Microsoft.NET.Sdk.Worker", references:
        [
            $"{import}/ProtoFast.DocumentImport.Data/ProtoFast.DocumentImport.Data.csproj",
            $"{import}/ProtoFast.DocumentImport.Engine/ProtoFast.DocumentImport.Engine.csproj",
            $"{import}/ProtoFast.DocumentImport.Storage/ProtoFast.DocumentImport.Storage.csproj",
            $"{shared}/ServiceDefaults/ServiceDefaults.csproj",
        ]);

        const string api = "services/api/src";
        repo.Project($"{api}/ProtoFast.Data/ProtoFast.Data.csproj", references: $"{shared}/Database/Database.csproj");
        repo.Project($"{api}/ProtoFast.SchemaMigrations/ProtoFast.SchemaMigrations.csproj", references: $"{api}/ProtoFast.Data/ProtoFast.Data.csproj");
        repo.Project($"{api}/ProtoFast.Api/ProtoFast.Api.csproj", "Microsoft.NET.Sdk.Web", references:
        [
            $"{api}/ProtoFast.Data/ProtoFast.Data.csproj",
            $"{import}/ProtoFast.DocumentImport.Data/ProtoFast.DocumentImport.Data.csproj",
            $"{import}/ProtoFast.DocumentImport.Engine/ProtoFast.DocumentImport.Engine.csproj",
            $"{import}/ProtoFast.DocumentImport.Storage/ProtoFast.DocumentImport.Storage.csproj",
            $"{shared}/Grpc/Grpc.csproj",
            $"{shared}/ServiceDefaults/ServiceDefaults.csproj",
        ]);
        repo.Project("services/api/tests/ProtoFast.Api.Tests/ProtoFast.Api.Tests.csproj", references: $"{api}/ProtoFast.Api/ProtoFast.Api.csproj");

        repo.Project("services/payments/src/ProtoFast.Payments.Api/ProtoFast.Payments.Api.csproj", "Microsoft.NET.Sdk.Web",
            references: $"{shared}/ServiceDefaults/ServiceDefaults.csproj");

        repo.Write("services/conversion/Dockerfile", "FROM python:3.13-slim\n");
        repo.Write("services/conversion/.dockerignore", "tests/\n");
    }

    /// <summary>DescribeProtoFast describes the AppHost as docs/protostrap-aspire.md writes it.</summary>
    private static void DescribeProtoFast(FixtureRepo repo, IDistributedApplicationBuilder builder)
    {
        WriteRepository(repo);
        // A reference gives only the connection string, as ProtoFast's services read it: each
        // connection property would be one more key for an Org Admin to set in the secret.
        const ReferenceEnvironmentInjectionFlags references =
            ReferenceEnvironmentInjectionFlags.ConnectionString | ReferenceEnvironmentInjectionFlags.ServiceDiscovery;

        var protostrap = builder.AddProtoStrapEnvironment("protostrap", app: "protofast")
            .WithVersion("1.4.0")
            .WithIngressPool(vm: "c8g.medium")
            .WithGeneralPool(vm: ["m7g.large", "m6g.large", "m8g.large", "m7gd.large"],
                min: 1, max: 6, spare: 1, target: 0.70)
            .WithComputePool(vm: ["c7g.xlarge", "c6g.xlarge", "c8g.xlarge", "c7gd.xlarge"],
                min: 0, max: 4)
            .WithDataPool(vm: "m7g.xlarge", count: 1);
        var gateway = protostrap.AddEnvoyGateway("gateway")
            .WithHost("app", "protofast.dev", local: "app.dev.localhost")
            .WithHost("auth", "auth.protofast.dev", local: "auth.dev.localhost")
            .WithHost("admin", "admin.protofast.dev", local: "admin.dev.localhost")
            .WithRequest(cpu: "500m", memory: "256Mi");
        var tunnel = protostrap.AddCloudflareTunnel("tunnel", gateway);
        protostrap.AddTelemetry("telemetry");
        protostrap.AddCloudflareCdn("cdn", tunnel)
            .WithRateLimit(requests: 300)
            .WithWafRule("scanners", r => r.Paths("/wp-admin/", "/.env").Block())
            .WithWafRule("admin-abroad", r => r.Hosts("admin").NotCountries("US", "CA").ManagedChallenge())
            .WithWafRule("bots", r => r.Expression("http.user_agent contains \"sqlmap\"").Block())
            .WithCacheRule("assets", r => r.Hosts("app").Paths("/assets/").Cache(edge: TimeSpan.FromDays(30), browser: TimeSpan.FromDays(1)))
            .WithCacheRule("api", r => r.Paths("/api/").Bypass());

        var postgres = builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "2000m", memory: "8Gi")
                .Volume(size: "100Gi"));
        var protofastDb = postgres.AddDatabase("protofast-db", databaseName: "protofast");

        var redis = builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "500m", memory: "2Gi")
                .Volume(size: "10Gi"));

        // ProtoFast's documents are versioned, locked for its frozen run artifacts, and uploaded
        // straight from the browser with a signed POST.
        var documents = builder.AddProtoStrapBucket("documents", browser: true, versions: TimeSpan.FromDays(7), locked: true);
        var documentImportQueue = builder.AddProtoStrapQueue("protofast-document-upload");
        var workflowOutcomes = builder.AddProtoStrapQueue("protofast-workflow-outcomes", fifo: true);

        var protofastMigrations = builder.AddProject("protofast-db-migrations",
                repo.PathOf("services/api/src/ProtoFast.SchemaMigrations/ProtoFast.SchemaMigrations.csproj"))
            .WithReferenceEnvironment(references)
            .WithReference(protofastDb, connectionName: "protofast").WaitFor(protofastDb)
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));

        var conversion = builder.AddDockerfile("conversion", "../services/conversion")
            .WithHttpEndpoint(targetPort: 8090, name: "http")
            .WithHttpHealthCheck("/healthz")
            .WithReference(documents)
            .PublishAsProtoStrapService(s => s
                .Pool(Pool.Compute)
                .Request(cpu: "1000m", memory: "2Gi")
                .Scale(min: 1, max: 4, demand => demand.Cpu(0.70)));

        var api = builder.AddProject("api", repo.PathOf("services/api/src/ProtoFast.Api/ProtoFast.Api.csproj"))
            .WithReferenceEnvironment(references)
            .WithReference(redis).WaitFor(redis)
            .WithReference(protofastDb, connectionName: "protofast").WaitFor(protofastDb)
            .WithReference(documents)
            .WithReference(documentImportQueue)
            .WaitForCompletion(protofastMigrations)
            .WithEnvironment("Api_Sqs__DocumentImport__QueueName", "protofast-document-upload")
            .PublishAsProtoStrapService(s => s
                .Request(cpu: "500m", memory: "512Mi")
                .Ready(Readiness.Grpc)
                .Stop(TimeSpan.FromSeconds(30))
                .Scale(min: 2, max: 10, demand => demand.Cpu(0.70)));
        gateway.WithUpstream(api, u => u.Routes("app/api/"));

        var payments = builder.AddProject("payments", repo.PathOf("services/payments/src/ProtoFast.Payments.Api/ProtoFast.Payments.Api.csproj"))
            .PublishAsProtoStrapService(s => s
                .Request(cpu: "250m", memory: "256Mi")
                .Ready(Readiness.Grpc));
        gateway.WithUpstream(payments, u => u.Routes("app/payments/"));

        builder.AddProject("document-import",
                repo.PathOf("services/document_import/src/ProtoFast.DocumentImport.Worker/ProtoFast.DocumentImport.Worker.csproj"))
            .WithReferenceEnvironment(references)
            .WithReference(protofastDb, connectionName: "protofast").WaitFor(protofastDb)
            .WithReference(documents)
            .WithReference(documentImportQueue)
            .WithReference(workflowOutcomes)
            .WithEnvironment("DocumentImport_Conversion__BaseUrl", conversion.GetEndpoint("http"))
            .PublishAsProtoStrapWorker(w => w
                .Pool(Pool.Compute)
                .Request(cpu: "1000m", memory: "1Gi")
                .Stop(TimeSpan.FromSeconds(120))
                .Scale(min: 0, max: 20, demand => demand
                    .Queue(documentImportQueue, rate: 2, drain: TimeSpan.FromSeconds(60))));

        // These resources are dev-only, so the generator leaves them out.
        builder.AddContainer("smtp4dev", "rnwood/smtp4dev")
            .WithHttpEndpoint(port: 8025, targetPort: 80, name: "web")
            .ExcludeFromManifest();
        builder.AddExecutable("dev-operators", "bash", "../scripts", "seed-dev-operators.sh")
            .ExcludeFromManifest();
    }

    private static string GoldenPath([CallerFilePath] string source = "") =>
        Path.Combine(Path.GetDirectoryName(source)!, "Golden", "protofast.yaml");
}
