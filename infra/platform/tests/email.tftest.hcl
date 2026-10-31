# This test applies email (email.tf, and its statement in components.tf) to mocked providers:
# an SES identity per domain the application sends mail from, its DNS records, and the
# permission to send for each component with email in its uses (docs/email.md).
#
#   terraform -chdir=infra/platform init -backend=false && terraform -chdir=infra/platform test

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = { account_id = "123456789012" }
  }
  mock_data "aws_region" {
    defaults = { name = "us-east-1" }
  }
  mock_data "aws_partition" {
    defaults = { partition = "aws" }
  }
  mock_data "aws_availability_zones" {
    defaults = { names = ["us-east-1a", "us-east-1b", "us-east-1c"] }
  }
  mock_data "aws_ssm_parameter" {
    defaults = { insecure_value = "ami-0123456789abcdef0", value = "ami-0123456789abcdef0" }
  }
  mock_data "aws_iam_policy_document" {
    defaults = { json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}" }
  }
  mock_resource "aws_iam_role" {
    defaults = { arn = "arn:aws:iam::123456789012:role/protostrap/mock" }
  }
  mock_resource "aws_sqs_queue" {
    defaults = { arn = "arn:aws:sqs:us-east-1:123456789012:protostrap-mock", url = "https://sqs.us-east-1.amazonaws.com/123456789012/protostrap-mock" }
  }
  mock_resource "aws_s3_bucket" {
    defaults = { arn = "arn:aws:s3:::mock" }
  }
  mock_resource "aws_eip" {
    defaults = { allocation_id = "eipalloc-0123456789abcdef0" }
  }
  mock_resource "aws_sns_topic" {
    defaults = { arn = "arn:aws:sns:us-east-1:123456789012:protostrap-mock" }
  }
  mock_resource "aws_iam_instance_profile" {
    defaults = { arn = "arn:aws:iam::123456789012:instance-profile/protostrap/mock" }
  }
  mock_resource "aws_cloudwatch_log_group" {
    defaults = { arn = "arn:aws:logs:us-east-1:123456789012:log-group:/protostrap/mock" }
  }
  mock_resource "aws_secretsmanager_secret" {
    defaults = { arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:protostrap/mock-AbCdEf" }
  }
  mock_resource "aws_ebs_volume" {
    defaults = { id = "vol-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:volume/vol-0123456789abcdef0" }
  }
  mock_resource "aws_instance" {
    defaults = { id = "i-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0" }
  }
  mock_resource "aws_launch_template" {
    defaults = { id = "lt-0123456789abcdef0", latest_version = 1 }
  }
  mock_resource "aws_sesv2_email_identity" {
    defaults = {
      arn                     = "arn:aws:ses:us-east-1:123456789012:identity/mock"
      dkim_signing_attributes = { tokens = ["tok0", "tok1", "tok2"] }
    }
  }
  mock_resource "aws_route_table" {
    defaults = { id = "rtb-0123456789abcdef0", arn = "arn:aws:ec2:us-east-1:123456789012:route-table/rtb-0123456789abcdef0" }
  }
}

mock_provider "cloudflare" {
  mock_data "cloudflare_zone" {
    defaults = { id = "0123456789abcdef0123456789abcdef" }
  }
}

mock_provider "random" {}

variables {
  environment           = "staging"
  hosts                 = null
  cloudflare_account_id = "0123456789abcdef0123456789abcdef"
  cloudflare_zone       = "example.com"
  spec                  = <<-EOT
    app: mail
    protostrap: 0.1.0
    hosts:
      mail: mail.example.com
    pools:
      ingress: { vm: t4g.small }
      general: { vm: [t4g.medium], max: 2 }
      data: { vm: t4g.medium }
    email:
      domains: [example.com, news.example.com]
    components:
      web:
        kind: service
        build: web
        pool: general
        request: { cpu: 100m, memory: 128Mi }
        port: 80
        ready: /healthz
        routes: [/]
        uses: [email]
      worker:
        kind: worker
        build: worker
        pool: general
        request: { cpu: 100m, memory: 128Mi }
  EOT
}

run "email" {
  command = apply

  assert {
    condition     = toset(keys(aws_sesv2_email_identity.app)) == toset(["example.com", "news.example.com"])
    error_message = "Each of the spec's email domains gets an SES identity."
  }

  assert {
    condition = alltrue([for d, i in aws_sesv2_email_identity.app :
    i.email_identity == d && i.dkim_signing_attributes[0].next_signing_key_length == "RSA_2048_BIT"])
    error_message = "Each identity signs with Easy DKIM, 2048-bit."
  }

  assert {
    condition = alltrue([for d, m in aws_sesv2_email_identity_mail_from_attributes.app :
    m.mail_from_domain == "bounce.${d}" && m.behavior_on_mx_failure == "REJECT_MESSAGE"])
    error_message = "Bounces come back to bounce.<domain>, and a missing MX fails the send rather than leave the domain."
  }

  assert {
    condition = (length(cloudflare_dns_record.email_dkim) == 6 &&
      cloudflare_dns_record.email_dkim["example.com/1"].name == "tok1._domainkey.example.com" &&
      cloudflare_dns_record.email_dkim["example.com/1"].content == "tok1.dkim.amazonses.com" &&
      cloudflare_dns_record.email_dkim["news.example.com/2"].name == "tok2._domainkey.news.example.com" &&
    alltrue([for r in cloudflare_dns_record.email_dkim : r.type == "CNAME" && r.proxied == false]))
    error_message = "Each domain gets Easy DKIM's three CNAMEs, unproxied."
  }

  assert {
    condition = (cloudflare_dns_record.email_mx["news.example.com"].name == "bounce.news.example.com" &&
      cloudflare_dns_record.email_mx["news.example.com"].content == "feedback-smtp.us-east-1.amazonses.com" &&
      cloudflare_dns_record.email_mx["news.example.com"].type == "MX" &&
    cloudflare_dns_record.email_mx["news.example.com"].priority == 10)
    error_message = "The MAIL FROM domain's MX is SES's feedback endpoint in the region."
  }

  assert {
    condition = (cloudflare_dns_record.email_spf["example.com"].name == "bounce.example.com" &&
      cloudflare_dns_record.email_spf["example.com"].type == "TXT" &&
    cloudflare_dns_record.email_spf["example.com"].content == "\"v=spf1 include:amazonses.com ~all\"")
    error_message = "The MAIL FROM domain's SPF lets SES send for it."
  }

  assert {
    condition     = length(cloudflare_dns_record.email_dmarc) == 0
    error_message = "No DMARC record unless email_dmarc sets one: the domain may have one already."
  }

  assert {
    condition = (
      jsonencode(sort(flatten([for st in data.aws_iam_policy_document.component["web"].statement : st.actions if st.sid == "Email"]))) == jsonencode(["ses:SendEmail", "ses:SendRawEmail"]) &&
      toset(flatten([for st in data.aws_iam_policy_document.component["web"].statement : st.resources if st.sid == "Email"])) == toset([for i in aws_sesv2_email_identity.app : i.arn]) &&
    jsonencode(sort(flatten([for st in data.aws_iam_policy_document.component["web"].statement : [for c in st.condition : c.values if c.variable == "ses:FromAddress"] if st.sid == "Email"]))) == jsonencode(["*@example.com", "*@news.example.com"]))
    error_message = "A component with email in its uses may send from any address in the email domains, and only there."
  }

  assert {
    condition     = length([for st in data.aws_iam_policy_document.component["worker"].statement : st if st.sid == "Email"]) == 0
    error_message = "A component without email in its uses can't send mail."
  }
}

run "dmarc" {
  command = apply

  variables {
    email_dmarc = "v=DMARC1; p=none; rua=mailto:dmarc@example.com"
  }

  assert {
    condition = (toset(keys(cloudflare_dns_record.email_dmarc)) == toset(["example.com", "news.example.com"]) &&
      cloudflare_dns_record.email_dmarc["news.example.com"].name == "_dmarc.news.example.com" &&
      cloudflare_dns_record.email_dmarc["news.example.com"].type == "TXT" &&
    cloudflare_dns_record.email_dmarc["news.example.com"].content == "\"v=DMARC1; p=none; rua=mailto:dmarc@example.com\"")
    error_message = "email_dmarc is published at _dmarc.<domain> for each domain."
  }
}

# Two accounts can't share a domain, since both would write its bounce. records, so an
# account sends from domains of its own.
run "domains_of_the_account" {
  command = apply

  variables {
    email_domains = ["staging.example.com"]
  }

  assert {
    condition = (toset(keys(aws_sesv2_email_identity.app)) == toset(["staging.example.com"]) &&
    jsonencode(flatten([for st in data.aws_iam_policy_document.component["web"].statement : [for c in st.condition : c.values if c.variable == "ses:FromAddress"] if st.sid == "Email"])) == jsonencode(["*@staging.example.com"]))
    error_message = "email_domains replaces the spec's domains in this account."
  }
}

run "domain_outside_the_zone" {
  command = plan

  variables {
    email_domains = ["example.org"]
  }

  expect_failures = [aws_sesv2_email_identity.app]
}

run "no_email" {
  command = apply

  variables {
    spec = <<-EOT
      app: mail
      protostrap: 0.1.0
      hosts:
        mail: mail.example.com
      pools:
        ingress: { vm: t4g.small }
        general: { vm: [t4g.medium], max: 2 }
        data: { vm: t4g.medium }
      components:
        web:
          kind: service
          build: web
          pool: general
          request: { cpu: 100m, memory: 128Mi }
          port: 80
          ready: /healthz
          routes: [/]
    EOT
  }

  assert {
    condition = (length(aws_sesv2_email_identity.app) == 0 && length(cloudflare_dns_record.email_dkim) == 0 &&
      length(cloudflare_dns_record.email_mx) == 0 && length(cloudflare_dns_record.email_spf) == 0 &&
    length([for st in data.aws_iam_policy_document.component["web"].statement : st if st.sid == "Email"]) == 0)
    error_message = "Without an email section, there is no identity, record or permission to send."
  }
}
