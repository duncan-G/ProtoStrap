# The data pool (docs/vms.md#data) is a fixed set of plain instances, data-1 to
# data-<count>, at fixed addresses in the first zone's data subnet. They run the stateful
# components, each on its own EBS volume (components.tf), and data-1, the one VM never
# removed, also runs the swarm manager, the scheduler and the reporter, with the swarm's
# state and the scheduler's key on its control volume.
#
# Nothing on a data VM's root volume matters: replacing the instance (terraform apply
# -replace, in a maintenance window) keeps its address and reattaches its volumes. So the
# image and user data are read once, and newer ones wait for that replacement.

locals {
  data_ami = startswith(var.ami, "resolve:ssm:") ? data.aws_ssm_parameter.data_ami[0].insecure_value : var.ami
  data_ips = { for vm in local.data_vms : vm => cidrhost(local.subnets[local.zones[0]].data, 9 + tonumber(trimprefix(vm, "data-"))) }
  control  = "/var/lib/protostrap/control"

  # This is the scheduler's config, /etc/protostrap/scheduler.json on data-1. It holds only
  # what never changes: everything else the scheduler finds by name or by tag.
  scheduler_config = jsonencode({
    app           = local.app
    environment   = var.environment
    region        = local.region
    deploy_bucket = data.aws_s3_bucket.deploy.bucket
    files_bucket  = aws_s3_bucket.files.bucket
    registry      = local.registry
    listen        = ":7443"
    address       = local.scheduler_ip
    state_dir     = "${local.control}/scheduler"
    log_group     = aws_cloudwatch_log_group.scheduler.name
  })
}

data "aws_ssm_parameter" "data_ami" {
  count = startswith(var.ami, "resolve:ssm:") ? 1 : 0
  name  = trimprefix(var.ami, "resolve:ssm:")
}

resource "aws_iam_role" "data" {
  name                 = "protostrap-vm-data"
  path                 = local.path
  description          = "Data VMs: the agent, Docker and the SSM Agent, backups, and on data-1 the scheduler and reporter."
  assume_role_policy   = data.aws_iam_policy_document.vm_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "data_ssm" {
  role       = aws_iam_role.data.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "data_agent" {
  name   = "agent"
  role   = aws_iam_role.data.id
  policy = data.aws_iam_policy_document.agent.json
}

# This policy lets the agent read and write backups, and the VM read its volume list.
data "aws_iam_policy_document" "data_vm" {
  statement {
    sid       = "Backups"
    actions   = ["s3:PutObject", "s3:GetObject", "s3:AbortMultipartUpload"]
    resources = ["${aws_s3_bucket.backups.arn}/*"]
  }
  statement {
    sid       = "FindBackups"
    actions   = ["s3:ListBucket"]
    resources = [aws_s3_bucket.backups.arn]
  }
  statement {
    sid       = "ReadVolumeLists"
    actions   = ["s3:GetObject"]
    resources = ["${data.aws_s3_bucket.deploy.arn}/volumes/*"]
  }
}

resource "aws_iam_role_policy" "data_vm" {
  name   = "data-vm"
  role   = aws_iam_role.data.id
  policy = data.aws_iam_policy_document.data_vm.json
}

resource "aws_iam_instance_profile" "data" {
  name = "protostrap-vm-data"
  path = local.path
  role = aws_iam_role.data.name
  tags = local.tags
}

resource "aws_instance" "data" {
  for_each                = toset(local.data_vms)
  ami                     = local.data_ami
  instance_type           = local.data_vm
  subnet_id               = aws_subnet.data[local.zones[0]].id
  private_ip              = local.data_ips[each.key]
  vpc_security_group_ids  = [aws_security_group.node.id]
  iam_instance_profile    = aws_iam_instance_profile.data.name
  disable_api_termination = var.protect_data_vms

  user_data_base64 = base64encode(templatefile("${path.module}/templates/vm.sh.tftpl", merge(local.vm_settings, local.vm_files, {
    pool             = "data"
    asg              = ""
    data_vm          = true
    swarm_manager    = each.key == "data-1"
    scheduler        = each.key == "data-1"
    volumes_script   = file("${path.module}/templates/protostrap-volumes.sh")
    volume_list      = "s3://${data.aws_s3_bucket.deploy.bucket}/${aws_s3_object.volumes[each.key].key}"
    scheduler_config = each.key == "data-1" ? local.scheduler_config : ""
    units = merge(local.vm_files.units, {
      "protostrap-volumes.service" = file("${path.module}/templates/protostrap-volumes.service")
      "protostrap-volumes.timer"   = file("${path.module}/templates/protostrap-volumes.timer")
      }, each.key != "data-1" ? {} : {
      "protostrap-scheduler.service" = file("${path.module}/templates/protostrap-scheduler.service")
      "protostrap-report.service"    = file("${path.module}/templates/protostrap-report.service")
      "protostrap-report.timer"      = file("${path.module}/templates/protostrap-report.timer")
    })
    agent_config = jsonencode(merge(local.agent_base, {
      pool           = "data"
      name           = each.key
      swarm_manager  = each.key == "data-1"
      backups_bucket = aws_s3_bucket.backups.bucket
      backup_hour    = var.backup_hour
    }))
  })))

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  # EC2 restarts the same instance on new hardware, with the same address and volumes.
  maintenance_options {
    auto_recovery = "default"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_gb
    encrypted   = true
  }

  tags = merge(local.vm_tags, {
    Name              = "protostrap-${each.key}"
    "protostrap:pool" = "data"
    "protostrap:vm"   = each.key
    "protostrap:zone" = local.zones[0]
  })

  lifecycle {
    ignore_changes = [ami, user_data, user_data_base64]
  }
}

# This is data-1's control volume, which holds the swarm's state and the scheduler's key so
# that they outlive the instance.
resource "aws_ebs_volume" "control" {
  availability_zone = local.zones[0]
  size              = var.control_volume_gb
  type              = "gp3"
  encrypted         = true
  tags = merge(local.tags, {
    Name                  = "protostrap-control"
    "protostrap:mount"    = local.control
    "protostrap:snapshot" = "daily"
  })
}

resource "aws_volume_attachment" "control" {
  device_name = "/dev/sdz"
  volume_id   = aws_ebs_volume.control.id
  instance_id = aws_instance.data["data-1"].id
}

# Each data VM's volume list says which volume to mount where, one "<volume id> <directory>"
# per line, the directory being the volume's protostrap:mount tag. It lives in the deploy
# bucket, which a data VM reaches through the S3 gateway endpoint: a data VM mounts its
# volumes before its zone has a way out to the internet, which the ingress VM provides only
# once the first deploy has installed its agent. templates/protostrap-volumes.sh reads it
# every minute, so a new component's volume is mounted soon after it is attached.
locals {
  data_volumes = concat(
    [for name, c in local.stateful : { vm = c.vm, id = aws_ebs_volume.stateful[name].id, dir = aws_ebs_volume.stateful[name].tags["protostrap:mount"] }],
    [{ vm = "data-1", id = aws_ebs_volume.control.id, dir = aws_ebs_volume.control.tags["protostrap:mount"] }],
    [for v in aws_ebs_volume.telemetry : { vm = "data-1", id = v.id, dir = v.tags["protostrap:mount"] }],
  )
}

resource "aws_s3_object" "volumes" {
  for_each     = toset(local.data_vms)
  bucket       = data.aws_s3_bucket.deploy.bucket
  key          = "volumes/${each.key}"
  content      = join("", sort([for v in local.data_volumes : "${v.id} ${v.dir}\n" if v.vm == each.key]))
  content_type = "text/plain"
  tags         = local.tags
}

# These alarms handle recovery (docs/vms.md#recovery): EC2 recovers the instance when its
# hardware fails, and an alarm reboots it when its OS hangs.
resource "aws_cloudwatch_metric_alarm" "data_hardware" {
  for_each            = aws_instance.data
  alarm_name          = "protostrap-${each.key}-hardware"
  alarm_description   = "${each.key}'s hardware failed its status check: EC2 recovers it on new hardware."
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed_System"
  dimensions          = { InstanceId = each.value.id }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 2
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  alarm_actions       = ["arn:${local.partition}:automate:${local.region}:ec2:recover", aws_sns_topic.alerts.arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "data_os" {
  for_each            = aws_instance.data
  alarm_name          = "protostrap-${each.key}-os"
  alarm_description   = "${each.key}'s OS failed its status check: the alarm reboots it."
  namespace           = "AWS/EC2"
  metric_name         = "StatusCheckFailed_Instance"
  dimensions          = { InstanceId = each.value.id }
  statistic           = "Maximum"
  period              = 60
  evaluation_periods  = 3
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  alarm_actions       = ["arn:${local.partition}:automate:${local.region}:ec2:reboot", aws_sns_topic.alerts.arn]
  tags                = local.tags
}
