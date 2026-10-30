using Aspire.Hosting.ApplicationModel;

namespace ProtoStrap.Hosting;

/// <summary>
/// ProtoStrapComponentAnnotation records a PublishAsProtoStrap* call: which one it was, and the
/// callback that makes its settings.
/// </summary>
internal sealed record ProtoStrapComponentAnnotation(string Method, Func<ComponentSettings> Configure) : IResourceAnnotation;

/// <summary>
/// ProtoStrapSourcesAnnotation holds the sources a component declares with WithProtoStrapSources.
/// </summary>
internal sealed record ProtoStrapSourcesAnnotation(List<string> Sources) : IResourceAnnotation;

/// <summary>
/// ProtoStrapUsesAnnotation records a queue or the files bucket that a component references,
/// which goes in the component's uses.
/// </summary>
internal sealed record ProtoStrapUsesAnnotation(IResource Target) : IResourceAnnotation;

/// <summary>
/// ProtoStrapConfigAnnotation records WithProtoStrapConfig: the directory a component's config
/// files come from, as a full path, and where they are mounted.
/// </summary>
internal sealed record ProtoStrapConfigAnnotation(string Source, string Target) : IResourceAnnotation;

/// <summary>
/// ProtoStrapConfigureAnnotation records WithProtoStrapConfigure: the task that configures the
/// component through its API once it is ready.
/// </summary>
internal sealed record ProtoStrapConfigureAnnotation(IResource Task) : IResourceAnnotation;
