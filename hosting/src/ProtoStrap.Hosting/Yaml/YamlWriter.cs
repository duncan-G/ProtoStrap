using System.Globalization;
using System.Text;
using System.Text.RegularExpressions;

namespace ProtoStrap.Hosting.Yaml;

/// <summary>
/// YamlWriter writes the YAML protostrap.yaml is made of: block and flow mappings and sequences,
/// and plain and double-quoted scalars. ProtoStrap's own parser (internal/yaml) reads exactly
/// this, and so does Terraform's yamldecode, which is why a string is plain only when no YAML 1.1
/// or 1.2 reader could take it for a number, a boolean, a null or a date.
/// </summary>
internal static partial class YamlWriter
{
    public const int DefaultWidth = 120;

    /// <summary>Write returns a document's text: the header as comments, then the root mapping.</summary>
    public static string Write(YamlMap root, IEnumerable<string>? header = null, int width = DefaultWidth)
    {
        var output = new StringBuilder();
        foreach (var line in header ?? [])
            output.Append("# ").Append(line).Append('\n');
        WriteMapEntries(output, root, indent: 0, width);
        return output.ToString();
    }

    private static void WriteMapEntries(StringBuilder output, YamlMap map, int indent, int width)
    {
        var pad = new string(' ', indent);
        for (var i = 0; i < map.Count; i++)
        {
            if (map.BreakBefore(i))
                output.Append('\n');
            var (key, value) = map.Entries[i];
            var prefix = pad + Key(key) + ":";
            WriteValue(output, prefix, value, indent, width);
        }
    }

    /// <summary>
    /// WriteValue writes "prefix value" on one line if it can, otherwise the prefix and a block.
    /// </summary>
    private static void WriteValue(StringBuilder output, string prefix, YamlNode value, int indent, int width)
    {
        if (value is YamlScalar scalar)
        {
            output.Append(prefix).Append(' ').Append(Scalar(scalar, inFlow: false)).Append('\n');
            return;
        }
        if (Flow(value, prefix.Length + 1, width) is { } flow)
        {
            output.Append(prefix).Append(' ').Append(flow).Append('\n');
            return;
        }
        output.Append(prefix).Append('\n');
        switch (value)
        {
            case YamlMap map:
                WriteMapEntries(output, map, indent + 2, width);
                break;
            case YamlSeq seq:
                var pad = new string(' ', indent + 2);
                foreach (var item in seq.Items)
                {
                    // An item that can be one line is, however long: a block mapping inside a
                    // block sequence is harder to read than a long line.
                    if (item is not YamlScalar && CanFlow(item))
                        output.Append(pad).Append("- ").Append(FlowText(item)).Append('\n');
                    else
                        WriteValue(output, pad + "-", item, indent + 2, width);
                }
                break;
        }
    }

    /// <summary>
    /// Flow returns the value in flow style if it may be written so and fits in the width from
    /// column, or null if it must be a block. Empty collections are always written in flow style.
    /// </summary>
    private static string? Flow(YamlNode node, int column, int width)
    {
        var empty = node is YamlMap { Count: 0 } || node is YamlSeq { Items.Count: 0 };
        if (empty)
            return node is YamlMap ? "{}" : "[]";
        if (!CanFlow(node))
            return null;
        var flow = FlowText(node);
        return column + flow.Length <= width ? flow : null;
    }

    private static bool CanFlow(YamlNode node) => node switch
    {
        YamlScalar => true,
        YamlMap { Count: 0 } or YamlSeq { Items.Count: 0 } => true,
        YamlMap map => map.Style == YamlStyle.Auto && map.Entries.All(e => CanFlow(e.Value)),
        YamlSeq seq => seq.Style == YamlStyle.Auto && seq.Items.All(CanFlow),
        _ => false,
    };

    private static string FlowText(YamlNode node) => node switch
    {
        YamlScalar scalar => Scalar(scalar, inFlow: true),
        YamlMap { Count: 0 } => "{}",
        YamlSeq { Items.Count: 0 } => "[]",
        YamlMap map => "{ " + string.Join(", ", map.Entries.Select(e => Key(e.Key) + ": " + FlowText(e.Value))) + " }",
        YamlSeq seq => "[" + string.Join(", ", seq.Items.Select(FlowText)) + "]",
        _ => throw new InvalidOperationException(),
    };

    private static string Key(string key) => Scalar(key, inFlow: true);

    private static string Scalar(YamlScalar scalar, bool inFlow) =>
        scalar.IsString ? Scalar(scalar.Value, inFlow) : scalar.Value;

    /// <summary>Scalar writes a string as a scalar: plain if it is unambiguous, otherwise double-quoted.</summary>
    public static string Scalar(string value, bool inFlow) =>
        NeedsQuotes(value, inFlow) ? Quote(value) : value;

    private static bool NeedsQuotes(string value, bool inFlow)
    {
        if (value.Length == 0 || value != value.Trim())
            return true;
        if (!PlainCharacters().IsMatch(value))
            return true;
        if (NotAString().IsMatch(value))
            return true;
        switch (value[0])
        {
            case '-' when value.Length == 1 || value[1] == ' ':
            case '?' or ':' or ',' or '[' or ']' or '{' or '}' or '#' or '&' or '*' or '!' or '|' or '>' or '\'' or '"' or '%' or '@' or '`':
                return true;
        }
        if (value.Contains(": ", StringComparison.Ordinal) || value.EndsWith(':') || value.Contains(" #", StringComparison.Ordinal))
            return true;
        return inFlow && value.IndexOfAny([',', '[', ']', '{', '}', ':']) >= 0;
    }

    private static string Quote(string value)
    {
        var quoted = new StringBuilder("\"");
        foreach (var c in value)
        {
            switch (c)
            {
                case '"': quoted.Append("\\\""); break;
                case '\\': quoted.Append("\\\\"); break;
                case '\n': quoted.Append("\\n"); break;
                case '\t': quoted.Append("\\t"); break;
                case '\r': quoted.Append("\\r"); break;
                case < ' ' or '\u007f':
                    quoted.Append("\\x").Append(((int)c).ToString("x2", CultureInfo.InvariantCulture));
                    break;
                default: quoted.Append(c); break;
            }
        }
        return quoted.Append('"').ToString();
    }

    // PlainCharacters matches printable ASCII, which leaves out the characters that change a plain
    // scalar's meaning anywhere in it.
    [GeneratedRegex(@"^[\x20-\x7e]+$")]
    private static partial Regex PlainCharacters();

    // NotAString matches what a YAML 1.1 or 1.2 reader may take for something other than a
    // string: booleans (including y and n), nulls, integers in any base, floats, infinities, NaN,
    // sexagesimal numbers and dates.
    [GeneratedRegex("""
        ^(
            y|Y|yes|Yes|YES|n|N|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF
          | ~|null|Null|NULL
          | [-+]?[0-9][0-9_]*
          | [-+]?0[xX][0-9a-fA-F_]+
          | [-+]?0[oO]?[0-7_]+
          | [-+]?0[bB][01_]+
          | [-+]?([0-9][0-9_]*)?\.[0-9_]*([eE][-+]?[0-9]+)?
          | [-+]?[0-9][0-9_]*([eE][-+]?[0-9]+)
          | [-+]?\.(inf|Inf|INF)|\.(nan|NaN|NAN)
          | [-+]?[0-9][0-9_]*(:[0-5]?[0-9])+(\.[0-9_]*)?
          | [0-9]{4}-[0-9]{1,2}-[0-9]{1,2}.*
        )$
        """, RegexOptions.IgnorePatternWhitespace)]
    private static partial Regex NotAString();
}
