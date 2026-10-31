# This file defines the two identity boundaries (docs/identity.md#identity-boundaries). A
# boundary is the most a role may ever do: AWS allows a call only if the role's policy and
# its boundary both allow it. Only an Org Admin, applying this root, changes them.
#
# - The Infra boundary caps the CI roles. It allows building ProtoStrap: its network and
#   VMs, Auto Scaling, ECR, S3, SQS, SSM documents, CloudWatch, EventBridge and SNS, Data
#   Lifecycle Manager, AWS Config, and IAM roles under /protostrap/, always with the Role
#   boundary. It allows sending SSM commands, for the deploy workflow. It can create
#   secrets but never read or write their values.
# - The Role boundary caps every role Infra creates: VM roles and component roles. It
#   allows running ProtoStrap: pulling images, its buckets and queues, the SSM Agent, Auto
#   Scaling, CloudWatch, reading secrets and taking on component roles. It allows no IAM
#   at all.
#
# Both allow only ProtoStrap's own resources, in this account's region, and are kept
# separate so a role that runs ProtoStrap can never change its network or IAM.

data "aws_iam_policy_document" "infra_boundary" {
  statement {
    sid    = "NetworkVMsAndMonitoring"
    effect = "Allow"
    actions = [
      "ec2:*",
      "autoscaling:*",
      "cloudwatch:*",
      "logs:*",
      "events:*",
      "sns:*",
      "dlm:*",
      "config:*",
      "sts:GetCallerIdentity",
    ]
    resources = ["*"]
  }

  # IAM caps a managed policy at 6,144 characters (tests/bootstrap.tftest.hcl), so action
  # lists here are wildcards, each matching only the actions its comment names. Here
  # *Document is Create, Delete, Describe, Get and Update (sharing a document is
  # ModifyDocumentPermission, which isn't allowed), ListDocument* lists documents, their
  # versions and metadata history, and *Tags* adds, removes and lists tags.
  #
  # The deploy workflow's role has this boundary too. It sends the deploy document to
  # data-1 and follows the command.
  statement {
    sid    = "SSMDocumentsParametersAndCommands"
    effect = "Allow"
    actions = [
      "ssm:*Document",
      "ssm:UpdateDocumentDefaultVersion",
      "ssm:DescribeDocumentPermission",
      "ssm:ListDocument*",
      "ssm:*Tags*",
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:DescribeParameters",
      "ssm:SendCommand",
      "ssm:ListCommand*",
      "ssm:GetCommandInvocation",
      "ssm:DescribeInstanceInformation",
    ]
    resources = ["*"]
  }

  statement {
    sid       = "Registries"
    effect    = "Allow"
    actions   = ["ecr:*"]
    resources = [local.own.repos]
  }

  statement {
    sid       = "RegistryLogin"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken", "ecr:DescribeRegistry"]
    resources = ["*"]
  }

  statement {
    sid       = "Buckets"
    effect    = "Allow"
    actions   = ["s3:*"]
    resources = [local.own.buckets, local.own.objects]
  }

  statement {
    sid       = "ListBuckets"
    effect    = "Allow"
    actions   = ["s3:ListAllMyBuckets", "s3:GetBucketLocation"]
    resources = ["*"]
  }

  statement {
    sid       = "Queues"
    effect    = "Allow"
    actions   = ["sqs:*"]
    resources = [local.own.queues]
  }

  statement {
    sid       = "ListQueues"
    effect    = "Allow"
    actions   = ["sqs:ListQueues"]
    resources = ["*"]
  }

  # The platform verifies the domains the application sends mail from (email.tf). SES names
  # an identity after its domain, so it can't be narrowed to ProtoStrap's own by name.
  # *EmailIdentity is Create, Delete and Get, and SES v1's VerifyEmailIdentity;
  # PutEmailIdentity*Attributes sets an identity's DKIM, DKIM signing, MAIL FROM, feedback
  # and configuration set; *Resource tags, untags and lists tags.
  statement {
    sid       = "EmailIdentities"
    effect    = "Allow"
    actions   = ["ses:*EmailIdentity", "ses:PutEmailIdentity*Attributes", "ses:*Resource"]
    resources = [local.own.email]
  }

  # CI manages only the empty secret shells. Their values are set by an Org Admin. *Secret
  # is Create, Delete, Describe, Restore, Update, Rotate and CancelRotate, and
  # NoSecretValues denies Update and Rotate; *ResourcePolicy is Get, Put, Delete and
  # Validate; *Resource tags and untags.
  statement {
    sid       = "SecretShells"
    effect    = "Allow"
    actions   = ["secretsmanager:*Secret", "secretsmanager:*ResourcePolicy", "secretsmanager:*Resource"]
    resources = [local.own.secrets, local.own.app_secret]
  }

  statement {
    sid       = "ListSecrets"
    effect    = "Allow"
    actions   = ["secretsmanager:ListSecrets"]
    resources = ["*"]
  }

  statement {
    sid       = "ReadIAM"
    effect    = "Allow"
    actions   = ["iam:Get*", "iam:List*"]
    resources = ["*"]
  }

  statement {
    sid    = "ProtoStrapRoles"
    effect = "Allow"
    # *Role is Create, Delete, Get, Pass, Tag, Untag and Update, ListInstanceProfilesForRole,
    # and the service-linked role actions, whose roles live outside /protostrap/.
    # *RolePolicy is Put, Get, Delete, Attach and Detach, and UpdateAssumeRolePolicy.
    actions = [
      "iam:*Role",
      "iam:*RolePolicy",
      "iam:UpdateRoleDescription",
      "iam:PutRolePermissionsBoundary",
    ]
    resources = [local.own.roles]
  }

  # *InstanceProfile is Create, Delete, Get, Tag, Untag, AddRoleTo and RemoveRoleFrom.
  statement {
    sid       = "ProtoStrapInstanceProfiles"
    effect    = "Allow"
    actions   = ["iam:*InstanceProfile"]
    resources = [local.own.profiles]
  }

  statement {
    sid       = "ServiceLinkedRoles"
    effect    = "Allow"
    actions   = ["iam:CreateServiceLinkedRole"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "iam:AWSServiceName"
      values = [
        "autoscaling.amazonaws.com",
        "spot.amazonaws.com",
        "config.amazonaws.com",
        "events.amazonaws.com",
        "ssm.amazonaws.com",
      ]
    }
  }

  # EBS volumes and the rest are encrypted with AWS managed keys, through their services.
  statement {
    sid    = "KeysThroughServices"
    effect = "Allow"
    actions = [
      "kms:CreateGrant",
      "kms:Decrypt",
      "kms:DescribeKey",
      "kms:Encrypt",
      "kms:GenerateDataKey*",
      "kms:ReEncrypt*",
    ]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = local.own.kms_through
    }
  }

  dynamic "statement" {
    for_each = length(var.extra_infra_actions) > 0 ? [1] : []
    content {
      sid       = "WidenedByAnOrgAdmin"
      effect    = "Allow"
      actions   = var.extra_infra_actions
      resources = ["*"]
    }
  }

  # The statements below deny what no CI role may ever do.

  statement {
    sid         = "ThisRegionOnly"
    effect      = "Deny"
    not_actions = ["iam:*", "sts:*", "s3:ListAllMyBuckets", "support:*", "health:*"]
    resources   = ["*"]
    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestedRegion"
      values   = [local.region]
    }
  }

  statement {
    sid       = "NoOrganizationOrBilling"
    effect    = "Deny"
    actions   = ["organizations:*", "account:*", "aws-portal:*", "billing:*", "ce:*", "sso:*", "identitystore:*"]
    resources = ["*"]
  }

  statement {
    sid    = "NoUsersOrKeys"
    effect = "Deny"
    actions = [
      "iam:*User*",
      "iam:*AccessKey*",
      "iam:*LoginProfile*",
      "iam:*Group*",
      "iam:*SSHPublicKey*",
      "iam:*ServiceSpecificCredential*",
    ]
    resources = ["*"]
  }

  statement {
    sid    = "NoChangingBoundaries"
    effect = "Deny"
    actions = [
      "iam:CreatePolicyVersion",
      "iam:DeletePolicy",
      "iam:DeletePolicyVersion",
      "iam:SetDefaultPolicyVersion",
      "iam:TagPolicy",
      "iam:UntagPolicy",
    ]
    resources = local.own.boundaries
  }

  statement {
    sid    = "NoChangingTheCIRoles"
    effect = "Deny"
    actions = [
      "iam:Update*",
      "iam:Put*",
      "iam:Delete*",
      "iam:Attach*",
      "iam:Detach*",
      "iam:Tag*",
      "iam:Untag*",
    ]
    resources = local.own.ci_roles
  }

  statement {
    sid       = "EveryRoleGetsTheRoleBoundary"
    effect    = "Deny"
    actions   = ["iam:CreateRole", "iam:PutRolePermissionsBoundary"]
    resources = ["*"]
    condition {
      test     = "StringNotEquals"
      variable = "iam:PermissionsBoundary"
      values   = [local.role_boundary_arn]
    }
  }

  statement {
    sid       = "NoRemovingBoundaries"
    effect    = "Deny"
    actions   = ["iam:DeleteRolePermissionsBoundary"]
    resources = ["*"]
  }

  # This also denies reading the provider, which no CI role needs.
  statement {
    sid       = "NoChangingGitHubsProvider"
    effect    = "Deny"
    actions   = ["iam:*OpenIDConnectProvider*"]
    resources = ["*"]
  }

  statement {
    sid    = "NoSecretValues"
    effect = "Deny"
    actions = [
      "secretsmanager:GetSecretValue",
      "secretsmanager:BatchGetSecretValue",
      "secretsmanager:PutSecretValue",
      "secretsmanager:UpdateSecret",
      "secretsmanager:RotateSecret",
    ]
    resources = ["*"]
  }

  # The platform's state lives under platform/. The bucket's own settings, and the states
  # an Org Admin keeps beside it, belong to whoever applied bootstrap.
  statement {
    sid       = "NoChangingTheStateBucket"
    effect    = "Deny"
    actions   = ["s3:Put*", "s3:Delete*"]
    resources = [local.own.state]
  }

  statement {
    sid       = "NotTheOrgAdminsStates"
    effect    = "Deny"
    actions   = ["s3:PutObject*", "s3:DeleteObject*", "s3:GetObject*"]
    resources = ["${local.own.state}/bootstrap/*", "${local.own.state}/people/*"]
  }
}

resource "aws_iam_policy" "infra_boundary" {
  name        = "protostrap-infra-boundary"
  path        = local.path
  description = "The most ProtoStrap's CI roles may ever do: build ProtoStrap's own resources."
  policy      = data.aws_iam_policy_document.infra_boundary.json
}

data "aws_iam_policy_document" "role_boundary" {
  statement {
    sid       = "PullImages"
    effect    = "Allow"
    actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability", "ecr:DescribeImages", "ecr:DescribeRepositories"]
    resources = [local.own.repos]
  }

  statement {
    sid       = "RegistryLogin"
    effect    = "Allow"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }

  # Retention is for a locked bucket's objects, written under GOVERNANCE retention, which
  # no role may bypass.
  statement {
    sid    = "Buckets"
    effect = "Allow"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:AbortMultipartUpload",
      "s3:ListMultipartUploadParts",
      "s3:ListBucket",
      "s3:ListBucketMultipartUploads",
      "s3:GetBucketLocation",
      "s3:PutObjectRetention",
      "s3:GetObjectRetention",
    ]
    resources = [local.own.buckets, local.own.objects]
  }

  statement {
    sid    = "Queues"
    effect = "Allow"
    actions = [
      "sqs:SendMessage",
      "sqs:ReceiveMessage",
      "sqs:DeleteMessage",
      "sqs:ChangeMessageVisibility",
      "sqs:GetQueueAttributes",
      "sqs:GetQueueUrl",
      "sqs:ListDeadLetterSourceQueues",
    ]
    resources = [local.own.queues]
  }

  # A component that uses email sends with the SES API and its own role, so no SMTP user or
  # key ever exists. Its policy narrows this to the application's domains.
  statement {
    sid       = "SendEmail"
    effect    = "Allow"
    actions   = ["ses:SendEmail", "ses:SendRawEmail"]
    resources = [local.own.email]
  }

  # This allows the SSM Agent, as in AmazonSSMManagedInstanceCore, so operators can open a
  # shell and the deploy command reaches data-1.
  statement {
    sid    = "SSMAgent"
    effect = "Allow"
    actions = [
      "ssm:DescribeAssociation",
      "ssm:GetDeployablePatchSnapshotForInstance",
      "ssm:GetDocument",
      "ssm:DescribeDocument",
      "ssm:GetManifest",
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:ListAssociations",
      "ssm:ListInstanceAssociations",
      "ssm:PutInventory",
      "ssm:PutComplianceItems",
      "ssm:PutConfigurePackageResult",
      "ssm:UpdateAssociationStatus",
      "ssm:UpdateInstanceAssociationStatus",
      "ssm:UpdateInstanceInformation",
      "ssmmessages:CreateControlChannel",
      "ssmmessages:CreateDataChannel",
      "ssmmessages:OpenControlChannel",
      "ssmmessages:OpenDataChannel",
      "ec2messages:*",
    ]
    resources = ["*"]
  }

  statement {
    sid    = "AutoScaling"
    effect = "Allow"
    actions = [
      "autoscaling:Describe*",
      "autoscaling:SetDesiredCapacity",
      "autoscaling:TerminateInstanceInAutoScalingGroup",
      "autoscaling:SetInstanceHealth",
      "autoscaling:CompleteLifecycleAction",
      "autoscaling:RecordLifecycleActionHeartbeat",
    ]
    resources = ["*"]
  }

  # The ingress VM takes over its zone, data VMs find their volumes, and Data Lifecycle
  # Manager snapshots them.
  statement {
    sid    = "EC2"
    effect = "Allow"
    actions = [
      "ec2:Describe*",
      "ec2:AssociateAddress",
      "ec2:DisassociateAddress",
      "ec2:CreateRoute",
      "ec2:ReplaceRoute",
      "ec2:ModifyInstanceAttribute",
      "ec2:CreateTags",
      "ec2:CreateSnapshot",
      "ec2:CreateSnapshots",
      "ec2:DeleteSnapshot",
    ]
    resources = ["*"]
  }

  statement {
    sid       = "Metrics"
    effect    = "Allow"
    actions   = ["cloudwatch:PutMetricData", "cloudwatch:GetMetricData", "cloudwatch:GetMetricStatistics", "cloudwatch:ListMetrics"]
    resources = ["*"]
  }

  statement {
    sid    = "Logs"
    effect = "Allow"
    actions = [
      "logs:CreateLogGroup",
      "logs:CreateLogStream",
      "logs:PutLogEvents",
      "logs:DescribeLogStreams",
      "logs:GetLogEvents",
      "logs:FilterLogEvents",
      "logs:StartQuery",
      "logs:StopQuery",
      "logs:GetQueryResults",
    ]
    resources = [local.own.logs, "${local.own.logs}:*"]
  }

  statement {
    sid       = "FindLogs"
    effect    = "Allow"
    actions   = ["logs:DescribeLogGroups", "logs:GetQueryResults", "logs:StopQuery"]
    resources = ["*"]
  }

  statement {
    sid       = "ReadSecrets"
    effect    = "Allow"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = [local.own.secrets, local.own.app_secret]
  }

  statement {
    sid       = "TakeOnComponentRoles"
    effect    = "Allow"
    actions   = ["sts:AssumeRole"]
    resources = [local.own.roles]
  }

  statement {
    sid       = "Identity"
    effect    = "Allow"
    actions   = ["sts:GetCallerIdentity"]
    resources = ["*"]
  }

  statement {
    sid       = "KeysThroughServices"
    effect    = "Allow"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey*", "kms:DescribeKey", "kms:CreateGrant"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = local.own.kms_through
    }
  }

  dynamic "statement" {
    for_each = length(var.extra_role_actions) > 0 ? [1] : []
    content {
      sid       = "WidenedByAnOrgAdmin"
      effect    = "Allow"
      actions   = var.extra_role_actions
      resources = ["*"]
    }
  }

  statement {
    sid       = "NoIAM"
    effect    = "Deny"
    actions   = ["iam:*", "organizations:*", "account:*", "sso:*"]
    resources = ["*"]
  }

  statement {
    sid         = "ThisRegionOnly"
    effect      = "Deny"
    not_actions = ["sts:*", "health:*"]
    resources   = ["*"]
    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestedRegion"
      values   = [local.region]
    }
  }

  statement {
    sid       = "NotTheState"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [local.own.state, "${local.own.state}/*"]
  }
}

resource "aws_iam_policy" "role_boundary" {
  name        = "protostrap-role-boundary"
  path        = local.path
  description = "The most a role that runs ProtoStrap may ever do."
  policy      = data.aws_iam_policy_document.role_boundary.json
}
