# This file holds what the scheduler (docs/protostrap.md#scheduler) needs on data-1: what
# it may do with the data VMs' role. It reads releases and keeps its state in the deploy
# bucket, writes the infra log's archive there (docs/protostrap.md#infra-log), reads queue
# depths, adds, removes and replaces pool VMs through their Auto Scaling groups, publishes
# its certificate for the agents, and watches the environment's secret for changed keys.
#
# Its log group is where earlier versions wrote their decisions, before the infra log. It
# stays, with its permission, while a spec may pin such a version, and its records expire.
#
# It finds the groups by their protostrap:app tag, and queues and component roles by name,
# so nothing here changes when the application does.

resource "aws_cloudwatch_log_group" "scheduler" {
  name              = "${local.log_prefix}scheduler"
  retention_in_days = var.log_retention_days
  tags              = local.tags
}

data "aws_iam_policy_document" "scheduler" {
  statement {
    sid       = "ReadReleases"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.deploy.arn}/*"]
  }
  statement {
    sid       = "ListReleases"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.deploy.arn]
  }
  # The scheduler writes its state, its certificate, and protostrap/current, the version
  # new VMs install.
  statement {
    sid     = "WriteState"
    actions = ["s3:PutObject"]
    resources = [
      "${aws_s3_bucket.deploy.arn}/state.json",
      "${aws_s3_bucket.deploy.arn}/scheduler/*",
      "${aws_s3_bucket.deploy.arn}/protostrap/current",
      "${aws_s3_bucket.deploy.arn}/logs/*",
    ]
  }
  statement {
    sid       = "QueueDepth"
    actions   = ["sqs:GetQueueAttributes", "sqs:GetQueueUrl", "sqs:ListQueues"]
    resources = ["arn:${local.partition}:sqs:${local.region}:${local.account}:protostrap-*"]
  }
  statement {
    sid       = "SeePools"
    actions   = ["autoscaling:DescribeAutoScalingGroups", "autoscaling:DescribeScalingActivities", "autoscaling:DescribeAutoScalingInstances", "ec2:DescribeInstances"]
    resources = ["*"]
  }
  statement {
    sid       = "SizePools"
    actions   = ["autoscaling:SetDesiredCapacity", "autoscaling:TerminateInstanceInAutoScalingGroup", "autoscaling:SetInstanceHealth"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "autoscaling:ResourceTag/protostrap:app"
      values   = [local.app]
    }
  }
  statement {
    sid       = "LogDecisions"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = ["${aws_cloudwatch_log_group.scheduler.arn}:*"]
  }
  # It reads the secret's version once a minute, and its value only when the version is
  # new, to hash each component's keys and learn whose changed. It keeps no value.
  statement {
    sid       = "WatchSecret"
    actions   = ["secretsmanager:DescribeSecret", "secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn]
  }
}

resource "aws_iam_role_policy" "scheduler" {
  name   = "scheduler"
  role   = aws_iam_role.data.id
  policy = data.aws_iam_policy_document.scheduler.json
}
