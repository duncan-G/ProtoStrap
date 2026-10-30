using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting;

namespace Aspire.Hosting;

/// <summary>
/// ProtoStrapEmailResource is the domains the application sends mail from with Amazon SES: the
/// spec's email section. A component that references it has email in its uses, which lets its
/// role send from any address in the domains. In run mode it does nothing: the component sends
/// through whatever local mail server the AppHost gives it, such as smtp4dev.
/// </summary>
public sealed class ProtoStrapEmailResource : Resource
{
    /// <summary>This constructor creates the email resource.</summary>
    public ProtoStrapEmailResource(string name, IReadOnlyList<string> domains)
        : base(name)
    {
        Domains = domains;
    }

    /// <summary>Domains are the domains the application sends mail from, such as example.com.</summary>
    public IReadOnlyList<string> Domains { get; }
}

/// <summary>ProtoStrapEmailExtensions adds the application's email and lets components send it.</summary>
public static class ProtoStrapEmailExtensions
{
    /// <summary>
    /// AddProtoStrapEmail adds the domains the application sends mail from. ProtoStrap verifies
    /// each one with SES, with DKIM and a MAIL FROM domain, and writes its DNS records in the
    /// application's Cloudflare zone. An application has one, listing every domain.
    /// </summary>
    /// <param name="builder">It is the AppHost's builder.</param>
    /// <param name="name">It is the resource's name. In uses it is always called email.</param>
    /// <param name="domains">They are the domains, such as example.com, each in the Cloudflare zone.</param>
    public static IResourceBuilder<ProtoStrapEmailResource> AddProtoStrapEmail(
        this IDistributedApplicationBuilder builder, string name, params string[] domains)
    {
        ArgumentNullException.ThrowIfNull(builder);
        ArgumentNullException.ThrowIfNull(domains);
        var email = builder.AddResource(new ProtoStrapEmailResource(name, [.. domains]));
        if (builder.ExecutionContext.IsPublishMode)
            return email;
        return email.WithInitialState(new CustomResourceSnapshot
        {
            ResourceType = "Email",
            State = "Sent by SES once deployed",
            Properties = [new("protostrap.email", string.Join(", ", domains))],
        });
    }

    /// <summary>
    /// This WithReference lets the component send mail from the email domains, with the SES API
    /// and its own role: email goes in its uses. It sets no variable, in either mode: the
    /// component knows its From address, and in run mode sends to a local mail server instead.
    /// </summary>
    public static IResourceBuilder<T> WithReference<T>(this IResourceBuilder<T> builder, IResourceBuilder<ProtoStrapEmailResource> email)
        where T : IResourceWithEnvironment
    {
        ArgumentNullException.ThrowIfNull(email);
        builder.WithUses(email.Resource);
        return builder;
    }
}
