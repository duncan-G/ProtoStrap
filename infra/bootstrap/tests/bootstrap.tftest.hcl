# These tests render bootstrap's policies with the real AWS provider, which builds policy
# JSON without calling AWS, so IAM's size limit, what the boundaries allow and the inputs'
# checks are tested offline:
#
#   terraform -chdir=infra/bootstrap init -backend=false && terraform -chdir=infra/bootstrap test
#
# The names are the longest the variables accept, in a region with one of the longest
# names, because each appears in the boundaries' ARNs.

provider "aws" {
  region                      = "ap-southeast-2"
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
}

mock_provider "github" {}
mock_provider "tls" {}

override_data {
  target = data.aws_caller_identity.current
  values = { account_id = "123456789012" }
}

variables {
  app                         = "abcdefghijklmnopqrstuvwxyz0123"
  environment                 = "abcdefghijklmnop"
  github_repository           = "owner/repository"
  create_github_oidc_provider = false
}

# IAM refuses a managed policy over 6,144 characters, not counting whitespace. Each
# boundary leaves room for an Org Admin's extra actions (extra_infra_actions,
# extra_role_actions).
run "boundaries_fit_iam_with_room_to_widen" {
  command = plan

  assert {
    condition     = length(replace(data.aws_iam_policy_document.infra_boundary.json, "/\\s/", "")) <= 6144 - 512
    error_message = "The Infra boundary is ${length(replace(data.aws_iam_policy_document.infra_boundary.json, "/\\s/", ""))} characters without whitespace. IAM allows 6,144, and 512 are kept for extra_infra_actions."
  }

  assert {
    condition     = length(replace(data.aws_iam_policy_document.role_boundary.json, "/\\s/", "")) <= 6144 - 512
    error_message = "The Role boundary is ${length(replace(data.aws_iam_policy_document.role_boundary.json, "/\\s/", ""))} characters without whitespace. IAM allows 6,144, and 512 are kept for extra_role_actions."
  }
}

# The Deploy role's boundary is the Infra boundary, and AWS allows a call only if both
# allow it, so every action in the Deploy role's policy has to match an action the
# boundary allows.
run "the_deploy_role_can_do_what_its_policy_says" {
  command = plan

  variables {
    # extra_infra_actions widens the boundary, so this test checks without it.
    extra_infra_actions = []
  }

  assert {
    condition = length([
      for action in flatten([for s in jsondecode(data.aws_iam_policy_document.deploy.json).Statement : s.Action]) : action
      if !anytrue([
        for pattern in flatten([for s in jsondecode(data.aws_iam_policy_document.infra_boundary.json).Statement : try(s.Action, []) if s.Effect == "Allow"]) :
        can(regex("^${replace(replace(lower(pattern), ".", "\\."), "*", ".*")}$", lower(action)))
      ])
    ]) == 0
    error_message = "The Infra boundary doesn't allow these actions of the Deploy role's policy: ${join(", ", [
      for action in flatten([for s in jsondecode(data.aws_iam_policy_document.deploy.json).Statement : s.Action]) : action
      if !anytrue([
        for pattern in flatten([for s in jsondecode(data.aws_iam_policy_document.infra_boundary.json).Statement : try(s.Action, []) if s.Effect == "Allow"]) :
        can(regex("^${replace(replace(lower(pattern), ".", "\\."), "*", ".*")}$", lower(action)))
      ])
    ])}."
  }
}

# GitHub uppercases variable names and the infra workflow lowercases them, so a name with a
# capital letter could never reach the root's variable.
run "infra_variables_are_lowercase" {
  command = plan

  variables {
    infra_variables = { Cloudflare_Zone = "example.com" }
  }

  expect_failures = [var.infra_variables]
}

# Bootstrap makes the deploy bucket, so the infra workflow can put the protostrap binary in
# it before the platform's first apply makes a VM: every VM boots with its agent. The
# platform adds the lifecycle rules, which follow its settings.
run "the_deploy_bucket" {
  command = plan

  assert {
    condition = (aws_s3_bucket.deploy.bucket == "protostrap-123456789012-ap-southeast-2-deploy" &&
      aws_s3_bucket_versioning.deploy.versioning_configuration[0].status == "Enabled" &&
      one(aws_s3_bucket_server_side_encryption_configuration.deploy.rule).apply_server_side_encryption_by_default[0].sse_algorithm == "AES256" &&
      aws_s3_bucket_ownership_controls.deploy.rule[0].object_ownership == "BucketOwnerEnforced" &&
      aws_s3_bucket_public_access_block.deploy.block_public_acls && aws_s3_bucket_public_access_block.deploy.block_public_policy &&
    aws_s3_bucket_public_access_block.deploy.ignore_public_acls && aws_s3_bucket_public_access_block.deploy.restrict_public_buckets)
    error_message = "The deploy bucket is protostrap-<account>-<region>-deploy: versioned, encrypted, owned by the account and never public."
  }
}
