using Aspire.Hosting;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// A Dockerfile build is its directory and context. Its sources are its build context, minus
/// .dockerignore; anything else it is built from is declared next to the resource
/// (docs/protostrap-aspire.md, "Image tags").
/// </summary>
public class DockerfileTests
{
    [Fact]
    public async Task ADockerfileBuildsFromItsDirectory()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("keycloak", "../services/keycloak")
            .WithHttpEndpoint(targetPort: 8080)
            .WithHttpHealthCheck("/health/ready")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi"));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            keycloak:
              kind: service
              build: services/keycloak
              pool: general
              request: { cpu: 500m, memory: 1Gi }
              port: 8080
              ready: /health/ready

            """, SpecText.Component(yaml, "keycloak"));
    }

    [Fact]
    public async Task AParentContextIsASource()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/dotnet/Api/Dockerfile", "FROM mcr.microsoft.com/dotnet/aspnet:10.0\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("api", repo.PathOf("services/dotnet"), "Api/Dockerfile")
            .WithHttpEndpoint(targetPort: 8080)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.StartsWith("""
            api:
              kind: service
              build: services/dotnet/Api
              context: services/dotnet
              sources:
                - services/dotnet
              pool: general
            """, api);
    }

    [Fact]
    public async Task DockerignoreIsLeftOutOfTheSources()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        repo.Write("services/web/.dockerignore", """
            # Not part of the image
            node_modules
            **/*.md
            /tests/
            coverage/**

            """);
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", repo.PathOf("services/web"))
            .WithHttpEndpoint(targetPort: 3000)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

        var web = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "web");

        Assert.StartsWith("""
            web:
              kind: service
              build: services/web
              sources:
                - ":(exclude,glob)services/web/**/*.md"
                - ":(exclude,glob)services/web/**/*.md/**"
                - ":(exclude,glob)services/web/coverage/**"
                - ":(exclude,glob)services/web/node_modules"
                - ":(exclude,glob)services/web/node_modules/**"
                - ":(exclude,glob)services/web/tests"
                - ":(exclude,glob)services/web/tests/**"
              pool: general
            """, web);
    }

    [Fact]
    public async Task ADockerfileNamedDockerfileDotSomethingIgnoreFileIsUsedFirst()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/dotnet/Api/Dockerfile", "FROM mcr.microsoft.com/dotnet/aspnet:10.0\n");
        repo.Write("services/dotnet/Api/Dockerfile.dockerignore", "**/bin\n");
        repo.Write("services/dotnet/.dockerignore", "everything\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("api", repo.PathOf("services/dotnet"), "Api/Dockerfile")
            .WithHttpEndpoint(targetPort: 8080)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var api = SpecText.Component(await TestApp.GenerateYamlAsync(builder), "api");

        Assert.Contains("""
              sources:
                - ":(exclude,glob)services/dotnet/**/bin"
                - ":(exclude,glob)services/dotnet/**/bin/**"
                - services/dotnet
              pool: general
            """, api);
    }

    [Fact]
    public async Task ANegatedPatternKeepsTheWholeContext()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        repo.Write("services/web/.dockerignore", "*.md\n!README.md\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", repo.PathOf("services/web"))
            .WithHttpEndpoint(targetPort: 3000)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

        var result = await TestApp.GenerateAsync(builder);

        Assert.Empty(result.Problems);
        Assert.DoesNotContain("sources:", SpecText.Component(result.Yaml!, "web"));
        Assert.Contains("web: services/web/.dockerignore re-includes files with !, which a pathspec can't say: every tracked file in the context counts toward the image tag", result.Warnings);
    }

    [Fact]
    public async Task ADockerfileMustBeCalledDockerfile()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/web/Dockerfile.prod", "FROM node:22-slim\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", repo.PathOf("services/web"), "Dockerfile.prod")
            .WithHttpEndpoint(targetPort: 3000)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["web: its Dockerfile is services/web/Dockerfile.prod: ProtoStrap builds the file called Dockerfile in a component's build directory"], problems);
    }

    [Fact]
    public async Task BuildArgumentsAndStagesArentSupported()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/web/Dockerfile", "FROM node:22-slim\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", repo.PathOf("services/web"), stage: "runtime")
            .WithBuildArg("NODE_VERSION", "22")
            .WithHttpEndpoint(targetPort: 3000)
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "web: builds the stage runtime: ProtoStrap builds a Dockerfile's last stage",
            "web: build arguments aren't supported: ProtoStrap builds with none, so the image tag says everything about it",
        ], problems);
    }

    [Fact]
    public async Task WithProtoStrapSourcesDeclaresWhatTheBuildCantSee()
    {
        using var repo = new FixtureRepo();
        repo.Write("clients/web/Dockerfile", "FROM node:22-slim\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddDockerfile("web", repo.PathOf("clients/web"))
            .WithHttpEndpoint(targetPort: 3000)
            .WithProtoStrapSources(":(glob)services/**/Protos/**", "buf.gen.yaml")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithProtoStrapSources("buf.gen.yaml")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("""
              sources:
                - ":(glob)services/**/Protos/**"
                - buf.gen.yaml

            """, SpecText.Component(yaml, "web"));
        Assert.Contains("""
              sources:
                - buf.gen.yaml
                - global.json
                - services/api

            """, SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task DeclaredSourcesMustStayInsideTheRepository()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .WithProtoStrapSources("../elsewhere", "/etc/passwd", "")
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "api: WithProtoStrapSources: \"../elsewhere\" must be a relative path or pathspec inside the repository",
            "api: WithProtoStrapSources: \"/etc/passwd\" must be a relative path or pathspec inside the repository",
            "api: WithProtoStrapSources: \"\" must be a relative path or pathspec inside the repository",
        ], problems);
    }
}
