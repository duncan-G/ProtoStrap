using System.Text;
using System.Text.Json;
using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using Microsoft.Extensions.Configuration;
using Microsoft.Extensions.DependencyInjection;
using Microsoft.Extensions.Diagnostics.HealthChecks;

namespace ProtoStrap.Hosting;

/// <summary>
/// LocalStack adds the LocalStack container that backs queues and buckets in run mode:
/// one per AppHost, whose init script creates them with the names ProtoStrap gives them, and
/// which is healthy once the script has run.
/// </summary>
internal static class LocalStack
{
    public const string Name = "protostrap-localstack";

    private const string InitDirectory = "/etc/localstack/init/ready.d";

    public static IResourceBuilder<ProtoStrapLocalStackResource> Ensure(IDistributedApplicationBuilder builder)
    {
        if (builder.Resources.OfType<ProtoStrapLocalStackResource>().SingleOrDefault() is { } existing)
            return builder.CreateResourceBuilder(existing);

        var configuration = builder.Configuration;
        var resource = new ProtoStrapLocalStackResource(Name);
        var healthCheck = Name + "_init";
        builder.Services.AddHealthChecks().Add(new HealthCheckRegistration(
            healthCheck, _ => new LocalStackInitCheck(resource), failureStatus: null, tags: null));

        return builder.AddResource(resource)
            .WithImage("localstack/localstack", "4")
            .WithHttpEndpoint(targetPort: 4566, name: "http")
            .WithEnvironment(context =>
            {
                context.EnvironmentVariables["SERVICES"] = "s3,sqs";
                // Queue URLs say their region, so a client in any region finds the queue.
                context.EnvironmentVariables["SQS_ENDPOINT_STRATEGY"] = "path";
                context.EnvironmentVariables["AWS_DEFAULT_REGION"] = Region(configuration);
            })
            .WithContainerFiles(InitDirectory, (_, _) =>
                Task.FromResult<IEnumerable<ContainerFileSystemItem>>([InitScript(builder.Resources, Region(configuration))]))
            .WithHealthCheck(healthCheck)
            .ExcludeFromManifest();
    }

    /// <summary>
    /// Region returns the region LocalStack and the components that use it run in: AWS_REGION,
    /// otherwise AWS_DEFAULT_REGION, otherwise us-east-1.
    /// </summary>
    public static string Region(IConfiguration configuration) =>
        configuration["AWS_REGION"] is { Length: > 0 } region ? region
        : configuration["AWS_DEFAULT_REGION"] is { Length: > 0 } defaultRegion ? defaultRegion
        : "us-east-1";

    /// <summary>
    /// InitScript makes the init script, which creates the files bucket, if there is one, then
    /// each of the application's buckets, then each queue. A bucket's expiry and browser access
    /// are left to ProtoStrap's account: LocalStack keeps every object and answers every origin.
    /// </summary>
    public static ContainerFile InitScript(IEnumerable<IResource> resources, string region)
    {
        var all = resources.ToList();
        var script = new StringBuilder()
            .Append("#!/bin/bash\n")
            .Append("# Written by ProtoStrap.Hosting: the queues and buckets of the AppHost, as ProtoStrap names them.\n")
            .Append("set -euo pipefail\n");
        void CreateBucket(string name, bool locked)
        {
            script.Append("awslocal s3api create-bucket --bucket ").Append(name);
            if (region != "us-east-1")
                script.Append(" --create-bucket-configuration LocationConstraint=").Append(region);
            if (locked)
                script.Append(" --object-lock-enabled-for-bucket");
            script.Append(" >/dev/null\n");
        }
        if (all.OfType<ProtoStrapFilesResource>().Any())
            CreateBucket(ProtoStrapFilesResource.RunModeBucket, locked: false);
        foreach (var bucket in all.OfType<ProtoStrapBucketResource>().OrderBy(b => b.Name, StringComparer.Ordinal))
        {
            CreateBucket(bucket.RunModeBucket, bucket.Locked);
            if (bucket.Versions is not null)
                script.Append("awslocal s3api put-bucket-versioning --bucket ").Append(bucket.RunModeBucket)
                    .Append(" --versioning-configuration Status=Enabled >/dev/null\n");
        }
        foreach (var queue in all.OfType<ProtoStrapQueueResource>().OrderBy(q => q.Name, StringComparer.Ordinal))
        {
            script.Append("awslocal sqs create-queue --queue-name ").Append(queue.SqsName);
            var attributes = new Dictionary<string, string>();
            if (queue.Fifo)
                attributes["FifoQueue"] = "true";
            if (queue.Visibility is { } visibility)
                attributes["VisibilityTimeout"] = ((long)visibility.TotalSeconds).ToString(System.Globalization.CultureInfo.InvariantCulture);
            if (queue.Retention is { } retention)
                attributes["MessageRetentionPeriod"] = ((long)retention.TotalSeconds).ToString(System.Globalization.CultureInfo.InvariantCulture);
            if (attributes.Count > 0)
                script.Append(" --attributes '").Append(JsonSerializer.Serialize(attributes)).Append('\'');
            script.Append(" >/dev/null\n");
        }

        return new ContainerFile
        {
            Name = "protostrap.sh",
            Contents = script.ToString(),
            Mode = UnixFileMode.UserRead | UnixFileMode.UserWrite | UnixFileMode.UserExecute
                | UnixFileMode.GroupRead | UnixFileMode.GroupExecute | UnixFileMode.OtherRead | UnixFileMode.OtherExecute,
        };
    }
}

/// <summary>
/// LocalStackInitCheck is healthy once LocalStack has run its init scripts, so a component that
/// waits for LocalStack finds its queues and buckets already there.
/// </summary>
internal sealed class LocalStackInitCheck(ProtoStrapLocalStackResource localstack) : IHealthCheck
{
    private static readonly HttpClient Client = new() { Timeout = TimeSpan.FromSeconds(5) };

    public async Task<HealthCheckResult> CheckHealthAsync(HealthCheckContext context, CancellationToken cancellationToken = default)
    {
        try
        {
            var url = localstack.Gateway.Url + "/_localstack/init/ready";
            using var response = await Client.GetAsync(url, cancellationToken);
            if (!response.IsSuccessStatusCode)
                return HealthCheckResult.Unhealthy($"{url} answered {(int)response.StatusCode}");
            using var json = JsonDocument.Parse(await response.Content.ReadAsStringAsync(cancellationToken));
            var root = json.RootElement;
            if (root.TryGetProperty("scripts", out var scripts) && scripts.ValueKind == JsonValueKind.Array
                && scripts.EnumerateArray().Any(s => s.TryGetProperty("state", out var state) && state.GetString() == "ERROR"))
            {
                return HealthCheckResult.Unhealthy("an init script failed: see the logs of " + localstack.Name);
            }
            return root.TryGetProperty("completed", out var completed) && completed.ValueKind == JsonValueKind.True
                ? HealthCheckResult.Healthy()
                : HealthCheckResult.Unhealthy("creating the queues and buckets");
        }
        catch (Exception e) when (e is HttpRequestException or TaskCanceledException or JsonException or InvalidOperationException)
        {
            return HealthCheckResult.Unhealthy(e.Message);
        }
    }
}
