# Telemetry (docs/telemetry.md) is ProtoStrap's own replica on data-1: an OpenTelemetry
# collector, Loki, Tempo, Prometheus and Grafana, in the protostrap/telemetry image, which
# runs when the spec has a telemetry section. This file makes what it needs:
#
#   - its EBS volume on data-1, mounted at /data/telemetry like a stateful component's, for
#     Prometheus's metrics and Loki's and Tempo's write-ahead logs;
#   - the telemetry bucket, where Loki and Tempo keep logs and traces, which the scheduler
#     names protostrap-<account>-<region>-telemetry;
#   - its role, which uses that bucket and nothing else;
#   - its repository, which the deploy workflow pushes each version's image to;
#   - the protostrap-telemetry session document, which forwards Grafana's port on data-1's
#     loopback, 7482, apart from the operator UI's (ui.tf): Developers may start it, and never
#     reach the infra log (infra/people).
#
# Docker on every VM sends replicas' output to data-1's address, port 24224, which the node
# security group admits between VMs (network.tf).
#
# Without telemetry, the volume, its port and the session document go: replicas' output
# goes to their log groups instead. The bucket, role and repository stay, as a retired
# component's do: removing telemetry never fails on a bucket with logs in it, and its
# lifecycle rule expires them.

resource "aws_ecr_repository" "telemetry" {
  name                 = "protostrap/telemetry"
  force_delete         = var.force_destroy
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration {
    scan_on_push = true
  }
  tags = local.tags
}

# The volume outlives the VM, and is snapshotted daily with the others (backups.tf). Unlike a
# stateful component's, losing it loses only the metrics' history, so it may be destroyed.
resource "aws_ebs_volume" "telemetry" {
  count             = local.has_telemetry ? 1 : 0
  availability_zone = local.zones[0]
  size              = local.telemetry.volume
  type              = "gp3"
  encrypted         = true
  tags = merge(local.tags, {
    Name                   = "protostrap-telemetry"
    "protostrap:component" = "telemetry"
    "protostrap:mount"     = "/data/telemetry"
    "protostrap:snapshot"  = "daily"
  })
}

resource "aws_volume_attachment" "telemetry" {
  count       = local.has_telemetry ? 1 : 0
  device_name = "/dev/sdy"
  volume_id   = aws_ebs_volume.telemetry[0].id
  instance_id = aws_instance.data["data-1"].id
}

resource "aws_s3_bucket" "telemetry" {
  bucket        = "${local.bucket_prefix}-telemetry"
  force_destroy = var.force_destroy
  tags          = merge(local.tags, { "protostrap:bucket" = "telemetry" })
}

resource "aws_s3_bucket_public_access_block" "telemetry" {
  bucket                  = aws_s3_bucket.telemetry.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "telemetry" {
  bucket = aws_s3_bucket.telemetry.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "telemetry" {
  bucket = aws_s3_bucket.telemetry.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

resource "aws_s3_bucket_policy" "telemetry" {
  bucket = aws_s3_bucket.telemetry.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Sid       = "TLSOnly"
      Effect    = "Deny"
      Principal = "*"
      Action    = "s3:*"
      Resource  = [aws_s3_bucket.telemetry.arn, "${aws_s3_bucket.telemetry.arn}/*"]
      Condition = { Bool = { "aws:SecureTransport" = "false" } }
    }]
  })

  depends_on = [aws_s3_bucket_public_access_block.telemetry]
}

# Loki's and Tempo's compactors delete what is past its retention. The bucket also expires
# every object a week after the longer of the two, in case either misses one.
resource "aws_s3_bucket_lifecycle_configuration" "telemetry" {
  bucket = aws_s3_bucket.telemetry.id

  rule {
    id     = "abandoned-uploads"
    status = "Enabled"
    filter {}
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  rule {
    id     = "expire"
    status = "Enabled"
    filter {}
    expiration {
      days = max(local.telemetry.logs_days, local.telemetry.traces_days) + 7
    }
  }
}

# This is telemetry's own role, which the agent on data-1 takes on for it: Loki and Tempo
# read and write the telemetry bucket, and it reads no secret.
data "aws_iam_policy_document" "telemetry_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.data.arn]
    }
  }
}

resource "aws_iam_role" "telemetry" {
  name                 = "protostrap-c-telemetry"
  path                 = local.path
  description          = "Telemetry: the collector, Loki, Tempo, Prometheus and Grafana."
  assume_role_policy   = data.aws_iam_policy_document.telemetry_trust.json
  permissions_boundary = local.role_boundary
  max_session_duration = 3600
  tags                 = local.tags
}

data "aws_iam_policy_document" "telemetry" {
  statement {
    sid       = "ListTheBucket"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.telemetry.arn]
  }
  statement {
    sid       = "LogsAndTraces"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:GetObjectTagging", "s3:PutObjectTagging"]
    resources = ["${aws_s3_bucket.telemetry.arn}/*"]
  }
}

resource "aws_iam_role_policy" "telemetry" {
  name   = "telemetry"
  role   = aws_iam_role.telemetry.id
  policy = data.aws_iam_policy_document.telemetry.json
}

# Grafana has no sign-in of its own, as the operator UI hasn't: this document forwards its
# port on data-1's loopback, which the scheduler serves from Grafana's socket, and the
# document a person may start is what they see (infra/people). The port is the scheduler's
# default, telemetry_listen, which Terraform doesn't write into scheduler.json.
resource "aws_ssm_document" "telemetry" {
  count           = local.has_telemetry ? 1 : 0
  name            = "protostrap-telemetry"
  document_type   = "Session"
  document_format = "JSON"
  tags            = local.tags
  content = jsonencode({
    schemaVersion = "1.0"
    description   = "Forward a local port to Grafana, ProtoStrap's telemetry, on data-1, port 7482."
    sessionType   = "Port"
    # Only the local port is the caller's to pick; the remote port is fixed.
    parameters = {
      localPortNumber = {
        type           = "String"
        description    = "The port on your machine. An open one is picked if it is 0."
        allowedPattern = "^([0-9]|[1-9][0-9]{1,3}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$"
        default        = "0"
      }
    }
    properties = {
      portNumber      = "7482"
      type            = "LocalPortForwarding"
      localPortNumber = "{{ localPortNumber }}"
    }
  })
}

# The volume and the session document were made unconditionally before the spec could leave
# telemetry out.
moved {
  from = aws_ebs_volume.telemetry
  to   = aws_ebs_volume.telemetry[0]
}

moved {
  from = aws_volume_attachment.telemetry
  to   = aws_volume_attachment.telemetry[0]
}

moved {
  from = aws_ssm_document.telemetry
  to   = aws_ssm_document.telemetry[0]
}
