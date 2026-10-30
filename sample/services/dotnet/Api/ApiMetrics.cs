using System.Text;
using Sample.Shared;
using StackExchange.Redis;

namespace Sample.Api;

/// <summary>ApiMetrics renders the Prometheus text served on the metrics port.</summary>
public sealed class ApiMetrics(IServiceProvider services, ILogger<ApiMetrics> log)
{
    private long requests;

    public void CountRequest() => Interlocked.Increment(ref requests);

    public async Task<string> RenderAsync()
    {
        var text = new StringBuilder();
        Write(text, "jobs_budget_cap", "gauge",
            "Replica cap for the jobs worker: 0 once this month's jobs reach JOBS_BUDGET, else -1 for no cap.",
            await BudgetCapAsync());
        Write(text, "api_requests_total", "counter",
            "Requests on the app port, health checks left out.", Interlocked.Read(ref requests));
        Write(text, "dotnet_threadpool_queue_length", "gauge",
            "Work items waiting for a thread-pool thread.", ThreadPool.PendingWorkItemCount);
        Write(text, "dotnet_threadpool_threads", "gauge",
            "Thread-pool threads.", ThreadPool.ThreadCount);
        return text.ToString();
    }

    private static void Write(StringBuilder text, string name, string type, string help, long value) =>
        text.Append("# HELP ").Append(name).Append(' ').Append(help).Append('\n')
            .Append("# TYPE ").Append(name).Append(' ').Append(type).Append('\n')
            .Append(name).Append(' ').Append(value).Append('\n');

    private async Task<long> BudgetCapAsync()
    {
        var budget = Budget.Limit();
        if (budget <= 0)
            return -1;
        try
        {
            var redis = services.GetRequiredService<IConnectionMultiplexer>();
            if (!redis.IsConnected)
                return -1;
            var value = await redis.GetDatabase().StringGetAsync(Budget.SpentKey(DateTime.UtcNow));
            var spent = (long?)value ?? 0;
            return spent >= budget ? 0 : -1;
        }
        catch (Exception ex)
        {
            log.LogWarning("Can't read this month's jobs from Redis, so no cap: {Error}", ex.Message);
            return -1;
        }
    }
}
