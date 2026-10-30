namespace Aspire.Hosting.ApplicationModel;

/// <summary>
/// ProtoStrapQueueResource is an SQS queue with its dead-letter queue: an entry under the spec's
/// queues. In run mode it is a queue in LocalStack.
/// </summary>
public sealed class ProtoStrapQueueResource : Resource
{
    /// <summary>
    /// This constructor creates a queue. Its name is the spec's, and ProtoStrap's SQS queue is
    /// protostrap-&lt;name&gt;.
    /// </summary>
    public ProtoStrapQueueResource(string name)
        : base(name)
    {
    }

    /// <summary>Fifo says whether the queue is a FIFO queue.</summary>
    public bool Fifo { get; init; }

    /// <summary>Visibility is how long a received job stays hidden. ProtoStrap's default is 2 minutes.</summary>
    public TimeSpan? Visibility { get; init; }

    /// <summary>Retention is how long a job nobody receives is kept. ProtoStrap's default is 4 days.</summary>
    public TimeSpan? Retention { get; init; }

    /// <summary>
    /// MaxReceives is how many receives a job gets before it goes to the dead-letter queue.
    /// ProtoStrap's default is 5.
    /// </summary>
    public int? MaxReceives { get; init; }

    /// <summary>SqsName is the queue's name in SQS, and in LocalStack in run mode.</summary>
    public string SqsName => "protostrap-" + Name + (Fifo ? ".fifo" : "");
}

/// <summary>
/// ProtoStrapFilesResource is the application's files bucket, which ProtoStrap creates for every
/// application. A component that references it has files in its uses. In run mode it is a bucket
/// in LocalStack.
/// </summary>
public sealed class ProtoStrapFilesResource : Resource
{
    /// <summary>This constructor creates the files bucket's resource.</summary>
    public ProtoStrapFilesResource(string name)
        : base(name)
    {
    }

    /// <summary>RunModeBucket is the bucket's name in LocalStack, in run mode.</summary>
    public const string RunModeBucket = "protostrap-files";
}

/// <summary>
/// ProtoStrapBucketResource is one of the application's own S3 buckets, besides files: an entry
/// under the spec's buckets. A component that references it has it in its uses. In run mode it
/// is a bucket in LocalStack.
/// </summary>
public sealed class ProtoStrapBucketResource : Resource
{
    /// <summary>
    /// This constructor creates a bucket. Its name is the spec's, and ProtoStrap's bucket is
    /// protostrap-&lt;account&gt;-&lt;region&gt;-&lt;name&gt;.
    /// </summary>
    public ProtoStrapBucketResource(string name)
        : base(name)
    {
    }

    /// <summary>Browser says whether browsers on the application's hosts may use signed URLs on it.</summary>
    public bool Browser { get; init; }

    /// <summary>
    /// Versions turns versioning on: a replaced or deleted object's earlier version is kept this
    /// long. Null leaves versioning off.
    /// </summary>
    public TimeSpan? Versions { get; init; }

    /// <summary>
    /// Locked turns object lock on, so replicas may write objects under GOVERNANCE retention. It
    /// needs Versions, and can't be turned off.
    /// </summary>
    public bool Locked { get; init; }

    /// <summary>Expire is how long after it is written an object is deleted. Null keeps objects.</summary>
    public TimeSpan? Expire { get; init; }

    /// <summary>RunModeBucket is the bucket's name in LocalStack, in run mode.</summary>
    public string RunModeBucket => "protostrap-" + Name;
}

/// <summary>
/// ProtoStrapLocalStackResource is LocalStack, which backs the queues and the buckets in run
/// mode. ProtoStrap.Hosting adds one when the first queue or bucket is added, and never in
/// publish mode.
/// </summary>
public sealed class ProtoStrapLocalStackResource : ContainerResource
{
    /// <summary>This constructor creates the LocalStack container's resource.</summary>
    public ProtoStrapLocalStackResource(string name)
        : base(name)
    {
    }

    /// <summary>Gateway is LocalStack's gateway endpoint, for every AWS service it emulates.</summary>
    public EndpointReference Gateway => new(this, "http");
}
