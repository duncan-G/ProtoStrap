using System.Globalization;
using System.Text.Json;
using Amazon.SQS;
using Amazon.SQS.Model;
using Sample.Shared;

namespace Sample.Api;

/// <summary>JobQueue puts jobs on the jobs queue, whose URL ProtoStrap sets in QUEUE_JOBS_URL.</summary>
public sealed class JobQueue(IServiceProvider services, ILogger<JobQueue> log)
{
    public const int MaxCount = 100_000;
    public const int MaxBurnMs = 5000;
    private const int BatchSize = 10; // SendMessageBatch takes at most 10 messages.

    private readonly string? url = Env.Get("QUEUE_JOBS_URL");

    public bool Configured => url is not null;

    /// <returns>It returns how many jobs SQS accepted.</returns>
    public async Task<int> EnqueueAsync(int count, int workMs, CancellationToken cancellationToken)
    {
        if (url is null)
            throw new InvalidOperationException("QUEUE_JOBS_URL is not set");

        // The client is resolved here, not injected, because it needs a region, which local
        // runs don't have.
        var sqs = services.GetRequiredService<IAmazonSQS>();
        count = Math.Clamp(count, 0, MaxCount);
        workMs = Math.Clamp(workMs, 0, Jobs.MaxWorkMs);

        var sent = 0;
        var batches = Enumerable.Range(0, (count + BatchSize - 1) / BatchSize);
        var parallel = new ParallelOptions { MaxDegreeOfParallelism = 8, CancellationToken = cancellationToken };
        await Parallel.ForEachAsync(batches, parallel, async (batch, token) =>
        {
            var entries = Enumerable.Range(0, Math.Min(BatchSize, count - batch * BatchSize))
                .Select(i => new SendMessageBatchRequestEntry
                {
                    Id = i.ToString(CultureInfo.InvariantCulture),
                    MessageBody = JsonSerializer.Serialize(new JobMessage(Guid.NewGuid().ToString(), workMs)),
                })
                .ToList();
            var response = await sqs.SendMessageBatchAsync(new SendMessageBatchRequest { QueueUrl = url, Entries = entries }, token);
            // The v4 SDK leaves empty lists null.
            Interlocked.Add(ref sent, response.Successful?.Count ?? 0);
            if (response.Failed is { Count: > 0 } failed)
                log.LogWarning("{Failed} jobs not sent: {Error}", failed.Count, failed[0].Message);
        });

        log.LogInformation("Enqueued {Sent} jobs of {WorkMs} ms", sent, workMs);
        return sent;
    }
}
