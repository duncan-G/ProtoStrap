# This file is the deploy workflow's side of the platform
# (docs/protostrap.md#deploy-workflow): the deploy bucket, which holds every release, the
# protostrap binary of each version, the scheduler's state and certificate, and the
# reporter's reports; and the protostrap-deploy command, which the workflow sends to data-1
# through SSM to start a release.
#
# The Deploy role itself comes from bootstrap.

resource "aws_s3_bucket" "deploy" {
  bucket        = "${local.bucket_prefix}-deploy"
  force_destroy = var.force_destroy
  tags          = local.tags
}

resource "aws_s3_bucket_versioning" "deploy" {
  bucket = aws_s3_bucket.deploy.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_public_access_block" "deploy" {
  bucket                  = aws_s3_bucket.deploy.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "deploy" {
  bucket = aws_s3_bucket.deploy.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "deploy" {
  bucket = aws_s3_bucket.deploy.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "deploy" {
  bucket = aws_s3_bucket.deploy.id
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
