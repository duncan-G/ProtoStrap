using System.Diagnostics;
using System.Runtime.CompilerServices;
using System.Text;
using System.Text.RegularExpressions;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Logging;
using Microsoft.Extensions.Logging.Abstractions;
using ProtoStrap.Hosting.Spec;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// The local edge is rendered by `protostrap edge render`: the protostrap WithCheck names, or
/// one the AppHost builds with the machine's Go from the ProtoStrap checkout ProtoStrap.Hosting
/// is built from, or else protostrap on PATH (docs/protostrap-aspire.md, "The local edge").
/// </summary>
public class ProtoStrapBinaryTests
{
    [Fact]
    public async Task WithACheckoutAndGoTheAppHostBuildsItsOwnEvenWithProtoStrapOnThePath()
    {
        // One on PATH may be from another commit, or from before the checkout changed.
        using var repo = new FixtureRepo();
        repo.WriteExecutable("path/protostrap", "#!/bin/sh\n");
        var fake = new FakeGo(repo, onPath: true);
        var binary = fake.Binary(path: repo.PathOf("path"));

        var found = await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(fake.Output, found);
        Assert.Single(fake.Builds);
    }

    [Fact]
    public async Task WithoutGoProtoStrapOnThePathIsUsed()
    {
        using var repo = new FixtureRepo();
        var installed = repo.WriteExecutable("path/protostrap", "#!/bin/sh\n");
        var fake = new FakeGo(repo, onPath: false);
        var binary = fake.Binary(path: repo.PathOf("path"));

        var found = await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(installed, found);
        Assert.Empty(fake.Builds);
    }

    [Fact]
    public async Task WithoutACheckoutProtoStrapOnThePathIsUsed()
    {
        using var repo = new FixtureRepo();
        var installed = repo.WriteExecutable("path/protostrap", "#!/bin/sh\n");
        var fake = new FakeGo(repo, onPath: true);
        var binary = fake.Binary(path: repo.PathOf("path"), checkout: null);

        var found = await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(installed, found);
        Assert.Empty(fake.Builds);
    }

    [Fact]
    public async Task WithoutProtoStrapTheEdgeBuildsItFromTheCheckout()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);

        var found = await fake.Binary().FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(fake.Output, found);
        Assert.True(File.Exists(fake.Output));
        // The build never downloads a Go toolchain: the machine's Go builds it, or says it can't.
        Assert.Equal([$"{fake.Checkout}|local|build -o {fake.Output} ./cmd/protostrap"], fake.Builds);
    }

    [Fact]
    public async Task ItBuildsEachTimeTheEdgeStartsSoItFollowsTheCheckout()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var binary = fake.Binary();

        await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);
        await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        // Go's build cache makes the second build a no-op unless the checkout changed.
        Assert.Equal(2, fake.Builds.Length);
    }

    [Fact]
    public async Task GoIsFoundUnderGoRoot()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: false);
        var binary = fake.Binary(variables: new() { ["GOROOT"] = fake.Root });

        var found = await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(fake.Output, found);
        Assert.Single(fake.Builds);
    }

    [Fact]
    public async Task GoIsFoundWhereItsInstallerPutsIt()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: false);
        var binary = fake.Binary(installs: [Path.Combine(fake.Root, "bin")]);

        var found = await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.Equal(fake.Output, found);
        Assert.Single(fake.Builds);
    }

    [Fact]
    public async Task AGoToolchainTheMachineSetsIsKept()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var binary = fake.Binary(variables: new() { ["GOTOOLCHAIN"] = "go1.27.3" });

        await binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken);

        Assert.StartsWith($"{fake.Checkout}|go1.27.3|", Assert.Single(fake.Builds));
    }

    [Fact]
    public async Task WithoutGoTheEdgeSaysWhatToInstall()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: false);

        var problem = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            fake.Binary().FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("protostrap isn't on PATH", problem.Message);
        Assert.Contains(fake.Checkout, problem.Message);
        // The version is the go directive in the checkout's go.mod.
        Assert.Contains("Go 1.25 or later", problem.Message);
        Assert.False(File.Exists(fake.Output));
    }

    [Fact]
    public async Task AFailedBuildSaysWhy()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true, fails: "cmd/protostrap/main.go:3:1: syntax error: non-declaration statement outside function body");

        var problem = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            fake.Binary().FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains($"building protostrap from {fake.Checkout} failed", problem.Message);
        Assert.Contains("main.go:3:1: syntax error", problem.Message);
    }

    [Fact]
    public async Task WithoutACheckoutTheEdgeNeedsProtoStrapOnThePath()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var binary = fake.Binary(checkout: null);

        var problem = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("protostrap isn't installed", problem.Message);
        Assert.Contains("go build", problem.Message);
        Assert.Empty(fake.Builds);
    }

    [Fact]
    public async Task ACheckoutThatIsntProtoStrapIsntBuilt()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        repo.Write("other/go.mod", "module example.com/other\n\ngo 1.25\n");
        repo.Write("other/cmd/protostrap/main.go", "package main\n\nfunc main() {}\n");
        var binary = fake.Binary(checkout: repo.PathOf("other"));

        var problem = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            binary.FindOrBuildAsync("protostrap", fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains("protostrap isn't installed", problem.Message);
        Assert.Empty(fake.Builds);
    }

    [Theory]
    [InlineData("protostrap-1.2")]
    [InlineData("bin/protostrap")]
    public async Task ABinaryWithCheckNamesIsNeverBuilt(string command)
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        if (command.Contains('/'))
            command = repo.PathOf(command);

        var problem = await Assert.ThrowsAsync<InvalidOperationException>(() =>
            fake.Binary().FindOrBuildAsync(command, fake.Output, NullLogger.Instance, TestContext.Current.CancellationToken));

        Assert.Contains($"{command} isn't installed", problem.Message);
        Assert.Empty(fake.Builds);
    }

    [Fact]
    public async Task PublishingNeverBuildsProtoStrap()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        protostrap.Resource.SourceTracerFactory = root => new ProjectDirectoryTracer(root);
        protostrap.Resource.Binary = fake.Binary();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc).Route("app/", rate: 100));
        await using var app = builder.Build();

        await SpecStep.WriteAsync(
            protostrap.Resource, app.Services.GetRequiredService<DistributedApplicationModel>(),
            app.Services.GetRequiredService<DistributedApplicationExecutionContext>(), NullLogger.Instance, TestContext.Current.CancellationToken);

        // A pipeline builds protostrap and puts it on PATH: publishing only looks for it, and
        // has no edge to render.
        Assert.True(File.Exists(repo.SpecPath));
        Assert.Empty(fake.Builds);
        Assert.DoesNotContain(builder.Resources, r => r.Name == "protostrap-edge");
    }

    // A protostrap the AppHost doesn't build itself, one on PATH or one WithCheck names, may not
    // be the AppHost's ProtoStrap. The AppHost still uses it, and warns when it can tell it isn't:
    // with a checkout, by the commit Go records in the binary and when the binary was built; with
    // a package, by its version.

    private const string Head = "8f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b";
    private const string Other = "31997bd7434073d64528ab72c3c862941048d7e2";

    [Fact]
    public async Task AProtoStrapBuiltFromAnotherCommitIsntTheAppHosts()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var installed = GoBinaries.Write(repo, "path/protostrap", Other);

        var mismatch = await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken);

        Assert.NotNull(mismatch);
        Assert.Contains($"{installed} isn't the AppHost's ProtoStrap", mismatch);
        Assert.Contains("built from commit 31997bd74340", mismatch);
        Assert.Contains($"{fake.Checkout} at 8f0c1a2b3c4d", mismatch);
        Assert.Contains($"go build -o {installed} ./cmd/protostrap", mismatch);
    }

    [Fact]
    public async Task AProtoStrapBuiltBeforeTheCheckoutChangedIsntTheAppHosts()
    {
        // The checkout's commit, but its Go changed after the binary was built, as a working
        // tree does while ProtoStrap itself is changing.
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var built = new DateTime(2026, 10, 30, 23, 55, 0, DateTimeKind.Utc);
        fake.Touch(built.AddHours(-1));
        var installed = GoBinaries.Write(repo, "path/protostrap", Head, modified: true, built: built);
        var changed = repo.Write("protostrap/internal/edge/config.go", "package edge\n");
        File.SetLastWriteTimeUtc(changed, built.AddMinutes(36));

        var mismatch = await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken);

        Assert.NotNull(mismatch);
        Assert.Contains($"{installed} isn't the AppHost's ProtoStrap", mismatch);
        Assert.Contains($"before internal/edge/config.go changed in {fake.Checkout}", mismatch);
        Assert.Contains($"go build -o {installed} ./cmd/protostrap", mismatch);
    }

    [Fact]
    public async Task AProtoStrapBuiltFromTheCheckoutAsItIsIsTheAppHosts()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var built = new DateTime(2026, 10, 30, 23, 55, 0, DateTimeKind.Utc);
        fake.Touch(built.AddHours(-1));
        var installed = GoBinaries.Write(repo, "path/protostrap", Head, built: built);

        Assert.Null(await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken));
    }

    [Theory]
    [InlineData("protostrap/internal/edge/config_test.go")]
    [InlineData("protostrap/internal/edge/testdata/routing.json")]
    [InlineData("protostrap/docs/protostrap.md")]
    [InlineData("protostrap/hosting/src/ProtoStrap.Hosting/LocalEdge.cs")]
    [InlineData("protostrap/sample/services/go/main.go")]
    public async Task ChangesThatDontGoIntoTheBinaryDontCount(string file)
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var built = new DateTime(2026, 10, 30, 23, 55, 0, DateTimeKind.Utc);
        fake.Touch(built.AddHours(-1));
        var installed = GoBinaries.Write(repo, "path/protostrap", Head, modified: true, built: built);
        File.SetLastWriteTimeUtc(repo.Write(file, "changed\n"), built.AddMinutes(10));

        Assert.Null(await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken));
    }

    [Fact]
    public async Task FilesTheBinaryEmbedsCount()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var built = new DateTime(2026, 10, 30, 23, 55, 0, DateTimeKind.Utc);
        fake.Touch(built.AddHours(-1));
        var installed = GoBinaries.Write(repo, "path/protostrap", Head, built: built);
        File.SetLastWriteTimeUtc(repo.Write("protostrap/internal/ui/static/app.js", "changed\n"), built.AddMinutes(10));

        var mismatch = await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken);

        Assert.Contains("before internal/ui/static/app.js changed", mismatch);
    }

    [Fact]
    public async Task AProtoStrapThatDoesntRecordItsCommitMayNotBeTheAppHosts()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var installed = repo.WriteExecutable("path/protostrap", "#!/bin/sh\n");

        var mismatch = await fake.Binary(path: repo.PathOf("path"), head: Head).MismatchAsync(installed, TestContext.Current.CancellationToken);

        Assert.NotNull(mismatch);
        Assert.Contains($"{installed} may not be the AppHost's ProtoStrap: it doesn't record the commit it was built from", mismatch);
        Assert.Contains($"{fake.Checkout} at 8f0c1a2b3c4d", mismatch);
    }

    [Fact]
    public async Task WithoutGitTheCommitIsntCompared()
    {
        // The checkout's commit is git's to say; without git, a binary built after the checkout
        // last changed is taken as its.
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var built = new DateTime(2026, 10, 30, 23, 55, 0, DateTimeKind.Utc);
        fake.Touch(built.AddHours(-1));
        var installed = GoBinaries.Write(repo, "path/protostrap", Other, built: built);

        Assert.Null(await fake.Binary(path: repo.PathOf("path")).MismatchAsync(installed, TestContext.Current.CancellationToken));
    }

    [Theory]
    [InlineData("0.1.0", false)]
    [InlineData("v0.1.0", false)]
    [InlineData("0.0.0-dev", true)]
    [InlineData("0.2.0", true)]
    public async Task WithoutACheckoutItsVersionIsCompared(string version, bool mismatched)
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var installed = repo.WriteExecutable("path/protostrap", $"#!/bin/sh\n[ \"$1\" = version ] && echo {version}\n");
        var binary = fake.Binary(path: repo.PathOf("path"), checkout: null, head: Head);

        var mismatch = await binary.MismatchAsync(installed, TestContext.Current.CancellationToken);

        if (mismatched)
            Assert.Equal($"{installed} isn't the AppHost's ProtoStrap: it is ProtoStrap {version}, and ProtoStrap.Hosting is 0.1.0. Install protostrap 0.1.0.", mismatch);
        else
            Assert.Null(mismatch);
    }

    [Fact]
    public async Task TheEdgeWarnsAndStillUsesIt()
    {
        // Without Go, the edge renders with protostrap on PATH, whatever its version.
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: false);
        var installed = GoBinaries.Write(repo, "path/protostrap", Other);
        var logger = new ListLogger();

        var found = await fake.Binary(path: repo.PathOf("path"), head: Head).FindOrBuildAsync("protostrap", fake.Output, logger, TestContext.Current.CancellationToken);

        Assert.Equal(installed, found);
        Assert.Empty(fake.Builds);
        var (level, message) = Assert.Single(logger.Entries);
        Assert.Equal(LogLevel.Warning, level);
        Assert.Contains($"{installed} isn't the AppHost's ProtoStrap", message);
    }

    [Fact]
    public async Task TheSpecCheckWarnsToo()
    {
        using var repo = new FixtureRepo();
        var fake = new FakeGo(repo, onPath: true);
        var installed = repo.WriteExecutable("path/protostrap", "#!/bin/sh\nexit 0\n");
        var builder = TestApp.Publish(repo);
        var protostrap = builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.dev.localhost");
        protostrap.Resource.SourceTracerFactory = root => new ProjectDirectoryTracer(root);
        protostrap.Resource.Binary = fake.Binary(path: repo.PathOf("path"), head: Head);
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc).Route("app/", rate: 100));
        await using var app = builder.Build();
        var logger = new ListLogger();

        await SpecStep.WriteAsync(
            protostrap.Resource, app.Services.GetRequiredService<DistributedApplicationModel>(),
            app.Services.GetRequiredService<DistributedApplicationExecutionContext>(), logger, TestContext.Current.CancellationToken);

        Assert.Contains(logger.Entries, e => e.Level == LogLevel.Warning && e.Message.Contains($"{installed} may not be the AppHost's ProtoStrap"));
    }

    [Fact]
    public async Task ItReadsTheCommitGoRecordsInABinary()
    {
        // A binary Go builds itself, in a git repository with a commit and a change since.
        var go = new ProtoStrapBinary().FindGo();
        var git = Which("git");
        if (go is null || git is null)
            Assert.Skip("needs go and git");
        using var repo = new FixtureRepo();
        var dir = repo.PathOf("probe");
        repo.Write("probe/go.mod", "module example.com/probe\n\ngo 1.21\n");
        repo.Write("probe/main.go", "package main\n\nfunc main() {}\n");
        await RunAsync(git, dir, "init", "-q", ".");
        await RunAsync(git, dir, "add", ".");
        await RunAsync(git, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "probe");
        var head = (await RunAsync(git, dir, "rev-parse", "HEAD")).Trim();
        repo.Write("probe/notes.txt", "a change since\n");
        await RunAsync(go, dir, "build", "-o", "probe", ".");

        var build = ProtoStrapBinary.ReadBuild(Path.Combine(dir, "probe"));

        Assert.Equal(new ProtoStrapBinary.GoBuild(head, Modified: true), build);
    }

    [Fact]
    public async Task TheFilesComparedAreTheOnesGoBuildsProtoStrapFrom()
    {
        // The checkout's files that go into protostrap are its go.mod, and cmd's and internal's
        // files but tests and test data, which is everything Go lists for ./cmd/protostrap.
        var go = new ProtoStrapBinary().FindGo();
        if (go is null)
            Assert.Skip("needs go");
        var checkout = CheckoutOfThisFile();
        // Go never downloads a toolchain here, so one older than go.mod's can't list the module.
        var required = Version.Parse(Regex.Match(File.ReadAllText(Path.Combine(checkout, "go.mod")), @"^go\s+(\d+\.\d+)", RegexOptions.Multiline).Groups[1].Value);
        var running = (await RunAsync(go, checkout, "env", "GOVERSION")).Trim();
        if (Version.Parse(Regex.Match(running, @"^go(\d+\.\d+)").Groups[1].Value) < required)
            Assert.Skip($"needs Go {required} or later, which go.mod names, and {go} is {running}");
        var listed = await RunAsync(go, checkout, "list", "-deps",
            "-f", "{{if not .Standard}}{{range .GoFiles}}{{$.Dir}}/{{.}}\n{{end}}{{range .EmbedFiles}}{{$.Dir}}/{{.}}\n{{end}}{{end}}", "./cmd/protostrap");
        var files = listed.Split('\n', StringSplitOptions.RemoveEmptyEntries).Select(f => Path.GetRelativePath(checkout, f).Replace('\\', '/')).ToList();

        Assert.NotEmpty(files);
        Assert.All(files, f => Assert.True(ProtoStrapBinary.GoesIntoTheBinary(f), $"{f} goes into protostrap, but isn't compared"));
    }

    [Fact]
    public void ProtoStrapHostingRecordsTheCheckoutItIsBuiltFrom()
    {
        Assert.Equal(CheckoutOfThisFile(), new ProtoStrapBinary().Checkout);
    }

    private static string CheckoutOfThisFile([CallerFilePath] string path = "")
    {
        var dir = Path.GetDirectoryName(path)!;
        while (!File.Exists(Path.Combine(dir, "go.mod")))
            dir = Path.GetDirectoryName(dir)!;
        return dir;
    }

    private static string? Which(string command) =>
        (Environment.GetEnvironmentVariable("PATH") ?? "").Split(Path.PathSeparator, StringSplitOptions.RemoveEmptyEntries)
            .Select(dir => Path.Combine(dir, command)).FirstOrDefault(File.Exists);

    private static async Task<string> RunAsync(string file, string dir, params string[] args)
    {
        var start = new ProcessStartInfo(file) { WorkingDirectory = dir, RedirectStandardOutput = true, RedirectStandardError = true };
        foreach (var arg in args)
            start.ArgumentList.Add(arg);
        start.Environment["GOTOOLCHAIN"] = "local";
        start.Environment["GOFLAGS"] = "";
        using var process = Process.Start(start)!;
        var output = process.StandardOutput.ReadToEndAsync();
        var error = process.StandardError.ReadToEndAsync();
        await process.WaitForExitAsync();
        Assert.True(process.ExitCode == 0, $"{file} {string.Join(' ', args)}: {await error}");
        return await output;
    }

}
