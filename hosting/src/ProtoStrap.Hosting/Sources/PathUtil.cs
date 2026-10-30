namespace ProtoStrap.Hosting.Sources;

internal static class PathUtil
{
    /// <summary>IsInside says whether a full path is a directory or inside it.</summary>
    public static bool IsInside(string path, string directory)
    {
        var relative = Path.GetRelativePath(directory, path);
        return relative == "."
            || (relative != ".."
                && !relative.StartsWith(".." + Path.DirectorySeparatorChar, StringComparison.Ordinal)
                && !relative.StartsWith("../", StringComparison.Ordinal)
                && !Path.IsPathRooted(relative));
    }

    /// <summary>
    /// Relative returns a full path relative to a directory, with forward slashes, as the spec
    /// writes paths.
    /// </summary>
    public static string Relative(string directory, string path) =>
        Path.GetRelativePath(directory, path).Replace('\\', '/');
}
