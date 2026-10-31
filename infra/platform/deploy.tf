# This file is the deploy workflow's side of the platform
# (docs/protostrap.md#deploy-workflow): the deploy bucket's lifecycle rules, and the
# protostrap-deploy command, which the workflow sends to data-1 through SSM to start a
# release.
#
# The deploy bucket, which holds every release, the protostrap binary of each version, the
# scheduler's state and certificate, and the reporter's reports, comes from bootstrap, with
# the Deploy role. Bootstrap makes it so the infra workflow can put the protostrap binary in
# it before this root makes a VM: every VM boots with its agent.

data "aws_s3_bucket" "deploy" {
  bucket = "${local.bucket_prefix}-deploy"
}

resource "aws_s3_bucket_lifecycle_configuration" "deploy" {
  bucket = data.aws_s3_bucket.deploy.bucket
  rule {
    id     = "old-versions"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
  rule {
    id     = "reports"
    status = "Enabled"
    filter {
      prefix = "reports/"
    }
    expiration {
      days = 90
    }
  }
  # The infra log's archive (docs/protostrap.md#infra-log): the scheduler rewrites each
  # hour's object every minute, so its old versions go after a day.
  rule {
    id     = "infra-logs"
    status = "Enabled"
    filter {
      prefix = "logs/"
    }
    expiration {
      days = var.log_retention_days
    }
    noncurrent_version_expiration {
      noncurrent_days = 1
    }
  }
}

resource "aws_cloudwatch_log_group" "deploy" {
  name              = "${local.log_prefix}deploy"
  retention_in_days = var.log_retention_days
  tags              = local.tags
}

resource "aws_ssm_document" "deploy" {
  name            = "protostrap-deploy"
  document_type   = "Command"
  document_format = "JSON"
  tags            = local.tags
  content = jsonencode({
    schemaVersion = "2.2"
    description   = "Deploy a ProtoStrap release: protostrap deploy <commit>, on data-1."
    parameters = {
      commit = {
        type           = "String"
        description    = "The commit of the release, releases/<commit>.json in the deploy bucket."
        allowedPattern = "^[0-9a-f]{7,40}$"
      }
    }
    mainSteps = [{
      action = "aws:runShellScript"
      name   = "deploy"
      inputs = {
        timeoutSeconds = "5400"
        runCommand     = ["/usr/local/bin/protostrap deploy --config /etc/protostrap/scheduler.json {{ commit }}"]
      }
    }]
  })
}

# The SSM Agent on data-1 streams the command's output to the deploy log group, where the
# workflow reads it.
data "aws_iam_policy_document" "deploy_command" {
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = ["${aws_cloudwatch_log_group.deploy.arn}:*"]
  }
  statement {
    actions   = ["logs:DescribeLogGroups"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "deploy_command" {
  name   = "deploy-command"
  role   = aws_iam_role.data.id
  policy = data.aws_iam_policy_document.deploy_command.json
}
