# The CDN (docs/network.md#cloudflare) is Cloudflare in front of everything. Every host
# points at the tunnel; Cloudflare terminates TLS, sends plain HTTP to HTTPS, caches static
# assets (its default, which leaves API responses alone), and checks who operators are
# before an operator UI gets a request. The zone already exists: it is referenced, never
# created.

data "cloudflare_zone" "app" {
  filter = {
    name = var.cloudflare_zone
  }
}

resource "cloudflare_dns_record" "host" {
  for_each = toset(local.tunnel_hosts)
  zone_id  = data.cloudflare_zone.app.id
  name     = each.key
  content  = "${cloudflare_zero_trust_tunnel_cloudflared.edge.id}.cfargotunnel.com"
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

resource "cloudflare_ruleset" "rate_limit" {
  count   = var.client_rate_limit > 0 && length(local.tunnel_hosts) > 0 ? 1 : 0
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
      period              = 10
      requests_per_period = var.client_rate_limit
      mitigation_timeout  = 10
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
