using System.Globalization;
using System.Text.RegularExpressions;

namespace ProtoStrap.Hosting.Spec;

/// <summary>
/// Quantity reads the quantities an AppHost sets and writes them the one way the spec does, with
/// the units and rules of internal/spec/quantity.go. Each method but Decimal returns null for a
/// value that isn't a quantity of its kind.
/// </summary>
internal static partial class Quantity
{
    /// <summary>
    /// Cpu reads CPU as millicores, "500m", or as cores, "2" or "0.5", and writes it in
    /// millicores, like "500m".
    /// </summary>
    public static string? Cpu(string cpu)
    {
        var millicores = cpu.EndsWith('m')
            ? Whole(Number(cpu[..^1]))
            : Whole(Number(cpu) * 1000);
        return millicores is { } m ? m.ToString(CultureInfo.InvariantCulture) + "m" : null;
    }

    /// <summary>
    /// Memory reads memory with a binary unit, Ki to Ti, and writes it in Gi when whole, otherwise
    /// in Mi.
    /// </summary>
    public static string? Memory(string memory)
    {
        var mib = WithUnit(memory, ("Ki", 1.0 / 1024), ("Mi", 1), ("Gi", 1024), ("Ti", 1024 * 1024));
        return mib is { } m ? FormatMiB(m) : null;
    }

    /// <summary>Size reads a volume's size, a whole, positive number of GiB, from Gi or Ti.</summary>
    public static string? Size(string size)
    {
        var gib = WithUnit(size, ("Gi", 1), ("Ti", 1024));
        return gib is > 0 ? gib.Value.ToString(CultureInfo.InvariantCulture) + "Gi" : null;
    }

    /// <summary>Percent writes a fraction above 0 and at most 1 as a percentage: 0.70 is "70%".</summary>
    public static string? Percent(double fraction) =>
        fraction > 0 && fraction <= 1 ? Decimal(fraction * 100) + "%" : null;

    /// <summary>Rate writes a positive rate per second: 2 is "2/s".</summary>
    public static string? Rate(double perSecond) =>
        perSecond > 0 && double.IsFinite(perSecond) ? Decimal(perSecond) + "/s" : null;

    /// <summary>Duration writes a duration in its largest whole unit: 60 seconds is "1m", 90 is "90s".</summary>
    public static string? Duration(TimeSpan duration)
    {
        if (duration < TimeSpan.Zero)
            return null;
        if (duration == TimeSpan.Zero)
            return "0s";
        foreach (var (unit, length) in new[] { ("d", TimeSpan.FromDays(1)), ("h", TimeSpan.FromHours(1)), ("m", TimeSpan.FromMinutes(1)), ("s", TimeSpan.FromSeconds(1)), ("ms", TimeSpan.FromMilliseconds(1)) })
        {
            if (duration.Ticks % length.Ticks == 0)
                return (duration.Ticks / length.Ticks).ToString(CultureInfo.InvariantCulture) + unit;
        }
        return null;
    }

    /// <summary>Decimal writes a number the way the spec writes one: no exponent, no trailing zeros.</summary>
    public static string Decimal(double value) =>
        Math.Round(value, 9).ToString("0.#########", CultureInfo.InvariantCulture);

    private static string FormatMiB(long mib) =>
        mib > 0 && mib % 1024 == 0
            ? (mib / 1024).ToString(CultureInfo.InvariantCulture) + "Gi"
            : mib.ToString(CultureInfo.InvariantCulture) + "Mi";

    private static long? WithUnit(string text, params (string Suffix, double Factor)[] units)
    {
        foreach (var (suffix, factor) in units)
        {
            if (text.EndsWith(suffix, StringComparison.Ordinal))
                return Whole(Number(text[..^suffix.Length]) * factor);
        }
        return null;
    }

    // Number reads a non-negative decimal, refusing signs, exponents, separators and spaces, as
    // Go's number() does.
    private static double Number(string text) =>
        DecimalNumber().IsMatch(text) ? double.Parse(text, CultureInfo.InvariantCulture) : double.NaN;

    private static long? Whole(double value)
    {
        if (double.IsNaN(value) || value < 0)
            return null;
        var rounded = Math.Round(value);
        return Math.Abs(value - rounded) < 1e-9 ? (long)rounded : null;
    }

    [GeneratedRegex(@"^([0-9]+(\.[0-9]*)?|\.[0-9]+)$")]
    private static partial Regex DecimalNumber();
}
