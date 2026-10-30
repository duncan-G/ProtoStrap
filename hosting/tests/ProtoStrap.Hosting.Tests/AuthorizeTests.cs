using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// A service's requests can be authorized at the edge by another service, which the edge asks
/// about each request before sending it on (docs/protostrap.md, "Authorizing requests").
/// </summary>
public class AuthorizeTests
{
    private static IResourceBuilder<ProjectResource> AddAuth(IDistributedApplicationBuilder builder, FixtureRepo repo, bool routed = true) =>
        builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s =>
            {
                s.Request(cpu: "250m", memory: "256Mi");
                if (routed)
                    s.Routes("app/auth/");
            });

    [Fact]
    public async Task AServicesRequestsAreAuthorizedByAnother()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.localhost");
        var auth = AddAuth(builder, repo);
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s
                .Request(cpu: "500m", memory: "512Mi")
                .Ready(Readiness.Grpc)
                .Routes("app/api/")
                .Authorize(auth, path: "/auth/check", headers: ["x-identity"], forward: ["cookie"]));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  routes: [app/api/]\n  authorize: { by: auth, path: /auth/check, headers: [x-identity], forward: [cookie] }\n", SpecText.Component(yaml, "api"));
        Assert.DoesNotContain("authorize", SpecText.Component(yaml, "auth"));
    }

    [Fact]
    public async Task ThePathDefaultsToTheRequestsOwn()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.localhost");
        var auth = AddAuth(builder, repo, routed: false);
        builder.AddProject("web", repo.Project("services/web/Web.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 4000, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Routes("app/").Authorize(auth));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  authorize: { by: auth }\n", SpecText.Component(yaml, "web"));
    }

    [Fact]
    public async Task WhatCantBeAuthorizedIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        builder.AddEnvironment().WithHost("app", "protofast.dev", local: "app.localhost");
        var auth = AddAuth(builder, repo);
        var redis = builder.AddRedis("redis").PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "250m", memory: "512Mi").Volume(size: "5Gi"));
        var smtp = builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3.8").ExcludeFromManifest();
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc)
                .Authorize(auth, path: "check", headers: ["X Identity"], forward: ["host"]));
        builder.AddProject("web", repo.Project("services/web/Web.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 4000, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Routes("app/").Authorize(redis));
        builder.AddProject("admin", repo.Project("services/admin/Admin.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 4001, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi").Routes("app/admin/").Authorize(smtp));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("api: Authorize: only a service with routes takes requests through the edge", problems);
        Assert.Contains("api: Authorize: path \"check\" must start with /", problems);
        Assert.Contains("api: Authorize: \"X Identity\" isn't a header name: lower case, such as x-user", problems);
        Assert.Contains("api: Authorize: the edge sets host itself", problems);
        Assert.Contains("web: Authorize: redis is a stateful component: only a service answers the edge", problems);
        Assert.Contains("admin: Authorize: smtp4dev is excluded from publish", problems);
    }
}
