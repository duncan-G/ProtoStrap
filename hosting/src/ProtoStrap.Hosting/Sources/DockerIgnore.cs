namespace ProtoStrap.Hosting.Sources;

/// <summary>
/// DockerIgnore turns a Dockerfile build's .dockerignore into git pathspecs that leave its files
/// out of the image tag's sources. Docker matches patterns from the context's root, with * not
/// crossing a / and ** crossing any number, which is what git's glob magic does; and a pattern
/// that matches a directory leaves out everything in it, so each pattern also gets a /** form.
/// </summary>
internal static class DockerIgnore
{
    /// <summary>
    /// Exclusions returns the exclusions, relative to root, and a warning if the file can't be
    /// followed.
    /// </summary>
    public static (IReadOnlyList<string> Exclusions, string? Warning) Exclusions(string context, string dockerfile, string root)
    {
        // BuildKit reads <Dockerfile>.dockerignore beside the Dockerfile first.
        var file = new[] { dockerfile + ".dockerignore", Path.Combine(context, ".dockerignore") }.FirstOrDefault(File.Exists);
        if (file is null)
            return ([], null);

        var patterns = File.ReadAllLines(file)
            .Select(l => l.Trim())
            .Where(l => l.Length > 0 && !l.StartsWith('#'))
            .ToList();
        if (patterns.Any(p => p.StartsWith('!')))
        {
            return ([], $"{PathUtil.Relative(root, file)} re-includes files with !, which a pathspec can't say: " +
                "every tracked file in the context counts toward the image tag");
        }

        var prefix = PathUtil.Relative(root, context);
        prefix = prefix == "." ? "" : prefix + "/";
        var exclusions = new List<string>();
        foreach (var raw in patterns)
        {
            var pattern = raw.Replace('\\', '/').Trim('/');
            while (pattern.StartsWith("./", StringComparison.Ordinal))
                pattern = pattern[2..];
            if (pattern.Length == 0 || pattern == "." || pattern == ".." || pattern.StartsWith("../", StringComparison.Ordinal))
                continue;
            exclusions.Add($":(exclude,glob){prefix}{pattern}");
            if (!pattern.EndsWith("/**", StringComparison.Ordinal))
                exclusions.Add($":(exclude,glob){prefix}{pattern}/**");
        }
        return (exclusions, null);
    }
}
