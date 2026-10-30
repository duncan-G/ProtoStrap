using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// A stateful component runs an upstream image, pinned to a data VM with its own volume. On
/// PostgreSQL and Redis, PublishAsProtoStrapStateful fills in what the integration knows: the
/// port, the data path and the dump command (docs/protostrap-aspire.md, "Component kinds").
/// </summary>
public class StatefulTests
{
    [Fact]
    public async Task PostgresFillsInWhatTheIntegrationKnows()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var postgres = builder.AddPostgres("postgres")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "2000m", memory: "8Gi")
                .Volume(size: "100Gi"));
        postgres.AddDatabase("protofast-db", databaseName: "protofast");

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // Its password is the agent's file of POSTGRES_PASSWORD from the environment's secret,
        // which connections over the network need. A new one is applied in place by a hook,
        // through the socket, which still trusts.
        Assert.Equal("""
            postgres:
              kind: stateful
              image: postgres:18.3
              vm: data-1
              request: { cpu: 2000m, memory: 8Gi }
              port: 5432
              env:
                POSTGRES_HOST_AUTH_METHOD: scram-sha-256
                POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD
              secrets: [POSTGRES_PASSWORD]
              secret_files: true
              on_secret_change:
                - sh
                - -c
                - printf '%s\n' '\set pw `cat /run/secrets/POSTGRES_PASSWORD`' "ALTER ROLE postgres PASSWORD :'pw';" | psql -X -q -v ON_ERROR_STOP=1 -U postgres -d postgres
              volume: { path: /var/lib/postgresql, size: 100Gi }
              backup: [pg_dumpall, -U, postgres]

            """, SpecText.Component(yaml, "postgres"));
        Assert.False(SpecText.HasComponent(yaml, "protofast-db"));
    }

    [Fact]
    public async Task PostgresBefore18KeepsItsDataInData()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddPostgres("postgres").WithImageTag("17.6")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "1000m", memory: "3Gi").Volume(size: "20Gi"));

        var postgres = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "postgres");

        Assert.Contains("\n  image: postgres:17.6\n", postgres);
        Assert.Contains("\n  volume: { path: /var/lib/postgresql/data, size: 20Gi }\n", postgres);
    }

    [Fact]
    public async Task PostgresKeepsVariablesTheAppHostAdds()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddPostgres("postgres")
            .WithEnvironment("POSTGRES_INITDB_ARGS", "--data-checksums")
            .WithEnvironment("TZ", "UTC")
            .WithDataVolume()
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "1000m", memory: "3Gi").Volume(size: "20Gi"));

        var postgres = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "postgres");

        Assert.Contains("""
              env:
                POSTGRES_HOST_AUTH_METHOD: scram-sha-256
                POSTGRES_INITDB_ARGS: --data-checksums
                POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD
                TZ: UTC
            """, postgres);
        Assert.Contains("\n  volume: { path: /var/lib/postgresql, size: 20Gi }\n", postgres);
    }

    [Fact]
    public async Task RedisFillsInWhatTheIntegrationKnows()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi"));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // Redis has no *_FILE variable, so its command reads the agent's file, through the
        // image's entrypoint so it still drops to the redis user. A new password is set in
        // place, and the backup logs in.
        Assert.Equal("""
            redis:
              kind: stateful
              image: redis:8.6
              command: [sh, -c, exec docker-entrypoint.sh redis-server --requirepass "$(cat /run/secrets/REDIS_PASSWORD)"]
              vm: data-1
              request: { cpu: 500m, memory: 1Gi }
              port: 6379
              secrets: [REDIS_PASSWORD]
              secret_files: true
              on_secret_change:
                - sh
                - -c
                - for f in /run/secrets/REDIS_PASSWORD /run/secrets/previous/REDIS_PASSWORD; do [ -f "$f" ] && REDISCLI_AUTH="$(cat "$f")" redis-cli --no-auth-warning -x CONFIG SET requirepass </run/secrets/REDIS_PASSWORD | grep -qx OK && exit 0; done; exit 1
              volume: { path: /data, size: 5Gi }
              backup: [sh, -c, REDISCLI_AUTH="$(cat /run/secrets/REDIS_PASSWORD)" exec redis-cli --no-auth-warning --rdb -]

            """, SpecText.Component(yaml, "redis"));
    }

    [Fact]
    public async Task RedisWarnsWhenTheCallbackReplacesItsCommand()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi")
                .Command("redis-server", "--appendonly", "yes"));

        var result = await TestApp.GenerateAsync(builder);

        Assert.Contains("redis: a command of its own must start Redis with the password in /run/secrets/REDIS_PASSWORD, as the default does", result.Warnings);
    }

    [Fact]
    public async Task TheCallbackCanSetTheCommandBackupAndVolumePath()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1")
                .Request(cpu: "500m", memory: "1Gi")
                .Command("redis-server", "--appendonly", "yes", "--maxmemory", "768mb")
                .Volume(size: "5Gi", path: "/var/lib/redis", iops: 4000, throughput: 250)
                .Backup("redis-cli", "--rdb", "/dev/stdout")
                .Metrics(port: 9121)
                .Stop(TimeSpan.FromSeconds(30)));

        var redis = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "redis");

        Assert.Equal("""
            redis:
              kind: stateful
              image: redis:8.6
              command: [redis-server, --appendonly, "yes", --maxmemory, 768mb]
              vm: data-1
              request: { cpu: 500m, memory: 1Gi }
              port: 6379
              metrics: 9121/metrics
              secrets: [REDIS_PASSWORD]
              secret_files: true
              on_secret_change:
                - sh
                - -c
                - for f in /run/secrets/REDIS_PASSWORD /run/secrets/previous/REDIS_PASSWORD; do [ -f "$f" ] && REDISCLI_AUTH="$(cat "$f")" redis-cli --no-auth-warning -x CONFIG SET requirepass </run/secrets/REDIS_PASSWORD | grep -qx OK && exit 0; done; exit 1
              stop: 30s
              volume: { path: /var/lib/redis, size: 5Gi, iops: 4000, throughput: 250 }
              backup: [redis-cli, --rdb, /dev/stdout]

            """, redis);
    }

    [Fact]
    public async Task AnyContainerCanBeStateful()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .WithHttpEndpoint(targetPort: 9000, name: "api")
            .WithHttpEndpoint(targetPort: 9001, name: "console")
            .WithEnvironment("MINIO_BROWSER", "off")
            .WithArgs("server", "/data")
            .WithVolume("minio-data", "/data")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "50Gi"));

        var minio = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "minio");

        Assert.Equal("""
            minio:
              kind: stateful
              image: minio/minio:RELEASE.2025-09-07T16-13-09Z
              command: [server, /data]
              vm: data-1
              request: { cpu: 500m, memory: 1Gi }
              port: 9000
              env:
                MINIO_BROWSER: "off"
              volume: { path: /data, size: 50Gi }

            """, minio);
    }

    [Theory]
    [InlineData("docker.io/library/postgres", "17.6", null, "postgres:17.6")]
    [InlineData("library/postgres", "17.6", null, "postgres:17.6")]
    [InlineData("docker.io/bitnami/redis", "7.4", null, "bitnami/redis:7.4")]
    [InlineData("ghcr.io/protofast/store", "1.2.3", null, "ghcr.io/protofast/store:1.2.3")]
    [InlineData("postgres", "17.6", "4f0c3a2c1b7e5e0f4e7c0b1d9a8f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f", "postgres@sha256:4f0c3a2c1b7e5e0f4e7c0b1d9a8f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b0a9f")]
    public async Task ImagesAreNamedAsDockerWouldPullThem(string image, string tag, string? sha256, string expected)
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var store = builder.AddContainer("store", image, tag)
            .WithEndpoint(targetPort: 5432, name: "tcp")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi", path: "/data"));
        if (sha256 is not null)
            store.WithImageSHA256(sha256);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains($"\n  image: {expected}\n", SpecText.Component(yaml, "store"));
    }

    [Fact]
    public async Task ImagesMustBePinned()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddContainer("latest", "minio/minio", "latest")
            .WithEndpoint(targetPort: 9000, name: "tcp")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi", path: "/data"));
        builder.AddContainer("untagged", "minio/minio")
            .WithEndpoint(targetPort: 9000, name: "tcp")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi", path: "/data"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "latest: image minio/minio:latest must be pinned to a tag other than latest, or a digest",
            "untagged: image minio/minio:latest must be pinned to a tag other than latest, or a digest",
        ], problems);
    }

    [Fact]
    public async Task SecretsCantReachAnUpstreamImage()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var password = builder.AddParameter("minio-password", secret: true);
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .WithHttpEndpoint(targetPort: 9000, name: "api")
            .WithEnvironment("MINIO_ROOT_PASSWORD", password)
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "50Gi", path: "/data"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
            ["minio: MINIO_ROOT_PASSWORD holds a secret or a connection detail, which would be written into a plain environment variable: a stateful component runs an upstream image, which can't read its secret, so call SecretFiles() and read it from /run/secrets/MINIO_ROOT_PASSWORD"],
            problems);
    }

    [Fact]
    public async Task MissingSettingsAreReportedWithTheCallToMake()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .PublishAsProtoStrapStateful(_ => { });
        builder.AddRedis("redis")
            .PublishAsProtoStrapStateful(s => s.Vm("data-2").Request(cpu: "500m", memory: "1Gi").Volume(size: "5.5Gi"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "minio: no data VM: call Vm(\"data-1\")",
            "minio: no request: call Request(cpu: ..., memory: ...)",
            "minio: no port: give it an endpoint with a target port, such as WithEndpoint(targetPort: 9000, name: \"tcp\")",
            "minio: no volume: call Volume(size: ..., path: ...)",
            "redis: Vm: there is no data-2: the data pool has data-1 to data-1",
            "redis: Volume: size \"5.5Gi\" isn't a whole number of GiB, like \"100Gi\"",
        ], problems);
    }

    [Fact]
    public async Task OnlyOneVolumeAndNoEntrypoint()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddContainer("minio", "minio/minio", "RELEASE.2025-09-07T16-13-09Z")
            .WithEndpoint(targetPort: 9000, name: "tcp")
            .WithEntrypoint("/bin/sh")
            .WithVolume("one", "/data")
            .WithVolume("two", "/config")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "minio: WithEntrypoint isn't supported: a component's image runs with its own entrypoint",
            "minio: 2 volumes, at /data and /config: a stateful component has one volume",
        ], problems);
    }

    [Fact]
    public async Task OnlyStatefulComponentsKeepData()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        builder.AddDockerfile("web", repo.PathOf("services/web"))
            .WithHttpEndpoint(targetPort: 3000)
            .WithVolume("cache", "/cache")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["web: a volume at /cache: only a stateful component keeps data, so replicas can come and go"], problems);
    }

    [Fact]
    public async Task AStatefulComponentRunsAnUpstreamImage()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("store", repo.Project("services/store/Store.csproj"))
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi", path: "/data"));
        repo.Write("services/cache/Dockerfile", "FROM redis:8.6\n");
        builder.AddDockerfile("cache", repo.PathOf("services/cache"))
            .WithEndpoint(targetPort: 6379, name: "tcp")
            .PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "500m", memory: "1Gi").Volume(size: "5Gi", path: "/data"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "cache: a stateful component runs an upstream image, pinned to a tag, not one built from a Dockerfile",
            "store: a stateful component runs an upstream image, pinned to a tag, not a project",
        ], problems);
    }
}
