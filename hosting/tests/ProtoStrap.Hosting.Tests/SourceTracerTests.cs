using Aspire.Hosting;
using ProtoStrap.Hosting.Sources;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// A project's sources are what MSBuild sees: its ProjectReference closure's directories, the
/// MSBuild files the evaluation imported from inside the repository, nuget.config, global.json,
/// and items outside their project's directory (docs/protostrap-aspire.md, "Image tags"). These
/// tests run MSBuild on real projects.
/// </summary>
public class SourceTracerTests
{
    private static async Task<IReadOnlyList<string>> TraceAsync(FixtureRepo repo, string project, string? root = null) =>
        (await new MSBuildSourceTracer(root is null ? repo.Root : repo.PathOf(root)).TraceAsync(repo.PathOf(project), CancellationToken.None)).Paths;

    [Fact]
    public async Task TheSourcesAreTheReferenceClosure()
    {
        using var repo = new FixtureRepo();
        repo.Project("services/shared/Database.Abstractions/Database.Abstractions.csproj");
        repo.Project("services/shared/Database/Database.csproj", references: "services/shared/Database.Abstractions/Database.Abstractions.csproj");
        repo.Project("services/shared/ServiceDefaults/ServiceDefaults.csproj");
        repo.Project("services/shared/Storage/Storage.csproj");
        repo.Project("services/api/src/ProtoFast.Data/ProtoFast.Data.csproj", references: "services/shared/Database/Database.csproj");
        repo.Project("services/api/src/ProtoFast.Api/ProtoFast.Api.csproj", "Microsoft.NET.Sdk.Web", references:
        [
            "services/api/src/ProtoFast.Data/ProtoFast.Data.csproj",
            "services/shared/ServiceDefaults/ServiceDefaults.csproj",
        ]);
        repo.Project("services/api/tests/ProtoFast.Api.Tests/ProtoFast.Api.Tests.csproj", references: "services/api/src/ProtoFast.Api/ProtoFast.Api.csproj");
        repo.Project("services/payments/src/ProtoFast.Payments.Api/ProtoFast.Payments.Api.csproj", "Microsoft.NET.Sdk.Web", references: "services/shared/ServiceDefaults/ServiceDefaults.csproj");

        Assert.Equal(
        [
            "services/api/src/ProtoFast.Api",
            "services/api/src/ProtoFast.Data",
            "services/shared/Database",
            "services/shared/Database.Abstractions",
            "services/shared/ServiceDefaults",
        ], await TraceAsync(repo, "services/api/src/ProtoFast.Api/ProtoFast.Api.csproj"));
        Assert.Equal(
        [
            "services/payments/src/ProtoFast.Payments.Api",
            "services/shared/ServiceDefaults",
        ], await TraceAsync(repo, "services/payments/src/ProtoFast.Payments.Api/ProtoFast.Payments.Api.csproj"));
    }

    [Fact]
    public async Task ImportedMSBuildFilesInsideTheRepositoryAreSources()
    {
        using var repo = new FixtureRepo();
        repo.Write("Directory.Build.props", "<Project><PropertyGroup><Nullable>enable</Nullable></PropertyGroup></Project>\n");
        repo.Write("Directory.Packages.props", "<Project><PropertyGroup><ManagePackageVersionsCentrally>false</ManagePackageVersionsCentrally></PropertyGroup></Project>\n");
        repo.Write("Directory.Build.targets", "<Project><Import Project=\"build/common.targets\" /></Project>\n");
        repo.Write("build/common.targets", "<Project />\n");
        repo.Write("build/unused.targets", "<Project />\n");
        repo.Write("services/Directory.Build.props", """
            <Project>
              <Import Project="$([MSBuild]::GetPathOfFileAbove('Directory.Build.props', '$(MSBuildThisFileDirectory)../'))" />
            </Project>

            """);
        repo.Project("services/api/Api.csproj");

        Assert.Equal(
        [
            "Directory.Build.props",
            "Directory.Build.targets",
            "Directory.Packages.props",
            "build/common.targets",
            "services/Directory.Build.props",
            "services/api",
        ], await TraceAsync(repo, "services/api/Api.csproj"));
    }

    [Fact]
    public async Task NuGetConfigAndGlobalJsonAreSources()
    {
        using var repo = new FixtureRepo();
        repo.Write("nuget.config", "<configuration />\n");
        repo.Write("services/NuGet.Config", "<configuration />\n");
        repo.Write("global.json", """{ "sdk": { "version": "10.0.100", "rollForward": "latestFeature" } }""");
        repo.Write("other/nuget.config", "<configuration />\n");
        repo.Project("services/api/Api.csproj");

        Assert.Equal(
        [
            "global.json",
            "nuget.config",
            "services/NuGet.Config",
            "services/api",
        ], await TraceAsync(repo, "services/api/Api.csproj"));
    }

    [Fact]
    public async Task TheSdkIsReadFromTheNearestGlobalJson()
    {
        using var repo = new FixtureRepo();
        // The SDK reads comments in global.json, and so does the tracer.
        repo.Write("global.json", """
            {
              // Every project builds with this SDK.
              "sdk": { "version": "10.0.100", "rollForward": "latestFeature" }
            }

            """);
        repo.Write("services/legacy/global.json", """{ "sdk": { "version": "10.0.101", "rollForward": "latestFeature" } }""");
        repo.Project("services/api/Api.csproj");
        repo.Project("services/legacy/Legacy.csproj");
        var tracer = new MSBuildSourceTracer(repo.Root);

        var api = await tracer.TraceAsync(repo.PathOf("services/api/Api.csproj"), CancellationToken.None);
        var legacy = await tracer.TraceAsync(repo.PathOf("services/legacy/Legacy.csproj"), CancellationToken.None);

        Assert.Equal(("global.json", "10.0.100"), (api.GlobalJson, api.SdkVersion));
        Assert.Equal(("services/legacy/global.json", "10.0.101"), (legacy.GlobalJson, legacy.SdkVersion));
        // The project's directory covers its own global.json.
        Assert.Equal(["services/legacy"], legacy.Paths);
    }

    [Fact]
    public async Task AGlobalJsonThatDoesNotPinTheSdkHasNoVersion()
    {
        using var repo = new FixtureRepo();
        repo.Write("global.json", """{ "test": { "runner": "Microsoft.Testing.Platform" } }""");
        repo.Project("services/api/Api.csproj");

        var traced = await new MSBuildSourceTracer(repo.Root).TraceAsync(repo.PathOf("services/api/Api.csproj"), CancellationToken.None);

        Assert.Equal("global.json", traced.GlobalJson);
        Assert.Contains("global.json", traced.Paths);
        Assert.Null(traced.SdkVersion);
    }

    [Fact]
    public async Task AGlobalJsonOutsideTheRootPinsNothing()
    {
        using var repo = new FixtureRepo();
        repo.Write("global.json", """{ "sdk": { "version": "10.0.100", "rollForward": "latestFeature" } }""");
        repo.Project("app/services/api/Api.csproj");

        // The root is app/, where protostrap.yaml is, so the image tag can't hash the
        // global.json above it.
        var traced = await new MSBuildSourceTracer(repo.PathOf("app")).TraceAsync(repo.PathOf("app/services/api/Api.csproj"), CancellationToken.None);

        Assert.Equal(["services/api"], traced.Paths);
        Assert.Null(traced.GlobalJson);
        Assert.Null(traced.SdkVersion);
    }

    [Fact]
    public async Task ItemsOutsideTheirProjectsDirectoryAreSources()
    {
        using var repo = new FixtureRepo();
        repo.Write("shared/Linked.cs", "class Linked {}\n");
        repo.Write("protos/api.proto", "syntax = \"proto3\";\n");
        repo.Write("infra/realms/protofast.json", "{}\n");
        repo.Write("infra/realms/theplot.json", "{}\n");
        repo.Write("infra/other.txt", "x\n");
        repo.Project("services/api/Api.csproj", extra: """
              <ItemGroup>
                <Compile Include="../../shared/Linked.cs" Link="Linked.cs" />
                <None Include="../../protos/api.proto" />
                <Content Include="../../infra/realms/*.json" />
                <None Include="local.txt" />
              </ItemGroup>
            """);
        repo.Write("services/api/local.txt", "x\n");

        Assert.Equal(
        [
            "infra/realms/protofast.json",
            "infra/realms/theplot.json",
            "protos/api.proto",
            "services/api",
            "shared/Linked.cs",
        ], await TraceAsync(repo, "services/api/Api.csproj"));
    }

    [Fact]
    public async Task AProjectInsideAnothersDirectoryIsCoveredByIt()
    {
        using var repo = new FixtureRepo();
        repo.Project("services/app/plugins/Plugin/Plugin.csproj");
        repo.Project("services/app/App.csproj", extra: """
              <ItemGroup>
                <Compile Remove="plugins/**" />
              </ItemGroup>
            """, references: "services/app/plugins/Plugin/Plugin.csproj");

        Assert.Equal(["services/app"], await TraceAsync(repo, "services/app/App.csproj"));
    }

    [Fact]
    public async Task OnlyWhatIsInsideTheRootCounts()
    {
        using var repo = new FixtureRepo();
        repo.Write("Directory.Build.props", "<Project />\n");
        repo.Write("shared/Linked.cs", "class Linked {}\n");
        repo.Project("app/services/api/Api.csproj", extra: """
              <ItemGroup>
                <Compile Include="../../../shared/Linked.cs" Link="Linked.cs" />
              </ItemGroup>
            """);

        // The root is app/, where protostrap.yaml is: the SDK's own files, the parent
        // Directory.Build.props and the linked file are outside it.
        Assert.Equal(["services/api"], await TraceAsync(repo, "app/services/api/Api.csproj", root: "app"));
    }

    [Fact]
    public async Task TheBaseImageIsReadFromTheProject()
    {
        using var repo = new FixtureRepo();
        repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web", extra: """
              <PropertyGroup>
                <ContainerBaseImage>mcr.microsoft.com/dotnet/aspnet:10.0@sha256:4f0c3a2c</ContainerBaseImage>
              </PropertyGroup>
            """);
        repo.Project("services/worker/Worker.csproj");

        var tracer = new MSBuildSourceTracer(repo.Root);

        Assert.Equal("mcr.microsoft.com/dotnet/aspnet:10.0@sha256:4f0c3a2c",
            (await tracer.TraceAsync(repo.PathOf("services/api/Api.csproj"), CancellationToken.None)).ContainerBaseImage);
        Assert.Null((await tracer.TraceAsync(repo.PathOf("services/worker/Worker.csproj"), CancellationToken.None)).ContainerBaseImage);
    }

    [Fact]
    public async Task AProjectMSBuildCantReadFails()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/api/Api.csproj", "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup>\n");

        var error = await Assert.ThrowsAsync<SourceTraceException>(() => TraceAsync(repo, "services/api/Api.csproj"));

        Assert.Contains("Api.csproj", error.Message);
    }

    [Fact]
    public async Task AMissingProjectFails()
    {
        using var repo = new FixtureRepo();

        var error = await Assert.ThrowsAsync<SourceTraceException>(() => TraceAsync(repo, "services/api/Api.csproj"));

        Assert.Contains("services/api/Api.csproj", error.Message.Replace('\\', '/'));
    }

    [Fact]
    public async Task SharedProjectsAreEvaluatedOnce()
    {
        using var repo = new FixtureRepo();
        repo.Project("services/shared/Shared.csproj");
        repo.Project("services/a/A.csproj", references: "services/shared/Shared.csproj");
        repo.Project("services/b/B.csproj", references: "services/shared/Shared.csproj");
        var tracer = new MSBuildSourceTracer(repo.Root);

        await tracer.TraceAsync(repo.PathOf("services/a/A.csproj"), CancellationToken.None);
        await tracer.TraceAsync(repo.PathOf("services/b/B.csproj"), CancellationToken.None);

        Assert.Equal(3, tracer.Evaluations);
    }

    [Fact]
    public async Task TheGeneratorWritesTracedSources()
    {
        using var repo = new FixtureRepo();
        repo.Write("nuget.config", "<configuration />\n");
        repo.Write("global.json", """{ "sdk": { "version": "10.0.100", "rollForward": "latestFeature" } }""");
        repo.Project("services/shared/ServiceDefaults/ServiceDefaults.csproj");
        var api = repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web", references: "services/shared/ServiceDefaults/ServiceDefaults.csproj");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProject("api", api)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Aspire.Hosting.Readiness.Grpc));

        var result = await TestApp.GenerateAsync(builder, new MSBuildSourceTracer(repo.Root));

        Assert.Empty(result.Problems);
        Assert.Contains("""
              project: services/api/Api.csproj
              sources:
                - global.json
                - nuget.config
                - services/api
                - services/shared/ServiceDefaults
              pool: general

            """, SpecText.Component(result.Yaml!, "api"));
        // A floating base image is warned about: the tag would depend on when you deploy. The
        // SDK can't float, since global.json must pin it.
        Assert.Equal(["api: ContainerBaseImage isn't pinned by digest, so a patched base image changes nothing in the repository"], result.Warnings);
    }

    [Fact]
    public async Task ATraceThatFailsIsAProblem()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        // Aspire reads only the project's launch settings, but MSBuild can't read the project
        // itself.
        repo.Write("services/api/Api.csproj", "<Project Sdk=\"Microsoft.NET.Sdk\"><PropertyGroup>\n");
        builder.AddProject("api", repo.PathOf("services/api/Api.csproj"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Aspire.Hosting.Readiness.Grpc));

        var problems = await TestApp.ProblemsAsync(builder, new MSBuildSourceTracer(repo.Root));

        var problem = Assert.Single(problems);
        Assert.StartsWith("api: tracing its sources: ", problem);
    }
}
