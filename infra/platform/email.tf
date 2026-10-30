# This file is email (docs/email.md): an SES identity for each domain the
# application sends mail from, and the DNS records in its Cloudflare zone that verify the
# domain and authenticate its mail. The permission to send is in each component's policy
# (components.tf), for the components with email in their uses: they send with the SES API
# and their own role's credentials, so no SMTP password exists.

locals {
  email_domains = toset(var.email_domains != null ? var.email_domains : try(local.spec.email.domains, []))
  # Easy DKIM always gives three tokens, so each domain's records are keyed by their index,
  # which is known before the identity exists.
  email_dkim = { for p in setproduct(local.email_domains, [0, 1, 2]) : "${p[0]}/${p[1]}" => { domain = p[0], index = p[1] } }
}

resource "aws_sesv2_email_identity" "app" {
  for_each       = local.email_domains
  email_identity = each.key
  tags           = local.tags

  dkim_signing_attributes {
    next_signing_key_length = "RSA_2048_BIT"
  }

  lifecycle {
    precondition {
      condition     = each.key == var.cloudflare_zone || endswith(each.key, ".${var.cloudflare_zone}")
      error_message = "The email domain ${each.key} isn't in the Cloudflare zone ${var.cloudflare_zone}, where its DNS records go."
    }
  }
}

# Bounces and complaints come back to bounce.<domain>, under the application's own domain, so
# the envelope sender passes SPF and lines up with the From address for DMARC. If its MX is
# missing, SES refuses the send rather than fall back to amazonses.com, which DMARC would fail.
resource "aws_sesv2_email_identity_mail_from_attributes" "app" {
  for_each               = aws_sesv2_email_identity.app
  email_identity         = each.value.email_identity
  mail_from_domain       = "bounce.${each.key}"
  behavior_on_mx_failure = "REJECT_MESSAGE"
}

# These CNAMEs prove the domain is the application's and sign its mail.
resource "cloudflare_dns_record" "email_dkim" {
  for_each = local.email_dkim
  zone_id  = data.cloudflare_zone.app.id
  name     = "${aws_sesv2_email_identity.app[each.value.domain].dkim_signing_attributes[0].tokens[each.value.index]}._domainkey.${each.value.domain}"
  content  = "${aws_sesv2_email_identity.app[each.value.domain].dkim_signing_attributes[0].tokens[each.value.index]}.dkim.amazonses.com"
  type     = "CNAME"
  proxied  = false
  ttl      = 1
  comment  = "ProtoStrap ${local.app} ${var.environment}: SES DKIM for ${each.value.domain}"
}

resource "cloudflare_dns_record" "email_mx" {
  for_each = local.email_domains
  zone_id  = data.cloudflare_zone.app.id
  name     = "bounce.${each.key}"
  content  = "feedback-smtp.${local.region}.amazonses.com"
  type     = "MX"
  priority = 10
  proxied  = false
  ttl      = 1
  comment  = "ProtoStrap ${local.app} ${var.environment}: SES bounces for ${each.key}"
}

resource "cloudflare_dns_record" "email_spf" {
  for_each = local.email_domains
  zone_id  = data.cloudflare_zone.app.id
  name     = "bounce.${each.key}"
  content  = "\"v=spf1 include:amazonses.com ~all\""
  type     = "TXT"
  proxied  = false
  ttl      = 1
  comment  = "ProtoStrap ${local.app} ${var.environment}: SES may send for ${each.key}"
}

# A domain has one DMARC record, and it may have one already, from another mail service, so
# this one is published only when email_dmarc says what it is.
resource "cloudflare_dns_record" "email_dmarc" {
  for_each = var.email_dmarc == null ? toset([]) : local.email_domains
  zone_id  = data.cloudflare_zone.app.id
  name     = "_dmarc.${each.key}"
  content  = "\"${var.email_dmarc}\""
  type     = "TXT"
  proxied  = false
  ttl      = 1
  comment  = "ProtoStrap ${local.app} ${var.environment}: DMARC for ${each.key}"
}
