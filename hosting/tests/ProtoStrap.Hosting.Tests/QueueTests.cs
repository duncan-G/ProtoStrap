using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// Queues and the files bucket are resources. In publish mode a queue is an entry under queues
/// and WithReference adds it to uses; in run mode both are backed by LocalStack
/// (docs/protostrap-aspire.md, "Queues and files").
/// </summary>
public class QueueTests
{
    private static IResourceBuilder<ProjectResource> AddApi(IDistributedApplicationBuilder builder, FixtureRepo repo) =>
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

    [Fact]
    public async Task QueuesAreListedWithTheirSettings()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapQueue("protofast-workflow-outcomes", fifo: true);
        builder.AddProtoStrapQueue("protofast-document-upload");
        builder.AddProtoStrapQueue("thumbnails", visibility: TimeSpan.FromMinutes(5), retention: TimeSpan.FromDays(7), maxReceives: 10);
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            queues:
              protofast-document-upload: {}
              protofast-workflow-outcomes: { fifo: true }
              thumbnails: { visibility: 5m, retention: 7d, max_receives: 10 }

            """, SpecText.Section(yaml, "queues"));
    }

    [Fact]
    public async Task WithoutQueuesThereIsNoQueuesSection()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.DoesNotContain("queues:", yaml);
    }

    [Fact]
    public async Task ReferencesToQueuesAndFilesAreUses()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var files = builder.AddProtoStrapFiles("files");
        var uploads = builder.AddProtoStrapQueue("protofast-document-upload");
        var outcomes = builder.AddProtoStrapQueue("protofast-workflow-outcomes", fifo: true);
        AddApi(builder, repo).WithReference(uploads).WithReference(files);
        builder.AddProject("document-import", repo.Project("services/worker/Worker.csproj"))
            .WithReference(files)
            .WithReference(uploads)
            .WithReference(outcomes)
            .PublishAsProtoStrapWorker(w => w
                .Request(cpu: "1000m", memory: "1Gi")
                .Scale(min: 0, max: 20, demand => demand.Queue(uploads, rate: 2, drain: TimeSpan.FromSeconds(60))));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        var api = SpecText.Component(yaml, "api");
        Assert.Contains("\n  uses: [files, protofast-document-upload]\n", api);
        Assert.DoesNotContain("QUEUE_", api);
        Assert.DoesNotContain("FILES_BUCKET", api);
        var worker = SpecText.Component(yaml, "document-import");
        // The queue the worker's own signal reads is left out: the signal already lets the worker
        // receive from it.
        Assert.Contains("\n  uses: [files, protofast-workflow-outcomes]\n", worker);
        Assert.Contains("\n  scale: { min: 0, max: 20, demand: [{ queue: protofast-document-upload, rate: 2/s, drain: 1m }] }\n", worker);
    }

    [Fact]
    public async Task TheFilesBucketIsCalledFilesInUses()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var uploads = builder.AddProtoStrapFiles("uploads");
        AddApi(builder, repo).WithReference(uploads);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  uses: [files]\n", SpecText.Component(yaml, "api"));
        Assert.False(SpecText.HasComponent(yaml, "uploads"));
    }

    [Fact]
    public async Task QueueNamesFollowProtoStrapsRules()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapQueue("Uploads");
        builder.AddProtoStrapQueue("files");
        builder.AddProtoStrapQueue("slow", visibility: TimeSpan.FromHours(13), retention: TimeSpan.FromDays(15), maxReceives: 0);
        AddApi(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "Uploads: a queue's name is lowercase letters, digits and dashes",
            "files: a queue can't be called files or backups, which name ProtoStrap's buckets in uses",
            "slow: visibility must be between 1s and 12h",
            "slow: retention must be between 1m and 14d",
            "slow: maxReceives must be 1 to 1000",
        ], problems);
    }

    [Fact]
    public void RunModeBacksQueuesAndFilesWithLocalStack()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        var files = builder.AddProtoStrapFiles("files");
        var uploads = builder.AddProtoStrapQueue("protofast-document-upload");
        var outcomes = builder.AddProtoStrapQueue("protofast-workflow-outcomes", fifo: true);
        var api = AddApi(builder, repo).WithReference(uploads).WithReference(outcomes).WithReference(files);

        var localstack = Assert.Single(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        Assert.Equal("protostrap-localstack", localstack.Name);
        Assert.True(localstack.TryGetLastAnnotation<ContainerImageAnnotation>(out var image));
        Assert.Equal("localstack/localstack", image.Image);
        Assert.Equal("4", image.Tag);
        Assert.Contains(api.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == localstack);

        var env = RunModeEnvironment(api.Resource);
        Assert.Equal(
            ["AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL_SQS", "AWS_REGION", "FILES_BUCKET", "QUEUE_PROTOFAST_DOCUMENT_UPLOAD_URL", "QUEUE_PROTOFAST_WORKFLOW_OUTCOMES_URL"],
            env.Keys.Order(StringComparer.Ordinal));
        Assert.Equal("protostrap-files", env["FILES_BUCKET"]);
        Assert.Equal("us-east-1", env["AWS_REGION"]);
        var queueUrl = Assert.IsType<ReferenceExpression>(env["QUEUE_PROTOFAST_WORKFLOW_OUTCOMES_URL"]);
        Assert.Equal("{0}/queue/us-east-1/000000000000/protostrap-protofast-workflow-outcomes.fifo", queueUrl.Format);
        Assert.IsType<EndpointReference>(Assert.Single(queueUrl.ValueProviders));
    }

    [Fact]
    public void TheRegionComesFromConfiguration()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.Configuration["AWS_REGION"] = "eu-west-1";
        builder.AddEnvironment();
        var uploads = builder.AddProtoStrapQueue("uploads");
        var api = AddApi(builder, repo).WithReference(uploads);

        var env = RunModeEnvironment(api.Resource);

        Assert.Equal("eu-west-1", env["AWS_REGION"]);
        Assert.Equal("{0}/queue/eu-west-1/000000000000/protostrap-uploads", Assert.IsType<ReferenceExpression>(env["QUEUE_UPLOADS_URL"]).Format);
        var localstack = Assert.Single(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        Assert.Equal("eu-west-1", RunModeEnvironment(localstack)["AWS_DEFAULT_REGION"]);
    }

    [Fact]
    public async Task LocalStacksInitScriptCreatesEveryQueueAndTheBucket()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapQueue("uploads");
        builder.AddProtoStrapQueue("outcomes", fifo: true, visibility: TimeSpan.FromMinutes(5));
        builder.AddProtoStrapFiles("files");

        var localstack = Assert.Single(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        var files = Assert.Single(localstack.Annotations.OfType<ContainerFileSystemCallbackAnnotation>());
        Assert.Equal("/etc/localstack/init/ready.d", files.DestinationPath);
        var items = (await files.Callback(new ContainerFileSystemCallbackContext { Services = null!, Model = localstack }, CancellationToken.None)).ToList();
        var script = Assert.IsType<ContainerFile>(Assert.Single(items));
        Assert.Equal("protostrap.sh", script.Name);
        Assert.Equal("""
            #!/bin/bash
            # Written by ProtoStrap.Hosting: the queues and buckets of the AppHost, as ProtoStrap names them.
            set -euo pipefail
            awslocal s3api create-bucket --bucket protostrap-files >/dev/null
            awslocal sqs create-queue --queue-name protostrap-outcomes.fifo --attributes '{"FifoQueue":"true","VisibilityTimeout":"300"}' >/dev/null
            awslocal sqs create-queue --queue-name protostrap-uploads >/dev/null

            """, script.Contents!.Replace("\r\n", "\n"));
    }

    [Fact]
    public void LocalStackIsOnlyAddedInRunMode()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var uploads = builder.AddProtoStrapQueue("uploads");
        var files = builder.AddProtoStrapFiles("files");
        AddApi(builder, repo).WithReference(uploads).WithReference(files);

        Assert.Empty(builder.Resources.OfType<ProtoStrapLocalStackResource>());
    }

    [Fact]
    public void LocalStacksVariablesAreSetOnlyInRunMode()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var uploads = builder.AddProtoStrapQueue("uploads");
        var api = AddApi(builder, repo).WithReference(uploads);

        var env = RunModeEnvironment(api.Resource);

        Assert.DoesNotContain("QUEUE_UPLOADS_URL", env.Keys);
        Assert.DoesNotContain("AWS_ENDPOINT_URL_SQS", env.Keys);
    }

    /// <summary>
    /// RunModeEnvironment returns the variables a resource gets in run mode that
    /// ProtoStrap.Hosting sets: the AWS ones, the ones ProtoStrap sets in every replica, and
    /// LocalStack's own. Aspire's own callbacks may fail outside a running AppHost, and what they
    /// set isn't kept.
    /// </summary>
    private static Dictionary<string, object> RunModeEnvironment(IResource resource)
    {
        var env = new Dictionary<string, object>();
        var context = new EnvironmentCallbackContext(new DistributedApplicationExecutionContext(DistributedApplicationOperation.Run), resource, env);
        foreach (var callback in resource.Annotations.OfType<EnvironmentCallbackAnnotation>())
        {
            try
            {
                callback.Callback(context).GetAwaiter().GetResult();
            }
            catch (Exception e) when (e is InvalidOperationException or NullReferenceException or ArgumentException)
            {
            }
        }
        return env.Where(e => e.Key.StartsWith("AWS_", StringComparison.Ordinal) || e.Key.StartsWith("QUEUE_", StringComparison.Ordinal)
                || e.Key == "FILES_BUCKET" || e.Key == "SERVICES" || e.Key == "SQS_ENDPOINT_STRATEGY")
            .ToDictionary(e => e.Key, e => e.Value);
    }
}
