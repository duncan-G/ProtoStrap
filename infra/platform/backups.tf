# Backups (docs/vms.md#backups) come in two layers. Beneath, Data Lifecycle Manager
# snapshots every ProtoStrap volume daily: crash-consistent, up to a day old. On top, the
# agent on each data VM runs every stateful component's backup command daily and streams
# it, compressed, to <component>/<date>.gz in the backups bucket: a logical dump that
# restores into a newer version, or into a managed service.

resource "aws_s3_bucket" "backups" {
  bucket        = "${local.bucket_prefix}-backups"
  force_destroy = var.force_destroy
  tags          = local.tags
}

resource "aws_s3_bucket_public_access_block" "backups" {
  bucket                  = aws_s3_bucket.backups.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "backups" {
  bucket = aws_s3_bucket.backups.id
  rule {
    id     = "expire"
    status = "Enabled"
    filter {}
    expiration {
      days = var.backup_retention_days
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 2
    }
  }
}

# AWS Config writes its security group history here when record_security_groups is on
# (network.tf).
data "aws_iam_policy_document" "backups" {
  statement {
    sid       = "TLSOnly"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [aws_s3_bucket.backups.arn, "${aws_s3_bucket.backups.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }

  dynamic "statement" {
    for_each = var.record_security_groups ? [1] : []
    content {
      sid       = "ConfigCheck"
      actions   = ["s3:GetBucketAcl", "s3:ListBucket"]
      resources = [aws_s3_bucket.backups.arn]
      principals {
        type        = "Service"
        identifiers = ["config.amazonaws.com"]
      }
    }
  }

  dynamic "statement" {
    for_each = var.record_security_groups ? [1] : []
    content {
      sid       = "ConfigWrite"
      actions   = ["s3:PutObject"]
      resources = ["${aws_s3_bucket.backups.arn}/config/AWSLogs/${local.account}/Config/*"]
      principals {
        type        = "Service"
        identifiers = ["config.amazonaws.com"]
      }
    }
  }
}

resource "aws_s3_bucket_policy" "backups" {
  bucket     = aws_s3_bucket.backups.id
  policy     = data.aws_iam_policy_document.backups.json
  depends_on = [aws_s3_bucket_public_access_block.backups]
}

data "aws_iam_policy_document" "snapshots_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["dlm.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "snapshots" {
  name                 = "protostrap-snapshots"
  path                 = local.path
  description          = "Data Lifecycle Manager: daily snapshots of ProtoStrap's volumes."
  assume_role_policy   = data.aws_iam_policy_document.snapshots_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

data "aws_iam_policy_document" "snapshots" {
  statement {
    actions   = ["ec2:CreateSnapshot", "ec2:CreateSnapshots", "ec2:DeleteSnapshot", "ec2:DescribeInstances", "ec2:DescribeVolumes", "ec2:DescribeSnapshots"]
    resources = ["*"]
  }
  statement {
    actions   = ["ec2:CreateTags"]
    resources = ["arn:${local.partition}:ec2:${local.region}::snapshot/*"]
  }
}

resource "aws_iam_role_policy" "snapshots" {
  name   = "snapshots"
  role   = aws_iam_role.snapshots.id
  policy = data.aws_iam_policy_document.snapshots.json
}

resource "aws_dlm_lifecycle_policy" "snapshots" {
  description        = "protostrap ${local.app} daily snapshots"
  execution_role_arn = aws_iam_role.snapshots.arn
  state              = "ENABLED"

  policy_details {
    resource_types = ["VOLUME"]
    target_tags    = { "protostrap:snapshot" = "daily" }

    schedule {
      name      = "daily"
      copy_tags = true
      create_rule {
        interval      = 24
        interval_unit = "HOURS"
        times         = [format("%02d:00", (var.backup_hour + 23) % 24)]
      }
      retain_rule {
        count = var.snapshot_retention
      }
      tags_to_add = { "protostrap:app" = local.app }
    }
  }

  tags = local.tags
}
