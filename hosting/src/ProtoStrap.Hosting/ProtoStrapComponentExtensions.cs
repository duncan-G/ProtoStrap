using Aspire.Hosting.ApplicationModel;
using Aspire.Hosting.Eventing;
using ProtoStrap.Hosting;

namespace Aspire.Hosting;

/// <summary>
/// These extension methods say what kind of ProtoStrap component a compute resource is, in the
/// style of Aspire's own PublishAsDockerComposeService. Every callback runs when the
/// protostrap-spec step writes the spec, and under `aspire run` only when the local edge reads
/// a service's routes; otherwise run mode is unchanged.
/// </summary>
public static class ProtoStrapComponentExtensions
{
    /// <summary>
    /// PublishAsProtoStrapService makes the resource a service, which gets requests from the
    /// gateway on its routes.
    /// </summary>
    public static IResourceBuilder<T> PublishAsProtoStrapService<T>(this IResourceBuilder<T> builder, Action<ProtoStrapService> configure)
        where T : IComputeResource =>
        builder.PublishAsProtoStrap(nameof(PublishAsProtoStrapService), () =>
        {
            var service = new ProtoStrapService();
            configure(service);
            return service.Settings;
        });

    /// <summary>
    /// PublishAsProtoStrapWorker makes the resource a worker, which takes jobs from a queue and
    /// takes no requests.
    /// </summary>
    public static IResourceBuilder<T> PublishAsProtoStrapWorker<T>(this IResourceBuilder<T> builder, Action<ProtoStrapWorker> configure)
        where T : IComputeResource =>
        builder.PublishAsProtoStrap(nameof(PublishAsProtoStrapWorker), () =>
        {
            var worker = new ProtoStrapWorker();
            configure(worker);
            return worker.Settings;
        });

    /// <summary>
    /// PublishAsProtoStrapStateful makes the resource a stateful component: an upstream image
    /// pinned to a data VM, with its own volume. On PostgreSQL and Redis the port, the data path
    /// and the backup command are filled in.
    /// </summary>
    public static IResourceBuilder<T> PublishAsProtoStrapStateful<T>(this IResourceBuilder<T> builder, Action<ProtoStrapStateful> configure)
        where T : IComputeResource =>
        builder.PublishAsProtoStrap(nameof(PublishAsProtoStrapStateful), () =>
        {
            var stateful = new ProtoStrapStateful();
            configure(stateful);
            return stateful.Settings;
        });

    /// <summary>
    /// PublishAsProtoStrapTask makes the resource a task, such as a project of database
    /// migrations, which runs to completion in each release in which its image is new, before
    /// any new replica of a component that waits for it with WaitForCompletion.
    /// </summary>
    public static IResourceBuilder<T> PublishAsProtoStrapTask<T>(this IResourceBuilder<T> builder, Action<ProtoStrapTask> configure)
        where T : IComputeResource =>
        builder.PublishAsProtoStrap(nameof(PublishAsProtoStrapTask), () =>
        {
            var task = new ProtoStrapTask();
            configure(task);
            return task.Settings;
        });

    /// <summary>
    /// WithProtoStrapSources declares inputs to the component's image that its build can't show:
    /// files or git pathspecs relative to the spec's directory, such as
    /// ":(glob)services/**/Protos/**" for code generated from protobuf files. They are added to
    /// the component's sources, and so to its image tag.
    /// </summary>
    public static IResourceBuilder<T> WithProtoStrapSources<T>(this IResourceBuilder<T> builder, params string[] sources)
        where T : IComputeResource
    {
        if (!builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;
        if (builder.Resource.TryGetLastAnnotation<ProtoStrapSourcesAnnotation>(out var existing))
        {
            existing.Sources.AddRange(sources);
            return builder;
        }
        return builder.WithAnnotation(new ProtoStrapSourcesAnnotation([.. sources]));
    }

    /// <summary>
    /// WithProtoStrapConfig gives the component config files: the files git tracks in source, a
    /// directory of the repository relative to the AppHost, mounted read-only at target in each
    /// replica (docs/config.md). They hold no secret. In an account each release carries them,
    /// and the component's OnConfigChange says how it takes new ones; under `aspire run` a
    /// container gets the directory as a read-only bind mount.
    /// </summary>
    public static IResourceBuilder<T> WithProtoStrapConfig<T>(this IResourceBuilder<T> builder, string source, string target)
        where T : IComputeResource
    {
        ArgumentException.ThrowIfNullOrEmpty(source);
        ArgumentException.ThrowIfNullOrEmpty(target);
        var path = Path.GetFullPath(Path.Combine(builder.ApplicationBuilder.AppHostDirectory, source));
        builder.WithAnnotation(new ProtoStrapConfigAnnotation(path, target), ResourceAnnotationMutationBehavior.Append);
        if (!builder.ApplicationBuilder.ExecutionContext.IsPublishMode && builder.Resource is ContainerResource)
            builder.WithAnnotation(new ContainerMountAnnotation(path, target, ContainerMountType.BindMount, isReadOnly: true));
        return builder;
    }

    /// <summary>
    /// WithProtoStrapConfigure names the task that configures the component through its API
    /// once it is ready, such as an identity provider's realms (docs/config.md#configure-tasks).
    /// The task waits for the component. In an account it runs again whenever the task, its keys
    /// or the component change, and whatever waits for the component waits for it too; under
    /// `aspire run` it runs once, before whatever waits for the component starts.
    /// </summary>
    public static IResourceBuilder<T> WithProtoStrapConfigure<T, TTask>(this IResourceBuilder<T> builder, IResourceBuilder<TTask> task)
        where T : IComputeResource, IResourceWithWaitSupport
        where TTask : IComputeResource, IResourceWithWaitSupport
    {
        IResource component = builder.Resource, configure = task.Resource;
        builder.WithAnnotation(new ProtoStrapConfigureAnnotation(configure), ResourceAnnotationMutationBehavior.Append);
        task.WaitFor(builder.ApplicationBuilder.CreateResourceBuilder(component));
        if (builder.ApplicationBuilder.ExecutionContext.IsPublishMode)
            return builder;

        // Under `aspire run`, what waits for the component, or for one of its databases, waits for
        // the task to finish too, as the scheduler has it wait in an account. It is added once the
        // model is complete, since what waits for the component may be added after this call.
        if (!builder.ApplicationBuilder.Resources.Any(r => r.HasAnnotationOfType<ProtoStrapConfigureAnnotation>() && r != component))
        {
            builder.ApplicationBuilder.Eventing.Subscribe<BeforeStartEvent>((e, _) =>
            {
                WaitForConfigureTasks(e.Model.Resources);
                return Task.CompletedTask;
            });
        }
        return builder;
    }

    /// <summary>
    /// WaitForConfigureTasks has whatever waits for a configured component wait for its configure
    /// task to finish too, unless the task itself waits for it.
    /// </summary>
    internal static void WaitForConfigureTasks(IEnumerable<IResource> resources)
    {
        var all = resources.ToList();
        foreach (var component in all)
        {
            foreach (var configure in component.Annotations.OfType<ProtoStrapConfigureAnnotation>().Select(a => a.Task))
            {
                foreach (var resource in all)
                {
                    if (resource == configure || resource == component)
                        continue;
                    var waits = resource.Annotations.OfType<WaitAnnotation>().ToList();
                    var waitsForComponent = waits.Any(w => w.WaitType != WaitType.WaitForCompletion && Owner(w.Resource) == component);
                    var waitsForTask = waits.Any(w => w.Resource == configure);
                    var taskWaitsForIt = configure.Annotations.OfType<WaitAnnotation>().Any(w => w.Resource == resource);
                    if (waitsForComponent && !waitsForTask && !taskWaitsForIt)
                        resource.Annotations.Add(new WaitAnnotation(configure, WaitType.WaitForCompletion));
                }
            }
        }
    }

    // Owner is the resource a child, such as a database, belongs to.
    private static IResource Owner(IResource resource)
    {
        while (resource is IResourceWithParent child)
            resource = child.Parent;
        return resource;
    }

    // The settings are kept in run mode too, where the local edge routes by them; the
    // callback still runs only when they are read.
    private static IResourceBuilder<T> PublishAsProtoStrap<T>(this IResourceBuilder<T> builder, string method, Func<ComponentSettings> configure)
        where T : IComputeResource
    {
        return builder.WithAnnotation(new ProtoStrapComponentAnnotation(method, configure), ResourceAnnotationMutationBehavior.Replace);
    }
}
