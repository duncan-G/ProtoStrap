using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// A component takes its config in one of three ways (docs/config.md): in its image, as config
/// files from a directory of the repository, or through its API from a configure task. The
/// AppHost says which with WithProtoStrapConfig and WithProtoStrapConfigure, and both run the
/// same way under `aspire run` as in an account.
/// </summary>
public class ConfigTests
{
    [Fact]
    public async Task AComponentGetsConfigFilesFromADirectoryOfTheRepository()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/postgres/pg_hba.conf", "local all all trust\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddPostgres("postgres")
            .WithProtoStrapConfig("../config/postgres", "/etc/postgresql/conf")
            .PublishAsProtoStrapStateful(s => s
                .Vm("data-1").Request(cpu: "1000m", memory: "2Gi").Volume(size: "20Gi")
                .OnConfigChange(ConfigChange.Run("sh", "-c", "psql -X -q -U postgres -c 'SELECT pg_reload_conf()'")));

        var postgres = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "postgres");

        Assert.Contains("\n  config: { from: config/postgres, path: /etc/postgresql/conf }\n", postgres);
        Assert.Contains("\n  on_config_change: [sh, -c, psql -X -q -U postgres -c 'SELECT pg_reload_conf()']\n", postgres);
    }

    [Fact]
    public async Task AServiceCanReadNewConfigFilesItself()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/gateway/lds.yaml", "resources: []\n");
        repo.Write("services/gateway/Dockerfile", "FROM envoyproxy/envoy:v1.40.2\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("gateway", "../services/gateway")
            .WithHttpEndpoint(targetPort: 8080)
            .WithHttpHealthCheck("/ready")
            .WithProtoStrapConfig("../config/gateway", "/etc/envoy/xds")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").OnConfigChange(ConfigChange.Reload));

        Assert.Equal("""
            gateway:
              kind: service
              build: services/gateway
              pool: general
              request: { cpu: 250m, memory: 256Mi }
              port: 8080
              ready: /ready
              config: { from: config/gateway, path: /etc/envoy/xds }
              on_config_change: reload

            """, SpecText.Component(await TestApp.GenerateYamlAsync(builder), "gateway"));
    }

    [Fact]
    public async Task ATaskReadsItsConfigFilesEachTimeItRuns()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/idp/realm.json", "{}\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("idp-config", repo.Project("services/idp-config/IdpConfig.csproj"))
            .WithProtoStrapConfig("../config/idp", "/config")
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi"));

        var task = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "idp-config");

        Assert.Contains("\n  config: { from: config/idp, path: /config }\n", task);
        Assert.DoesNotContain("on_config_change", task);
    }

    [Fact]
    public async Task ConfigFilesMustComeFromADirectoryInTheRepository()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/postgres.conf", "max_connections = 50\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddPostgres(builder).WithProtoStrapConfig("../config/postgres.conf", "/etc/postgresql/conf");
        AddPostgres(builder, "other").WithProtoStrapConfig("../../elsewhere", "/etc/postgresql/conf");
        AddPostgres(builder, "missing").WithProtoStrapConfig("../config/nothing", "/etc/postgresql/conf");
        AddPostgres(builder, "whole").WithProtoStrapConfig("..", "/etc/postgresql/conf");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("postgres: WithProtoStrapConfig: config/postgres.conf isn't a directory: config files come from a directory of the repository", problems);
        Assert.Contains(problems, p => p.StartsWith("other: WithProtoStrapConfig: ", StringComparison.Ordinal) && p.EndsWith("is outside the repository, where protostrap.yaml is", StringComparison.Ordinal));
        Assert.Contains("missing: WithProtoStrapConfig: config/nothing isn't a directory: config files come from a directory of the repository", problems);
        Assert.Contains("whole: WithProtoStrapConfig: the whole repository can't be config files: name a directory in it", problems);
    }

    [Fact]
    public async Task ConfigFilesAreMountedAtAnAbsolutePathOutsideTheSecretFiles()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/postgres/pg_hba.conf", "local all all trust\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddPostgres(builder).WithProtoStrapConfig("../config/postgres", "etc/postgresql");
        AddPostgres(builder, "other").WithProtoStrapConfig("../config/postgres", "/run/secrets/conf");
        AddPostgres(builder, "root").WithProtoStrapConfig("../config/postgres", "/");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("postgres: WithProtoStrapConfig: \"etc/postgresql\" must be an absolute path in the container", problems);
        Assert.Contains("other: WithProtoStrapConfig: /run/secrets/conf would cover or sit in the secret files at /run/secrets", problems);
        Assert.Contains("root: WithProtoStrapConfig: can't be the root of the container", problems);
    }

    [Fact]
    public async Task AComponentHasOneConfigDirectory()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/a/x.conf", "\n");
        repo.Write("config/b/y.conf", "\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddPostgres(builder)
            .WithProtoStrapConfig("../config/a", "/etc/a")
            .WithProtoStrapConfig("../config/b", "/etc/b");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["postgres: WithProtoStrapConfig: called twice, for /etc/a and /etc/b: a component has one config directory"], problems);
    }

    [Fact]
    public async Task ANewFileNeedsConfigFilesToChange()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddPostgres("postgres").PublishAsProtoStrapStateful(s => s
            .Vm("data-1").Request(cpu: "1000m", memory: "2Gi").Volume(size: "20Gi")
            .OnConfigChange(ConfigChange.Reload));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["postgres: OnConfigChange: it has no config files to change: call WithProtoStrapConfig(...)"], problems);
    }

    [Fact]
    public async Task OtherBindMountsAreStillRefused()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/postgres/pg_hba.conf", "local all all trust\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddPostgres(builder)
            .WithProtoStrapConfig("../config/postgres", "/etc/postgresql/conf")
            .WithBindMount("../config/postgres", "/docker-entrypoint-initdb.d");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["postgres: bind mount at /docker-entrypoint-initdb.d: a replica can't mount the repository, so copy the files into the image, or make them config files with WithProtoStrapConfig"], problems);
    }

    [Fact]
    public void RunModeBindMountsConfigFilesReadOnly()
    {
        using var repo = new FixtureRepo();
        repo.Write("config/postgres/pg_hba.conf", "local all all trust\n");
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        var postgres = builder.AddPostgres("postgres").WithProtoStrapConfig("../config/postgres", "/etc/postgresql/conf");

        var mount = Assert.Single(postgres.Resource.Annotations.OfType<ContainerMountAnnotation>(), m => m.Type == ContainerMountType.BindMount);
        Assert.Equal(repo.PathOf("config/postgres"), mount.Source);
        Assert.Equal("/etc/postgresql/conf", mount.Target);
        Assert.True(mount.IsReadOnly);
    }

    [Fact]
    public async Task AComponentIsConfiguredByATaskOnceItIsReady()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/idp/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var adminSecret = builder.AddParameter("idp-admin-secret", secret: true);
        var idp = builder.AddDockerfile("idp", "../services/idp")
            .WithHttpEndpoint(targetPort: 8080)
            .WithHttpHealthCheck("/health/ready")
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", adminSecret)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").SecretFiles());
        var idpConfig = builder.AddProject("idp-config", repo.Project("services/idp-config/IdpConfig.csproj"))
            .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", adminSecret)
            .WithEnvironment("Idp__Url", idp.GetEndpoint("http"))
            .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "256Mi").SecretPrefix("Idp_"));
        idp.WithProtoStrapConfigure(idpConfig);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // The task waits for the component it configures, and reads its keys with its prefix,
        // so both read one value of the admin client's secret.
        Assert.Contains("\n  configure: idp-config\n", SpecText.Component(yaml, "idp"));
        Assert.Equal("""
            idp-config:
              kind: task
              project: services/idp-config/IdpConfig.csproj
              sources:
                - global.json
                - services/idp-config
              pool: general
              request: { cpu: 250m, memory: 256Mi }
              depends_on: [idp]
              env:
                Idp__Url: http://idp:8080
                OTEL_DOTNET_EXPERIMENTAL_OTLP_RETRY: in_memory
              secrets: [KC_BOOTSTRAP_ADMIN_CLIENT_SECRET]
              secret_prefix: Idp_

            """, SpecText.Component(yaml, "idp-config"));
    }

    [Fact]
    public async Task OnlyATaskConfiguresAComponent()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var postgres = AddPostgres(builder);
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Grpc));
        postgres.WithProtoStrapConfigure(api);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["postgres: WithProtoStrapConfigure: api is a service, not a task: a configure task is published with PublishAsProtoStrapTask"], problems);
    }

    [Fact]
    public async Task EachComponentHasItsOwnConfigureTask()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var task = AddTask(builder, repo, "db-config");
        AddPostgres(builder).WithProtoStrapConfigure(task);
        AddPostgres(builder, "other").WithProtoStrapConfigure(task);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["other: WithProtoStrapConfigure: db-config already configures postgres: each component has its own configure task"], problems);
    }

    [Fact]
    public async Task AConfigureTaskNeverRunsAsAMigration()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var task = AddTask(builder, repo, "db-config");
        AddPostgres(builder).WithProtoStrapConfigure(task);
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WaitForCompletion(task)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready(Readiness.Grpc));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["api: waits for the completion of db-config, which configures postgres once it is ready: wait for postgres with WaitFor, which waits for its configure task too"], problems);
    }

    [Fact]
    public async Task ATaskIsNotConfigured()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var config = AddTask(builder, repo, "db-config");
        AddTask(builder, repo, "migrations").WithProtoStrapConfigure(config);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("migrations: WithProtoStrapConfigure: only services, workers and stateful components are configured", problems);
    }

    [Fact]
    public async Task ASecretPrefixMustBeOneProtoStrapAccepts()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddTask(builder, repo, "one", t => t.SecretPrefix("idp_"));
        AddTask(builder, repo, "two", t => t.SecretPrefix("Shared_"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("one: SecretPrefix: \"idp_\" must be a capital letter, then letters and digits, then _, such as Api_", problems);
        Assert.Contains("two: SecretPrefix: Shared_ is reserved: Shared_ keys are every component's, and Edge_ and Reporter_ are ProtoStrap's", problems);
    }

    [Fact]
    public void RunModeRunsTheConfigureTaskBeforeWhatDependsOnTheComponent()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        var postgres = builder.AddPostgres("postgres");
        var db = postgres.AddDatabase("shop-db", databaseName: "shop");
        var config = builder.AddProject("db-config", repo.Project("services/db-config/DbConfig.csproj"));
        postgres.WithProtoStrapConfigure(config);
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web")).WaitFor(db);
        var worker = builder.AddProject("worker", repo.Project("services/worker/Worker.csproj")).WaitFor(postgres);

        // The waits are added once the model is complete, before anything starts.
        ProtoStrapComponentExtensions.WaitForConfigureTasks(builder.Resources);

        Assert.Contains(config.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == postgres.Resource && w.WaitType == WaitType.WaitUntilHealthy);
        foreach (var dependent in new IResource[] { api.Resource, worker.Resource })
            Assert.Contains(dependent.Annotations.OfType<WaitAnnotation>(), w => w.Resource == config.Resource && w.WaitType == WaitType.WaitForCompletion);
        Assert.DoesNotContain(config.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == config.Resource);

        // Adding them again changes nothing.
        ProtoStrapComponentExtensions.WaitForConfigureTasks(builder.Resources);
        Assert.Single(api.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == config.Resource);
    }

    private static IResourceBuilder<PostgresServerResource> AddPostgres(IDistributedApplicationBuilder builder, string name = "postgres") =>
        builder.AddPostgres(name).PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "250m", memory: "512Mi").Volume(size: "5Gi"));

    private static IResourceBuilder<ProjectResource> AddTask(IDistributedApplicationBuilder builder, FixtureRepo repo, string name, Action<ProtoStrapTask>? configure = null) =>
        builder.AddProject(name, repo.Project($"services/{name}/{name}.csproj"))
            .PublishAsProtoStrapTask(t =>
            {
                t.Request(cpu: "250m", memory: "256Mi");
                configure?.Invoke(t);
            });
}
