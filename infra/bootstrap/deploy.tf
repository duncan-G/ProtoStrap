# This is the GitHub Actions Deploy role (docs/identity.md#github-actions-roles). The
# deploy workflow takes it on to push images, write the release and tell the scheduler to
# deploy it through SSM. Only runs in the deploy GitHub environment can take it on.
#
# Its boundary is the Infra boundary, the most any CI role may do; its own policy is far
# narrower.

data "aws_iam_policy_document" "deploy_trust" {
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
      values   = ["repo:${var.github_repository}:environment:${local.deploy_environment}"]
    }
  }
}

resource "aws_iam_role" "deploy" {
  name                 = "protostrap-deploy"
  path                 = local.path
  description          = "The deploy workflow: pushes images, writes releases and sends the deploy command."
  assume_role_policy   = data.aws_iam_policy_document.deploy_trust.json
  permissions_boundary = aws_iam_policy.infra_boundary.arn
  # The release job waits for the rollout, which may take up to 80 minutes.
  max_session_duration = 7200
}

data "aws_iam_policy_document" "deploy" {
  statement {
    sid       = "RegistryLogin"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  # The workflow pushes built images, the edge image and copies of stateful components'
  # upstream images.
  statement {
    sid    = "PushImages"
    effect = "Allow"
    actions = [
      "ecr:BatchCheckLayerAvailability",
      "ecr:BatchGetImage",
      "ecr:CompleteLayerUpload",
      "ecr:DescribeImages",
      "ecr:DescribeRepositories",
      "ecr:GetDownloadUrlForLayer",
      "ecr:InitiateLayerUpload",
      "ecr:ListImages",
      "ecr:PutImage",
      "ecr:UploadLayerPart",
    ]
    resources = [local.own.repos]
  }

  statement {
    sid       = "ReadTheDeployBucket"
    effect    = "Allow"
    actions   = ["s3:ListBucket", "s3:GetObject"]
    resources = [local.own.deploy, "${local.own.deploy}/*"]
  }

  # The workflow writes releases, and the protostrap binary of each version.
  statement {
    sid       = "WriteReleases"
    effect    = "Allow"
    actions   = ["s3:PutObject"]
    resources = ["${local.own.deploy}/releases/*", "${local.own.deploy}/protostrap/*"]
  }

  # The workflow may send `protostrap-deploy <commit>` to data-1 only.
  statement {
    sid       = "SendTheDeployCommand"
    effect    = "Allow"
    actions   = ["ssm:SendCommand"]
    resources = [local.own.deploy_doc]
  }

  statement {
    sid       = "ToDataOne"
    effect    = "Allow"
    actions   = ["ssm:SendCommand"]
    resources = [local.own.instances]
    condition {
      test     = "StringEquals"
      variable = "ssm:resourceTag/protostrap:vm"
      values   = ["data-1"]
    }
  }

  statement {
    sid    = "FollowTheCommand"
    effect = "Allow"
    actions = [
      "ssm:GetCommandInvocation",
      "ssm:ListCommands",
      "ssm:ListCommandInvocations",
      "ssm:DescribeInstanceInformation",
      "ec2:DescribeInstances",
      "sts:GetCallerIdentity",
    ]
    resources = ["*"]
  }

  statement {
    sid       = "ReadTheCommandsOutput"
    effect    = "Allow"
    actions   = ["logs:GetLogEvents", "logs:FilterLogEvents", "logs:DescribeLogStreams"]
    resources = [local.own.logs, "${local.own.logs}:*"]
  }
}

resource "aws_iam_role_policy" "deploy" {
  name   = "deploy"
  role   = aws_iam_role.deploy.id
  policy = data.aws_iam_policy_document.deploy.json
}
