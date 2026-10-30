# This root manages people (docs/identity.md#people): who signs in, and what they may do.
# People sign in through IAM Identity Center, with MFA, for credentials that last the
# session. There are no IAM users and no access keys. Each group has one permission set per
# account, with its policies, session length and accounts:
#
#   Org-Admins       OrgAdmin       AdministratorAccess                          4 hours   every account
#   Platform-Admins  PlatformAdmin  read-only view, operator UI, Grafana         12 hours  every account
#   Developers       Developer      read-only view, development secrets, Grafana 12 hours  developer accounts
#
# Org Admins set the platform's secret values, change boundaries (by applying bootstrap)
# and handle anything else. Platform Admins watch the platform: they see every account and
# open the operator UI and Grafana, and change nothing. Developers run services locally,
# which read development secrets, protostrap-dev/<component>, with the developer's sign-in;
# their code reaches the platform only through GitHub Actions.
#
# The operator UI (docs/protostrap.md#operator-ui) and Grafana (docs/telemetry.md) are each
# reached with an SSM port forward to data-1, and the session document a person may start is
# what they see. Infra telemetry is for admins, service telemetry for Developers too:
#
#   protostrap-ui         the operator UI's read-only port, 7480   Platform Admins, Org Admins
#   protostrap-ui-admin   the port of Org Admins' actions, 7481    Org Admins
#   protostrap-telemetry  Grafana, the services' telemetry, 7482   Developers, Platform Admins, Org Admins
#
# Org Admins may start any document.
#
# An Org Admin applies this once, from the organization's management account (or Identity
# Center's delegated administrator), after enabling Identity Center in the console. People
# are added to the groups in Identity Center, not here.

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
  }
}

variable "accounts" {
  description = "The AWS accounts ProtoStrap runs in, by environment, such as { dev = \"111111111111\", prod = \"222222222222\" }."
  type        = map(string)
}

variable "developer_environments" {
  description = "The environments Developers can sign in to."
  type        = list(string)
  default     = ["dev"]

  validation {
    condition     = alltrue([for env in var.developer_environments : contains(keys(var.accounts), env)])
    error_message = "Each of developer_environments must be an environment in accounts."
  }
}

variable "groups_from_identity_provider" {
  description = "The groups come from an external identity provider over SCIM: reference them by name instead of creating them."
  type        = bool
  default     = false
}

data "aws_ssoadmin_instances" "this" {
  lifecycle {
    postcondition {
      condition     = length(self.arns) > 0
      error_message = "IAM Identity Center isn't enabled in this region. Enable it once in the console (organization instance), then apply again."
    }
  }
}

locals {
  instance          = tolist(data.aws_ssoadmin_instances.this.arns)[0]
  identity_store_id = tolist(data.aws_ssoadmin_instances.this.identity_store_ids)[0]
  groups            = { org_admins = "Org-Admins", platform_admins = "Platform-Admins", developers = "Developers" }
  group_ids = var.groups_from_identity_provider ? {
    for key, g in data.aws_identitystore_group.existing : key => g.group_id
    } : {
    for key, g in aws_identitystore_group.group : key => g.group_id
  }

  # A group has one permission set in an account.
  assignments = merge(
    { for env, account in var.accounts : "org_admins/${env}" => { group = "org_admins", set = aws_ssoadmin_permission_set.org_admin.arn, account = account } },
    { for env, account in var.accounts : "platform_admins/${env}" => { group = "platform_admins", set = aws_ssoadmin_permission_set.platform_admin.arn, account = account } },
    { for env in var.developer_environments : "developers/${env}" => { group = "developers", set = aws_ssoadmin_permission_set.developer.arn, account = var.accounts[env] } },
  )
}

resource "aws_identitystore_group" "group" {
  for_each          = var.groups_from_identity_provider ? {} : local.groups
  identity_store_id = local.identity_store_id
  display_name      = each.value
  description       = "ProtoStrap ${each.value}"
}

data "aws_identitystore_group" "existing" {
  for_each          = var.groups_from_identity_provider ? local.groups : {}
  identity_store_id = local.identity_store_id
  alternate_identifier {
    unique_attribute {
      attribute_path  = "DisplayName"
      attribute_value = each.value
    }
  }
}

resource "aws_ssoadmin_permission_set" "org_admin" {
  name             = "OrgAdmin"
  description      = "Everything: secrets, boundaries, and anything else."
  instance_arn     = local.instance
  session_duration = "PT4H"
}

resource "aws_ssoadmin_managed_policy_attachment" "org_admin" {
  instance_arn       = local.instance
  permission_set_arn = aws_ssoadmin_permission_set.org_admin.arn
  managed_policy_arn = "arn:aws:iam::aws:policy/AdministratorAccess"
}

resource "aws_ssoadmin_permission_set" "platform_admin" {
  name             = "PlatformAdmin"
  description      = "Read-only view, and the ProtoStrap operator UI and Grafana, read-only, through SSM port forwards to data-1."
  instance_arn     = local.instance
  session_duration = "PT12H"
}

resource "aws_ssoadmin_managed_policy_attachment" "platform_admin" {
  instance_arn       = local.instance
  permission_set_arn = aws_ssoadmin_permission_set.platform_admin.arn
  managed_policy_arn = "arn:aws:iam::aws:policy/job-function/ViewOnlyAccess"
}

# These open the operator UI and Grafana: port forwards to data-1, through the protostrap-ui
# and protostrap-telemetry session documents only. SessionDocumentAccessCheck has SSM check
# the document, so no default shell session gets through, and a session's ID tag lets a
# person end only their own.
resource "aws_ssoadmin_permission_set_inline_policy" "platform_admin" {
  instance_arn       = local.instance
  permission_set_arn = aws_ssoadmin_permission_set.platform_admin.arn
  inline_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid       = "OperatorUIOnDataOne"
        Effect    = "Allow"
        Action    = ["ssm:StartSession"]
        Resource  = ["arn:aws:ec2:*:*:instance/*"]
        Condition = { StringEquals = { "ssm:resourceTag/protostrap:vm" = "data-1" }, BoolIfExists = { "ssm:SessionDocumentAccessCheck" = "true" } }
      },
      {
        Sid       = "OperatorUIDocument"
        Effect    = "Allow"
        Action    = ["ssm:StartSession"]
        Resource  = ["arn:aws:ssm:*:*:document/protostrap-ui", "arn:aws:ssm:*:*:document/protostrap-telemetry"]
        Condition = { BoolIfExists = { "ssm:SessionDocumentAccessCheck" = "true" } }
      },
      {
        Sid       = "EndOwnSessions"
        Effect    = "Allow"
        Action    = ["ssm:TerminateSession", "ssm:ResumeSession"]
        Resource  = ["arn:aws:ssm:*:*:session/*"]
        Condition = { StringLike = { "ssm:resourceTag/aws:ssmmessages:session-id" = ["$${aws:userid}"] } }
      },
      {
        # scripts/operator-ui.sh finds data-1 by its tags.
        Sid      = "FindDataOne"
        Effect   = "Allow"
        Action   = ["ec2:DescribeInstances"]
        Resource = ["*"]
      },
    ]
  })
}

resource "aws_ssoadmin_permission_set" "developer" {
  name             = "Developer"
  description      = "Read-only view, the development secrets services read when run locally, and Grafana through an SSM port forward to data-1."
  instance_arn     = local.instance
  session_duration = "PT12H"
}

resource "aws_ssoadmin_managed_policy_attachment" "developer" {
  instance_arn       = local.instance
  permission_set_arn = aws_ssoadmin_permission_set.developer.arn
  managed_policy_arn = "arn:aws:iam::aws:policy/job-function/ViewOnlyAccess"
}

resource "aws_ssoadmin_permission_set_inline_policy" "developer" {
  instance_arn       = local.instance
  permission_set_arn = aws_ssoadmin_permission_set.developer.arn
  inline_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid    = "DevelopmentSecrets"
        Effect = "Allow"
        Action = [
          "secretsmanager:CreateSecret",
          "secretsmanager:DescribeSecret",
          "secretsmanager:GetSecretValue",
          "secretsmanager:PutSecretValue",
          "secretsmanager:TagResource",
        ]
        Resource = "arn:aws:secretsmanager:*:*:secret:protostrap-dev/*"
      },
      {
        Sid      = "NotThePlatformsSecrets"
        Effect   = "Deny"
        Action   = ["secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"]
        Resource = "arn:aws:secretsmanager:*:*:secret:protostrap/*"
      },
      {
        # The environment's secret, <app>/<environment>, is named for the app, which this
        # root doesn't know, but it carries the platform's tag.
        Sid       = "NotTheEnvironmentsSecret"
        Effect    = "Deny"
        Action    = ["secretsmanager:GetSecretValue", "secretsmanager:PutSecretValue"]
        Resource  = "*"
        Condition = { Null = { "aws:ResourceTag/protostrap:app" = "false" } }
      },
      # These open Grafana, the services' logs, traces and metrics, through the
      # protostrap-telemetry session document only: not the operator UI's, whose infra log is
      # for admins (docs/telemetry.md#two-kinds-of-telemetry).
      {
        Sid       = "GrafanaOnDataOne"
        Effect    = "Allow"
        Action    = ["ssm:StartSession"]
        Resource  = ["arn:aws:ec2:*:*:instance/*"]
        Condition = { StringEquals = { "ssm:resourceTag/protostrap:vm" = "data-1" }, BoolIfExists = { "ssm:SessionDocumentAccessCheck" = "true" } }
      },
      {
        Sid       = "GrafanaDocument"
        Effect    = "Allow"
        Action    = ["ssm:StartSession"]
        Resource  = ["arn:aws:ssm:*:*:document/protostrap-telemetry"]
        Condition = { BoolIfExists = { "ssm:SessionDocumentAccessCheck" = "true" } }
      },
      {
        Sid       = "EndOwnSessions"
        Effect    = "Allow"
        Action    = ["ssm:TerminateSession", "ssm:ResumeSession"]
        Resource  = ["arn:aws:ssm:*:*:session/*"]
        Condition = { StringLike = { "ssm:resourceTag/aws:ssmmessages:session-id" = ["$${aws:userid}"] } }
      },
      {
        # scripts/telemetry.sh finds data-1 by its tags.
        Sid      = "FindDataOne"
        Effect   = "Allow"
        Action   = ["ec2:DescribeInstances"]
        Resource = ["*"]
      },
    ]
  })
}

resource "aws_ssoadmin_account_assignment" "this" {
  for_each           = local.assignments
  instance_arn       = local.instance
  permission_set_arn = each.value.set
  principal_type     = "GROUP"
  principal_id       = local.group_ids[each.value.group]
  target_type        = "AWS_ACCOUNT"
  target_id          = each.value.account
}

output "permission_sets" {
  value = {
    OrgAdmin      = aws_ssoadmin_permission_set.org_admin.arn
    PlatformAdmin = aws_ssoadmin_permission_set.platform_admin.arn
    Developer     = aws_ssoadmin_permission_set.developer.arn
  }
}

output "groups" {
  value = local.group_ids
}
