using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.Logging.Abstractions;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Under `aspire run`, WithDevSecret gives every parameter a component takes its value from
/// dev's secret, &lt;app&gt;/dev, by the key the component reads in an account: its prefix and
/// the variable's name, or the Shared_ one. The AppHost reads it with the developer's AWS SSO
/// sign-in, signing in first if the session is missing or expired (docs/secrets.md#dev).
/// </summary>
public class DevSecretTests
{
    private static readonly DistributedApplicationExecutionContext Run = new(DistributedApplicationOperation.Run);

    private static Task<IReadOnlyList<DevSecret.Use>> UsesAsync(IDistributedApplicationBuilder builder) =>
        DevSecret.UsesAsync(builder.Resources, Run, TestContext.Current.CancellationToken);

    private static string[] Lines(IReadOnlyDictionary<string, string> values) =>
        [.. values.Select(v => $"{v.Key}={v.Value}").Order(StringComparer.Ordinal)];

    private static IResourceBuilder<ContainerResource> Keycloak(IDistributedApplicationBuilder builder) =>
        builder.AddContainer("keycloak", "quay.io/keycloak/keycloak", "26.7").WithHttpEndpoint(targetPort: 8080);

    [Fact]
    public async Task AParameterTakesTheKeyItsComponentReadsInAnAccount()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var secret = builder.AddParameter("keycloak-admin-secret", secret: true);
        Keycloak(builder)
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", secret)
            .WithEnvironment("KC_HEALTH_ENABLED", "true")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());

        var values = DevSecret.Values(await UsesAsync(builder),
            new Dictionary<string, string> { ["Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET"] = "s3cret" }, "protofast/dev");

        Assert.Equal(["Parameters:keycloak-admin-secret=s3cret"], Lines(values));
    }

    [Fact]
    public async Task ItsOwnKeyWinsOverASharedOne()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var web = builder.AddParameter("web-client-secret", secret: true);
        var jwt = builder.AddParameter("internal-jwt-public-key", secret: true);
        builder.AddContainer("auth", "example/auth", "1")
            .WithEnvironment("Keycloak__ClientSecretWeb", web)
            .WithEnvironment("InternalJwt__PublicKeyPem", jwt)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));

        var values = DevSecret.Values(await UsesAsync(builder), new Dictionary<string, string>
        {
            ["Shared_Keycloak__ClientSecretWeb"] = "shared",
            ["Auth_Keycloak__ClientSecretWeb"] = "own",
            ["Shared_InternalJwt__PublicKeyPem"] = "public",
        }, "protofast/dev");

        Assert.Equal(["Parameters:internal-jwt-public-key=public", "Parameters:web-client-secret=own"], Lines(values));
    }

    [Fact]
    public async Task ASecretPrefixNamesItsKeys()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var admin = builder.AddParameter("keycloak-admin-secret", secret: true);
        builder.AddContainer("keycloak-config", "adorsys/keycloak-config-cli", "6.4")
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", admin)
            .PublishAsProtoStrapTask(t => t.Request(cpu: "100m", memory: "256Mi").SecretPrefix("Keycloak_"));

        var uses = await UsesAsync(builder);

        Assert.Equal("Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", Assert.Single(uses).Key);
    }

    [Fact]
    public async Task AComponentsNameInPascalCaseIsItsPrefix()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var key = builder.AddParameter("deepseek-api-key", secret: true);
        builder.AddContainer("document-import", "example/import", "1")
            .WithEnvironment("Providers__deepseek__ApiKey", key)
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "500m", memory: "512Mi"));

        var uses = await UsesAsync(builder);

        Assert.Equal("DocumentImport_Providers__deepseek__ApiKey", Assert.Single(uses).Key);
    }

    [Fact]
    public async Task PostgresAndRedisTakeTheirPasswordsFromTheKeysAnAccountWritesToTheirFiles()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "1000m", memory: "1Gi").Volume(size: "20Gi"));
        builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "250m", memory: "512Mi").Volume(size: "1Gi"));

        var values = DevSecret.Values(await UsesAsync(builder), new Dictionary<string, string>
        {
            ["Postgres_POSTGRES_PASSWORD"] = "pg",
            ["Redis_REDIS_PASSWORD"] = "rd",
        }, "protofast/dev");

        Assert.Equal(["Parameters:postgres-password=pg", "Parameters:redis-password=rd"], Lines(values));
    }

    [Fact]
    public async Task ConnectionStringsAndTheirPartsAreAspiresLocally()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var postgres = builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "1000m", memory: "1Gi").Volume(size: "20Gi"));
        var db = postgres.AddDatabase("auth-db", databaseName: "auth");
        builder.AddContainer("auth", "example/auth", "1")
            .WithReference(db, connectionName: "auth")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));

        var uses = await UsesAsync(builder);

        // Only PostgreSQL's own password: auth's connection string, and the parts beside it, are
        // the ones Aspire makes for the local database.
        Assert.Equal(["postgres POSTGRES_PASSWORD Postgres_POSTGRES_PASSWORD"],
            uses.Select(u => $"{u.Component} {u.Variable} {u.Key}").ToArray());
    }

    [Fact]
    public async Task OnlyProtoStrapComponentsTakeKeys()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var token = builder.AddParameter("smtp4dev-token", secret: true);
        builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3").WithEnvironment("TOKEN", token);

        Assert.Empty(await UsesAsync(builder));
    }

    [Fact]
    public async Task AParameterTwoComponentsTakeNeedsTheSameValueUnderEachKey()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var admin = builder.AddParameter("keycloak-admin-secret", secret: true);
        Keycloak(builder)
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", admin)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());
        builder.AddContainer("keycloak-config", "adorsys/keycloak-config-cli", "6.4")
            .WithEnvironment("KEYCLOAK_CLIENTSECRET", admin)
            .PublishAsProtoStrapTask(t => t.Request(cpu: "100m", memory: "256Mi").SecretFiles());
        var uses = await UsesAsync(builder);

        var same = DevSecret.Values(uses, new Dictionary<string, string>
        {
            ["Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET"] = "s3cret-a",
            ["KeycloakConfig_KEYCLOAK_CLIENTSECRET"] = "s3cret-a",
        }, "protofast/dev");
        var problem = Assert.Throws<DevSecretException>(() => DevSecret.Values(uses, new Dictionary<string, string>
        {
            ["Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET"] = "s3cret-a",
            ["KeycloakConfig_KEYCLOAK_CLIENTSECRET"] = "s3cret-b",
        }, "protofast/dev"));

        Assert.Equal(["Parameters:keycloak-admin-secret=s3cret-a"], Lines(same));
        Assert.Contains("KeycloakConfig_KEYCLOAK_CLIENTSECRET and Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET differ", problem.Message);
        Assert.Contains("keycloak-admin-secret", problem.Message);
        Assert.DoesNotContain("s3cret", problem.Message);
    }

    [Fact]
    public async Task EveryMissingKeyIsNamedAndTheRunStops()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        Keycloak(builder)
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", builder.AddParameter("keycloak-admin-secret", secret: true))
            .WithEnvironment("GOOGLE_CLIENT_ID", builder.AddParameter("google-client-id", secret: true))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());

        var problem = Assert.Throws<DevSecretException>(() =>
            DevSecret.Values(UsesAsync(builder).Result, new Dictionary<string, string>(), "protofast/dev"));

        Assert.Contains("protofast/dev has no Keycloak_GOOGLE_CLIENT_ID or Shared_GOOGLE_CLIENT_ID", problem.Message);
        Assert.Contains("protofast/dev has no Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET or Shared_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", problem.Message);
        Assert.Contains("set-secret.sh protofast/dev", problem.Message);
    }

    [Fact]
    public async Task AGeneratedParameterTakesTheSecretsValueInsteadOfMakingOne()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { Secret = """{"Keycloak_KC_BOOTSTRAP_ADMIN_CLIENT_SECRET":"from-dev","Shared_Unused":"x"}""" };
        var builder = TestApp.Run(repo);
        var admin = builder.AddParameter("keycloak-admin-secret", new GenerateParameterDefault { MinLength = 32, Special = false }, secret: true, persist: true);
        Keycloak(builder)
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", admin)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());

        await aws.DevSecret().ApplyAsync(builder.Resources, Run, builder.Configuration, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal("from-dev", builder.Configuration["Parameters:keycloak-admin-secret"]);
#pragma warning disable CS0618 // The value Aspire resolves when it starts, read the way it reads it.
        Assert.Equal("from-dev", admin.Resource.Value);
#pragma warning restore CS0618
        Assert.Contains("secretsmanager get-secret-value --secret-id protofast/dev --profile developer --region us-west-2 --query SecretString --output text", aws.Calls);
    }

    [Fact]
    public async Task WithoutParametersNothingIsRead()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo);
        var builder = TestApp.Run(repo);
        Keycloak(builder).PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi"));

        await aws.DevSecret().ApplyAsync(builder.Resources, Run, builder.Configuration, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Empty(aws.Calls);
    }

    // Signing in and reading the secret.

    [Fact]
    public async Task ASignedInProfileReadsTheSecretAsItIs()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { Secret = """{"Api_Key":"v"}""" };

        var secret = aws.DevSecret();
        var region = await secret.SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        var keys = await secret.ReadAsync(region, TestContext.Current.CancellationToken);

        Assert.Equal("us-west-2", region);
        Assert.Equal("v", keys["Api_Key"]);
        Assert.DoesNotContain(aws.Calls, c => c.StartsWith("sso login", StringComparison.Ordinal));
    }

    [Fact]
    public async Task AnExpiredSessionSignsInWithSso()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { SignedIn = false };

        await aws.DevSecret().SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal([
            "--version",
            "configure list-profiles",
            "sts get-caller-identity --profile developer --region us-west-2 --output text",
            "sso login --profile developer",
            "sts get-caller-identity --profile developer --region us-west-2 --output text",
        ], aws.Calls);
    }

    [Fact]
    public async Task AFailedSignInStopsTheRun()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { SignedIn = false, LoginFails = true };

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret().SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("aws sso login --profile developer", problem.Message);
    }

    [Fact]
    public async Task AMissingProfileSaysHowToMakeIt()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo);
        aws.Profiles("default", "admin");

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret().SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("aws configure sso --profile developer", problem.Message);
        Assert.Contains("Developer permission set", problem.Message);
    }

    [Fact]
    public async Task WithoutTheAwsCliItSaysToInstallIt()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo);

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret(path: repo.PathOf("empty")).SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("AWS CLI", problem.Message);
    }

    [Fact]
    public async Task TheRegionIsTheEnvironmentsThenTheProfiles()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { Region = "eu-west-1" };

        var fromProfile = await aws.DevSecret(region: null).SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        var fromEnvironment = await aws.DevSecret(region: "us-west-2").SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        aws.Region = null;
        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret(region: null).SignInAsync(NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Equal("eu-west-1", fromProfile);
        Assert.Equal("us-west-2", fromEnvironment);
        Assert.Contains("aws configure set region <region> --profile developer", problem.Message);
    }

    [Fact]
    public async Task AMissingSecretSaysHowToMakeIt()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo);

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret().ReadAsync("us-west-2", TestContext.Current.CancellationToken));

        Assert.Contains("aws secretsmanager create-secret --name protofast/dev --region us-west-2", problem.Message);
    }

    [Fact]
    public async Task AProfileThatCantReadItIsTold()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { Denied = true };

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret().ReadAsync("us-west-2", TestContext.Current.CancellationToken));

        Assert.Contains("can't read protofast/dev", problem.Message);
        Assert.Contains("Developer permission set", problem.Message);
    }

    [Fact]
    public async Task AValueThatIsntAJsonObjectIsReportedWithoutTheValue()
    {
        using var repo = new FixtureRepo();
        var aws = new FakeAws(repo) { Secret = "hunter2" };

        var problem = await Assert.ThrowsAsync<DevSecretException>(() =>
            aws.DevSecret().ReadAsync("us-west-2", TestContext.Current.CancellationToken));

        Assert.Contains("isn't a JSON object of strings", problem.Message);
        Assert.DoesNotContain("hunter2", problem.Message);
    }

    // The AppHost's side.

    [Fact]
    public async Task InPublishModeItChangesNothing()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        string[] yaml = new string[2];
        for (var i = 0; i < 2; i++)
        {
            var builder = TestApp.Publish(repo);
            var environment = builder.AddEnvironment();
            if (i == 1)
                environment.WithDevSecret();
            builder.AddDockerfile("keycloak", "../services/keycloak")
                .WithHttpEndpoint(targetPort: 8080)
                .WithHttpHealthCheck("/health/ready")
                .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", builder.AddParameter("keycloak-admin-secret", secret: true))
                .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());
            yaml[i] = await TestApp.GenerateYamlAsync(builder);
        }

        Assert.Equal(yaml[0], yaml[1]);
    }
}
