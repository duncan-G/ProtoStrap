# This file sets up what runs on every VM (README.md#what-runs-on-every-vm): Docker, the
# ECR credential helper, the SSM Agent and the ProtoStrap agent. Every VM boots Amazon
# Linux 2023 and the same user data, which installs Docker and the credential helper from
# Amazon Linux's own repositories, writes the agent's config and starts it once the
# protostrap binary is in the deploy bucket. The agent then does the rest: it completes the
# launch hook, sets up the container firewall, joins the swarm and runs what the scheduler
# assigns.
#
# Each pool renders the template with its own settings (ingress.tf, general.tf,
# compute.tf, data.tf), and gives its VMs a role built on the agent's permissions here.

locals {
  scheduler_ip   = cidrhost(local.subnets[local.zones[0]].data, 10)
  scheduler_addr = "${local.scheduler_ip}:7443"
  launch_hook    = "protostrap-launch"
  terminate_hook = "protostrap-terminate"
  hook_timeout   = 600

  # This is the network the container firewall is built from (internal/firewall).
  agent_network = {
    vpc      = var.vpc_cidr
    resolver = local.resolver
    managed  = [for s in local.subnets : s.managed]
  }

  # These are the settings every pool's user data shares; each pool merges in its own.
  vm_settings = {
    app            = local.app
    region         = local.region
    deploy_bucket  = aws_s3_bucket.deploy.bucket
    backups_bucket = aws_s3_bucket.backups.bucket
    registry       = local.registry
    launch_hook    = local.launch_hook
    log_prefix     = local.log_prefix
    swarm_manager  = false
    data_vm        = false
    scheduler      = false
    agent_config   = ""
  }

  # These are the scripts and units every VM installs; data.tf adds the data VMs' own.
  vm_files = {
    install_script   = file("${path.module}/templates/protostrap-install.sh")
    volumes_script   = ""
    scheduler_config = ""
    units = {
      "protostrap-install.service" = file("${path.module}/templates/protostrap-install.service")
      "protostrap-agent.service"   = file("${path.module}/templates/protostrap-agent.service")
    }
  }

  # This is the agent's config, /etc/protostrap/agent.json (cmd/protostrap/config.go). The
  # scheduler's certificate is fetched from the deploy bucket once data-1 has made it.
  agent_base = {
    app              = local.app
    scheduler        = local.scheduler_addr
    scheduler_ca     = "/etc/protostrap/scheduler-ca.pem"
    scheduler_ca_key = "scheduler/ca.pem"
    deploy_bucket    = aws_s3_bucket.deploy.bucket
    network          = local.agent_network
  }

  # Pool VMs, in Auto Scaling groups, also complete their lifecycle hooks. The launch hook
  # discards a VM whose agent never says it's ready; the terminate hook lets AWS go ahead
  # if the agent is gone.
  agent_hooks = {
    launch_hook            = local.launch_hook
    terminate_hook         = local.terminate_hook
    terminate_hook_timeout = "${local.hook_timeout}s"
  }
  lifecycle_hooks = [
    { name = local.launch_hook, transition = "autoscaling:EC2_INSTANCE_LAUNCHING", default = "ABANDON" },
    { name = local.terminate_hook, transition = "autoscaling:EC2_INSTANCE_TERMINATING", default = "CONTINUE" },
  ]

  vm_tags = merge(local.tags, { "protostrap:managed-by" = "protostrap" })
}

# This policy covers what every VM's agent does with its VM's role: pull images, read its
# binary and the scheduler's certificate, ship replicas' logs (Docker's awslogs driver uses
# the VM's role), take on component roles for its replicas, and complete its lifecycle
# hooks.
data "aws_iam_policy_document" "agent" {
  statement {
    sid       = "PullImages"
    actions   = ["ecr:GetAuthorizationToken"]
    resources = ["*"]
  }
  statement {
    sid       = "PullImagesFromProtoStrap"
    actions   = ["ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer", "ecr:BatchCheckLayerAvailability"]
    resources = ["arn:${local.partition}:ecr:${local.region}:${local.account}:repository/protostrap/*"]
  }
  statement {
    sid       = "ReadTheBinaryAndCertificate"
    actions   = ["s3:GetObject"]
    resources = ["${aws_s3_bucket.deploy.arn}/protostrap/*", "${aws_s3_bucket.deploy.arn}/scheduler/ca.pem"]
  }
  statement {
    sid       = "ShipLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = ["arn:${local.partition}:logs:${local.region}:${local.account}:log-group:${local.log_prefix}*"]
  }
  statement {
    sid       = "TakeOnComponentRoles"
    actions   = ["sts:AssumeRole"]
    resources = ["arn:${local.partition}:iam::${local.account}:role/protostrap/protostrap-c-*"]
  }
  statement {
    sid       = "LifecycleHooks"
    actions   = ["autoscaling:CompleteLifecycleAction", "autoscaling:RecordLifecycleActionHeartbeat"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "autoscaling:ResourceTag/protostrap:app"
      values   = [local.app]
    }
  }
}

data "aws_iam_policy_document" "vm_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}
