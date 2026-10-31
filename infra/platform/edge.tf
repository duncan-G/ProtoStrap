# The edge (docs/protostrap.md#edge) is cloudflared and Envoy in one container, one replica
# alone on each zone's ingress VM. Here are the tunnel it connects and, like any
# component, its ECR repository, log group and role. The scheduler places the edge
# and writes Envoy's routes; the deploy workflow copies the edge image of the spec's
# ProtoStrap version into the repository.
#
# The tunnel is created with its routes in Cloudflare, not on a VM: every host goes to
# Envoy in the same container, and any other host gets a 404. Its token is
# Edge_TUNNEL_TOKEN in the environment's secret (secret.tf), which an Org Admin sets:
# scripts/set-edge-token.sh fetches it from Cloudflare.

locals {
  tunnel_hosts = distinct(concat(local.hosts, keys(var.operator_hosts)))
}

# hosts.json gives the scheduler each of the spec's hosts' hostname in this account, for the
# edge's routes and every replica's HOST_<NAME> and ORIGIN_<NAME>. The scheduler reads it
# again every minute, so a new hostname reloads the edge's routes and replaces the replicas.
resource "aws_s3_object" "hosts" {
  bucket       = data.aws_s3_bucket.deploy.bucket
  key          = "hosts.json"
  content      = jsonencode(local.hostnames)
  content_type = "application/json"
  tags         = local.tags

  lifecycle {
    precondition {
      condition     = var.hosts == null || toset(keys(coalesce(var.hosts, {}))) == toset(keys(local.spec_hosts))
      error_message = "hosts must give a hostname to every host the spec names, and only those: ${length(local.spec_hosts) == 0 ? "the spec names none" : join(", ", sort(keys(local.spec_hosts)))}. A name left out would fall back to the spec's hostname, which another account serves."
    }
  }
}

resource "random_id" "tunnel_secret" {
  byte_length = 35
}

resource "cloudflare_zero_trust_tunnel_cloudflared" "edge" {
  account_id    = var.cloudflare_account_id
  name          = "protostrap-${local.app}-${var.environment}"
  tunnel_secret = random_id.tunnel_secret.b64_std
  config_src    = "cloudflare"
}

resource "cloudflare_zero_trust_tunnel_cloudflared_config" "edge" {
  account_id = var.cloudflare_account_id
  tunnel_id  = cloudflare_zero_trust_tunnel_cloudflared.edge.id

  config = {
    ingress = concat(
      [for host in local.tunnel_hosts : {
        hostname = host
        service  = "http://127.0.0.1:8080"
      }],
      [{ service = "http_status:404" }],
    )
  }
}

resource "aws_ecr_repository" "edge" {
  name                 = "protostrap/edge"
  force_delete         = var.force_destroy
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration {
    scan_on_push = true
  }
  tags = local.tags
}

resource "aws_cloudwatch_log_group" "edge" {
  name              = "${local.log_prefix}edge"
  retention_in_days = var.log_retention_days
  tags              = local.tags
}

# This is the edge's own role, which the agent on an ingress VM takes on for it: it reads
# the environment's secret, for its tunnel token, Edge_TUNNEL_TOKEN, and nothing else.
data "aws_iam_policy_document" "edge_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.ingress.arn]
    }
  }
}

resource "aws_iam_role" "edge" {
  name                 = "protostrap-c-edge"
  path                 = local.path
  description          = "The edge: cloudflared and Envoy."
  assume_role_policy   = data.aws_iam_policy_document.edge_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

data "aws_iam_policy_document" "edge" {
  statement {
    sid       = "EnvironmentSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn, "${local.old_secret}edge-??????"]
  }
}

resource "aws_iam_role_policy" "edge" {
  name   = "edge"
  role   = aws_iam_role.edge.id
  policy = data.aws_iam_policy_document.edge.json
}
