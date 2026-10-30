# This file derives ProtoStrap's names. One application runs per AWS account, so
# ProtoStrap's resources share the protostrap prefix, and its buckets add the account and
# region to be globally unique. The one exception is the environment's secret,
# <app>/<environment>, named for whoever sets its keys. The platform root, infra/platform, derives the same names:
# keep the two in step.

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}
data "aws_partition" "current" {}

locals {
  account   = data.aws_caller_identity.current.account_id
  region    = data.aws_region.current.name
  partition = data.aws_partition.current.partition

  bucket_prefix = "protostrap-${local.account}-${local.region}"
  state_bucket  = "${local.bucket_prefix}-tfstate"
  deploy_bucket = "${local.bucket_prefix}-deploy"
  registry      = "${local.account}.dkr.ecr.${local.region}.amazonaws.com"

  # Every role and policy ProtoStrap makes lives under this path, which is how the
  # boundaries recognise its own.
  path = "/protostrap/"

  infra_boundary_arn = "arn:${local.partition}:iam::${local.account}:policy/protostrap/protostrap-infra-boundary"
  role_boundary_arn  = "arn:${local.partition}:iam::${local.account}:policy/protostrap/protostrap-role-boundary"

  infra_role_arn  = "arn:${local.partition}:iam::${local.account}:role/protostrap/protostrap-infra"
  deploy_role_arn = "arn:${local.partition}:iam::${local.account}:role/protostrap/protostrap-deploy"

  # These are the ARN patterns for ProtoStrap's own resources.
  own = {
    buckets     = "arn:${local.partition}:s3:::${local.bucket_prefix}-*"
    objects     = "arn:${local.partition}:s3:::${local.bucket_prefix}-*/*"
    state       = "arn:${local.partition}:s3:::${local.state_bucket}"
    deploy      = "arn:${local.partition}:s3:::${local.deploy_bucket}"
    repos       = "arn:${local.partition}:ecr:${local.region}:${local.account}:repository/protostrap/*"
    queues      = "arn:${local.partition}:sqs:${local.region}:${local.account}:protostrap-*"
    email       = "arn:${local.partition}:ses:${local.region}:${local.account}:identity/*"
    secrets     = "arn:${local.partition}:secretsmanager:${local.region}:${local.account}:secret:protostrap/*"
    app_secret  = "arn:${local.partition}:secretsmanager:${local.region}:${local.account}:secret:${var.app}/${var.environment}-??????"
    logs        = "arn:${local.partition}:logs:${local.region}:${local.account}:log-group:/protostrap/*"
    roles       = "arn:${local.partition}:iam::${local.account}:role/protostrap/*"
    profiles    = "arn:${local.partition}:iam::${local.account}:instance-profile/protostrap/*"
    deploy_doc  = "arn:${local.partition}:ssm:${local.region}:${local.account}:document/protostrap-deploy"
    instances   = "arn:${local.partition}:ec2:${local.region}:${local.account}:instance/*"
    oidc        = "arn:${local.partition}:iam::${local.account}:oidc-provider/token.actions.githubusercontent.com"
    boundaries  = [local.infra_boundary_arn, local.role_boundary_arn]
    ci_roles    = [local.infra_role_arn, local.deploy_role_arn]
    kms_through = [for s in ["ec2", "s3", "sqs", "secretsmanager", "logs"] : "${s}.${local.region}.amazonaws.com"]
  }

  deploy_environment = var.github_deploy_environment != "" ? var.github_deploy_environment : var.environment
  infra_environment  = var.github_infra_environment != "" ? var.github_infra_environment : "${var.environment}-infra"
}
