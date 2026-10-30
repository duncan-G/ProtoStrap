using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Environment variables are resolved in publish mode. Endpoints render as the component's
/// name on the overlay and its port; anything holding a secret or a connection detail becomes
/// a key in secrets, never a value (docs/protostrap-aspire.md, "References, endpoints and
/// secrets").
/// </summary>
public class ReferenceTests
{
    private static IResourceBuilder<ProjectResource> AddWorker(IDistributedApplicationBuilder builder, FixtureRepo repo, string name = "worker") =>
        builder.AddProject(name, repo.Project($"services/{name}/{name}.csproj", "Microsoft.NET.Sdk.Worker"))
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi"));

    private static IResourceBuilder<ContainerResource> AddConversion(IDistributedApplicationBuilder builder, FixtureRepo repo)
    {
        repo.Write("services/conversion/Dockerfile", "FROM python:3.13-slim\n");
        return builder.AddDockerfile("conversion", repo.PathOf("services/conversion"))
            .WithHttpEndpoint(targetPort: 8090, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi"));
    }

    private static IResourceBuilder<PostgresServerResource> AddPostgres(IDistributedApplicationBuilder builder) =>
        builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "2000m", memory: "8Gi").Volume(size: "100Gi"));

    [Fact]
    public async Task LiteralsAreWrittenAsTheyAre()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddWorker(builder, repo)
            .WithEnvironment("Api_Sqs__DocumentImport__QueueName", "protofast-document-upload")
            .WithEnvironment("JOBS_BUDGET", "100000")
            .WithEnvironment("FEATURE", "yes")
            .WithEnvironment("GREETING", "hello: world")
            .WithEnvironment(context => context.EnvironmentVariables["COUNT"] = 3);

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("""
              env:
                Api_Sqs__DocumentImport__QueueName: protofast-document-upload
                COUNT: "3"
                FEATURE: "yes"
                GREETING: "hello: world"
                JOBS_BUDGET: "100000"
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
            """, worker);
    }

    [Fact]
    public async Task EndpointsRenderAsTheComponentsNameAndPort()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var conversion = AddConversion(builder, repo);
        var http = conversion.GetEndpoint("http");
        AddWorker(builder, repo)
            .WithEnvironment("DocumentImport_Conversion__BaseUrl", http)
            .WithEnvironment("CONVERSION_HOST", http.Property(EndpointProperty.Host))
            .WithEnvironment("CONVERSION_IPV4", http.Property(EndpointProperty.IPV4Host))
            .WithEnvironment("CONVERSION_PORT", http.Property(EndpointProperty.Port))
            .WithEnvironment("CONVERSION_TARGET_PORT", http.Property(EndpointProperty.TargetPort))
            .WithEnvironment("CONVERSION_HOST_AND_PORT", http.Property(EndpointProperty.HostAndPort))
            .WithEnvironment("CONVERSION_SCHEME", http.Property(EndpointProperty.Scheme))
            .WithEnvironment("CONVERSION_URL", http.Property(EndpointProperty.Url))
            .WithEnvironment("CONVERSION_TLS", http.Property(EndpointProperty.TlsEnabled))
            .WithEnvironment("CONVERT_URL", ReferenceExpression.Create($"{http}/v1/convert?from={http.Property(EndpointProperty.Host)}"));

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("""
              env:
                CONVERSION_HOST: conversion
                CONVERSION_HOST_AND_PORT: conversion:8090
                CONVERSION_IPV4: conversion
                CONVERSION_PORT: "8090"
                CONVERSION_SCHEME: http
                CONVERSION_TARGET_PORT: "8090"
                CONVERSION_TLS: "false"
                CONVERSION_URL: http://conversion:8090
                CONVERT_URL: http://conversion:8090/v1/convert?from=conversion
                DocumentImport_Conversion__BaseUrl: http://conversion:8090
            """, worker);
        Assert.DoesNotContain("secrets", worker);
        Assert.DoesNotContain("depends_on", worker);
    }

    [Fact]
    public async Task ReferencesOnAnyNetworkRenderTheSame()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var conversion = AddConversion(builder, repo);
        AddWorker(builder, repo)
            .WithEnvironment("CONTAINERS", conversion.GetEndpoint("http", KnownNetworkIdentifiers.DefaultAspireContainerNetwork))
            .WithEnvironment("HOST", conversion.GetEndpoint("http", KnownNetworkIdentifiers.LocalhostNetwork).Property(EndpointProperty.Host));

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        // A project resolves the endpoints it references on the host's network; a replica
        // reaches them all on the overlay.
        Assert.Contains("\n    CONTAINERS: http://conversion:8090\n", worker);
        Assert.Contains("\n    HOST: conversion\n", worker);
    }

    [Fact]
    public async Task ReferencingAServiceGivesItsHttpAddress()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/api/Properties/launchSettings.json", """
            { "profiles": { "https": { "commandName": "Project", "applicationUrl": "https://localhost:7001;http://localhost:5001" } } }
            """);
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        AddWorker(builder, repo).WithReference(api).WaitFor(api);

        var result = await TestApp.GenerateAsync(builder);

        Assert.Empty(result.Problems);
        var worker = SpecText.Component(result.Yaml!, "worker");
        Assert.Contains("""
              depends_on: [api]
              env:
                API_HTTP: http://api:8080
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
                services__api__http__0: http://api:8080
            """, worker);
        // Nothing listens on the https endpoint in a replica: ProtoStrap has no TLS on the overlay.
        Assert.Contains(result.Warnings, w => w.Contains("worker: API_HTTPS is left out", StringComparison.Ordinal));
        Assert.Contains(result.Warnings, w => w.Contains("worker: services__api__https__0 is left out", StringComparison.Ordinal));
    }

    [Fact]
    public async Task ConnectionStringsAndTheirPropertiesAreSecrets()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var postgres = AddPostgres(builder);
        var protofastDb = postgres.AddDatabase("protofast-db", databaseName: "protofast");
        var redis = builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi"));
        AddWorker(builder, repo)
            .WithReference(protofastDb, connectionName: "protofast").WaitFor(protofastDb)
            .WithReference(redis).WaitFor(redis);

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("""
              depends_on: [postgres, redis]
              env:
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
              secrets:
                - ConnectionStrings__protofast
                - ConnectionStrings__redis
                - PROTOFAST_DATABASENAME
                - PROTOFAST_HOST
                - PROTOFAST_JDBCCONNECTIONSTRING
                - PROTOFAST_PASSWORD
                - PROTOFAST_PORT
                - PROTOFAST_URI
                - PROTOFAST_USERNAME
                - REDIS_HOST
                - REDIS_PASSWORD
                - REDIS_PORT
                - REDIS_URI
            """, worker);
    }

    [Fact]
    public async Task ShortSecretListsAreOneLine()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var password = builder.AddParameter("smtp-password", secret: true);
        AddWorker(builder, repo).WithEnvironment("Auth_Smtp__Password", password);

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("\n  secrets: [Auth_Smtp__Password]\n", worker);
    }

    [Fact]
    public async Task ParametersAreNeverWrittenSecretOrNot()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var password = builder.AddParameter("smtp-password", secret: true);
        var from = builder.AddParameter("smtp-from", "no-reply@protofast.dev");
        AddWorker(builder, repo)
            .WithEnvironment("SMTP_PASSWORD", password)
            .WithEnvironment("SMTP_FROM", from)
            .WithEnvironment("SMTP_URL", ReferenceExpression.Create($"smtp://user:{password}@mail:25"));

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("\n  secrets: [SMTP_FROM, SMTP_PASSWORD, SMTP_URL]\n", worker);
        Assert.DoesNotContain("no-reply", worker);
    }

    [Fact]
    public async Task WaitingForADatabaseDependsOnItsServer()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var postgres = AddPostgres(builder);
        var authDb = postgres.AddDatabase("auth-db", databaseName: "auth");
        var protofastDb = postgres.AddDatabase("protofast-db", databaseName: "protofast");
        AddWorker(builder, repo).WaitFor(authDb).WaitFor(protofastDb).WaitFor(postgres);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  depends_on: [postgres]\n", SpecText.Component(yaml, "worker"));
        Assert.False(SpecText.HasComponent(yaml, "auth-db"));
        Assert.False(SpecText.HasComponent(yaml, "protofast-db"));
    }

    [Fact]
    public async Task WaitingForWhatIsntPublishedIsLeftOut()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var smtp4dev = builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3.6").ExcludeFromManifest();
        var parameter = builder.AddParameter("feature", "on");
        AddWorker(builder, repo).WaitFor(smtp4dev).WaitFor(parameter);

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.DoesNotContain("depends_on", worker);
    }

    [Fact]
    public async Task ReferencesToWhatIsntPublishedAreReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var smtp4dev = builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3.6")
            .WithEndpoint(targetPort: 25, name: "smtp")
            .ExcludeFromManifest();
        AddWorker(builder, repo).WithEnvironment("SMTP", smtp4dev.GetEndpoint("smtp"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["worker: SMTP refers to smtp4dev, which is excluded from publish"], problems);
    }

    [Fact]
    public async Task VariablesProtoStrapSetsAreReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddWorker(builder, repo)
            .WithEnvironment("AWS_REGION", "eu-west-1")
            .WithEnvironment("PROTOSTRAP_APP", "x")
            .WithEnvironment("QUEUE_JOBS_URL", "x")
            .WithEnvironment("FILES_BUCKET", "x")
            .WithEnvironment("SECRET_ID", "x")
            .WithEnvironment("SECRET_PREFIX", "x");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "worker: AWS_REGION is set by ProtoStrap in every replica",
            "worker: FILES_BUCKET is set by ProtoStrap in every replica",
            "worker: PROTOSTRAP_APP is set by ProtoStrap in every replica",
            "worker: QUEUE_JOBS_URL is set by ProtoStrap in every replica",
            "worker: SECRET_ID is set by ProtoStrap in every replica",
            "worker: SECRET_PREFIX is set by ProtoStrap in every replica",
        ], problems);
    }

    [Fact]
    public async Task VariableNamesMustBeVariableNames()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddWorker(builder, repo).WithEnvironment("Not-A-Name", "x");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["worker: \"Not-A-Name\" isn't a variable name"], problems);
    }

    [Fact]
    public async Task ArgumentsBecomeTheCommand()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var conversion = AddConversion(builder, repo);
        AddWorker(builder, repo).WithArgs("--conversion", conversion.GetEndpoint("http"), "--verbose", "yes");

        var worker = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "worker");

        Assert.Contains("\n  command: [--conversion, \"http://conversion:8090\", --verbose, \"yes\"]\n", worker);
    }

    [Fact]
    public async Task SecretsInArgumentsAreReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var token = builder.AddParameter("token", secret: true);
        AddWorker(builder, repo).WithArgs("--token", token);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["worker: argument 2 holds a secret or a connection detail, which can't be written into its command: pass it in an environment variable, which goes to its secret"], problems);
    }

    [Fact]
    public async Task EndpointsWithoutAKnownPortAreLeftOutWithAWarning()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        var web = builder.AddDockerfile("web", repo.PathOf("services/web"))
            .WithHttpEndpoint(targetPort: 3000, name: "http")
            .WithEndpoint(name: "debug", scheme: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        AddWorker(builder, repo)
            .WithEnvironment("WEB", web.GetEndpoint("http"))
            .WithEnvironment("WEB_DEBUG", web.GetEndpoint("debug"));

        var result = await TestApp.GenerateAsync(builder);

        Assert.Empty(result.Problems);
        var worker = SpecText.Component(result.Yaml!, "worker");
        Assert.Contains("\n    WEB: http://web:3000\n", worker);
        Assert.DoesNotContain("WEB_DEBUG", worker);
        Assert.Contains("worker: WEB_DEBUG is left out: web's debug endpoint has no target port, so nothing in a replica listens on it", result.Warnings);
    }
}
