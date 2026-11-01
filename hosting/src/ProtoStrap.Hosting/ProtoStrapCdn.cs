using Aspire.Hosting.ApplicationModel;
using ProtoStrap.Hosting;

namespace Aspire.Hosting.ApplicationModel
{
    /// <summary>
    /// ProtoStrapCdnResource is the CDN (docs/protostrap.md#cdn): Cloudflare in front of the
    /// tunnel, with the application's own WAF rules, cache rules and per-client rate limit. It is
    /// the spec's cdn section, and runs only in an account.
    /// </summary>
    public sealed class ProtoStrapCdnResource(string name, ProtoStrapEnvironmentResource environment, ProtoStrapTunnelResource tunnel)
        : Resource(name), IResourceWithParent<ProtoStrapEnvironmentResource>
    {
        /// <summary>Parent is the ProtoStrap environment the CDN is part of.</summary>
        public ProtoStrapEnvironmentResource Parent { get; } = environment;

        /// <summary>Tunnel is the tunnel the CDN sends requests to.</summary>
        public ProtoStrapTunnelResource Tunnel { get; } = tunnel;

        /// <summary>RateLimit is the per-client rate limit, as WithRateLimit gave it.</summary>
        internal RateLimitSettings? RateLimit { get; set; }

        /// <summary>WafRules are the WAF rules, in the order WithWafRule added them.</summary>
        internal List<CdnRuleSettings> WafRules { get; } = [];

        /// <summary>CacheRules are the cache rules, in the order WithCacheRule added them.</summary>
        internal List<CdnRuleSettings> CacheRules { get; } = [];
    }
}

namespace Aspire.Hosting
{
    /// <summary>
    /// ProtoStrapCdnRule says which requests a WAF or cache rule matches. Every field set must
    /// match, and within a field any value does. Whatever it matches, the rule applies only to
    /// the application's hosts: those Hosts names, or else every host the tunnel serves.
    /// </summary>
    /// <typeparam name="TRule">It is the rule, so each call returns it.</typeparam>
    public abstract class ProtoStrapCdnRule<TRule>
        where TRule : ProtoStrapCdnRule<TRule>
    {
        private protected ProtoStrapCdnRule(CdnRuleSettings settings) => Settings = settings;

        internal CdnRuleSettings Settings { get; }

        private TRule Self => (TRule)this;

        /// <summary>Hosts are gateway hosts, by the names WithHost gave them, such as "admin".</summary>
        public TRule Hosts(params string[] hosts) { Settings.Hosts.AddRange(hosts); return Self; }

        /// <summary>Paths are prefixes of the request's path, such as "/wp-admin/".</summary>
        public TRule Paths(params string[] paths) { Settings.Paths.AddRange(paths); return Self; }

        /// <summary>Extensions are the path's file extension, without the dot, such as "js".</summary>
        public TRule Extensions(params string[] extensions) { Settings.Extensions.AddRange(extensions); return Self; }

        /// <summary>Methods are HTTP methods, such as "POST".</summary>
        public TRule Methods(params string[] methods) { Settings.Methods.AddRange(methods); return Self; }

        /// <summary>Countries are the client's country, as two-letter codes, such as "US".</summary>
        public TRule Countries(params string[] countries) { Settings.Countries.AddRange(countries); return Self; }

        /// <summary>NotCountries are countries the client isn't in.</summary>
        public TRule NotCountries(params string[] countries) { Settings.NotCountries.AddRange(countries); return Self; }

        /// <summary>Ips are the client's address, as addresses or ranges, such as "203.0.113.0/24".</summary>
        public TRule Ips(params string[] ranges) { Settings.Ips.AddRange(ranges); return Self; }

        /// <summary>NotIps are addresses and ranges the client isn't in.</summary>
        public TRule NotIps(params string[] ranges) { Settings.NotIps.AddRange(ranges); return Self; }

        /// <summary>
        /// Expression is a condition in Cloudflare's rules language, for what the other fields
        /// can't say, such as <c>http.user_agent contains "bot"</c>. It must match too.
        /// </summary>
        public TRule Expression(string expression) { Settings.Expression = expression; return Self; }
    }

    /// <summary>
    /// ProtoStrapWafRule is one of the CDN's WAF rules: what it matches, and its action. Rules
    /// run in order, and the first that matches acts.
    /// </summary>
    public sealed class ProtoStrapWafRule : ProtoStrapCdnRule<ProtoStrapWafRule>
    {
        internal ProtoStrapWafRule(CdnRuleSettings settings)
            : base(settings)
        {
        }

        /// <summary>Block refuses the request.</summary>
        public ProtoStrapWafRule Block() { Settings.Action = "block"; return this; }

        /// <summary>ManagedChallenge has Cloudflare challenge the client as it sees fit.</summary>
        public ProtoStrapWafRule ManagedChallenge() { Settings.Action = "managed_challenge"; return this; }

        /// <summary>JsChallenge has the client's browser solve a JavaScript challenge.</summary>
        public ProtoStrapWafRule JsChallenge() { Settings.Action = "js_challenge"; return this; }

        /// <summary>Challenge has the client solve an interactive challenge.</summary>
        public ProtoStrapWafRule Challenge() { Settings.Action = "challenge"; return this; }

        /// <summary>Skip lets the request past the remaining WAF rules and the rate limit, such as a trusted office's.</summary>
        public ProtoStrapWafRule Skip() { Settings.Action = "skip"; return this; }
    }

    /// <summary>
    /// ProtoStrapCacheRule is one of the CDN's cache rules: what it matches, and how Cloudflare
    /// caches it. Every rule that matches applies, a later one over an earlier.
    /// </summary>
    public sealed class ProtoStrapCacheRule : ProtoStrapCdnRule<ProtoStrapCacheRule>
    {
        internal ProtoStrapCacheRule(CdnRuleSettings settings)
            : base(settings)
        {
        }

        /// <summary>
        /// Cache makes the responses eligible for Cloudflare's cache, which a rule does unless it
        /// bypasses it, for edge at Cloudflare and browser in browsers when given, over the
        /// origin's headers.
        /// </summary>
        public ProtoStrapCacheRule Cache(TimeSpan? edge = null, TimeSpan? browser = null)
        {
            (Settings.Bypass, Settings.EdgeTtl, Settings.BrowserTtl) = (false, edge, browser);
            return this;
        }

        /// <summary>Bypass keeps the responses out of Cloudflare's cache.</summary>
        public ProtoStrapCacheRule Bypass()
        {
            (Settings.Bypass, Settings.EdgeTtl, Settings.BrowserTtl) = (true, null, null);
            return this;
        }
    }

    /// <summary>These extension methods add the CDN to the environment, and give it its rules.</summary>
    public static class ProtoStrapCdnExtensions
    {
        /// <summary>
        /// AddCloudflareCdn adds the CDN (docs/protostrap.md#cdn): Cloudflare in front of the
        /// tunnel, with the WAF rules, cache rules and rate limit given here. Without it, Cloudflare
        /// runs with ProtoStrap's settings alone. It runs only in an account.
        /// </summary>
        /// <param name="builder">It is the environment.</param>
        /// <param name="name">It is the CDN's resource name.</param>
        /// <param name="tunnel">It is the environment's tunnel, which the CDN is in front of.</param>
        public static IResourceBuilder<ProtoStrapCdnResource> AddCloudflareCdn(
            this IResourceBuilder<ProtoStrapEnvironmentResource> builder, string name, IResourceBuilder<ProtoStrapTunnelResource> tunnel)
        {
            ArgumentNullException.ThrowIfNull(builder);
            ArgumentNullException.ThrowIfNull(tunnel);
            var environment = builder.Resource;
            if (environment.Cdn is { } existing)
                throw new InvalidOperationException($"{environment.Name} already has a CDN, {existing.Name}: add rules to it");
            if (tunnel.Resource.Parent != environment)
                throw new InvalidOperationException($"{tunnel.Resource.Name} isn't this environment's tunnel: {environment.Name}'s CDN is in front of its own");
            var cdn = new ProtoStrapCdnResource(name, environment, tunnel.Resource);
            environment.Cdn = cdn;
            return builder.ApplicationBuilder.CreateResourceBuilder(cdn);
        }

        /// <summary>
        /// WithRateLimit has Cloudflare block a client that sends more than requests in period,
        /// for as long as the period: 10 seconds by default, or 1, 2, 5 or 10 minutes, or an hour.
        /// Each upstream's own route rates are the gateway's.
        /// </summary>
        public static IResourceBuilder<ProtoStrapCdnResource> WithRateLimit(
            this IResourceBuilder<ProtoStrapCdnResource> builder, int requests, TimeSpan? period = null)
        {
            builder.Resource.RateLimit = new(requests, period ?? TimeSpan.FromSeconds(10));
            return builder;
        }

        /// <summary>
        /// WithWafRule adds a WAF rule, after those added before: the requests it matches, and
        /// what Cloudflare does with them.
        /// </summary>
        /// <param name="builder">It is the CDN.</param>
        /// <param name="name">It is the rule's name: lowercase letters, digits and dashes, such as block-admin.</param>
        /// <param name="configure">It says what the rule matches, and its action.</param>
        public static IResourceBuilder<ProtoStrapCdnResource> WithWafRule(
            this IResourceBuilder<ProtoStrapCdnResource> builder, string name, Action<ProtoStrapWafRule> configure)
        {
            ArgumentNullException.ThrowIfNull(configure);
            var settings = new CdnRuleSettings(name);
            configure(new ProtoStrapWafRule(settings));
            builder.Resource.WafRules.Add(settings);
            return builder;
        }

        /// <summary>
        /// WithCacheRule adds a cache rule, after those added before: the requests it matches, and
        /// how Cloudflare caches their responses.
        /// </summary>
        /// <param name="builder">It is the CDN.</param>
        /// <param name="name">It is the rule's name: lowercase letters, digits and dashes, such as assets.</param>
        /// <param name="configure">It says what the rule matches, and how it caches.</param>
        public static IResourceBuilder<ProtoStrapCdnResource> WithCacheRule(
            this IResourceBuilder<ProtoStrapCdnResource> builder, string name, Action<ProtoStrapCacheRule> configure)
        {
            ArgumentNullException.ThrowIfNull(configure);
            var settings = new CdnRuleSettings(name);
            configure(new ProtoStrapCacheRule(settings));
            builder.Resource.CacheRules.Add(settings);
            return builder;
        }
    }
}
