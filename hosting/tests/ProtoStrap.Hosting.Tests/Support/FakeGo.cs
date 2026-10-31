using System.Text;
using Microsoft.Extensions.Logging;

namespace ProtoStrap.Hosting.Tests.Support;

/// <summary>
/// FakeGo is a ProtoStrap checkout in the fixture repository and a go that "builds" it: it
/// records where it ran, GOTOOLCHAIN and its arguments, then writes the -o file, or fails.
/// </summary>
internal sealed class FakeGo
{
    private readonly FixtureRepo _repo;
    private readonly bool _onPath;
    private readonly string _log;

    public FakeGo(FixtureRepo repo, bool onPath, string? fails = null)
    {
        _repo = repo;
        _onPath = onPath;
        _log = repo.PathOf("go.log");
        Checkout = repo.PathOf("protostrap");
        Root = repo.PathOf("go");
        Output = Path.Combine(repo.AppHost, "obj", "protostrap", "protostrap");
        repo.Write("protostrap/go.mod", "module github.com/duncan-G/protostrap\n\ngo 1.25\n");
        repo.Write("protostrap/cmd/protostrap/main.go", "package main\n\nfunc main() {}\n");
        var build = fails is null
            ? "printf '#!/bin/sh\\n' > \"$3\"\nchmod +x \"$3\"\n"
            : $"echo '{fails}' >&2\nexit 1\n";
        repo.WriteExecutable("go/bin/go", $"""
            #!/bin/sh
            echo "$(pwd)|$GOTOOLCHAIN|$*" >> '{_log}'
            {build}
            """);
    }

    public string Checkout { get; }

    /// <summary>Root is the fake Go's GOROOT: go is in its bin directory.</summary>
    public string Root { get; }

    /// <summary>Output is where the edge has protostrap built: obj/protostrap in the AppHost.</summary>
    public string Output { get; }

    public string[] Builds => File.Exists(_log) ? File.ReadAllLines(_log) : [];

    /// <summary>Touch sets when every file in the checkout last changed.</summary>
    public void Touch(DateTime time)
    {
        foreach (var file in Directory.EnumerateFiles(Checkout, "*", SearchOption.AllDirectories))
            File.SetLastWriteTimeUtc(file, time);
    }

    /// <summary>
    /// Binary is a ProtoStrapBinary that sees only the fixture: PATH is the given directory
    /// (an empty one by default), with the fake Go's bin after it when it's on the path, and
    /// with a fake git that says the checkout is at head, if there is one. ProtoStrap.Hosting
    /// is 0.1.0.
    /// </summary>
    public ProtoStrapBinary Binary(
        string? path = null, Dictionary<string, string>? variables = null, string[]? installs = null, string? checkout = "", string? head = null)
    {
        var dirs = new List<string> { path ?? Directory.CreateDirectory(_repo.PathOf("empty-path")).FullName };
        if (_onPath)
            dirs.Add(Path.Combine(Root, "bin"));
        if (head is not null)
        {
            _repo.WriteExecutable("git/bin/git", $"""
                #!/bin/sh
                [ "$*" = "-C {Checkout} rev-parse HEAD" ] && echo {head} && exit 0
                exit 128
                """);
            dirs.Add(_repo.PathOf("git/bin"));
        }
        var values = new Dictionary<string, string>(variables ?? []) { ["PATH"] = string.Join(Path.PathSeparator, dirs) };
        return new ProtoStrapBinary
        {
            Checkout = checkout == "" ? Checkout : checkout,
            Variable = name => values.GetValueOrDefault(name),
            GoInstalls = installs ?? [],
            HostingVersion = "0.1.0",
        };
    }
}

/// <summary>ListLogger keeps what is logged.</summary>
internal sealed class ListLogger : ILogger
{
    public List<(LogLevel Level, string Message)> Entries { get; } = [];

    public IDisposable? BeginScope<TState>(TState state) where TState : notnull => null;

    public bool IsEnabled(LogLevel logLevel) => true;

    public void Log<TState>(LogLevel logLevel, EventId eventId, TState state, Exception? exception, Func<TState, Exception?, string> formatter) =>
        Entries.Add((logLevel, formatter(state, exception)));
}

/// <summary>GoBinaries writes files that look to ProtoStrap.Hosting like binaries Go built.</summary>
internal static class GoBinaries
{
    /// <summary>
    /// GoBinary writes a file with a Go build info blob among other bytes, as Go's linker writes
    /// one: the module's path and, with a revision, the commit and whether the tree had changes.
    /// </summary>
    public static string Write(FixtureRepo repo, string relative, string? revision, bool modified = false, DateTime? built = null)
    {
        var info = "path\tgithub.com/duncan-G/protostrap/cmd/protostrap\nmod\tgithub.com/duncan-G/protostrap\t(devel)\t\n";
        if (revision is not null)
            info += $"build\tvcs=git\nbuild\tvcs.revision={revision}\nbuild\tvcs.time=2026-10-30T23:57:26Z\nbuild\tvcs.modified={(modified ? "true" : "false")}\n";
        byte[] start = [0x30, 0x77, 0xaf, 0x0c, 0x92, 0x74, 0x08, 0x02, 0x41, 0xe1, 0xc1, 0x07, 0xe6, 0xd6, 0x18, 0xe6];
        byte[] end = [0xf9, 0x32, 0x43, 0x31, 0x86, 0x18, 0x20, 0x72, 0x00, 0x82, 0x42, 0x10, 0x41, 0x16, 0xd8, 0xf2];
        byte[] mod = [.. start, .. Encoding.UTF8.GetBytes(info), .. end];
        var bytes = new List<byte>();
        bytes.AddRange(Encoding.ASCII.GetBytes("\x7fELF and the rest of a program; "));
        while (bytes.Count % 16 != 0)
            bytes.Add(0);
        bytes.Add(0xff);
        bytes.AddRange(Encoding.ASCII.GetBytes(" Go buildinf:"));
        bytes.AddRange([8, 2]);
        bytes.AddRange(new byte[16]);
        foreach (var value in new[] { Encoding.ASCII.GetBytes("go1.27.3"), mod })
        {
            for (var n = (uint)value.Length; ; n >>= 7)
            {
                bytes.Add((byte)(n < 0x80 ? n : (n & 0x7f) | 0x80));
                if (n < 0x80)
                    break;
            }
            bytes.AddRange(value);
        }
        bytes.AddRange(Encoding.ASCII.GetBytes("and more of the program"));
        var path = repo.PathOf(relative);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        File.WriteAllBytes(path, [.. bytes]);
        if (built is { } time)
            File.SetLastWriteTimeUtc(path, time);
        return path;
    }
}
