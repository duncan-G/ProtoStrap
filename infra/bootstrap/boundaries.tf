# This file defines the two identity boundaries (docs/identity.md#identity-boundaries). A
# boundary is the most a role may ever do: AWS allows a call only if the role's policy and
# its boundary both allow it. Only an Org Admin, applying this root, changes them.
#
# - The Infra boundary caps the CI roles. It allows building ProtoStrap: its network and
#   VMs, Auto Scaling, ECR, S3, SQS, SSM documents, CloudWatch, EventBridge and SNS, Data
#   Lifecycle Manager, AWS Config, and IAM roles under /protostrap/, always with the Role
#   boundary. It can create secrets but never read or write their values.
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

  statement {
    sid    = "SSMDocumentsAndParameters"
    effect = "Allow"
    actions = [
      "ssm:CreateDocument",
      "ssm:UpdateDocument",
      "ssm:UpdateDocumentDefaultVersion",
      "ssm:DeleteDocument",
      "ssm:DescribeDocument",
      "ssm:DescribeDocumentPermission",
      "ssm:GetDocument",
      "ssm:ListDocuments",
      "ssm:ListDocumentVersions",
      "ssm:AddTagsToResource",
      "ssm:RemoveTagsFromResource",
      "ssm:ListTagsForResource",
      "ssm:GetParameter",
      "ssm:GetParameters",
      "ssm:DescribeParameters",
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
  statement {
    sid    = "EmailIdentities"
    effect = "Allow"
    actions = [
      "ses:CreateEmailIdentity",
      "ses:DeleteEmailIdentity",
      "ses:GetEmailIdentity",
      "ses:PutEmailIdentityDkimSigningAttributes",
      "ses:PutEmailIdentityMailFromAttributes",
      "ses:TagResource",
      "ses:UntagResource",
      "ses:ListTagsForResource",
    ]
    resources = [local.own.email]
  }

  # CI manages only the empty secret shells. Their values are set by an Org Admin.
  statement {
    sid    = "SecretShells"
    effect = "Allow"
    actions = [
      "secretsmanager:CreateSecret",
      "secretsmanager:DescribeSecret",
      "secretsmanager:DeleteSecret",
      "secretsmanager:RestoreSecret",
      "secretsmanager:TagResource",
      "secretsmanager:UntagResource",
      "secretsmanager:GetResourcePolicy",
      "secretsmanager:PutResourcePolicy",
      "secretsmanager:DeleteResourcePolicy",
    ]
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
    actions = [
      "iam:CreateRole",
      "iam:DeleteRole",
      "iam:UpdateRole",
      "iam:UpdateRoleDescription",
      "iam:UpdateAssumeRolePolicy",
      "iam:TagRole",
      "iam:UntagRole",
      "iam:PutRolePolicy",
      "iam:DeleteRolePolicy",
      "iam:AttachRolePolicy",
      "iam:DetachRolePolicy",
      "iam:PutRolePermissionsBoundary",
      "iam:PassRole",
    ]
    resources = [local.own.roles]
  }

  statement {
    sid    = "ProtoStrapInstanceProfiles"
    effect = "Allow"
    actions = [
      "iam:CreateInstanceProfile",
      "iam:DeleteInstanceProfile",
      "iam:TagInstanceProfile",
      "iam:UntagInstanceProfile",
      "iam:AddRoleToInstanceProfile",
      "iam:RemoveRoleFromInstanceProfile",
    ]
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

  statement {
    sid    = "NoChangingGitHubsProvider"
    effect = "Deny"
    actions = [
      "iam:CreateOpenIDConnectProvider",
      "iam:DeleteOpenIDConnectProvider",
      "iam:UpdateOpenIDConnectProviderThumbprint",
      "iam:AddClientIDToOpenIDConnectProvider",
      "iam:RemoveClientIDFromOpenIDConnectProvider",
      "iam:TagOpenIDConnectProvider",
      "iam:UntagOpenIDConnectProvider",
    ]
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
