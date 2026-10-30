# These alerts tell operators about what ProtoStrap can't fix by itself, or that someone
# should know happened. They cover AWS Health events, such as maintenance scheduled on a
# data VM, which an operator moves by stopping and starting it (docs/vms.md#recovery); a
# data VM failing its status checks (data.tf); an ingress VM replaced, which pauses its
# zone (docs/network.md#watching-the-network); and pool VMs that fail to launch.

resource "aws_sns_topic" "alerts" {
  name = "protostrap-${local.app}-alerts"
  tags = local.tags
}

resource "aws_sns_topic_subscription" "alerts" {
  for_each  = toset(var.alert_emails)
  topic_arn = aws_sns_topic.alerts.arn
  protocol  = "email"
  endpoint  = each.key
}

data "aws_iam_policy_document" "alerts" {
  statement {
    sid       = "EventsAndAlarms"
    actions   = ["sns:Publish"]
    resources = [aws_sns_topic.alerts.arn]
    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com", "cloudwatch.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
  }
}

resource "aws_sns_topic_policy" "alerts" {
  arn    = aws_sns_topic.alerts.arn
  policy = data.aws_iam_policy_document.alerts.json
}

locals {
  alert_rules = {
    health = {
      description = "AWS Health events for EC2 and EBS, such as maintenance scheduled on a data VM"
      pattern = {
        source      = ["aws.health"]
        detail-type = ["AWS Health Event"]
        detail      = { service = ["EC2", "EBS"] }
      }
    }
    ingress-replaced = {
      description = "An ingress VM was removed: its zone's requests and outbound traffic paused until its replacement took over"
      pattern = {
        source      = ["aws.autoscaling"]
        detail-type = ["EC2 Instance Terminate Successful"]
        detail      = { AutoScalingGroupName = values(local.ingress_groups) }
      }
    }
    launch-failed = {
      description = "A pool VM failed to launch"
      pattern = {
        source      = ["aws.autoscaling"]
        detail-type = ["EC2 Instance Launch Unsuccessful"]
        detail = { AutoScalingGroupName = concat(
          values(local.ingress_groups),
          values(local.general_groups),
          [for g in local.compute_groups : g.name],
        ) }
      }
    }
  }
}

resource "aws_cloudwatch_event_rule" "alerts" {
  for_each      = local.alert_rules
  name          = "protostrap-${local.app}-${each.key}"
  description   = each.value.description
  event_pattern = jsonencode(each.value.pattern)
  tags          = local.tags
}

resource "aws_cloudwatch_event_target" "alerts" {
  for_each = local.alert_rules
  rule     = aws_cloudwatch_event_rule.alerts[each.key].name
  arn      = aws_sns_topic.alerts.arn
}
