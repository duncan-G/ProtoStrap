# The CDN (docs/network.md#cloudflare) is Cloudflare in front of everything. With a tunnel,
# every host points at it; Cloudflare terminates TLS, sends plain HTTP to HTTPS, caches static
# assets (its default, which leaves API responses alone) for as long as the origin's headers
# say, and checks who operators are before an operator UI gets a request. The zone already exists: it is referenced, never
# created.
#
# The spec's cdn section (docs/protostrap.md#cdn) adds the application's own WAF rules, cache
# rules and per-client rate limit. Each phase has one ruleset per zone, so each rule is scoped
# to the application's hosts: those it names, by their names in gateway.hosts, or else every
# host the tunnel serves.

locals {
  cdn = try(local.spec.cdn, null)

  # Each rule's expression is its fields, each one that is set a clause, all of which must
  # match: within a clause, any value does. `protostrap check` has made every value safe to
  # quote.
  cdn_rules = {
    for list in ["waf", "cache"] : list => [
      for r in try(concat(local.cdn[list]), []) : {
        name = r.name
        expression = join(" and ", compact([
          "(http.host in {${join(" ", [for h in(try(length(r.hosts), 0) == 0 ? local.tunnel_hosts : [for n in r.hosts : local.hostnames[n]]) : "\"${h}\""])}})",
          try(length(r.paths), 0) == 0 ? "" : "(${join(" or ", [for p in r.paths : "starts_with(http.request.uri.path, \"${p}\")"])})",
          try(length(r.extensions), 0) == 0 ? "" : "(http.request.uri.path.extension in {${join(" ", [for e in r.extensions : "\"${e}\""])}})",
          try(length(r.methods), 0) == 0 ? "" : "(http.request.method in {${join(" ", [for m in r.methods : "\"${m}\""])}})",
          try(length(r.countries), 0) == 0 ? "" : "(ip.src.country in {${join(" ", [for c in r.countries : "\"${c}\""])}})",
          try(length(r.not_countries), 0) == 0 ? "" : "(not ip.src.country in {${join(" ", [for c in r.not_countries : "\"${c}\""])}})",
          try(length(r.ips), 0) == 0 ? "" : "(ip.src in {${join(" ", r.ips)}})",
          try(length(r.not_ips), 0) == 0 ? "" : "(not ip.src in {${join(" ", r.not_ips)}})",
          try(r.expression, null) == null ? "" : "(${r.expression})",
        ]))
        action      = try(r.action, null)
        cache       = try(r.cache, true)
        edge_ttl    = try(r.edge_ttl, null) == null ? null : floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", r.edge_ttl) : tonumber(m[0]) * local.duration_units[m[1]]])))
        browser_ttl = try(r.browser_ttl, null) == null ? null : floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", r.browser_ttl) : tonumber(m[0]) * local.duration_units[m[1]]])))
      }
    ]
  }

  # The rate limit's period, in seconds: 10 unless the spec sets another.
  rate_limit        = try(local.cdn.rate_limit, null)
  rate_limit_period = local.rate_limit == null ? 0 : floor(sum(concat([0], [for m in regexall("([0-9]+)(ms|s|m|h|d)", try(local.rate_limit.period, "10s")) : tonumber(m[0]) * local.duration_units[m[1]]])))
}

data "cloudflare_zone" "app" {
  filter = {
    name = var.cloudflare_zone
  }
}

resource "cloudflare_dns_record" "host" {
  for_each = toset(local.tunnel_hosts)
  zone_id  = data.cloudflare_zone.app.id
  name     = each.key
  content  = "${one(cloudflare_zero_trust_tunnel_cloudflared.edge[*].id)}.cfargotunnel.com"
  type     = "CNAME"
  proxied  = true
  ttl      = 1
  comment  = "ProtoStrap ${local.app} ${var.environment}: the tunnel to the edge"
}

resource "cloudflare_zone_setting" "always_use_https" {
  zone_id    = data.cloudflare_zone.app.id
  setting_id = "always_use_https"
  value      = "on"
}

resource "cloudflare_zone_setting" "min_tls_version" {
  zone_id    = data.cloudflare_zone.app.id
  setting_id = "min_tls_version"
  value      = "1.2"
}

# 0 is Respect Existing Headers. Cloudflare's default, 4 hours, raises any shorter max-age the
# origin sends, so a file whose URL never changes stays stale in browsers, which no purge
# reaches.
resource "cloudflare_zone_setting" "browser_cache_ttl" {
  zone_id    = data.cloudflare_zone.app.id
  setting_id = "browser_cache_ttl"
  value      = 0
}

resource "cloudflare_ruleset" "rate_limit" {
  count   = local.rate_limit != null && length(local.tunnel_hosts) > 0 ? 1 : 0
  zone_id = data.cloudflare_zone.app.id
  name    = "protostrap-${local.app}-rate-limit"
  kind    = "zone"
  phase   = "http_ratelimit"

  rules = [{
    ref         = "per_client"
    description = "A broad limit per client; Envoy limits each route"
    expression  = join(" or ", [for host in local.tunnel_hosts : "(http.host eq \"${host}\")"])
    action      = "block"
    ratelimit = {
      characteristics     = ["ip.src", "cf.colo.id"]
      period              = local.rate_limit_period
      requests_per_period = local.rate_limit.requests
      mitigation_timeout  = local.rate_limit_period
    }
  }]
}

# The WAF rules are the zone's custom rules, in the spec's order: the first that matches acts.
# A skip rule lets its requests past the remaining rules and the rate limit.
resource "cloudflare_ruleset" "waf" {
  count   = length(local.cdn_rules.waf) > 0 && length(local.tunnel_hosts) > 0 ? 1 : 0
  zone_id = data.cloudflare_zone.app.id
  name    = "protostrap-${local.app}-waf"
  kind    = "zone"
  phase   = "http_request_firewall_custom"

  rules = [for r in local.cdn_rules.waf : {
    ref               = r.name
    description       = "ProtoStrap ${local.app} ${var.environment}: ${r.name}"
    expression        = r.expression
    action            = r.action
    action_parameters = r.action == "skip" ? { ruleset = "current", phases = ["http_ratelimit"] } : null
  }]
}

# The cache rules are the zone's cache settings, in the spec's order: each that matches
# applies, a later one over an earlier. A rule makes its responses eligible for the cache, for
# its TTLs over the origin's headers when it has them, or bypasses the cache.
resource "cloudflare_ruleset" "cache" {
  count   = length(local.cdn_rules.cache) > 0 && length(local.tunnel_hosts) > 0 ? 1 : 0
  zone_id = data.cloudflare_zone.app.id
  name    = "protostrap-${local.app}-cache"
  kind    = "zone"
  phase   = "http_request_cache_settings"

  rules = [for r in local.cdn_rules.cache : {
    ref         = r.name
    description = "ProtoStrap ${local.app} ${var.environment}: ${r.name}"
    expression  = r.expression
    action      = "set_cache_settings"
    action_parameters = {
      cache       = r.cache
      edge_ttl    = r.edge_ttl == null ? null : { mode = "override_origin", default = r.edge_ttl }
      browser_ttl = r.browser_ttl == null ? null : { mode = "override_origin", default = r.browser_ttl }
    }
  }]
}

# Each operator UI (docs/network.md#operator-uis) sits behind Cloudflare Access, which lets
# through only the emails on its allow-list. A UI without an allow-list gets no hostname.
resource "cloudflare_zero_trust_access_policy" "operators" {
  for_each   = var.operator_hosts
  account_id = var.cloudflare_account_id
  name       = "protostrap ${local.app} ${var.environment}: operators of ${each.key}"
  decision   = "allow"
  include = [for email in each.value : {
    email = {
      email = email
    }
  }]
}

resource "cloudflare_zero_trust_access_application" "operators" {
  for_each                  = var.operator_hosts
  account_id                = var.cloudflare_account_id
  name                      = "protostrap ${local.app} ${var.environment}: ${each.key}"
  domain                    = each.key
  type                      = "self_hosted"
  session_duration          = "8h"
  app_launcher_visible      = false
  auto_redirect_to_identity = false
  policies = [{
    id         = cloudflare_zero_trust_access_policy.operators[each.key].id
    precedence = 1
  }]
}
