# The ingress pool (docs/vms.md#ingress) has, in each zone, an Auto Scaling group of one VM
# that runs only the edge and forwards the zone's outbound traffic. The zone's Elastic IP
# and the private subnets' route out belong to the zone, not the VM: a new ingress VM takes
# both over when it boots (internal/ingress), so nothing else changes when one is replaced.
#
# The scheduler never sizes this pool. It only replaces an ingress VM, starting the new one
# before the old one goes, which is why each group's maximum is two.

resource "aws_eip" "ingress" {
  for_each = toset(local.zones)
  domain   = "vpc"
  tags     = merge(local.tags, { Name = "protostrap-ingress-${each.key}", "protostrap:zone" = each.key })
}

locals {
  ingress_groups = { for zone in local.zones : zone => "protostrap-ingress-${zone}" }
}

resource "aws_iam_role" "ingress" {
  name                 = "protostrap-vm-ingress"
  path                 = local.path
  description          = "Ingress VMs: the agent, Docker and the SSM Agent, and taking over the zone."
  assume_role_policy   = data.aws_iam_policy_document.vm_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "ingress_ssm" {
  role       = aws_iam_role.ingress.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "ingress_agent" {
  name   = "agent"
  role   = aws_iam_role.ingress.id
  policy = data.aws_iam_policy_document.agent.json
}

# This policy lets an ingress VM take over its zone, by taking its Elastic IP and the
# private route tables' default route, and forward traffic that isn't the VM's own.
data "aws_iam_policy_document" "ingress_zone" {
  statement {
    sid       = "FindTheZone"
    actions   = ["ec2:DescribeAddresses", "ec2:DescribeInstances", "ec2:DescribeRouteTables"]
    resources = ["*"]
  }
  statement {
    sid     = "TakeTheAddress"
    actions = ["ec2:AssociateAddress"]
    resources = concat(
      [for eip in aws_eip.ingress : "arn:${local.partition}:ec2:${local.region}:${local.account}:elastic-ip/${eip.allocation_id}"],
      ["arn:${local.partition}:ec2:${local.region}:${local.account}:instance/*", "arn:${local.partition}:ec2:${local.region}:${local.account}:network-interface/*"],
    )
  }
  statement {
    sid       = "TakeTheRoute"
    actions   = ["ec2:CreateRoute", "ec2:ReplaceRoute"]
    resources = [for rt in aws_route_table.private : rt.arn]
  }
  statement {
    sid       = "ForwardTraffic"
    actions   = ["ec2:ModifyInstanceAttribute"]
    resources = ["arn:${local.partition}:ec2:${local.region}:${local.account}:instance/*"]
    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/protostrap:pool"
      values   = ["ingress"]
    }
  }
}

resource "aws_iam_role_policy" "ingress_zone" {
  name   = "zone"
  role   = aws_iam_role.ingress.id
  policy = data.aws_iam_policy_document.ingress_zone.json
}

resource "aws_iam_instance_profile" "ingress" {
  name = "protostrap-vm-ingress"
  path = local.path
  role = aws_iam_role.ingress.name
  tags = local.tags
}

resource "aws_launch_template" "ingress" {
  for_each      = toset(local.zones)
  name          = "protostrap-ingress-${each.key}"
  image_id      = var.ami
  instance_type = local.ingress_vm
  user_data = base64encode(templatefile("${path.module}/templates/vm.sh.tftpl", merge(local.vm_settings, local.vm_files, {
    pool = "ingress"
    asg  = local.ingress_groups[each.key]
    agent_config = jsonencode(merge(local.agent_base, local.agent_hooks, {
      pool               = "ingress"
      auto_scaling_group = local.ingress_groups[each.key]
      ingress = {
        interface             = "ens5"
        elastic_ip_allocation = aws_eip.ingress[each.key].allocation_id
        route_tables          = [aws_route_table.private[each.key].id]
      }
    }))
  })))

  iam_instance_profile {
    arn = aws_iam_instance_profile.ingress.arn
  }

  # The VM has a temporary public IP of its own, to reach AWS until it takes over the zone's.
  network_interfaces {
    device_index                = 0
    subnet_id                   = aws_subnet.public[each.key].id
    security_groups             = [aws_security_group.node.id, aws_security_group.ingress.id]
    associate_public_ip_address = true
    delete_on_termination       = true
  }

  metadata_options {
    http_endpoint               = "enabled"
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_type           = "gp3"
      volume_size           = var.root_volume_gb
      encrypted             = true
      delete_on_termination = true
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = merge(local.vm_tags, { Name = "protostrap-ingress-${each.key}", "protostrap:pool" = "ingress", "protostrap:zone" = each.key })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.vm_tags, { Name = "protostrap-ingress-${each.key}" })
  }

  tags = local.tags
}

resource "aws_autoscaling_group" "ingress" {
  for_each                  = toset(local.zones)
  name                      = local.ingress_groups[each.key]
  min_size                  = 1
  max_size                  = 2
  desired_capacity          = 1
  vpc_zone_identifier       = [aws_subnet.public[each.key].id]
  protect_from_scale_in     = true
  health_check_type         = "EC2"
  health_check_grace_period = 300
  suspended_processes       = ["AZRebalance"]
  wait_for_capacity_timeout = "0"

  launch_template {
    id      = aws_launch_template.ingress[each.key].id
    version = aws_launch_template.ingress[each.key].latest_version
  }

  dynamic "initial_lifecycle_hook" {
    for_each = local.lifecycle_hooks
    content {
      name                 = initial_lifecycle_hook.value.name
      lifecycle_transition = initial_lifecycle_hook.value.transition
      heartbeat_timeout    = local.hook_timeout
      default_result       = initial_lifecycle_hook.value.default
    }
  }

  dynamic "tag" {
    for_each = merge(local.tags, { Name = "protostrap-ingress-${each.key}", "protostrap:pool" = "ingress", "protostrap:zone" = each.key })
    content {
      key                 = tag.key
      value               = tag.value
      propagate_at_launch = false
    }
  }

  lifecycle {
    ignore_changes = [desired_capacity]
  }
}
