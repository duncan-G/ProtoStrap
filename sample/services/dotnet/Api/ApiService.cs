using Grpc.Core;
using Sample.Rpc;
using Sample.Shared;

namespace Sample.Api;

/// <summary>ApiService implements the sample.Api gRPC service, with the same knobs as the HTTP endpoints.</summary>
public sealed class ApiService(Identity me, JobQueue queue) : Sample.Rpc.Api.ApiBase
{
    public override Task<HelloReply> Hello(HelloRequest request, ServerCallContext context) =>
        Task.FromResult(new HelloReply
        {
            Component = me.Component,
            Replica = me.Replica,
            Release = me.Release,
            Time = DateTimeOffset.UtcNow.ToString("O"),
        });

    public override Task<BurnReply> Burn(BurnRequest request, ServerCallContext context)
    {
        var elapsed = Cpu.BurnCpuTime(Math.Clamp(request.Ms, 0, JobQueue.MaxBurnMs));
        return Task.FromResult(new BurnReply { ElapsedMs = (int)elapsed.TotalMilliseconds, Replica = me.Replica });
    }

    public override async Task<EnqueueReply> Enqueue(EnqueueRequest request, ServerCallContext context)
    {
        if (!queue.Configured)
            throw new RpcException(new Status(StatusCode.FailedPrecondition, "QUEUE_JOBS_URL is not set"));
        var workMs = request.WorkMs > 0 ? request.WorkMs : Jobs.DefaultWorkMs;
        return new EnqueueReply { Sent = await queue.EnqueueAsync(request.Count, workMs, context.CancellationToken) };
    }
}
