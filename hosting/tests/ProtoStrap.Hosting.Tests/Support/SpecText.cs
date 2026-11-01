namespace ProtoStrap.Hosting.Tests.Support;

/// <summary>
/// SpecText picks parts out of a generated spec, so a test can compare just the part it is
/// about.
/// </summary>
public static class SpecText
{
    /// <summary>Component returns a component's entry, without the indentation it has under components.</summary>
    public static string Component(string yaml, string name) => Entry(yaml, "components", name);

    /// <summary>Section returns a top-level section, such as pools, with its own key line.</summary>
    public static string Section(string yaml, string key)
    {
        var lines = Lines(yaml);
        var start = lines.FindIndex(l => l == key + ":" || l.StartsWith(key + ": ", StringComparison.Ordinal));
        Assert.True(start >= 0, $"No {key} in:\n{yaml}");
        var end = start + 1;
        while (end < lines.Count && (lines[end].StartsWith(' ') || lines[end].Length == 0))
            end++;
        return Join(lines[start..end]);
    }

    /// <summary>
    /// Upstream returns one of the gateway's upstreams, without the indentation it has under
    /// gateway.upstreams.
    /// </summary>
    public static string Upstream(string yaml, string name)
    {
        var lines = Lines(yaml);
        var gateway = lines.IndexOf("gateway:");
        Assert.True(gateway >= 0, $"No gateway in:\n{yaml}");
        var upstreams = lines.IndexOf("  upstreams:", gateway);
        Assert.True(upstreams >= 0, $"No gateway.upstreams in:\n{yaml}");
        var start = lines.FindIndex(upstreams, l => l == "    " + name + ":" || l.StartsWith("    " + name + ": ", StringComparison.Ordinal));
        Assert.True(start >= 0, $"No gateway.upstreams.{name} in:\n{yaml}");
        var end = start + 1;
        while (end < lines.Count && lines[end].StartsWith("      ", StringComparison.Ordinal))
            end++;
        return Join(lines[start..end].Select(l => l[4..]).ToList());
    }

    /// <summary>HasComponent says whether the spec has a component of this name.</summary>
    public static bool HasComponent(string yaml, string name) =>
        Lines(yaml).Contains("  " + name + ":");

    private static string Entry(string yaml, string section, string name)
    {
        var lines = Lines(yaml);
        var sectionStart = lines.IndexOf(section + ":");
        Assert.True(sectionStart >= 0, $"No {section} in:\n{yaml}");
        var start = lines.FindIndex(sectionStart, l => l == "  " + name + ":" || l.StartsWith("  " + name + ": ", StringComparison.Ordinal));
        Assert.True(start >= 0, $"No {section}.{name} in:\n{yaml}");
        var end = start + 1;
        while (end < lines.Count && (lines[end].StartsWith("    ", StringComparison.Ordinal) || lines[end].Length == 0))
            end++;
        return Join(lines[start..end].Select(l => l.Length >= 2 ? l[2..] : l).ToList());
    }

    private static List<string> Lines(string yaml) => yaml.Replace("\r\n", "\n").Split('\n').ToList();

    private static string Join(List<string> lines)
    {
        while (lines.Count > 0 && lines[^1].Length == 0)
            lines.RemoveAt(lines.Count - 1);
        return string.Join("\n", lines) + "\n";
    }
}
