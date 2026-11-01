using System.Diagnostics;
using System.Globalization;
using System.Reflection;
using System.Text;
using System.Text.RegularExpressions;
using Microsoft.Extensions.Logging;

namespace ProtoStrap.Hosting;

/// <summary>
/// ProtoStrapBinary finds the protostrap binary the AppHost runs: a path, or a name on PATH. For
/// the local gateway, protostrap is built with the machine's Go from the ProtoStrap checkout this
/// assembly was built from, so it is the AppHost's version. One it finds may not be, which
/// MismatchAsync tells.
/// </summary>
internal sealed class ProtoStrapBinary
{
    /// <summary>DefaultCommand is the binary WithCheck names unless it's told otherwise.</summary>
    public const string DefaultCommand = "protostrap";

    private const string Module = "github.com/duncan-G/protostrap";

    /// <summary>
    /// Checkout is the ProtoStrap checkout this assembly was built from, which the csproj records,
    /// or null for a package.
    /// </summary>
    public string? Checkout { get; init; } = typeof(ProtoStrapBinary).Assembly
        .GetCustomAttributes<AssemblyMetadataAttribute>()
        .FirstOrDefault(a => a.Key == "ProtoStrapCheckout")?.Value is { Length: > 0 } dir
        ? Path.TrimEndingDirectorySeparator(dir)
        : null;

    /// <summary>
    /// HostingVersion is this assembly's version, which a package's protostrap must be.
    /// </summary>
    public string HostingVersion { get; init; } = typeof(ProtoStrapBinary).Assembly
        .GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion.Split('+')[0] ?? "0.0.0";

    /// <summary>Variable reads the environment: PATH, GOROOT and GOTOOLCHAIN. Tests replace it.</summary>
    public Func<string, string?> Variable { get; init; } = Environment.GetEnvironmentVariable;

    /// <summary>GoInstalls are where Go's installers put go, looked in after PATH and GOROOT.</summary>
    public IReadOnlyList<string> GoInstalls { get; init; } = OperatingSystem.IsWindows()
        ? [Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Go", "bin")]
        : ["/usr/local/go/bin"];

    /// <summary>
    /// Find returns a path as it is, or a name looked up on PATH, or null if there is no such
    /// file.
    /// </summary>
    public string? Find(string command)
    {
        if (command.Contains(Path.DirectorySeparatorChar) || command.Contains(Path.AltDirectorySeparatorChar))
            return File.Exists(command) ? command : null;
        return OnPath(command);
    }

    /// <summary>
    /// FindOrBuildAsync finds the protostrap the local gateway renders with. protostrap, unless
    /// WithCheck names another, is built from the checkout with the machine's Go into output,
    /// every time, so it follows the checkout: Go's build cache makes that a no-op when nothing
    /// changed. Without the checkout or Go, or for a binary WithCheck names, it is found as Find
    /// finds it, with a warning when it isn't the AppHost's ProtoStrap.
    /// </summary>
    public async Task<string> FindOrBuildAsync(string command, string output, ILogger logger, CancellationToken cancellationToken)
    {
        var checkout = command == DefaultCommand ? ProtoStrapCheckout() : null;
        if (checkout is not null && FindGo() is { } go)
        {
            await BuildAsync(checkout, go, output, logger, cancellationToken);
            return output;
        }
        if (Find(command) is { } found)
        {
            if (await MismatchAsync(found, cancellationToken) is { } mismatch)
                logger.LogWarning("{Mismatch}", mismatch);
            return found;
        }
        if (checkout is null)
            throw new InvalidOperationException(
                $"The local gateway is rendered by `protostrap edge render`, and {command} isn't installed. Build it from the ProtoStrap checkout at the AppHost's version, " +
                "`go build -o ~/.local/bin/protostrap ./cmd/protostrap`, and put it on PATH, or name it with WithCheck(...).");
        throw new InvalidOperationException(
            $"The local gateway is rendered by `protostrap edge render`. The AppHost builds protostrap from {checkout}, which needs Go {GoVersion(checkout)} or later, " +
            "and protostrap isn't on PATH either: install Go, or put protostrap on PATH.");
    }

    /// <summary>
    /// BuildAsync builds protostrap from the checkout into output with go. The machine's Go builds
    /// it, or says it's too old: ProtoStrap never downloads a toolchain unless GOTOOLCHAIN says to.
    /// </summary>
    private async Task BuildAsync(string checkout, string go, string output, ILogger logger, CancellationToken cancellationToken)
    {
        logger.LogInformation("Building protostrap from {Checkout} with {Go}", checkout, go);
        Directory.CreateDirectory(Path.GetDirectoryName(output)!);
        var start = new ProcessStartInfo(go)
        {
            WorkingDirectory = checkout,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        foreach (var arg in new[] { "build", "-o", output, "./cmd/protostrap" })
            start.ArgumentList.Add(arg);
        start.Environment["GOTOOLCHAIN"] = Variable("GOTOOLCHAIN") ?? "local";
        using var process = Process.Start(start)!;
        var stdout = process.StandardOutput.ReadToEndAsync(cancellationToken);
        var stderr = process.StandardError.ReadToEndAsync(cancellationToken);
        await process.WaitForExitAsync(cancellationToken);
        if (process.ExitCode != 0)
        {
            var message = new StringBuilder($"building protostrap from {checkout} failed:");
            foreach (var line in (await stdout + await stderr).Trim().Split('\n'))
                message.Append("\n  ").Append(line.TrimEnd());
            throw new InvalidOperationException(message.ToString());
        }
    }

    /// <summary>
    /// MismatchAsync says how the protostrap at executable isn't the AppHost's ProtoStrap, or
    /// returns null when it is, as far as can be told. With the checkout, it must be built from
    /// the checkout's commit, which git says, after the checkout's files that go into it last
    /// changed. Without one, as from a package, `protostrap version` must be this assembly's.
    /// </summary>
    public async Task<string?> MismatchAsync(string executable, CancellationToken cancellationToken)
    {
        if (ProtoStrapCheckout() is not { } checkout)
        {
            var version = await RunAsync(executable, null, ["version"], cancellationToken);
            if (version is null)
                return $"{executable} may not be the AppHost's ProtoStrap: `{executable} version` failed, and ProtoStrap.Hosting is {HostingVersion}. Install protostrap {HostingVersion}.";
            return version.TrimStart('v') == HostingVersion.TrimStart('v')
                ? null
                : $"{executable} isn't the AppHost's ProtoStrap: it is ProtoStrap {version}, and ProtoStrap.Hosting is {HostingVersion}. Install protostrap {HostingVersion}.";
        }

        var rebuild = $"Rebuild it from there with `go build -o {executable} ./cmd/protostrap`; without protostrap on PATH, the local gateway builds its own.";
        var head = OnPath("git") is { } git ? await RunAsync(git, null, ["-C", checkout, "rev-parse", "HEAD"], cancellationToken) : null;
        var at = head is null ? "" : $" at {Short(head)}";
        if (ReadBuild(executable)?.Revision is not { } revision)
            return $"{executable} may not be the AppHost's ProtoStrap: it doesn't record the commit it was built from, and ProtoStrap.Hosting is built from {checkout}{at}. {rebuild}";
        if (head is not null && !revision.Equals(head, StringComparison.OrdinalIgnoreCase))
            return $"{executable} isn't the AppHost's ProtoStrap: it was built from commit {Short(revision)}, and ProtoStrap.Hosting is built from {checkout}{at}. {rebuild}";
        var built = File.GetLastWriteTimeUtc(executable);
        if (NewestSource(checkout) is { } newest && newest.Changed > built)
            return $"{executable} isn't the AppHost's ProtoStrap: it was built at {Local(built)}, before {newest.Path} changed in {checkout} at {Local(newest.Changed)}. {rebuild}";
        return null;
    }

    private static string Short(string revision) => revision.Length > 12 ? revision[..12] : revision;

    private static string Local(DateTime utc) => utc.ToLocalTime().ToString("yyyy-MM-dd HH:mm:ss", CultureInfo.InvariantCulture);

    /// <summary>
    /// GoesIntoTheBinary says whether a file in the checkout, by its path there with / between
    /// names, is built into protostrap: go.mod and go.sum, and every file under cmd and internal
    /// but tests and test data, such as the operator UI's embedded files. A test checks this
    /// against what Go lists for ./cmd/protostrap.
    /// </summary>
    internal static bool GoesIntoTheBinary(string path)
    {
        if (path is "go.mod" or "go.sum")
            return true;
        var names = path.Split('/');
        return names[0] is "cmd" or "internal"
            && !path.EndsWith("_test.go", StringComparison.Ordinal)
            && !names[..^1].Any(n => n == "testdata" || n.StartsWith('.') || n.StartsWith('_'));
    }

    /// <summary>NewestSource is the checkout's file built into protostrap that changed last.</summary>
    internal static (string Path, DateTime Changed)? NewestSource(string checkout)
    {
        (string Path, DateTime Changed)? newest = null;
        var files = new[] { "go.mod", "go.sum" }.Select(f => Path.Combine(checkout, f)).Where(File.Exists)
            .Concat(new[] { "cmd", "internal" }.Select(d => Path.Combine(checkout, d)).Where(Directory.Exists)
                .SelectMany(d => Directory.EnumerateFiles(d, "*", SearchOption.AllDirectories)));
        foreach (var file in files)
        {
            var path = Path.GetRelativePath(checkout, file).Replace(Path.DirectorySeparatorChar, '/');
            if (!GoesIntoTheBinary(path))
                continue;
            var changed = File.GetLastWriteTimeUtc(file);
            if (newest is null || changed > newest.Value.Changed)
                newest = (path, changed);
        }
        return newest;
    }

    /// <summary>GoBuild is what Go records in a binary it builds: the commit, and whether the tree had changes.</summary>
    internal sealed record GoBuild(string? Revision, bool Modified);

    private static readonly byte[] BuildInfoMagic = [0xff, .. Encoding.ASCII.GetBytes(" Go buildinf:")];

    /// <summary>
    /// ReadBuild reads the build info Go's linker writes into a binary, as debug/buildinfo does,
    /// or returns null for a file without it.
    /// </summary>
    internal static GoBuild? ReadBuild(string path)
    {
        byte[] data;
        try
        {
            data = File.ReadAllBytes(path);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            return null;
        }
        for (var at = data.AsSpan().IndexOf(BuildInfoMagic); at >= 0;)
        {
            // The header is the magic, the pointer size, flags and padding to 32 bytes. Flag 2
            // says the Go version and the module info follow it, each a length and its bytes.
            var blob = data.AsSpan(at);
            if (blob.Length >= 32 && (blob[15] & 2) != 0 && Varstring(blob[32..], out var rest).Length >= 0 && Varstring(rest, out _) is { Length: >= 33 } info && info[^17] == '\n')
                return ParseBuild(Encoding.UTF8.GetString(info[16..^16]));
            var next = data.AsSpan(at + 1).IndexOf(BuildInfoMagic);
            at = next < 0 ? -1 : at + 1 + next;
        }
        return null;
    }

    // Varstring reads a length, as a uvarint, and that many bytes, or none if there aren't.
    private static ReadOnlySpan<byte> Varstring(ReadOnlySpan<byte> data, out ReadOnlySpan<byte> rest)
    {
        ulong length = 0;
        for (int i = 0, shift = 0; i < data.Length && shift < 64; i++, shift += 7)
        {
            length |= (ulong)(data[i] & 0x7f) << shift;
            if (data[i] < 0x80)
            {
                if (length <= (ulong)(data.Length - i - 1))
                {
                    rest = data[(i + 1 + (int)length)..];
                    return data.Slice(i + 1, (int)length);
                }
                break;
            }
        }
        rest = [];
        return [];
    }

    private static GoBuild ParseBuild(string info)
    {
        string? revision = null;
        var modified = false;
        foreach (var line in info.Split('\n'))
        {
            if (line.StartsWith("build\tvcs.revision=", StringComparison.Ordinal))
                revision = line["build\tvcs.revision=".Length..];
            else if (line == "build\tvcs.modified=true")
                modified = true;
        }
        return new(revision, modified);
    }

    /// <summary>
    /// RunAsync runs a program and returns what it printed, trimmed, or null if it failed or
    /// took more than 10 seconds.
    /// </summary>
    private static async Task<string?> RunAsync(string file, string? dir, string[] args, CancellationToken cancellationToken)
    {
        var start = new ProcessStartInfo(file)
        {
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
        };
        if (dir is not null)
            start.WorkingDirectory = dir;
        foreach (var arg in args)
            start.ArgumentList.Add(arg);
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeout.CancelAfter(TimeSpan.FromSeconds(10));
        try
        {
            using var process = Process.Start(start)!;
            var output = process.StandardOutput.ReadToEndAsync(timeout.Token);
            var error = process.StandardError.ReadToEndAsync(timeout.Token);
            try
            {
                await process.WaitForExitAsync(timeout.Token);
            }
            catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested)
            {
                process.Kill(entireProcessTree: true);
                return null;
            }
            await error;
            return process.ExitCode == 0 ? (await output).Trim() : null;
        }
        catch (System.ComponentModel.Win32Exception)
        {
            return null;
        }
    }

    /// <summary>
    /// ProtoStrapCheckout is the checkout if it's still there and is ProtoStrap's: its go.mod is
    /// the module's and it has cmd/protostrap.
    /// </summary>
    internal string? ProtoStrapCheckout()
    {
        if (Checkout is null || !Directory.Exists(Path.Combine(Checkout, "cmd", "protostrap")))
            return null;
        var goMod = Path.Combine(Checkout, "go.mod");
        if (!File.Exists(goMod) || !Regex.IsMatch(File.ReadAllText(goMod), $@"^module\s+{Regex.Escape(Module)}\s*$", RegexOptions.Multiline))
            return null;
        return Checkout;
    }

    /// <summary>GoVersion is the go directive in the checkout's go.mod.</summary>
    private static string GoVersion(string checkout) =>
        Regex.Match(File.ReadAllText(Path.Combine(checkout, "go.mod")), @"^go\s+(\S+)", RegexOptions.Multiline) is { Success: true } match
            ? match.Groups[1].Value
            : "";

    /// <summary>FindGo looks for go on PATH, then in GOROOT, then where Go's installers put it.</summary>
    internal string? FindGo()
    {
        if (OnPath("go") is { } go)
            return go;
        var dirs = new List<string>();
        if (Variable("GOROOT") is { Length: > 0 } root)
            dirs.Add(Path.Combine(root, "bin"));
        dirs.AddRange(GoInstalls);
        return dirs.SelectMany(Names("go"), Path.Combine).FirstOrDefault(File.Exists);
    }

    private string? OnPath(string command) =>
        (Variable("PATH") ?? "")
            .Split(Path.PathSeparator, StringSplitOptions.RemoveEmptyEntries)
            .SelectMany(Names(command), Path.Combine)
            .FirstOrDefault(File.Exists);

    private static Func<string, IEnumerable<string>> Names(string command) =>
        _ => OperatingSystem.IsWindows() ? [command + ".exe", command] : [command];
}
