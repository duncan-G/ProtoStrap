namespace ProtoStrap.Hosting.Tests.Support;

/// <summary>
/// FixtureRepo is a repository in a temporary directory: a <c>.git</c> directory, so the
/// generator finds its root, an <c>apphost</c> directory with a project file, and whatever files
/// a test writes.
/// </summary>
public sealed class FixtureRepo : IDisposable
{
    public FixtureRepo()
    {
        Root = Path.Combine(Path.GetTempPath(), "protostrap-hosting-tests", Guid.NewGuid().ToString("n"));
        Directory.CreateDirectory(Path.Combine(Root, ".git"));
        Write("apphost/Test.AppHost.csproj", "<Project Sdk=\"Microsoft.NET.Sdk\" />\n");
    }

    public string Root { get; }

    public string AppHost => Path.Combine(Root, "apphost");

    public string SpecPath => Path.Combine(Root, "protostrap.yaml");

    /// <summary>PathOf returns the absolute path of a repository-relative one.</summary>
    public string PathOf(string relative) => Path.Combine(Root, relative);

    public string Write(string relative, string contents)
    {
        var path = PathOf(relative);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        File.WriteAllText(path, contents);
        return path;
    }

    /// <summary>
    /// Project writes an SDK-style project. References are repository-relative project paths.
    /// </summary>
    public string Project(string relative, string sdk = "Microsoft.NET.Sdk", string extra = "", params string[] references)
    {
        var dir = Path.GetDirectoryName(relative)!;
        var refs = string.Concat(references.Select(r =>
            $"    <ProjectReference Include=\"{Path.GetRelativePath(PathOf(dir), PathOf(r))}\" />\n"));
        var path = Write(relative, $"""
            <Project Sdk="{sdk}">
              <PropertyGroup>
                <TargetFramework>net10.0</TargetFramework>
              </PropertyGroup>
            {extra}
              <ItemGroup>
            {refs}  </ItemGroup>
            </Project>

            """);
        Write(Path.Combine(dir, "Program.cs"), "return;\n");
        return path;
    }

    /// <summary>WriteExecutable writes a script the tests can run.</summary>
    public string WriteExecutable(string relative, string contents)
    {
        var path = Write(relative, contents);
        if (!OperatingSystem.IsWindows())
            File.SetUnixFileMode(path, UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute);
        return path;
    }

    public void Dispose()
    {
        try
        {
            Directory.Delete(Root, recursive: true);
        }
        catch (IOException)
        {
        }
    }
}
