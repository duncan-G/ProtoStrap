using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Under `aspire run`, with ProtoStrap.Hosting built from a checkout, protostrap-build builds the
/// protostrap the edge renders with: as the AppHost starts, whenever the edge starts, and on its
/// Rebuild command. Go's build cache makes a build a no-op when nothing changed. It says when the
/// checkout has moved on since (docs/protostrap-aspire.md, "The local edge").
/// </summary>
public class LocalBuildTests
{
    [Fact]
    public void RunModeAddsTheBuildUnderTheEdge()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");

        var build = Assert.Single(builder.Resources, r => r.Name == "protostrap-build");
        var parent = Assert.Single(build.Annotations.OfType<ResourceRelationshipAnnotation>());
        Assert.Equal(("protostrap-edge", "Parent"), (parent.Resource.Name, parent.Type));
        Assert.True(build.IsExcludedFromPublish());
        var rebuild = Assert.Single(build.Annotations.OfType<ResourceCommandAnnotation>(), c => c.Name == "rebuild-protostrap");
        Assert.Equal("Rebuild", rebuild.DisplayName);
    }

    [Fact]
    public void PublishModeHasNoBuild()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");

        Assert.DoesNotContain(builder.Resources, r => r.Name == "protostrap-build");
    }

    [Fact]
    public async Task ABuildIsUpToDateUntilTheCheckoutChanges()
    {
        using var repo = new FixtureRepo();
        var (fake, build, logger) = Build(repo, new FakeGo(repo, onPath: true));

        var path = await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        await build.CheckAsync();

        Assert.Equal(fake.Output, path);
        Assert.Equal("Up to date", build.State);
        Assert.Single(fake.Builds);

        // A file that goes into protostrap changes after the build started.
        File.SetLastWriteTimeUtc(repo.Write("protostrap/internal/edge/config.go", "package edge\n"), DateTime.UtcNow.AddMinutes(1));
        await build.CheckAsync();

        Assert.Equal("Behind", build.State);
        var (level, message) = Assert.Single(logger.Entries, e => e.Message.Contains("internal/edge/config.go"));
        Assert.Equal(LogLevel.Warning, level);
        Assert.Contains("Rebuild", message);

        // It says so once, not at every check.
        await build.CheckAsync();
        Assert.Single(logger.Entries, e => e.Message.Contains("internal/edge/config.go"));
    }

    [Fact]
    public async Task ChangesThatDontGoIntoTheBinaryLeaveItUpToDate()
    {
        using var repo = new FixtureRepo();
        var (_, build, _) = Build(repo, new FakeGo(repo, onPath: true));
        await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);

        File.SetLastWriteTimeUtc(repo.Write("protostrap/docs/protostrap.md", "changed\n"), DateTime.UtcNow.AddMinutes(1));
        File.SetLastWriteTimeUtc(repo.Write("protostrap/internal/edge/config_test.go", "changed\n"), DateTime.UtcNow.AddMinutes(1));
        await build.CheckAsync();

        Assert.Equal("Up to date", build.State);
    }

    [Fact]
    public async Task EveryEnsureBuildsAgainSoItFollowsTheCheckout()
    {
        // The edge ensures it each time it starts; Go's build cache makes that a no-op, in a tenth
        // of a second, when nothing changed, and rebuilds what did.
        using var repo = new FixtureRepo();
        var (fake, build, _) = Build(repo, new FakeGo(repo, onPath: true));
        await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        File.SetLastWriteTimeUtc(repo.Write("protostrap/internal/edge/config.go", "package edge\n"), DateTime.UtcNow.AddMinutes(1));
        await build.CheckAsync();

        // The file's time is in the future, so it is behind still, but it was built again.
        await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(2, fake.Builds.Length);
    }

    [Fact]
    public async Task AFailedBuildSaysWhy()
    {
        using var repo = new FixtureRepo();
        var (_, build, logger) = Build(repo, new FakeGo(repo, onPath: true, fails: "internal/edge/config.go:3:1: syntax error"));

        await Assert.ThrowsAsync<InvalidOperationException>(() => build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Equal("Build failed", build.State);
        Assert.Contains(logger.Entries, e => e.Level == LogLevel.Error && e.Message.Contains("config.go:3:1: syntax error"));
    }

    [Fact]
    public async Task RebuildBuildsAndThenRestartsTheEdge()
    {
        using var repo = new FixtureRepo();
        var (fake, build, _) = Build(repo, new FakeGo(repo, onPath: true));
        await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);
        var builds = 0;

        var problem = await build.RebuildAsync(_ =>
        {
            builds = fake.Builds.Length;
            return Task.FromResult<string?>(null);
        }, TestContext.Current.CancellationToken);

        Assert.Null(problem);
        Assert.Equal(2, builds);
        Assert.Equal("Up to date", build.State);
    }

    [Fact]
    public async Task AFailedRebuildLeavesTheEdgeAsItIs()
    {
        using var repo = new FixtureRepo();
        var (_, build, _) = Build(repo, new FakeGo(repo, onPath: true, fails: "internal/edge/config.go:3:1: syntax error"));
        var restarted = false;

        var problem = await build.RebuildAsync(_ =>
        {
            restarted = true;
            return Task.FromResult<string?>(null);
        }, TestContext.Current.CancellationToken);

        Assert.Contains("config.go:3:1: syntax error", problem);
        Assert.False(restarted);
        Assert.Equal("Build failed", build.State);
    }

    [Fact]
    public async Task ABinaryWithCheckNamesIsntBuiltAndTheBuildIsHidden()
    {
        using var repo = new FixtureRepo();
        var named = repo.WriteExecutable("bin/protostrap", "#!/bin/sh\n");
        var fake = new FakeGo(repo, onPath: true);
        var (_, build, _) = Build(repo, fake, check: named);

        var path = await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(named, path);
        Assert.Empty(fake.Builds);
        Assert.Equal(LocalBuild.Hidden, build.State);
    }

    [Fact]
    public async Task WithoutGoItSaysSoAndTheEdgeUsesProtoStrapOnThePath()
    {
        using var repo = new FixtureRepo();
        var installed = repo.WriteExecutable("path/protostrap", "#!/bin/sh\n");
        var (_, build, logger) = Build(repo, new FakeGo(repo, onPath: false), path: repo.PathOf("path"));

        var path = await build.EnsureAsync(NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(installed, path);
        Assert.Equal("Go not found", build.State);
        Assert.Contains(logger.Entries, e => e.Message.Contains($"the edge renders with {installed}"));
    }

    /// <summary>
    /// Build adds an environment with a host, so the edge and its build, and has the build see
    /// only the fixture, with every checkout file older than any build.
    /// </summary>
    private static (FakeGo Fake, LocalBuild Build, ListLogger Logger) Build(FixtureRepo repo, FakeGo fake, string? check = null, string? path = null)
    {
        var builder = TestApp.Run(repo);
        var protostrap = builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        if (check is not null)
            protostrap.WithCheck(check);
        protostrap.Resource.Binary = fake.Binary(path: path);
        fake.Touch(DateTime.UtcNow.AddHours(-1));
        var build = protostrap.Resource.LocalBuild!;
        var logger = new ListLogger();
        build.Logger = logger;
        return (fake, build, logger);
    }
}
