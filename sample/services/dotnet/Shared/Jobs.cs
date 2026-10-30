using System.Globalization;
using System.Text.Json.Serialization;

namespace Sample.Shared;

/// <summary>JobMessage is a message on the jobs queue, such as {"id": "...", "work_ms": 1000}.</summary>
public sealed record JobMessage(
    [property: JsonPropertyName("id")] string Id,
    [property: JsonPropertyName("work_ms")] int? WorkMs);

public static class Jobs
{
    public const int DefaultWorkMs = 1000;
    public const int MaxWorkMs = 10_000;
}

/// <summary>
/// Budget is the monthly budget for jobs. Workers count each job in a Redis counter per
/// calendar month (UTC), and the api exports a replica cap from it, so the cap is known while
/// workers are at zero.
/// </summary>
public static class Budget
{
    public static string SpentKey(DateTime utcNow) =>
        "jobs:spent:" + utcNow.ToString("yyyy-MM", CultureInfo.InvariantCulture);

    /// <summary>Limit returns JOBS_BUDGET, the jobs allowed per month, or 0 when there is no budget.</summary>
    public static long Limit() =>
        long.TryParse(Env.Get("JOBS_BUDGET"), CultureInfo.InvariantCulture, out var value) && value > 0 ? value : 0;
}
