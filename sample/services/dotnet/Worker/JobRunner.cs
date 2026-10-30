using System.Globalization;
using System.Text.Json;
using Amazon.SQS;
using Amazon.SQS.Model;
using Npgsql;
using Sample.Shared;
using StackExchange.Redis;

namespace Sample.Worker;

/// <summary>
/// JobRunner takes jobs from the queue, JOBS_CONCURRENCY at a time. A job burns a CPU for its
/// work_ms, records itself in jobs_done, and only then is deleted, so a job whose replica
/// dies runs again elsewhere; running one twice is safe. One replica does about two 1-second
/// jobs a second, the rate the spec's queue signal assumes.
/// </summary>
/// <remarks>
/// On stop it receives no more, lets the jobs in flight finish, and gives back any message it
/// received but hadn't started, with visibility 0, so another replica takes it at once.
/// </remarks>
public sealed class JobRunner(IServiceProvider services, Identity me, ILogger<JobRunner> log) : BackgroundService
{
    private readonly string? queueUrl = Env.Get("QUEUE_JOBS_URL");
    private readonly int concurrency = Math.Max(1, Env.Int("JOBS_CONCURRENCY", 2));
    private readonly long budget = Budget.Limit();
    private IAmazonSQS sqs = null!;
    private NpgsqlDataSource db = null!;
    private IDatabase redis = null!;

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        if (queueUrl is null)
        {
            log.LogWarning("QUEUE_JOBS_URL is not set: idling");
            try
            {
                await Task.Delay(Timeout.Infinite, stoppingToken);
            }
            catch (OperationCanceledException)
            {
            }
            return;
        }

        sqs = services.GetRequiredService<IAmazonSQS>();
        db = services.GetRequiredService<NpgsqlDataSource>();
        redis = services.GetRequiredService<IConnectionMultiplexer>().GetDatabase();
        log.LogInformation("Taking jobs, {Concurrency} at a time", concurrency);

        var stopped = Task.Delay(Timeout.Infinite, stoppingToken);
        var running = new List<Task>();
        while (!stoppingToken.IsCancellationRequested)
        {
            running.RemoveAll(job => job.IsCompleted);
            if (running.Count >= concurrency)
            {
                await Task.WhenAny(running.Append(stopped));
                continue;
            }

            var messages = await ReceiveAsync(concurrency - running.Count, stoppingToken);
            if (stoppingToken.IsCancellationRequested)
            {
                await ReleaseAsync(messages);
                break;
            }
            running.AddRange(messages.Select(message => Task.Run(() => RunAsync(message))));
        }

        await Task.WhenAll(running);
        log.LogInformation("Stopped taking jobs, and the last ones are done");
    }

    private async Task<List<Message>> ReceiveAsync(int max, CancellationToken stoppingToken)
    {
        try
        {
            // The receive is deliberately not cancelled on stop: a receive cut short can lose
            // messages SQS has already handed out, hidden until their visibility timeout. One
            // that returns after the stop is given back instead.
            var response = await sqs.ReceiveMessageAsync(new ReceiveMessageRequest
            {
                QueueUrl = queueUrl,
                MaxNumberOfMessages = Math.Min(max, 10),
                WaitTimeSeconds = 20,
            }, CancellationToken.None);
            return response.Messages ?? [];
        }
        catch (Exception ex)
        {
            log.LogError(ex, "Receiving jobs failed");
            try
            {
                await Task.Delay(TimeSpan.FromSeconds(5), stoppingToken);
            }
            catch (OperationCanceledException)
            {
            }
            return [];
        }
    }

    private async Task RunAsync(Message message)
    {
        string? jobId = null;
        try
        {
            var job = Parse(message.Body);
            if (job is null)
            {
                log.LogWarning("Deleting malformed message {MessageId}", message.MessageId);
                await DeleteAsync(message);
                return;
            }
            jobId = job.Id;

            if (await IsDoneAsync(job.Id))
            {
                log.LogInformation("Job {JobId} was already done", job.Id);
                await DeleteAsync(message);
                return;
            }
            if (!await ReserveAsync())
            {
                // The month is over budget, so the job waits in the queue, and the api's cap
                // scales workers to zero.
                await SetVisibilityAsync(message, 300);
                log.LogWarning("Job {JobId} put back: this month's budget of {Budget} jobs is spent", job.Id, budget);
                return;
            }

            var elapsed = Cpu.BurnWallTime(Math.Clamp(job.WorkMs ?? Jobs.DefaultWorkMs, 0, Jobs.MaxWorkMs));
            await RecordAsync(job.Id);
            await DeleteAsync(message);
            log.LogInformation("Job {JobId} done in {ElapsedMs} ms", job.Id, (int)elapsed.TotalMilliseconds);
        }
        catch (Exception ex)
        {
            log.LogError(ex, "Job {JobId} failed; it returns to the queue after its visibility timeout", jobId);
        }
    }

    private static JobMessage? Parse(string body)
    {
        try
        {
            return JsonSerializer.Deserialize<JobMessage>(body) is { Id.Length: > 0 } job ? job : null;
        }
        catch (JsonException)
        {
            return null;
        }
    }

    private async Task<bool> IsDoneAsync(string id)
    {
        await using var command = db.CreateCommand("SELECT EXISTS (SELECT 1 FROM jobs_done WHERE id = @id)");
        command.Parameters.AddWithValue("id", id);
        return (bool)(await command.ExecuteScalarAsync())!;
    }

    /// <summary>
    /// ReserveAsync counts the job in this month's spend before running it, and refuses it
    /// if that passes JOBS_BUDGET (when set), so the budget holds while the cap is scaling
    /// workers down.
    /// </summary>
    private async Task<bool> ReserveAsync()
    {
        var key = Budget.SpentKey(DateTime.UtcNow);
        var spent = await redis.StringIncrementAsync(key);
        if (budget <= 0 || spent <= budget)
            return true;
        await redis.StringDecrementAsync(key);
        return false;
    }

    private async Task RecordAsync(string id)
    {
        await using var command = db.CreateCommand(
            "INSERT INTO jobs_done (id, done_at, replica) VALUES (@id, now(), @replica) ON CONFLICT (id) DO NOTHING");
        command.Parameters.AddWithValue("id", id);
        command.Parameters.AddWithValue("replica", me.Replica);
        await command.ExecuteNonQueryAsync();
    }

    private Task DeleteAsync(Message message) =>
        sqs.DeleteMessageAsync(new DeleteMessageRequest { QueueUrl = queueUrl, ReceiptHandle = message.ReceiptHandle });

    private Task SetVisibilityAsync(Message message, int seconds) =>
        sqs.ChangeMessageVisibilityAsync(new ChangeMessageVisibilityRequest
        {
            QueueUrl = queueUrl,
            ReceiptHandle = message.ReceiptHandle,
            VisibilityTimeout = seconds,
        });

    private async Task ReleaseAsync(List<Message> messages)
    {
        if (messages.Count == 0)
            return;
        try
        {
            await sqs.ChangeMessageVisibilityBatchAsync(new ChangeMessageVisibilityBatchRequest
            {
                QueueUrl = queueUrl,
                Entries = messages.Select((message, i) => new ChangeMessageVisibilityBatchRequestEntry
                {
                    Id = i.ToString(CultureInfo.InvariantCulture),
                    ReceiptHandle = message.ReceiptHandle,
                    VisibilityTimeout = 0,
                }).ToList(),
            });
            log.LogInformation("Gave back {Count} jobs not started", messages.Count);
        }
        catch (Exception ex)
        {
            log.LogWarning(ex, "Giving back {Count} jobs failed; they return after their visibility timeout", messages.Count);
        }
    }
}
