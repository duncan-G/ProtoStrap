using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// SecretFiles has the agent write a component's keys to files in /run/secrets, for an image
/// that can't read the environment's secret itself, such as Keycloak's or MinIO's
/// (docs/secrets.md#secret-files). Any kind of component can ask for them; what a new value
/// does is the component's on_secret_change.
/// </summary>
public class SecretFilesTests
{
    [Fact]
    public async Task AServiceGetsItsKeysAsFiles()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var postgres = builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "1000m", memory: "2Gi").Volume(size: "20Gi"));
        var keycloakDb = postgres.AddDatabase("keycloak-db", databaseName: "keycloak");
        var adminPassword = builder.AddParameter("keycloak-admin-password", secret: true);
        var db = postgres.GetEndpoint("tcp");
        builder.AddDockerfile("keycloak", "../services/keycloak")
            .WithHttpEndpoint(targetPort: 8080)
            .WithHttpHealthCheck("/health/ready")
            .WithEnvironment("KC_DB", "postgres")
            .WithEnvironment("KC_DB_URL", ReferenceExpression.Create(
                $"jdbc:postgresql://{db.Property(EndpointProperty.Host)}:{db.Property(EndpointProperty.Port)}/keycloak"))
            .WithEnvironment("KC_DB_PASSWORD", postgres.Resource.PasswordParameter)
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_PASSWORD", adminPassword)
            .WaitFor(keycloakDb)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // Each key is the variable that passed it in run mode. The image's entrypoint reads
        // /run/secrets/KC_DB_PASSWORD and the rest; a new value replaces the replicas.
        Assert.Equal("""
            keycloak:
              kind: service
              build: services/keycloak
              pool: general
              request: { cpu: 500m, memory: 1Gi }
              port: 8080
              ready: /health/ready
              depends_on: [postgres]
              env:
                KC_DB: postgres
              secrets: [KC_BOOTSTRAP_ADMIN_PASSWORD, KC_DB_PASSWORD, KC_DB_URL]
              secret_files: true

            """, SpecText.Component(yaml, "keycloak"));
    }

    [Fact]
    public async Task AServiceCanReadNewFilesItself()
    {
        using var repo = new FixtureRepo();
        var builder = SecretService(repo, s => s.SecretFiles(SecretChange.Reload));

        var keycloak = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "keycloak");

        Assert.Contains("\n  secret_files: true\n  on_secret_change: reload\n", keycloak);
    }

    [Fact]
    public async Task AServiceCanTakeNewFilesWithACommand()
    {
        using var repo = new FixtureRepo();
        var builder = SecretService(repo, s => s.SecretFiles(SecretChange.Run("sh", "-c", "kill -HUP 1")));

        var keycloak = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "keycloak");

        Assert.Contains("\n  secret_files: true\n  on_secret_change: [sh, -c, kill -HUP 1]\n", keycloak);
    }

    [Fact]
    public async Task AWorkerGetsItsKeysAsFiles()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/mailer/Dockerfile", "FROM node:22-slim\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var smtp = builder.AddParameter("smtp-password", secret: true);
        builder.AddDockerfile("mailer", "../services/mailer")
            .WithEnvironment("SMTP_PASSWORD", smtp)
            .PublishAsProtoStrapWorker(w => w.Request(cpu: "250m", memory: "256Mi").SecretFiles(SecretChange.Reload));

        var mailer = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "mailer");

        Assert.Contains("\n  secrets: [SMTP_PASSWORD]\n  secret_files: true\n  on_secret_change: reload\n", mailer);
    }

    [Fact]
    public async Task ATaskGetsItsKeysAsFiles()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/realms/Dockerfile", "FROM adorsys/keycloak-config-cli:6.4.0-26\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var clientSecret = builder.AddParameter("admin-client-secret", secret: true);
        builder.AddDockerfile("realms", "../services/realms")
            .WithEnvironment("ADMIN_CLIENT_SECRET", clientSecret)
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "512Mi").SecretFiles());

        var realms = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "realms");

        // A task reads its keys each time it runs, so a new value has nothing to change.
        Assert.Contains("\n  secrets: [ADMIN_CLIENT_SECRET]\n  secret_files: true\n", realms);
        Assert.DoesNotContain("on_secret_change", realms);
    }

    [Fact]
    public async Task AnUpstreamImageGetsItsKeysAsFiles()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var password = builder.AddParameter("minio-password", secret: true);
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .WithHttpEndpoint(targetPort: 9000, name: "api")
            .WithEnvironment("MINIO_ROOT_PASSWORD", password)
            .WithEnvironment("MINIO_ROOT_PASSWORD_FILE", "/run/secrets/MINIO_ROOT_PASSWORD")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "500m", memory: "1Gi")
                .Volume(size: "50Gi", path: "/data")
                .SecretFiles());

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            minio:
              kind: stateful
              image: minio/minio:RELEASE.2025-09-07T16-13-09Z
              vm: data-1
              request: { cpu: 500m, memory: 1Gi }
              port: 9000
              env:
                MINIO_ROOT_PASSWORD_FILE: /run/secrets/MINIO_ROOT_PASSWORD
              secrets: [MINIO_ROOT_PASSWORD]
              secret_files: true
              volume: { path: /data, size: 50Gi }

            """, SpecText.Component(yaml, "minio"));
    }

    [Fact]
    public async Task AnUpstreamImageCanTakeNewFilesWithACommand()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var password = builder.AddParameter("minio-password", secret: true);
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .WithHttpEndpoint(targetPort: 9000, name: "api")
            .WithEnvironment("MINIO_ROOT_PASSWORD", password)
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "500m", memory: "1Gi")
                .Volume(size: "50Gi", path: "/data")
                .SecretFiles(SecretChange.Run("sh", "-c", "kill -HUP 1")));

        var minio = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "minio");

        Assert.Contains("\n  secret_files: true\n  on_secret_change: [sh, -c, kill -HUP 1]\n", minio);
    }

    [Fact]
    public async Task SecretFilesNeedsKeysToWrite()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", "../services/web")
            .WithHttpEndpoint(targetPort: 3000)
            .WithEnvironment("NODE_ENV", "production")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz").SecretFiles());

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
            ["web: SecretFiles: it has no keys for the agent to write: a secret parameter or a connection string in its environment is one"],
            problems);
    }

    [Fact]
    public async Task ACommandNeedsAProgram()
    {
        using var repo = new FixtureRepo();
        var builder = SecretService(repo, s => s.SecretFiles(SecretChange.Run()));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["keycloak: SecretFiles: the command to run on a new value is empty"], problems);
    }

    /// <summary>SecretService adds a service built from a Dockerfile with one secret key.</summary>
    private static IDistributedApplicationBuilder SecretService(FixtureRepo repo, Action<ProtoStrapService> configure)
    {
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var adminPassword = builder.AddParameter("keycloak-admin-password", secret: true);
        builder.AddDockerfile("keycloak", "../services/keycloak")
            .WithHttpEndpoint(targetPort: 8080)
            .WithHttpHealthCheck("/health/ready")
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_PASSWORD", adminPassword)
            .PublishAsProtoStrapService(s => configure(s.Request(cpu: "500m", memory: "1Gi")));
        return builder;
    }
}
