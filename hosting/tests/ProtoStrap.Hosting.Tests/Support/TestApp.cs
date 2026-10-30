using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.DependencyInjection;
using ProtoStrap.Hosting.Sources;
using ProtoStrap.Hosting.Spec;

namespace ProtoStrap.Hosting.Tests.Support;

/// <summary>TestApp makes AppHost builders over a fixture repository, and generates their specs.</summary>
internal static class TestApp
{
    /// <summary>
    /// Publish makes a builder in publish mode, as `aspire publish` and `aspire do` start the
    /// AppHost.
    /// </summary>
    public static IDistributedApplicationBuilder Publish(FixtureRepo repo, params string[] extraArgs) =>
        DistributedApplication.CreateBuilder(new DistributedApplicationOptions
        {
            Args = ["--operation", "publish", "--output-path", Path.Combine(repo.Root, "aspire-output"), .. extraArgs],
            DisableDashboard = true,
            ProjectDirectory = repo.AppHost,
        });

    /// <summary>
    /// Run makes a builder in run mode, as `aspire run` starts it. It is never built, since that
    /// needs DCP. The AWS region the machine running the tests may have is cleared, so
    /// LocalStack's is the default.
    /// </summary>
    public static IDistributedApplicationBuilder Run(FixtureRepo repo)
    {
        var builder = DistributedApplication.CreateBuilder(new DistributedApplicationOptions
        {
            Args = [],
            DisableDashboard = true,
            ProjectDirectory = repo.AppHost,
        });
        builder.Configuration["AWS_REGION"] = null;
        builder.Configuration["AWS_DEFAULT_REGION"] = null;
        return builder;
    }

    /// <summary>
    /// AddEnvironment adds the ProtoStrap environment the tests use unless they say otherwise:
    /// the pools from docs/protostrap-aspire.md, with a version and no hosts.
    /// </summary>
    public static IResourceBuilder<ProtoStrapEnvironmentResource> AddEnvironment(this IDistributedApplicationBuilder builder) =>
        builder.AddProtoStrapEnvironment("protostrap", app: "protofast")
            .WithVersion("1.4.0")
            .WithIngressPool(vm: "c8g.medium")
            .WithGeneralPool(vm: ["m7g.large", "m6g.large", "m8g.large", "m7gd.large"], min: 1, max: 6)
            .WithComputePool(vm: ["c7g.xlarge", "c6g.xlarge", "c8g.xlarge", "c7gd.xlarge"], min: 0, max: 4)
            .WithDataPool(vm: "m7g.xlarge", count: 1);

    /// <summary>
    /// GenerateAsync builds the app and generates its spec, without writing it. The tracer
    /// defaults to one that lists only each project's own directory, so tests that aren't about
    /// sources don't run MSBuild.
    /// </summary>
    public static async Task<SpecResult> GenerateAsync(IDistributedApplicationBuilder builder, ISourceTracer? tracer = null)
    {
        await using var app = builder.Build();
        var model = app.Services.GetRequiredService<DistributedApplicationModel>();
        var execution = app.Services.GetRequiredService<DistributedApplicationExecutionContext>();
        var environment = model.Resources.OfType<ProtoStrapEnvironmentResource>().Single();
        var generator = new SpecGenerator(model, execution, environment, root => tracer ?? new ProjectDirectoryTracer(root));
        return await generator.GenerateAsync(CancellationToken.None);
    }

    /// <summary>GenerateYamlAsync generates the spec and fails the test if it has problems.</summary>
    public static async Task<string> GenerateYamlAsync(IDistributedApplicationBuilder builder, ISourceTracer? tracer = null)
    {
        var result = await GenerateAsync(builder, tracer);
        Assert.True(result.Problems.Count == 0, "Unexpected problems:\n" + string.Join("\n", result.Problems));
        return result.Yaml!;
    }

    /// <summary>
    /// ProblemsAsync generates the spec and returns its problems, failing the test if it has
    /// none.
    /// </summary>
    public static async Task<IReadOnlyList<string>> ProblemsAsync(IDistributedApplicationBuilder builder, ISourceTracer? tracer = null)
    {
        var result = await GenerateAsync(builder, tracer);
        Assert.True(result.Problems.Count > 0, "Expected problems, got a spec:\n" + result.Yaml);
        Assert.Null(result.Yaml);
        return result.Problems;
    }
}

/// <summary>
/// ProjectDirectoryTracer gives each project its own directory and a global.json at the root,
/// pinning the SDK, as its sources.
/// </summary>
internal sealed class ProjectDirectoryTracer(string root) : ISourceTracer
{
    public Task<ProjectSources> TraceAsync(string projectPath, CancellationToken cancellationToken) =>
        Task.FromResult(new ProjectSources(
            [.. new[] { Path.GetRelativePath(root, Path.GetDirectoryName(projectPath)!).Replace('\\', '/'), "global.json" }.Order(StringComparer.Ordinal)],
            GlobalJson: "global.json",
            SdkVersion: "10.0.100"));
}
