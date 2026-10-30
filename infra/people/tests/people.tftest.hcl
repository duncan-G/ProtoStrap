# This test applies the people root to a mocked provider, to check who gets which
# permission set where, and who may open the operator UI:
#
#   terraform -chdir=infra/people init -backend=false && terraform -chdir=infra/people test

mock_provider "aws" {
  mock_data "aws_ssoadmin_instances" {
    defaults = {
      arns               = ["arn:aws:sso:::instance/ssoins-0123456789abcdef"]
      identity_store_ids = ["d-0123456789"]
    }
  }
  mock_resource "aws_identitystore_group" {
    defaults = { group_id = "90677a4d2e-1a2b3c4d-1111-2222-3333-444455556666" }
  }
}

override_resource {
  target = aws_ssoadmin_permission_set.org_admin
  values = { arn = "arn:aws:sso:::permissionSet/ssoins-0123456789abcdef/ps-orgadmin" }
}

override_resource {
  target = aws_ssoadmin_permission_set.platform_admin
  values = { arn = "arn:aws:sso:::permissionSet/ssoins-0123456789abcdef/ps-platformadmin" }
}

override_resource {
  target = aws_ssoadmin_permission_set.developer
  values = { arn = "arn:aws:sso:::permissionSet/ssoins-0123456789abcdef/ps-developer" }
}

variables {
  accounts = { dev = "111111111111", staging = "333333333333", prod = "222222222222" }
}

run "who_signs_in_where" {
  command = apply

  assert {
    condition = alltrue([for env in ["dev", "staging", "prod"] :
    aws_ssoadmin_account_assignment.this["org_admins/${env}"].permission_set_arn == aws_ssoadmin_permission_set.org_admin.arn])
    error_message = "Org Admins are OrgAdmin everywhere."
  }

  assert {
    condition = (alltrue([for env in ["dev", "staging", "prod"] :
      aws_ssoadmin_account_assignment.this["platform_admins/${env}"].permission_set_arn == aws_ssoadmin_permission_set.platform_admin.arn]) &&
    aws_ssoadmin_account_assignment.this["platform_admins/prod"].target_id == "222222222222")
    error_message = "Platform Admins are PlatformAdmin everywhere, prod included."
  }

  assert {
    condition = (sort([for k, _ in aws_ssoadmin_account_assignment.this : k if startswith(k, "developers/")]) == tolist(["developers/dev"]) &&
    aws_ssoadmin_account_assignment.this["developers/dev"].permission_set_arn == aws_ssoadmin_permission_set.developer.arn)
    error_message = "Developers sign in only where they develop, as Developer: they have nothing in staging or prod, the operator UI included."
  }

  assert {
    condition     = sort(keys(output.permission_sets)) == tolist(["Developer", "OrgAdmin", "PlatformAdmin"])
    error_message = "Three permission sets: OperatorUI, the Developers' UI-only set, is gone."
  }

  assert {
    condition = (sort([for g in aws_identitystore_group.group : g.display_name]) == tolist(["Developers", "Org-Admins", "Platform-Admins"]) &&
    sort(keys(output.groups)) == tolist(["developers", "org_admins", "platform_admins"]))
    error_message = "Three groups, Platform-Admins among them."
  }

  assert {
    condition = (aws_ssoadmin_permission_set.platform_admin.name == "PlatformAdmin" &&
      aws_ssoadmin_permission_set.platform_admin.session_duration == "PT12H" &&
    output.permission_sets.PlatformAdmin == aws_ssoadmin_permission_set.platform_admin.arn)
    error_message = "PlatformAdmin lasts a working day: it views and opens the UI, and changes nothing."
  }
}

run "platform_admins_open_the_operator_ui" {
  command = apply

  assert {
    condition = (aws_ssoadmin_managed_policy_attachment.platform_admin.managed_policy_arn == "arn:aws:iam::aws:policy/job-function/ViewOnlyAccess" &&
    aws_ssoadmin_managed_policy_attachment.platform_admin.permission_set_arn == aws_ssoadmin_permission_set.platform_admin.arn)
    error_message = "Platform Admins have the read-only view of every account."
  }

  # A session on data-1 only, through the read-only document only, and ending only their
  # own sessions.
  assert {
    condition = (contains(flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Resource if s.Sid == "OperatorUIDocument"]), "arn:aws:ssm:*:*:document/protostrap-ui") &&
      [for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Condition.StringEquals["ssm:resourceTag/protostrap:vm"] if s.Sid == "OperatorUIOnDataOne"] == ["data-1"] &&
      alltrue([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Condition.BoolIfExists["ssm:SessionDocumentAccessCheck"] == "true" if contains(s.Action, "ssm:StartSession")]) &&
    length([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s if s.Sid == "EndOwnSessions"]) == 1)
    error_message = "PlatformAdmin opens the UI on data-1 through protostrap-ui, and SSM checks the document, so no shell session gets through."
  }

  assert {
    condition = alltrue([for r in flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Resource if contains(s.Action, "ssm:StartSession")]) :
    contains(["arn:aws:ec2:*:*:instance/*", "arn:aws:ssm:*:*:document/protostrap-ui", "arn:aws:ssm:*:*:document/protostrap-telemetry"], r)])
    error_message = "Platform Admins can't start the Org Admins' document, protostrap-ui-admin, a generic port forward or a shell."
  }

  assert {
    condition     = contains(flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Resource if s.Sid == "OperatorUIDocument"]), "arn:aws:ssm:*:*:document/protostrap-telemetry")
    error_message = "Platform Admins open Grafana too: they see service telemetry as well as infra telemetry."
  }

  assert {
    condition = (sort(flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.platform_admin.inline_policy).Statement : s.Action if s.Effect == "Allow"])) ==
    sort(["ssm:StartSession", "ssm:StartSession", "ssm:TerminateSession", "ssm:ResumeSession", "ec2:DescribeInstances"]))
    error_message = "Beyond the read-only view, PlatformAdmin can find data-1 and forward the UI and Grafana, and nothing else."
  }
}

# Service telemetry is for Developers, infra telemetry for admins only
# (docs/telemetry.md#two-kinds-of-telemetry): Developers forward Grafana's port, never the
# operator UI's.
run "developers_open_grafana_and_not_the_operator_ui" {
  command = apply

  variables {
    developer_environments = ["dev", "staging"]
  }

  assert {
    condition     = sort([for k, _ in aws_ssoadmin_account_assignment.this : k if startswith(k, "developers/")]) == tolist(["developers/dev", "developers/staging"])
    error_message = "Developers sign in to each environment they develop in, and no other."
  }

  assert {
    condition = alltrue([for r in flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s.Resource if contains(s.Action, "ssm:StartSession")]) :
    contains(["arn:aws:ec2:*:*:instance/*", "arn:aws:ssm:*:*:document/protostrap-telemetry"], r)])
    error_message = "Developers start only protostrap-telemetry: never the operator UI's documents, a generic port forward or a shell."
  }

  assert {
    condition = (contains(flatten([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s.Resource if s.Sid == "GrafanaDocument"]), "arn:aws:ssm:*:*:document/protostrap-telemetry") &&
      [for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s.Condition.StringEquals["ssm:resourceTag/protostrap:vm"] if s.Sid == "GrafanaOnDataOne"] == ["data-1"] &&
      alltrue([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s.Condition.BoolIfExists["ssm:SessionDocumentAccessCheck"] == "true" if contains(s.Action, "ssm:StartSession")]) &&
    length([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s if s.Sid == "EndOwnSessions"]) == 1)
    error_message = "Developers open Grafana on data-1 through protostrap-telemetry, and SSM checks the document, so no shell session gets through."
  }

  assert {
    condition = (sort([for s in jsondecode(aws_ssoadmin_permission_set_inline_policy.developer.inline_policy).Statement : s.Sid]) ==
    tolist(["DevelopmentSecrets", "EndOwnSessions", "FindDataOne", "GrafanaDocument", "GrafanaOnDataOne", "NotTheEnvironmentsSecret", "NotThePlatformsSecrets"]))
    error_message = "Developer manages development secrets, can't read the platform's, and opens Grafana."
  }
}

run "an_unknown_environment" {
  command = plan

  variables {
    developer_environments = ["qa"]
  }

  expect_failures = [var.developer_environments]
}
