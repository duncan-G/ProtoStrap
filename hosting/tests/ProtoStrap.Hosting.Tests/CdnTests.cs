using Aspire.Hosting;
using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting.Tests.Support;

namespace ProtoStrap.Hosting.Tests;

/// <summary>
/// The CDN is Cloudflare in front of the tunnel, added to the environment like the tunnel, with
/// the application's own WAF rules, cache rules and per-client rate limit, each rule scoped to
/// the application's hosts (docs/protostrap-aspire.md, "The CDN").
/// </summary>
public class CdnTests
{
    private static (IResourceBuilder<ProtoStrapEnvironmentResource> Environment, IResourceBuilder<ProtoStrapTunnelResource> Tunnel) AddEnvironment(
        IDistributedApplicationBuilder builder, FixtureRepo repo)
    {
        var protostrap = builder.AddEnvironment();
        var gateway = protostrap.AddEnvoyGateway("gateway")
            .WithHost("app", "protofast.dev", local: "app.dev.localhost")
            .WithHost("admin", "admin.protofast.dev", local: "admin.dev.localhost");
        var tunnel = protostrap.AddCloudflareTunnel("tunnel", gateway);
        var api = builder.AddProject("api", repo.Project("services/api/Api.csproj", "Microsoft.NET.Sdk.Web"))
            .PublishAsProtoStrapService(s => s.Request(cpu: "500m", memory: "512Mi").Ready(Readiness.Grpc));
        gateway.WithUpstream(api, u => u.Routes("app/api/"));
        return (protostrap, tunnel);
    }

    [Fact]
    public async Task TheCdnsRulesAreItsSection()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        protostrap.AddCloudflareCdn("cdn", tunnel)
            .WithRateLimit(requests: 300, period: TimeSpan.FromMinutes(1))
            .WithWafRule("block-wp", r => r.Paths("/wp-admin/", "/wp-login.php").Block())
            .WithWafRule("geo", r => r.Hosts("admin").Methods("POST", "PUT").NotCountries("US", "CA").ManagedChallenge())
            .WithWafRule("office", r => r.Ips("203.0.113.0/24", "2001:db8::/32").Skip())
            .WithWafRule("bots", r => r.NotIps("198.51.100.7").Expression("http.user_agent contains \"sqlmap\"").JsChallenge())
            .WithWafRule("login", r => r.Paths("/signin").Countries("RU").Challenge())
            .WithCacheRule("assets", r => r.Hosts("app").Paths("/assets/").Extensions("js", "css")
                .Cache(edge: TimeSpan.FromDays(30), browser: TimeSpan.FromDays(1)))
            .WithCacheRule("api", r => r.Paths("/api/").Bypass())
            .WithCacheRule("pages", r => r.Hosts("app"));

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            cdn:
              rate_limit: { requests: 300, period: 1m }
              waf:
                - { name: block-wp, paths: [/wp-admin/, /wp-login.php], action: block }
                - { name: geo, hosts: [admin], methods: [POST, PUT], not_countries: [US, CA], action: managed_challenge }
                - { name: office, ips: [203.0.113.0/24, "2001:db8::/32"], action: skip }
                - { name: bots, not_ips: [198.51.100.7], expression: http.user_agent contains "sqlmap", action: js_challenge }
                - { name: login, paths: [/signin], countries: [RU], action: challenge }
              cache:
                - { name: assets, hosts: [app], paths: [/assets/], extensions: [js, css], edge_ttl: 30d, browser_ttl: 1d }
                - { name: api, paths: [/api/], cache: false }
                - { name: pages, hosts: [app] }

            """, SpecText.Section(yaml, "cdn"));
        // The CDN is in front of the tunnel, so it comes first of ProtoStrap's own.
        Assert.True(yaml.IndexOf("\ncdn:", StringComparison.Ordinal) < yaml.IndexOf("\ntunnel:", StringComparison.Ordinal), yaml);
    }

    [Fact]
    public async Task ACdnWithNothingIsCloudflaresDefaults()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        protostrap.AddCloudflareCdn("cdn", tunnel);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("cdn: {}\n", SpecText.Section(yaml, "cdn"));
    }

    [Fact]
    public async Task TheRateLimitsPeriodDefaultsToTenSeconds()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        protostrap.AddCloudflareCdn("cdn", tunnel).WithRateLimit(requests: 100);

        var yaml = await TestApp.GenerateYamlAsync(builder);

        Assert.Equal("""
            cdn:
              rate_limit: { requests: 100, period: 10s }

            """, SpecText.Section(yaml, "cdn"));
    }

    [Fact]
    public async Task WhatCloudflareCantTakeIsReported()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        protostrap.AddCloudflareCdn("cdn", tunnel)
            .WithRateLimit(requests: 0, period: TimeSpan.FromSeconds(30))
            .WithWafRule("Bad_Name", r => r.Block())
            .WithWafRule("a", r => r.Hosts("shop", "protofast.dev").Paths("admin/").Extensions(".js").Methods("post")
                .Countries("USA").NotIps("example.com").Expression(" ").Block())
            .WithWafRule("a", r => r.Skip())
            .WithWafRule("no-action", r => r.Paths("/x/"))
            .WithCacheRule("ttl", r => r.Cache(edge: TimeSpan.FromMilliseconds(1500), browser: TimeSpan.Zero));

        var problems = await TestApp.ProblemsAsync(builder);

        Assert.Contains("cdn: WithRateLimit: requests must be at least 1", problems);
        Assert.Contains("cdn: WithRateLimit: period 30s must be 10s, 1m, 2m, 5m, 10m or 1h, Cloudflare's periods", problems);
        Assert.Contains("cdn: WithWafRule: \"Bad_Name\" must be lowercase letters, digits and dashes", problems);
        Assert.Contains("cdn: WithWafRule: a is named twice: each rule has a name of its own", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): no host shop: call WithHost(\"shop\", ...) on gateway", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): a rule names a host by its name, such as admin, not protofast.dev", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): \"admin/\" isn't a path prefix that starts with /, such as /admin/", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): \".js\" isn't a file extension: up to 10 lower-case letters and digits, such as js", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): \"post\" isn't an HTTP method in upper case, such as POST", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): \"USA\" isn't a two-letter country code, such as US", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): \"example.com\" isn't an IP address or range, such as 203.0.113.0/24", problems);
        Assert.Contains("cdn: WithWafRule(\"a\"): the expression is empty", problems);
        Assert.Contains("cdn: WithWafRule(\"no-action\"): no action: call Block(), ManagedChallenge(), JsChallenge(), Challenge() or Skip()", problems);
        Assert.Contains("cdn: WithCacheRule(\"ttl\"): the edge TTL 1500ms isn't a whole number of seconds", problems);
        Assert.Contains("cdn: WithCacheRule(\"ttl\"): the browser TTL must be at least 1s", problems);
    }

    [Fact]
    public void TheCdnIsAddedOnceAndInFrontOfItsOwnTunnel()
    {
        using var repo = new FixtureRepo();
        var builder = TestApp.Publish(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        var cdn = protostrap.AddCloudflareCdn("cdn", tunnel);

        Assert.Same(cdn.Resource, protostrap.Resource.Cdn);
        Assert.Same(protostrap.Resource, cdn.Resource.Parent);
        Assert.Same(tunnel.Resource, cdn.Resource.Tunnel);
        Assert.Contains("already has a CDN, cdn", Assert.Throws<InvalidOperationException>(() => protostrap.AddCloudflareCdn("cdn2", tunnel)).Message);
        Assert.DoesNotContain(builder.Resources, r => r is ProtoStrapCdnResource);

        var other = TestApp.Publish(repo).AddProtoStrapEnvironment("other", app: "other");
        var otherTunnel = other.AddCloudflareTunnel("tunnel", other.AddEnvoyGateway("gateway"));
        var fresh = TestApp.Publish(repo).AddEnvironment();
        Assert.Contains("isn't this environment's", Assert.Throws<InvalidOperationException>(() => fresh.AddCloudflareCdn("cdn", otherTunnel)).Message);
    }

    [Fact]
    public void RunModeRunsNoCdn()
    {
        // Cloudflare is in front of an account only: under `aspire run` requests reach the local
        // gateway directly.
        using var repo = new FixtureRepo();
        var builder = TestApp.Run(repo);
        var (protostrap, tunnel) = AddEnvironment(builder, repo);
        protostrap.AddCloudflareCdn("cdn", tunnel).WithWafRule("block", r => r.Block());

        Assert.DoesNotContain(builder.Resources, r => r is ProtoStrapCdnResource or ProtoStrapTunnelResource);
    }
}
