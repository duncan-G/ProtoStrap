using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text;
using System.Text.Json;

namespace ProtoStrap.Hosting.Sources;

/// <summary>ProjectSources is what a project's image is built from, relative to the spec's directory.</summary>
/// <param name="Paths">They are directories and files, sorted, none inside another.</param>
/// <param name="ContainerBaseImage">It is the project's ContainerBaseImage, if it sets one.</param>
/// <param name="GlobalJson">
/// It is the global.json the .NET SDK reads when it builds from the project's directory, if one
/// is inside the root.
/// </param>
/// <param name="SdkVersion">It is the SDK version that global.json pins, if it pins one.</param>
internal sealed record ProjectSources(IReadOnlyList<string> Paths, string? ContainerBaseImage = null, string? GlobalJson = null, string? SdkVersion = null);

/// <summary>ISourceTracer works out a project's sources.</summary>
internal interface ISourceTracer
{
    Task<ProjectSources> TraceAsync(string projectPath, CancellationToken cancellationToken);
}

internal sealed class SourceTraceException(string message) : Exception(message);

/// <summary>
/// MSBuildSourceTracer traces a project's sources as MSBuild sees them: the directory of every
/// project in its ProjectReference closure, every MSBuild file the evaluation imported, every
/// item outside its project's directory, and the nuget.config and global.json files the build
/// reads. Only what is inside the root, where protostrap.yaml is, counts: the SDK's files and
/// NuGet's packages are pinned by global.json and the lock files instead.
/// </summary>
/// <remarks>
/// Each project is evaluated with `dotnet msbuild`, never built, so tracing needs neither a
/// restore nor the network. A project shared by several components is evaluated once.
/// </remarks>
internal sealed class MSBuildSourceTracer(string root) : ISourceTracer
{
    // These are the item types whose files go into a build. Evaluation lists each item with its
    // full path.
    private static readonly string[] ItemTypes =
        ["ProjectReference", "Compile", "Content", "None", "EmbeddedResource", "Protobuf", "AdditionalFiles", "Resource", "Page", "Analyzer"];

    private readonly string _root = Path.TrimEndingDirectorySeparator(Path.GetFullPath(root));
    private readonly ConcurrentDictionary<string, Lazy<Task<Evaluation>>> _evaluations = new(StringComparer.Ordinal);

    /// <summary>Evaluations is how many projects have been evaluated.</summary>
    public int Evaluations => _evaluations.Count;

    public async Task<ProjectSources> TraceAsync(string projectPath, CancellationToken cancellationToken)
    {
        var project = Path.GetFullPath(projectPath);
        if (!File.Exists(project))
            throw new SourceTraceException($"{project} doesn't exist");

        // The ProjectReference closure is walked a level at a time, with each level's projects
        // evaluated together.
        var closure = new Dictionary<string, Evaluation>(StringComparer.Ordinal);
        var level = new List<string> { project };
        while (level.Count > 0)
        {
            var evaluations = await Task.WhenAll(level.Select(p => EvaluateAsync(p, cancellationToken)));
            var next = new List<string>();
            foreach (var evaluation in evaluations)
            {
                closure[evaluation.Project] = evaluation;
                foreach (var reference in evaluation.References)
                {
                    if (!File.Exists(reference))
                        throw new SourceTraceException($"{evaluation.Project} references {reference}, which doesn't exist");
                    if (!closure.ContainsKey(reference) && !next.Contains(reference))
                        next.Add(reference);
                }
            }
            level = next;
        }

        var directories = closure.Keys.Select(p => Path.GetDirectoryName(p)!).ToHashSet(StringComparer.Ordinal);
        var files = new HashSet<string>(StringComparer.Ordinal);
        foreach (var evaluation in closure.Values)
        {
            files.UnionWith(evaluation.Imports);
            var directory = Path.GetDirectoryName(evaluation.Project)!;
            files.UnionWith(evaluation.Items.Where(item => !IsInside(item, directory)));
            files.UnionWith(Above(directory).SelectMany(NuGetConfigs));
        }
        var globalJson = Above(Path.GetDirectoryName(project)!).Select(d => Path.Combine(d, "global.json")).FirstOrDefault(File.Exists);
        string? sdkVersion = null;
        if (globalJson is not null)
        {
            files.Add(globalJson);
            sdkVersion = await SdkVersionAsync(globalJson, cancellationToken);
        }

        var kept = directories.Where(d => IsInside(d, _root)).ToList();
        kept = [.. kept.Where(d => !kept.Any(other => other != d && IsInside(d, other)))];
        var sources = kept
            .Concat(files.Where(f => IsInside(f, _root) && !kept.Any(d => IsInside(f, d))))
            .Select(Relative)
            .Distinct(StringComparer.Ordinal)
            .Order(StringComparer.Ordinal)
            .ToList();
        return new ProjectSources(sources, closure[project].ContainerBaseImage, globalJson is null ? null : Relative(globalJson), sdkVersion);
    }

    // SdkVersionAsync reads the SDK version a global.json pins, as the SDK reads it: comments
    // are allowed, trailing commas aren't.
    private async Task<string?> SdkVersionAsync(string globalJson, CancellationToken cancellationToken)
    {
        try
        {
            using var json = JsonDocument.Parse(await File.ReadAllTextAsync(globalJson, cancellationToken),
                new JsonDocumentOptions { CommentHandling = JsonCommentHandling.Skip });
            return json.RootElement.ValueKind == JsonValueKind.Object
                && json.RootElement.TryGetProperty("sdk", out var sdk) && sdk.ValueKind == JsonValueKind.Object
                && sdk.TryGetProperty("version", out var version) && version.ValueKind == JsonValueKind.String
                && version.GetString() is { Length: > 0 } pinned ? pinned : null;
        }
        catch (JsonException e)
        {
            throw new SourceTraceException($"{Relative(globalJson)} isn't JSON the .NET SDK can read: {e.Message}");
        }
    }

    private Task<Evaluation> EvaluateAsync(string project, CancellationToken cancellationToken) =>
        _evaluations.GetOrAdd(project, p => new Lazy<Task<Evaluation>>(() => EvaluateProjectAsync(p, cancellationToken))).Value;

    private static async Task<Evaluation> EvaluateProjectAsync(string project, CancellationToken cancellationToken)
    {
        var preprocessed = Path.Combine(Path.GetTempPath(), $"protostrap-{Guid.NewGuid():n}.xml");
        try
        {
            var itemsTask = MSBuildAsync(project,
                [.. ItemTypes.Select(t => "-getItem:" + t), "-getProperty:ContainerBaseImage", "-getProperty:MSBuildProjectFullPath"],
                cancellationToken);
            var importsTask = MSBuildAsync(project, ["-pp:" + preprocessed], cancellationToken);
            var items = await itemsTask;
            await importsTask;

            // Anything MSBuild warns about on stdout comes before or after the JSON.
            var first = items.IndexOf('{');
            var last = items.LastIndexOf('}');
            if (first < 0 || last < first)
                throw new SourceTraceException($"dotnet msbuild printed no items for {project}:\n  {items.Trim()}");
            using var json = JsonDocument.Parse(items[first..(last + 1)]);
            var properties = json.RootElement.GetProperty("Properties");
            var baseImage = properties.TryGetProperty("ContainerBaseImage", out var image) && image.GetString() is { Length: > 0 } value ? value : null;
            var references = new List<string>();
            var files = new List<string>();
            if (json.RootElement.TryGetProperty("Items", out var allItems))
            {
                foreach (var type in allItems.EnumerateObject())
                {
                    foreach (var item in type.Value.EnumerateArray())
                    {
                        if (!item.TryGetProperty("FullPath", out var fullPath) || fullPath.GetString() is not { Length: > 0 } path)
                            continue;
                        if (type.Name == "ProjectReference")
                            references.Add(Path.GetFullPath(path));
                        else
                            files.Add(Path.GetFullPath(path));
                    }
                }
            }

            // The preprocessed project names each imported file on a line of its own, inside
            // the comment that marks where the import was.
            var imports = (await File.ReadAllLinesAsync(preprocessed, cancellationToken))
                .Select(l => l.Trim())
                .Where(l => l.Length > 0 && Path.IsPathFullyQualified(l) && File.Exists(l))
                .Select(Path.GetFullPath)
                .ToList();
            return new Evaluation(project, references, files, imports, baseImage);
        }
        finally
        {
            File.Delete(preprocessed);
        }
    }

    private static async Task<string> MSBuildAsync(string project, IEnumerable<string> arguments, CancellationToken cancellationToken)
    {
        var start = new ProcessStartInfo(DotnetPath())
        {
            WorkingDirectory = Path.GetDirectoryName(project)!,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        foreach (var argument in (string[])["msbuild", project, "-nologo", "-nodeReuse:false", "-p:Configuration=Release", .. arguments])
            start.ArgumentList.Add(argument);
        // A parent `dotnet run` or `dotnet test` can leave MSBuild's own variables behind, which
        // would point this evaluation at the wrong SDK.
        foreach (var name in start.Environment.Keys.Where(k => k.StartsWith("MSBuild", StringComparison.OrdinalIgnoreCase) || k.StartsWith("MSBUILD", StringComparison.Ordinal)).ToList())
            start.Environment.Remove(name);
        start.Environment["DOTNET_CLI_TELEMETRY_OPTOUT"] = "1";
        start.Environment["DOTNET_NOLOGO"] = "1";
        start.Environment["DOTNET_SKIP_FIRST_TIME_EXPERIENCE"] = "1";
        start.Environment["DOTNET_CLI_UI_LANGUAGE"] = "en";
        start.Environment["DOTNET_CLI_USE_MSBUILD_SERVER"] = "0";

        using var process = new Process { StartInfo = start };
        try
        {
            process.Start();
        }
        catch (Exception e) when (e is System.ComponentModel.Win32Exception or InvalidOperationException)
        {
            throw new SourceTraceException($"running {start.FileName} msbuild for {project}: {e.Message}");
        }
        var output = process.StandardOutput.ReadToEndAsync(cancellationToken);
        var error = process.StandardError.ReadToEndAsync(cancellationToken);
        try
        {
            await process.WaitForExitAsync(cancellationToken);
        }
        catch (OperationCanceledException)
        {
            process.Kill(entireProcessTree: true);
            throw;
        }
        var stdout = await output;
        if (process.ExitCode != 0)
        {
            var message = new StringBuilder($"dotnet msbuild failed on {project}:");
            foreach (var line in (stdout + "\n" + await error).Split('\n').Select(l => l.TrimEnd()).Where(l => l.Length > 0).Take(20))
                message.Append("\n  ").Append(line);
            throw new SourceTraceException(message.ToString());
        }
        return stdout;
    }

    /// <summary>
    /// DotnetPath returns the dotnet the AppHost runs under, which knows the SDK it was built
    /// with.
    /// </summary>
    private static string DotnetPath() =>
        Environment.GetEnvironmentVariable("DOTNET_HOST_PATH") is { Length: > 0 } host && File.Exists(host) ? host : "dotnet";

    /// <summary>Above yields a directory and those above it, up to the root.</summary>
    private IEnumerable<string> Above(string directory)
    {
        for (var d = directory; d is not null && IsInside(d, _root); d = Path.GetDirectoryName(d))
            yield return d;
    }

    // NuGet reads every nuget.config from the project's directory up, whatever the case of its
    // name.
    private static IEnumerable<string> NuGetConfigs(string directory) =>
        Directory.EnumerateFiles(directory).Where(f => string.Equals(Path.GetFileName(f), "nuget.config", StringComparison.OrdinalIgnoreCase));

    private string Relative(string path) => PathUtil.Relative(_root, path);

    private static bool IsInside(string path, string directory) => PathUtil.IsInside(path, directory);

    private sealed record Evaluation(string Project, IReadOnlyList<string> References, IReadOnlyList<string> Items, IReadOnlyList<string> Imports, string? ContainerBaseImage);
}
