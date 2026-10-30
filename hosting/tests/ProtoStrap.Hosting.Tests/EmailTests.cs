using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// AddProtoStrapEmail is the domains the application sends mail from with Amazon SES. In
/// publish mode it is the spec's email section and WithReference adds email to a component's
/// uses, which lets its role send. In run mode it does nothing: the component sends through
/// whatever local mail server the AppHost gives it (docs/email.md).
/// </summary>
public class EmailTests
{
    private static IResourceBuilder<ProjectResource> AddAuth(IDistributedApplicationBuilder builder, FixtureRepo repo) =>
        builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Ready("/healthz"));

    [Fact]
    public async Task TheDomainsAreTheEmailSection()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("email", "protofast.dev", "news.protofast.dev");
        AddAuth(builder, repo).WithReference(email);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            email:
              domains: [news.protofast.dev, protofast.dev]

            """, SpecText.Section(yaml, "email"));
    }

    [Fact]
    public async Task EmailComesAfterBucketsAndBeforeComponents()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapBucket("reports");
        var email = builder.AddProtoStrapEmail("email", "protofast.dev");
        AddAuth(builder, repo).WithReference(email);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        var buckets = yaml.IndexOf("\nbuckets:\n", StringComparison.Ordinal);
        var section = yaml.IndexOf("\nemail:\n", StringComparison.Ordinal);
        var components = yaml.IndexOf("\ncomponents:\n", StringComparison.Ordinal);
        Assert.True(buckets >= 0 && buckets < section && section < components, yaml);
    }

    [Fact]
    public async Task WithoutEmailThereIsNoEmailSection()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        AddAuth(builder, repo);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.DoesNotContain("email:", yaml);
    }

    [Fact]
    public async Task AReferenceToEmailIsUses()
    {
        using var repo = new FixtureRepo();
        repo.Write("services/keycloak/Dockerfile", "FROM quay.io/keycloak/keycloak:26.7\n");
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("mail", "protofast.dev");
        var files = builder.AddProtoStrapFiles();
        AddAuth(builder, repo).WithReference(email).WithReference(files);
        builder.AddDockerfile("keycloak", "../services/keycloak")
            .WithHttpEndpoint(targetPort: 8080)
            .WithReference(email)
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "1Gi").Ready("/health/ready"));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // In uses it is always email, whatever the resource is called.
        Assert.Contains("\n  uses: [email, files]\n", SpecText.Component(yaml, "auth"));
        Assert.Contains("\n  uses: [email]\n", SpecText.Component(yaml, "keycloak"));
        Assert.False(SpecText.HasComponent(yaml, "mail"));
    }

    [Fact]
    public async Task DomainsFollowProtoStrapsRules()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("email", "protofast.dev", "*.protofast.dev", "bad..domain", "protofast.dev");
        AddAuth(builder, repo).WithReference(email);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(
        [
            "email: \"*.protofast.dev\" isn't a domain to send mail from, such as example.com",
            "email: \"bad..domain\" isn't a domain to send mail from, such as example.com",
            "email: protofast.dev is listed twice",
        ], problems);
    }

    [Fact]
    public async Task EmailNeedsADomain()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("email");
        AddAuth(builder, repo).WithReference(email);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["email: list at least one domain to send mail from"], problems);
    }

    [Fact]
    public async Task AnApplicationHasOneEmail()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        builder.AddProtoStrapEmail("email", "protofast.dev");
        builder.AddProtoStrapEmail("newsletter", "news.protofast.dev");
        AddAuth(builder, repo);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["newsletter: the application already has email: list every domain in one AddProtoStrapEmail"], problems);
    }

    [Fact]
    public async Task EmailIsntAValue()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("email", "protofast.dev");
        AddAuth(builder, repo).WithEnvironment(context => context.EnvironmentVariables["Mail__Sender"] = email.Resource);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Equal(["auth: Mail__Sender: pass email with WithReference, which lets the component send mail from its domains"], problems);
    }

    [Fact]
    public void RunModeGivesNoVariables()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        builder.AddEnvironment();
        var email = builder.AddProtoStrapEmail("email", "protofast.dev");
        var auth = AddAuth(builder, repo).WithReference(email);

        // A local run sends through the AppHost's own mail server, such as smtp4dev, so the
        // reference adds no variable, no LocalStack and nothing to wait for.
        Assert.Empty(builder.Resources.OfType<ProtoStrapLocalStackResource>());
        Assert.DoesNotContain(auth.Resource.Annotations.OfType<WaitAnnotation>(), w => w.Resource == email.Resource);
        Assert.Equal(["protofast.dev"], email.Resource.Domains);
    }
}
