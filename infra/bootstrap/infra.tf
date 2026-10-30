# This is the GitHub Actions Infra role (docs/identity.md#github-actions-roles). The infra
# workflow takes it on to create and change ProtoStrap's resources. Only runs in the infra
# GitHub environment can take it on, so required reviewers on that environment gate every
# change.
#
# It may do exactly what its boundary allows: the boundary is both its ceiling and its
# policy, so there is one list to review.

data "aws_iam_policy_document" "infra_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [local.github_oidc_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:environment:${local.infra_environment}"]
    }
  }
}

resource "aws_iam_role" "infra" {
  name                 = "protostrap-infra"
  path                 = local.path
  description          = "The infra workflow: creates and changes ProtoStrap's resources."
  assume_role_policy   = data.aws_iam_policy_document.infra_trust.json
  permissions_boundary = aws_iam_policy.infra_boundary.arn
  max_session_duration = 3600
}

resource "aws_iam_role_policy_attachment" "infra" {
  role       = aws_iam_role.infra.name
  policy_arn = aws_iam_policy.infra_boundary.arn
}
