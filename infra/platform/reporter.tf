# The reporter (docs/protostrap.md#reporter) runs on data-1: once a day and after every
# release, it reads the last day of logs and has an LLM write what happened and what to
# change, to reports/<date>.md in the deploy bucket. It is read-only: it never changes the
# spec, a replica or a VM.
#
# Its LLM key is Reporter_ANTHROPIC_API_KEY in the environment's secret (secret.tf), which
# an Org Admin sets. Without it, the reporter writes the facts and skips the LLM.

data "aws_iam_policy_document" "reporter" {
  statement {
    sid = "ReadLogs"
    actions = [
      "logs:StartQuery",
      "logs:GetQueryResults",
      "logs:StopQuery",
      "logs:FilterLogEvents",
      "logs:GetLogEvents",
    ]
    resources = [
      "arn:${local.partition}:logs:${local.region}:${local.account}:log-group:${local.log_prefix}*",
      "arn:${local.partition}:logs:${local.region}:${local.account}:log-group:${local.log_prefix}*:*",
    ]
  }
  statement {
    sid       = "FindLogs"
    actions   = ["logs:DescribeLogGroups", "logs:GetQueryResults", "logs:StopQuery"]
    resources = ["*"]
  }
  statement {
    sid       = "TheLLMKey"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn, "${local.old_secret}reporter-??????"]
  }
  statement {
    sid       = "WriteReports"
    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.deploy.arn}/reports/*"]
  }
}

resource "aws_iam_role_policy" "reporter" {
  name   = "reporter"
  role   = aws_iam_role.data.id
  policy = data.aws_iam_policy_document.reporter.json
}
