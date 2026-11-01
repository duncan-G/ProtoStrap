using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// An upstream's requests can be authorized at the gateway by another service, which the gateway
/// asks about each request before sending it on (docs/protostrap.md, "Authorizing requests").
/// </summary>
public class AuthorizeTests
{
    private static IResourceBuilder<ProtoStrapGatewayResource> AddGateway(IDistributedApplicationBuilder builder) =>
        builder.AddEnvironment().AddEnvoyGateway("gateway").WithHost("app", "protofast.dev", local: "app.localhost");

    private static IResourceBuilder<ProjectResource> AddAuth(IDistributedApplicationBuilder builder, FixtureRepo repo, IResourceBuilder<ProtoStrapGatewayResource> gateway, bool routed = true)
    {
        var auth = builder.AddProject("auth", repo.Project("services/auth/Auth.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: 8080, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));
        if (routed)
            gateway.WithUpstream(auth, u => u.Routes("app/auth/"));
        return auth;
    }

    private static IResourceBuilder<ProjectResource> AddWeb(IDistributedApplicationBuilder builder, FixtureRepo repo, string name, int port) =>
        builder.AddProject(name, repo.Project($"services/{name}/{name}.csproj", "Microsoft.NET.Sdk.Web"))
            .WithHttpEndpoint(targetPort: port, name: "http")
            .WithHttpHealthCheck("/healthz")
            .PublishAsProtoStrapService(s => s.Request(cpu: "250m", memory: "256Mi"));

    private static IResourceBuilder<ProjectResource> AddApi(IDistributedApplicationBuilder builder, FixtureRepo repo) =>
        builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));

    [Fact]
    public async Task AServicesRequestsAreAuthorizedByAnother()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway);
        gateway.WithUpstream(AddApi(builder, repo), u => u.Routes("app/api/").Authorize(auth, path: "/auth/check", headers: ["x-identity"], forward: ["cookie"]));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  routes: [app/api/]\n  authorize: { by: auth, path: /auth/check, headers: [x-identity], forward: [cookie] }\n", SpecText.Upstream(yaml, "api"));
        Assert.DoesNotContain("authorize", SpecText.Upstream(yaml, "auth"));
        Assert.DoesNotContain("authorize", SpecText.Component(yaml, "api"));
    }

    [Fact]
    public async Task TheAuthorizersAnswerCanReachTheClient()
    {
        // A 200 can carry headers back to the client too, such as a cookie the authorizer
        // renewed with the session.
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway);
        gateway.WithUpstream(AddApi(builder, repo), u => u.Routes("app/api/")
                .Authorize(auth, path: "/auth/check", headers: ["x-identity"], forward: ["cookie"], respond: ["set-cookie"]))
            .WithUpstream(AddWeb(builder, repo, "web", 4000), u => u.Routes("app/").Authorize(auth, respond: []));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  authorize: { by: auth, path: /auth/check, headers: [x-identity], forward: [cookie], respond: [set-cookie] }\n", SpecText.Upstream(yaml, "api"));
        // Nothing to respond with is no respond.
        Assert.Contains("\n  authorize: { by: auth }\n", SpecText.Upstream(yaml, "web"));
    }

    [Fact]
    public async Task AnAuthorizerCanBeOpenWaitedForAndSkipped()
    {
        // The edge sends on a request an open authorizer can't answer for, waits for it as long
        // as its timeout, and doesn't ask about the requests skip names.
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway);
        gateway.WithUpstream(AddWeb(builder, repo, "web", 4000), u => u.Routes("app")
                .Authorize(auth, headers: ["x-identity"], open: true, timeout: TimeSpan.FromMilliseconds(500), skip: ["/assets/", "*.js", "*.woff2"]))
            .WithUpstream(AddApi(builder, repo), u => u.Routes("app/api/").Authorize(auth, open: false, timeout: TimeSpan.FromSeconds(1), skip: ["/api/"]))
            .WithUpstream(AddWeb(builder, repo, "admin", 4001), u => u.Routes("app/admin/").Authorize(auth, skip: []));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        // An extension is quoted: a plain *.js would be an alias.
        Assert.Contains("\n  authorize: { by: auth, headers: [x-identity], open: true, timeout: 500ms, skip: [/assets/, \"*.js\", \"*.woff2\"] }\n", SpecText.Upstream(yaml, "web"));
        // A prefix can be a whole route, and a closed authorizer writes nothing.
        Assert.Contains("\n  authorize: { by: auth, timeout: 1s, skip: [/api/] }\n", SpecText.Upstream(yaml, "api"));
        Assert.Contains("\n  authorize: { by: auth }\n", SpecText.Upstream(yaml, "admin"));
    }

    [Fact]
    public async Task WhatCantBeWaitedForOrSkippedIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway);
        void Web(string name, int port, TimeSpan? timeout, string[] skip) =>
            gateway.WithUpstream(AddWeb(builder, repo, name, port), u => u.Routes($"app/{name}/").Authorize(auth, timeout: timeout, skip: skip));
        Web("web", 4000, TimeSpan.Zero, ["/other/", "web/assets/", "/web/assets", "/web//assets/", "/web/x/../assets/", "*.JS", "*.min.js", "*"]);
        Web("admin", 4001, TimeSpan.FromSeconds(6), []);
        Web("docs", 4002, TimeSpan.FromMicroseconds(500), [.. Enumerable.Range(0, 31).Select(i => $"*.x{i}")]);

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("web: Authorize: timeout must be above 0", problems);
        // The edge gives the service 120s once the authorizer answers, and Cloudflare gives up
        // at 125s.
        Assert.Contains("admin: Authorize: timeout 6s is more than 5s: the edge gives the service 120s once the authorizer answers, and Cloudflare gives up at 125s", problems);
        Assert.Contains("docs: Authorize: timeout 0.5ms isn't a whole number of milliseconds", problems);
        Assert.Contains("web: Authorize: /other/ isn't under any of the service's routes", problems);
        Assert.Contains("web: Authorize: \"web/assets/\" isn't a path prefix that starts and ends with /, such as /assets/, or a file extension, such as *.js", problems);
        Assert.Contains("web: Authorize: \"/web/assets\" isn't a path prefix that starts and ends with /, such as /assets/, or a file extension, such as *.js", problems);
        Assert.Contains("web: Authorize: \"*\" isn't a path prefix that starts and ends with /, such as /assets/, or a file extension, such as *.js", problems);
        // The edge matches the path as RFC 3986 normalises it, which never has // or a dot segment.
        Assert.Contains("web: Authorize: write /web//assets/ as /web/assets/", problems);
        Assert.Contains("web: Authorize: write /web/x/../assets/ as /web/assets/", problems);
        Assert.Contains("web: Authorize: \"*.JS\" isn't a file extension: up to 10 lower-case letters and digits after *., such as *.js", problems);
        Assert.Contains("web: Authorize: \"*.min.js\" isn't a file extension: up to 10 lower-case letters and digits after *., such as *.js", problems);
        Assert.Contains("docs: Authorize: skip has at most 30 file extensions", problems);
    }

    [Fact]
    public async Task ThePathDefaultsToTheRequestsOwn()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway, routed: false);
        gateway.WithUpstream(AddWeb(builder, repo, "web", 4000), u => u.Routes("app/").Authorize(auth));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Contains("\n  authorize: { by: auth }\n", SpecText.Upstream(yaml, "web"));
    }

    [Fact]
    public async Task WhatCantBeAuthorizedIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var gateway = AddGateway(builder);
        var auth = AddAuth(builder, repo, gateway);
        var redis = builder.AddRedis("redis").PublishAsProtoStrapStateful(s => s.Vm("data-1").Request(cpu: "250m", memory: "512Mi").Volume(size: "5Gi"));
        var smtp = builder.AddContainer("smtp4dev", "rnwood/smtp4dev", "3.8").ExcludeFromManifest();
        gateway.WithUpstream(AddApi(builder, repo), u => u.Routes("app/api/")
                .Authorize(auth, path: "check", headers: ["X Identity"], forward: ["host"], respond: [":status", "content-length", "connection"]))
            .WithUpstream(AddWeb(builder, repo, "web", 4000), u => u.Routes("app/").Authorize(redis))
            .WithUpstream(AddWeb(builder, repo, "admin", 4001), u => u.Routes("app/admin/").Authorize(smtp));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("api: Authorize: path \"check\" must start with /", problems);
        Assert.Contains("api: Authorize: \"X Identity\" isn't a header name: lower case, such as x-user", problems);
        Assert.Contains("api: Authorize: the edge sets host itself", problems);
        // respond can't hand the client a status, or what frames the response on the edge's own
        // connection to it.
        Assert.Contains("api: Authorize: \":status\" isn't a header name: lower case, such as x-user", problems);
        Assert.Contains("api: Authorize: the edge sets content-length itself", problems);
        Assert.Contains("api: Authorize: the edge sets connection itself", problems);
        Assert.Contains("web: Authorize: redis is a stateful component: only a service answers the edge", problems);
        Assert.Contains("admin: Authorize: smtp4dev is excluded from publish", problems);
    }
}
