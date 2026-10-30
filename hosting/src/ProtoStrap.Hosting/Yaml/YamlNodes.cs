using System.Collections;
using System.Globalization;

namespace ProtoStrap.Hosting.Yaml;

/// <summary>YamlStyle says how a collection is written.</summary>
internal enum YamlStyle
{
    /// <summary>Auto writes the collection on one line if it fits, otherwise one entry per line.</summary>
    Auto,

    /// <summary>Block writes one entry per line, so that adding one is a one-line diff.</summary>
    Block,
}

internal abstract class YamlNode
{
    public static implicit operator YamlNode(string value) => YamlScalar.String(value);
}

/// <summary>
/// YamlScalar is a scalar: a string, which the writer quotes when it must, or a number or boolean.
/// </summary>
internal sealed class YamlScalar : YamlNode
{
    private YamlScalar(string value, bool isString)
    {
        Value = value;
        IsString = isString;
    }

    public string Value { get; }

    /// <summary>IsString is false for numbers and booleans, which are written as they are.</summary>
    public bool IsString { get; }

    public static YamlScalar String(string value) => new(value, isString: true);

    public static YamlScalar Number(long value) => new(value.ToString(CultureInfo.InvariantCulture), isString: false);

    public static YamlScalar Number(double value) => new(value.ToString("0.############", CultureInfo.InvariantCulture), isString: false);

    public static YamlScalar Bool(bool value) => new(value ? "true" : "false", isString: false);
}

/// <summary>
/// YamlMap is a mapping, in the order its entries were added. A null value leaves the entry out.
/// </summary>
internal sealed class YamlMap(YamlStyle style = YamlStyle.Auto) : YamlNode, IEnumerable<KeyValuePair<string, YamlNode>>
{
    private readonly List<KeyValuePair<string, YamlNode>> _entries = [];
    private readonly HashSet<int> _breaks = [];

    public YamlStyle Style { get; } = style;

    /// <summary>BlankLineBetweenEntries puts a blank line between entries, as between components.</summary>
    public bool BlankLineBetweenEntries { get; init; }

    public int Count => _entries.Count;

    public IReadOnlyList<KeyValuePair<string, YamlNode>> Entries => _entries;

    public YamlNode? this[string key]
    {
        get => _entries.FirstOrDefault(e => e.Key == key).Value;
        set
        {
            _entries.RemoveAll(e => e.Key == key);
            if (value is not null)
                _entries.Add(new(key, value));
        }
    }

    /// <summary>AddGroupBreak puts a blank line before the next entry, which starts a new group.</summary>
    public void AddGroupBreak() => _breaks.Add(_entries.Count);

    /// <summary>BreakBefore says whether a blank line goes before the entry at index.</summary>
    public bool BreakBefore(int index) => index > 0 && (BlankLineBetweenEntries || _breaks.Contains(index));

    public IEnumerator<KeyValuePair<string, YamlNode>> GetEnumerator() => _entries.GetEnumerator();

    IEnumerator IEnumerable.GetEnumerator() => GetEnumerator();
}

/// <summary>YamlSeq is a sequence.</summary>
internal sealed class YamlSeq : YamlNode, IEnumerable<YamlNode>
{
    private readonly List<YamlNode> _items;

    public YamlSeq(params YamlNode[] items)
        : this(YamlStyle.Auto, items)
    {
    }

    public YamlSeq(YamlStyle style, params YamlNode[] items)
    {
        Style = style;
        _items = [.. items];
    }

    public YamlSeq(YamlStyle style, IEnumerable<YamlNode> items)
    {
        Style = style;
        _items = [.. items];
    }

    public YamlStyle Style { get; }

    public IReadOnlyList<YamlNode> Items => _items;

    public IEnumerator<YamlNode> GetEnumerator() => _items.GetEnumerator();

    IEnumerator IEnumerable.GetEnumerator() => GetEnumerator();
}
