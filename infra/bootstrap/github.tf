# This file sets up GitHub Actions: the OpenID Connect provider the workflows sign in with,
# so no AWS key is stored in GitHub, and the two environments they run in, with what each
# needs to know:
#
#   <environment>        deploy workflow   AWS_ROLE_ARN = protostrap-deploy
#   <environment>-infra  infra workflow    AWS_ROLE_ARN = protostrap-infra, TF_VAR_*, secrets
#
# Both also get AWS_REGION, the environment's name, the bucket and registry names, and the
# Infra role's ARN.

data "tls_certificate" "github" {
  count = var.create_github_oidc_provider ? 1 : 0
  url   = "https://token.actions.githubusercontent.com/.well-known/openid-configuration"
}

resource "aws_iam_openid_connect_provider" "github" {
  count           = var.create_github_oidc_provider ? 1 : 0
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = [data.tls_certificate.github[0].certificates[0].sha1_fingerprint]
}

locals {
  github_oidc_arn = var.create_github_oidc_provider ? aws_iam_openid_connect_provider.github[0].arn : local.own.oidc
  repository_name = split("/", var.github_repository)[1]

  common_variables = {
    AWS_REGION                = local.region
    PROTOSTRAP_ENVIRONMENT    = var.environment
    PROTOSTRAP_STATE_BUCKET   = aws_s3_bucket.state.id
    PROTOSTRAP_DEPLOY_BUCKET  = local.deploy_bucket
    PROTOSTRAP_REGISTRY       = local.registry
    PROTOSTRAP_INFRA_ROLE_ARN = aws_iam_role.infra.arn
  }

  deploy_variables = merge(local.common_variables, {
    AWS_ROLE_ARN = aws_iam_role.deploy.arn
  })

  infra_variables = merge(local.common_variables, {
    AWS_ROLE_ARN = aws_iam_role.infra.arn
  }, { for k, v in var.infra_variables : "TF_VAR_${k}" => v })
}

resource "github_repository_environment" "deploy" {
  count       = var.manage_github ? 1 : 0
  repository  = local.repository_name
  environment = local.deploy_environment
}

resource "github_repository_environment" "infra" {
  count       = var.manage_github ? 1 : 0
  repository  = local.repository_name
  environment = local.infra_environment
}

resource "github_actions_environment_variable" "deploy" {
  for_each      = var.manage_github ? local.deploy_variables : {}
  repository    = local.repository_name
  environment   = github_repository_environment.deploy[0].environment
  variable_name = each.key
  value         = each.value
}

resource "github_actions_environment_variable" "infra" {
  for_each      = var.manage_github ? local.infra_variables : {}
  repository    = local.repository_name
  environment   = github_repository_environment.infra[0].environment
  variable_name = each.key
  value         = each.value
}

resource "github_actions_environment_secret" "infra" {
  for_each        = var.manage_github ? nonsensitive(toset(keys(var.infra_secrets))) : toset([])
  repository      = local.repository_name
  environment     = github_repository_environment.infra[0].environment
  secret_name     = each.key
  plaintext_value = var.infra_secrets[each.key]
}
