using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// An application's own buckets, besides files, are resources too. In publish mode a bucket is
/// an entry under buckets and WithReference adds it to uses; in run mode it is backed by
/// LocalStack (docs/protostrap-aspire.md, "Queues and buckets").
/// </summary>
public class BucketTests
{
    private static IResourceBuilder<ProjectResource> AddApi(IDistributedApplicationBuilder builder, FixtureRepo repo) =>
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

    [Fact]
    public async Task BucketsAreListedWithTheirSettings()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("reports", expire: TimeSpan.FromDays(30));
        builder.AddProtoStrapBucket("documents", browser: true, versions: TimeSpan.FromDays(7), locked: true);
        builder.AddProtoStrapBucket("scratch");
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            buckets:
              documents: { browser: true, versions: 7d, lock: true }
              reports: { expire: 30d }
              scratch: {}

            """, SpecText.Section(yaml, "buckets"));
    }

    [Fact]
    public async Task BucketsComeAfterQueues()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("reports");
        builder.AddProtoStrapQueue("jobs");
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        var queues = yaml.IndexOf("\nqueues:\n", StringComparison.Ordinal);
        var buckets = yaml.IndexOf("\nbuckets:\n", StringComparison.Ordinal);
        var components = yaml.IndexOf("\ncomponents:\n", StringComparison.Ordinal);
        Assert.True(queues >= 0 && queues < buckets && buckets < components, yaml);
    }

    [Fact]
    public async Task WithoutBucketsThereIsNoBucketsSection()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapFiles();
        AddApi(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.DoesNotContain("buckets:", yaml);
    }

    [Fact]
    public async Task ReferencesToBucketsAreUses()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var files = builder.AddProtoStrapFiles();
        var reports = builder.AddProtoStrapBucket("reports");
        var documents = builder.AddProtoStrapBucket("documents", versions: TimeSpan.FromDays(7), locked: true);
        var jobs = builder.AddProtoStrapQueue("jobs");
        AddApi(builder, repo).WithReference(reports).WithReference(jobs).WithReference(documents).WithReference(files);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        var api = SpecText.Component(yaml, "api");
        Assert.Contains("\n  uses: [documents, files, jobs, reports]\n", api);
        Assert.DoesNotContain("BUCKET_", api);
        Assert.False(SpecText.HasComponent(yaml, "reports"));
    }

    [Fact]
    public async Task BucketNamesFollowProtoStrapsRules()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("Reports");
        builder.AddProtoStrapBucket(new string('b', 25));
        builder.AddProtoStrapBucket(new string('c', 24));
        // Aspire refuses a name with --, or a queue's, so only Go's check meets them.
        builder.AddProtoStrapBucket("x-s3alias");
        builder.AddProtoStrapBucket("deploy");
        builder.AddProtoStrapBucket("tfstate");
        builder.AddProtoStrapBucket("telemetry");
        AddApi(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "Reports: a bucket's name is lowercase letters, digits and dashes",
            $"{new string('b', 25)}: a bucket's name is at most 24 characters",
            "deploy: files, backups, deploy, tfstate and telemetry are ProtoStrap's own buckets",
            "telemetry: files, backups, deploy, tfstate and telemetry are ProtoStrap's own buckets",
            "tfstate: files, backups, deploy, tfstate and telemetry are ProtoStrap's own buckets",
            "x-s3alias: S3 refuses a bucket name ending in -s3alias",
        ], problems);
    }

    [Fact]
    public async Task TheFilesBucketCantBeDeclaredAgain()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("files");
        builder.AddProtoStrapBucket("backups");
        AddApi(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "backups: files, backups, deploy, tfstate and telemetry are ProtoStrap's own buckets",
            "files: files, backups, deploy, tfstate and telemetry are ProtoStrap's own buckets",
        ], problems);
    }

    [Fact]
    public async Task BucketSettingsFollowProtoStrapsRules()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("hours", versions: TimeSpan.FromHours(36), expire: TimeSpan.FromMinutes(90));
        builder.AddProtoStrapBucket("never", versions: TimeSpan.Zero, expire: TimeSpan.Zero);
        builder.AddProtoStrapBucket("unversioned", locked: true);
        AddApi(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "hours: versions must be a whole number of days, at least 1 day",
            "hours: expire must be a whole number of days, at least 1 day",
            "never: versions must be a whole number of days, at least 1 day",
            "never: expire must be a whole number of days, at least 1 day",
            "unversioned: locked needs versions: S3 locks only a versioned bucket",
        ], problems);
    }

    [Fact]
    public async Task BucketVariablesAreSetByProtoStrap()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddApi(builder, repo).WithEnvironment("BUCKET_REPORTS", "x");

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["api: BUCKET_REPORTS is set by ProtoStrap in every replica"], problems);
    }

    [Fact]
    public async Task ABucketIsntAValue()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var reports = builder.AddProtoStrapBucket("reports");
        AddApi(builder, repo).WithEnvironment(context => context.EnvironmentVariables["Reports__Bucket"] = reports.Resource);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["api: Reports__Bucket: pass a queue or a bucket with WithReference, which gives its replicas QUEUE_<NAME>_URL, BUCKET_<NAME> or FILES_BUCKET"], problems);
    }

    [Fact]
    public void RunModeBacksBucketsWithLocalStack()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        var reports = builder.AddProtoStrapBucket("reports", expire: TimeSpan.FromDays(30));
        var documents = builder.AddProtoStrapBucket("export-files");
        var api = AddApi(builder, repo).WithReference(reports).WithReference(documents);

        var localstack = Assert.Single(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        Assert.Contains(api.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == localstack);
        Assert.Equal(localstack, Assert.Single(reports.Resource.Annotations.OfType<ResourceRelationshipAnnotation>(), r => r.Type == "Parent").Resource);

        var env = RunModeEnvironment(api.Resource);
        Assert.Equal(
            ["AWS_ENDPOINT_URL_S3", "AWS_REGION", "BUCKET_EXPORT_FILES", "BUCKET_REPORTS"],
            env.Keys.Order(StringComparer.Ordinal));
        Assert.Equal("protostrap-reports", env["BUCKET_REPORTS"]);
        Assert.Equal("protostrap-export-files", env["BUCKET_EXPORT_FILES"]);
        Assert.Equal("us-east-1", env["AWS_REGION"]);
    }

    [Fact]
    public async Task LocalStacksInitScriptCreatesEveryBucket()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.Configuration["AWS_REGION"] = "eu-west-1";
        builder.AddEnvironment();
        builder.AddProtoStrapQueue("jobs");
        builder.AddProtoStrapBucket("scratch");
        builder.AddProtoStrapBucket("documents", browser: true, versions: TimeSpan.FromDays(7), locked: true);
        builder.AddProtoStrapBucket("history", versions: TimeSpan.FromDays(30));
        builder.AddProtoStrapFiles();

        var localstack = Assert.Single(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        var files = Assert.Single(localstack.Annotations.OfType<ContainerFileSystemCallbackAnnotation>());
        var items = (await files.Callback(new ContainerFileSystemCallbackContext { Services = null!, Model = localstack }, CancellationToken.None)).ToList();
        var script = Assert.IsType<ContainerFile>(Assert.Single(items));
        Assert.Equal("""
            #!/bin/bash
            # Written by ProtoStrap.Hosting: the queues and buckets of the AppHost, as ProtoStrap names them.
            set -euo pipefail
            awslocal s3api create-bucket --bucket protostrap-files --create-bucket-configuration LocationConstraint=eu-west-1 >/dev/null
            awslocal s3api create-bucket --bucket protostrap-documents --create-bucket-configuration LocationConstraint=eu-west-1 --object-lock-enabled-for-bucket >/dev/null
            awslocal s3api put-bucket-versioning --bucket protostrap-documents --versioning-configuration Status=Enabled >/dev/null
            awslocal s3api create-bucket --bucket protostrap-history --create-bucket-configuration LocationConstraint=eu-west-1 >/dev/null
            awslocal s3api put-bucket-versioning --bucket protostrap-history --versioning-configuration Status=Enabled >/dev/null
            awslocal s3api create-bucket --bucket protostrap-scratch --create-bucket-configuration LocationConstraint=eu-west-1 >/dev/null
            awslocal sqs create-queue --queue-name protostrap-jobs >/dev/null

            """, script.Contents!.Replace("\r\n", "\n"));
    }

    [Fact]
    public void LocalStackIsOnlyAddedInRunMode()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var reports = builder.AddProtoStrapBucket("reports");
        var api = AddApi(builder, repo).WithReference(reports);

        Assert.Empty(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        Assert.DoesNotContain("BUCKET_REPORTS", RunModeEnvironment(api.Resource).Keys);
    }

    [Fact]
    public void BucketEnvIsBucketAndTheName()
    {
        Assert.Equal("BUCKET_DOCUMENTS", ProtoStrapQueueExtensions.BucketEnv("documents"));
        Assert.Equal("BUCKET_EXPORT_FILES", ProtoStrapQueueExtensions.BucketEnv("export-files"));
    }

    /// <summary>
    /// RunModeEnvironment returns the variables a resource gets in run mode that
    /// ProtoStrap.Hosting sets for buckets: the AWS ones and BUCKET_&lt;NAME&gt;.
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
        return env.Where(e => e.Key.StartsWith("AWS_", StringComparison.Ordinal) || e.Key.StartsWith("BUCKET_", StringComparison.Ordinal))
            .ToDictionary(e => e.Key, e => e.Value);
    }
}
