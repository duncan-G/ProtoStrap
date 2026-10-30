# The job queue (README.md#managed) gives each queue in the spec an SQS queue and a
# dead-letter queue, which takes a job after max_receives failed tries. Replicas read a
# queue's URL from QUEUE_<NAME>_URL; the scheduler reads its depth for queue signals.
#
# A queue removed from the spec is kept, with its jobs, while it is in retired_queues.

locals {
  kept_queues = merge(
    { for q in var.retired_queues : q => { fifo = false, visibility = 120, retention = 345600, max_receives = 5 } },
    local.queues,
  )
}

resource "aws_sqs_queue" "dead" {
  for_each                  = local.kept_queues
  name                      = "protostrap-${each.key}-dlq${each.value.fifo ? ".fifo" : ""}"
  fifo_queue                = each.value.fifo
  message_retention_seconds = 1209600
  sqs_managed_sse_enabled   = true
  tags                      = merge(local.tags, { "protostrap:queue" = each.key })
}

resource "aws_sqs_queue" "main" {
  for_each                   = local.kept_queues
  name                       = "protostrap-${each.key}${each.value.fifo ? ".fifo" : ""}"
  fifo_queue                 = each.value.fifo
  visibility_timeout_seconds = each.value.visibility
  message_retention_seconds  = each.value.retention
  receive_wait_time_seconds  = 20
  sqs_managed_sse_enabled    = true
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dead[each.key].arn
    maxReceiveCount     = each.value.max_receives
  })
  tags = merge(local.tags, { "protostrap:queue" = each.key })
}

resource "aws_sqs_queue_redrive_allow_policy" "dead" {
  for_each  = local.kept_queues
  queue_url = aws_sqs_queue.dead[each.key].id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.main[each.key].arn]
  })
}
