using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting;

namespace Aspire.Hosting;

/// <summary>
/// These extension methods add queues, the files bucket and the application's own buckets as
/// resources. A component that references one has it in its uses, from which its role is built.
/// In run mode they are backed by LocalStack, and the reference sets the variables ProtoStrap
/// sets in every replica: QUEUE_&lt;NAME&gt;_URL, FILES_BUCKET, BUCKET_&lt;NAME&gt; and AWS_REGION.
/// </summary>
public static class ProtoStrapQueueExtensions
{
    /// <summary>
    /// AddProtoStrapQueue adds an SQS queue, which ProtoStrap creates with its dead-letter queue.
    /// </summary>
    /// <param name="builder">It is the AppHost's builder.</param>
    /// <param name="name">It is the queue's name: lowercase letters, digits and dashes.</param>
    /// <param name="fifo">It says whether the queue is a FIFO queue.</param>
    /// <param name="visibility">
    /// It is how long a received job stays hidden. ProtoStrap's default is 2 minutes.
    /// </param>
    /// <param name="retention">
    /// It is how long a job nobody receives is kept. ProtoStrap's default is 4 days.
    /// </param>
    /// <param name="maxReceives">
    /// It is how many receives a job gets before it goes to the dead-letter queue. ProtoStrap's
    /// default is 5.
    /// </param>
    public static IResourceBuilder<ProtoStrapQueueResource> AddProtoStrapQueue(
        this IDistributedApplicationBuilder builder, string name, bool fifo = false,
        TimeSpan? visibility = null, TimeSpan? retention = null, int? maxReceives = null)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var queue = new ProtoStrapQueueResource(name)
        {
            Fifo = fifo,
            Visibility = visibility,
            Retention = retention,
            MaxReceives = maxReceives,
        };
        if (builder.ExecutionContext.IsPublishMode)
            return builder.AddResource(queue);

        var localstack = LocalStack.Ensure(builder);
        return builder.AddResource(queue)
            .WithParentRelationship(localstack)
            .WithInitialState(new CustomResourceSnapshot
            {
                ResourceType = "Queue",
                Properties = [new("protostrap.sqs", queue.SqsName)],
            });
    }

    /// <summary>
    /// AddProtoStrapFiles adds the application's files bucket, which ProtoStrap creates for every
    /// application.
    /// </summary>
    /// <param name="builder">It is the AppHost's builder.</param>
    /// <param name="name">It is the resource's name. In uses the bucket is always called files.</param>
    public static IResourceBuilder<ProtoStrapFilesResource> AddProtoStrapFiles(this IDistributedApplicationBuilder builder, string name = "files")
    {
        ArgumentNullException.ThrowIfNull(builder);
        var files = new ProtoStrapFilesResource(name);
        if (builder.ExecutionContext.IsPublishMode)
            return builder.AddResource(files);

        var localstack = LocalStack.Ensure(builder);
        return builder.AddResource(files)
            .WithParentRelationship(localstack)
            .WithInitialState(new CustomResourceSnapshot
            {
                ResourceType = "Bucket",
                Properties = [new("protostrap.bucket", ProtoStrapFilesResource.RunModeBucket)],
            });
    }

    /// <summary>
    /// AddProtoStrapBucket adds one of the application's own S3 buckets, which ProtoStrap creates
    /// private, encrypted and reached only over TLS, with the settings given here.
    /// </summary>
    /// <param name="builder">It is the AppHost's builder.</param>
    /// <param name="name">
    /// It is the bucket's name: up to 24 lowercase letters, digits and dashes, and not files,
    /// backups, deploy, tfstate or a queue's name.
    /// </param>
    /// <param name="browser">It says whether browsers on the application's hosts may use signed URLs on it.</param>
    /// <param name="versions">
    /// It turns versioning on: a replaced or deleted object's earlier version is kept this long,
    /// in whole days.
    /// </param>
    /// <param name="locked">
    /// It turns object lock on, so replicas may write objects under GOVERNANCE retention. It needs
    /// versions, and can't be turned off.
    /// </param>
    /// <param name="expire">It is how long after it is written an object is deleted, in whole days.</param>
    public static IResourceBuilder<ProtoStrapBucketResource> AddProtoStrapBucket(
        this IDistributedApplicationBuilder builder, string name, bool browser = false,
        TimeSpan? versions = null, bool locked = false, TimeSpan? expire = null)
    {
        ArgumentNullException.ThrowIfNull(builder);
        var bucket = new ProtoStrapBucketResource(name)
        {
            Browser = browser,
            Versions = versions,
            Locked = locked,
            Expire = expire,
        };
        if (builder.ExecutionContext.IsPublishMode)
            return builder.AddResource(bucket);

        var localstack = LocalStack.Ensure(builder);
        return builder.AddResource(bucket)
            .WithParentRelationship(localstack)
            .WithInitialState(new CustomResourceSnapshot
            {
                ResourceType = "Bucket",
                Properties = [new("protostrap.bucket", bucket.RunModeBucket)],
            });
    }

    /// <summary>
    /// This WithReference lets the component send to and receive from the queue: the queue goes
    /// in its uses. The queue that the component's own Queue(...) signal reads is left out, since
    /// the signal already lets the component receive from it. In run mode this method sets
    /// QUEUE_&lt;NAME&gt;_URL to the queue in LocalStack.
    /// </summary>
    public static IResourceBuilder<T> WithReference<T>(this IResourceBuilder<T> builder, IResourceBuilder<ProtoStrapQueueResource> queue)
        where T : IResourceWithEnvironment
    {
        builder.WithUses(queue.Resource);
        if (builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;

        var localstack = LocalStack.Ensure(builder.ApplicationBuilder);
        var configuration = builder.ApplicationBuilder.Configuration;
        return builder.WaitForLocalStack(localstack.Resource).WithEnvironment(context =>
        {
            var region = LocalStack.Region(configuration);
            context.EnvironmentVariables[QueueEnv(queue.Resource.Name)] =
                ReferenceExpression.Create($"{localstack.Resource.Gateway}/queue/{region}/000000000000/{queue.Resource.SqsName}");
            context.EnvironmentVariables["AWS_ENDPOINT_URL_SQS"] = localstack.Resource.Gateway;
            context.EnvironmentVariables["AWS_REGION"] = region;
        });
    }

    /// <summary>
    /// This WithReference lets the component read and write the files bucket: files goes in its
    /// uses. In run mode this method sets FILES_BUCKET to the bucket in LocalStack.
    /// </summary>
    public static IResourceBuilder<T> WithReference<T>(this IResourceBuilder<T> builder, IResourceBuilder<ProtoStrapFilesResource> files)
        where T : IResourceWithEnvironment
    {
        builder.WithUses(files.Resource);
        if (builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;

        var localstack = LocalStack.Ensure(builder.ApplicationBuilder);
        var configuration = builder.ApplicationBuilder.Configuration;
        return builder.WaitForLocalStack(localstack.Resource).WithEnvironment(context =>
        {
            context.EnvironmentVariables["FILES_BUCKET"] = ProtoStrapFilesResource.RunModeBucket;
            context.EnvironmentVariables["AWS_ENDPOINT_URL_S3"] = localstack.Resource.Gateway;
            context.EnvironmentVariables["AWS_REGION"] = LocalStack.Region(configuration);
        });
    }

    /// <summary>
    /// This WithReference lets the component read and write the bucket, and set an object's
    /// retention if it is locked: the bucket goes in its uses. In run mode this method sets
    /// BUCKET_&lt;NAME&gt; to the bucket in LocalStack.
    /// </summary>
    public static IResourceBuilder<T> WithReference<T>(this IResourceBuilder<T> builder, IResourceBuilder<ProtoStrapBucketResource> bucket)
        where T : IResourceWithEnvironment
    {
        builder.WithUses(bucket.Resource);
        if (builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;

        var localstack = LocalStack.Ensure(builder.ApplicationBuilder);
        var configuration = builder.ApplicationBuilder.Configuration;
        return builder.WaitForLocalStack(localstack.Resource).WithEnvironment(context =>
        {
            context.EnvironmentVariables[BucketEnv(bucket.Resource.Name)] = bucket.Resource.RunModeBucket;
            context.EnvironmentVariables["AWS_ENDPOINT_URL_S3"] = localstack.Resource.Gateway;
            context.EnvironmentVariables["AWS_REGION"] = LocalStack.Region(configuration);
        });
    }

    /// <summary>
    /// BucketEnv returns the name of the variable that holds a bucket's name in every replica,
    /// BUCKET_&lt;NAME&gt;.
    /// </summary>
    public static string BucketEnv(string bucket) =>
        "BUCKET_" + bucket.Replace('-', '_').ToUpperInvariant();

    /// <summary>
    /// QueueEnv returns the name of the variable that holds a queue's URL in every replica,
    /// QUEUE_&lt;NAME&gt;_URL.
    /// </summary>
    public static string QueueEnv(string queue) =>
        "QUEUE_" + queue.Replace('-', '_').ToUpperInvariant() + "_URL";

    internal static void WithUses<T>(this IResourceBuilder<T> builder, IResource target)
        where T : IResource
    {
        if (!builder.Resource.Annotations.OfType<ProtoStrapUsesAnnotation>().Any(u => u.Target == target))
            builder.WithAnnotation(new ProtoStrapUsesAnnotation(target));
        builder.WithAnnotation(new ResourceRelationshipAnnotation(target, "Reference"));
    }

    private static IResourceBuilder<T> WaitForLocalStack<T>(this IResourceBuilder<T> builder, ProtoStrapLocalStackResource localstack)
        where T : IResource
    {
        if (!builder.Resource.Annotations.OfType<WaitAnnotation>().Any(w => w.Resource == localstack))
            builder.WithAnnotation(new WaitAnnotation(localstack, WaitType.WaitUntilHealthy));
        return builder;
    }
}
