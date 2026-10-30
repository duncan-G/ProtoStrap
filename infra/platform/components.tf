# This file creates the application's components: for each one, an ECR repository, a log
# group and a role, and for a stateful one, its own EBS volume on its
# data VM (docs/protostrap.md#infra-workflow).
#
# A component's role (docs/identity.md#component-roles) allows only what it declares: the
# queues in its uses, which it may send to and receive from, the queue its queue signal
# reads, which it may receive from, the buckets in its uses, files or backups if it uses
# them, sending mail from the email domains if it uses email (email.tf), and the
# environment's secret (secret.tf). In a locked bucket it may also set an object's retention, but
# never bypass it, and in a versioned one it can't delete an earlier version.
# The agent on its VM takes the role on for each replica, so components sharing a VM still
# can't use each other's permissions.
#
# A component removed from the spec keeps its repository, log group and role while
# it is in retired_components, so a rollback still finds them.

resource "aws_ecr_repository" "component" {
  for_each             = local.kept_components
  name                 = "protostrap/${each.key}"
  force_delete         = var.force_destroy
  image_tag_mutability = "IMMUTABLE"
  image_scanning_configuration {
    scan_on_push = true
  }
  tags = merge(local.tags, { "protostrap:component" = each.key })
}

# Every release's images stay, so a rollback finds them. Untagged images left by failed
# pushes expire after a day.
resource "aws_ecr_lifecycle_policy" "component" {
  for_each   = aws_ecr_repository.component
  repository = each.value.name
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Untagged images, after a day"
      selection = {
        tagStatus   = "untagged"
        countType   = "sinceImagePushed"
        countUnit   = "days"
        countNumber = 1
      }
      action = { type = "expire" }
    }]
  })
}

resource "aws_cloudwatch_log_group" "component" {
  for_each          = local.kept_components
  name              = "${local.log_prefix}${each.key}"
  retention_in_days = var.log_retention_days
  tags              = merge(local.tags, { "protostrap:component" = each.key })
}

# Only the VM roles of the pools a component can run on may take its role on: general and
# compute VMs (an ephemeral worker runs on general), and data VMs.
data "aws_iam_policy_document" "component_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "AWS"
      identifiers = [aws_iam_role.node.arn, aws_iam_role.data.arn]
    }
  }
}

resource "aws_iam_role" "component" {
  for_each             = local.kept_components
  name                 = "protostrap-c-${each.key}"
  path                 = local.path
  description          = "The ${each.key} component's replicas."
  assume_role_policy   = data.aws_iam_policy_document.component_trust.json
  permissions_boundary = local.role_boundary
  max_session_duration = 3600
  tags                 = merge(local.tags, { "protostrap:component" = each.key })
}

locals {
  component_access = {
    for name in local.kept_components : name => {
      uses    = try(local.components[name].uses, [])
      send    = [for q in try(local.components[name].uses, []) : q if contains(keys(aws_sqs_queue.main), q)]
      receive = [for q in try(local.components[name].queue_signals, []) : q if contains(keys(aws_sqs_queue.main), q) && !contains(try(local.components[name].uses, []), q)]
      buckets = [for b in try(local.components[name].uses, []) : b if contains(keys(local.buckets), b)]
      locked  = [for b in try(local.components[name].uses, []) : b if try(local.buckets[b].lock, false)]
      files   = contains(try(local.components[name].uses, []), "files")
      backups = contains(try(local.components[name].uses, []), "backups")
      email   = contains(try(local.components[name].uses, []), "email") && length(local.email_domains) > 0
    }
  }
}

data "aws_iam_policy_document" "component" {
  for_each = local.component_access

  statement {
    sid       = "EnvironmentSecret"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn, "${local.old_secret}${each.key}-??????"]
  }

  dynamic "statement" {
    for_each = length(each.value.send) > 0 ? [1] : []
    content {
      sid = "SendAndReceive"
      actions = [
        "sqs:SendMessage",
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes",
        "sqs:GetQueueUrl",
      ]
      resources = [for q in each.value.send : aws_sqs_queue.main[q].arn]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.receive) > 0 ? [1] : []
    content {
      sid = "Receive"
      actions = [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes",
        "sqs:GetQueueUrl",
      ]
      resources = [for q in each.value.receive : aws_sqs_queue.main[q].arn]
    }
  }

  dynamic "statement" {
    for_each = each.value.files ? [1] : []
    content {
      sid       = "Files"
      actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
      resources = ["${aws_s3_bucket.files.arn}/*"]
    }
  }

  dynamic "statement" {
    for_each = each.value.files ? [1] : []
    content {
      sid       = "ListFiles"
      actions   = ["s3:ListBucket"]
      resources = [aws_s3_bucket.files.arn]
    }
  }

  # A component that sends mail may send from any address in the email domains, and only
  # there (email.tf).
  dynamic "statement" {
    for_each = each.value.email ? [1] : []
    content {
      sid       = "Email"
      actions   = ["ses:SendEmail", "ses:SendRawEmail"]
      resources = [for d in local.email_domains : aws_sesv2_email_identity.app[d].arn]
      condition {
        test     = "StringLike"
        variable = "ses:FromAddress"
        values   = [for d in local.email_domains : "*@${d}"]
      }
    }
  }

  dynamic "statement" {
    for_each = length(each.value.buckets) > 0 ? [1] : []
    content {
      sid       = "Buckets"
      actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
      resources = [for b in each.value.buckets : "${aws_s3_bucket.app[b].arn}/*"]
    }
  }

  # ListBucket makes a missing object answer 404, not 403.
  dynamic "statement" {
    for_each = length(each.value.buckets) > 0 ? [1] : []
    content {
      sid       = "ListBuckets"
      actions   = ["s3:ListBucket"]
      resources = [for b in each.value.buckets : aws_s3_bucket.app[b].arn]
    }
  }

  dynamic "statement" {
    for_each = length(each.value.locked) > 0 ? [1] : []
    content {
      sid       = "Retention"
      actions   = ["s3:PutObjectRetention", "s3:GetObjectRetention"]
      resources = [for b in each.value.locked : "${aws_s3_bucket.app[b].arn}/*"]
    }
  }

  dynamic "statement" {
    for_each = each.value.backups ? [1] : []
    content {
      sid       = "Backups"
      actions   = ["s3:GetObject", "s3:PutObject", "s3:ListBucket"]
      resources = [aws_s3_bucket.backups.arn, "${aws_s3_bucket.backups.arn}/*"]
    }
  }
}

resource "aws_iam_role_policy" "component" {
  for_each = local.component_access
  name     = "component"
  role     = aws_iam_role.component[each.key].id
  policy   = data.aws_iam_policy_document.component[each.key].json
}

# Stateful components' volumes live in the data VMs' zone, attached to the VM each
# component is pinned to and mounted at /data/<component> (templates/protostrap-volumes.sh).
# They outlive the VM, and are snapshotted daily (backups.tf).
locals {
  volume_devices = ["/dev/sdf", "/dev/sdg", "/dev/sdh", "/dev/sdi", "/dev/sdj", "/dev/sdk", "/dev/sdl", "/dev/sdm", "/dev/sdn", "/dev/sdo", "/dev/sdp"]
  stateful_device = merge([
    for vm in local.data_vms : {
      for i, name in sort([for n, c in local.stateful : n if c.vm == vm]) : name => local.volume_devices[i]
    }
  ]...)
}

resource "aws_ebs_volume" "stateful" {
  for_each          = local.stateful
  availability_zone = local.zones[0]
  size              = each.value.volume.size
  type              = "gp3"
  iops              = each.value.volume.iops
  throughput        = each.value.volume.throughput
  encrypted         = true
  tags = merge(local.tags, {
    Name                   = "protostrap-${each.key}"
    "protostrap:component" = each.key
    "protostrap:mount"     = "/data/${each.key}"
    "protostrap:snapshot"  = "daily"
  })

  # Terraform never deletes a component's data: removing or renaming a stateful component,
  # or anything that would replace its volume, fails the plan instead. Deleting a volume is
  # done by hand, after it is forgotten with `terraform state rm`, as a destroy does.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_volume_attachment" "stateful" {
  for_each    = local.stateful
  device_name = local.stateful_device[each.key]
  volume_id   = aws_ebs_volume.stateful[each.key].id
  instance_id = aws_instance.data[each.value.vm].id
}
