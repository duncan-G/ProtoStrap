# The general pool (docs/vms.md#general) holds the On-Demand VMs that run the services, and
# workers while jobs are few. There is one Auto Scaling group per zone, which can launch any
# of the spec's VM types, all of one shape. The scheduler owns how many VMs each group has:
# it raises the desired capacity to add one, and terminates the one it chose to remove.
# Every VM is protected from scale-in, so AWS never picks one, and the groups have no
# scaling policies.
#
# General and compute VMs share a role: the node role.

locals {
  general_groups = { for zone in local.zones : zone => "protostrap-general-${zone}" }

  # The pool's minimum is split across zones as each group's minimum, so Auto Scaling keeps
  # the pool there even if the scheduler is down when VMs are lost.
  general_min = { for i, zone in local.zones : zone => floor(local.general.min / length(local.zones)) + (i < local.general.min % length(local.zones) ? 1 : 0) }
}

resource "aws_iam_role" "node" {
  name                 = "protostrap-vm-node"
  path                 = local.path
  description          = "General and compute VMs: the agent, Docker and the SSM Agent."
  assume_role_policy   = data.aws_iam_policy_document.vm_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

resource "aws_iam_role_policy_attachment" "node_ssm" {
  role       = aws_iam_role.node.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "node_agent" {
  name   = "agent"
  role   = aws_iam_role.node.id
  policy = data.aws_iam_policy_document.agent.json
}

resource "aws_iam_instance_profile" "node" {
  name = "protostrap-vm-node"
  path = local.path
  role = aws_iam_role.node.name
  tags = local.tags
}

resource "aws_launch_template" "general" {
  for_each      = toset(local.zones)
  name          = "protostrap-general-${each.key}"
  image_id      = var.ami
  instance_type = local.general.vm[0]
  user_data = base64encode(templatefile("${path.module}/templates/vm.sh.tftpl", merge(local.vm_settings, local.vm_files, {
    pool = "general"
    asg  = local.general_groups[each.key]
    agent_config = jsonencode(merge(local.agent_base, local.agent_hooks, {
      pool               = "general"
      auto_scaling_group = local.general_groups[each.key]
    }))
  })))

  iam_instance_profile {
    arn = aws_iam_instance_profile.node.arn
  }

  vpc_security_group_ids = [aws_security_group.node.id]

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
    tags          = merge(local.vm_tags, { Name = "protostrap-general", "protostrap:pool" = "general", "protostrap:zone" = each.key })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.vm_tags, { Name = "protostrap-general" })
  }

  tags = local.tags
}

resource "aws_autoscaling_group" "general" {
  for_each                  = toset(local.zones)
  name                      = local.general_groups[each.key]
  min_size                  = local.general_min[each.key]
  max_size                  = local.general.max + 1
  desired_capacity          = local.general_min[each.key]
  vpc_zone_identifier       = [aws_subnet.nodes[each.key].id]
  protect_from_scale_in     = true
  health_check_type         = "EC2"
  health_check_grace_period = 300
  suspended_processes       = ["AZRebalance"]
  wait_for_capacity_timeout = "0"

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = 100
      on_demand_allocation_strategy            = "prioritized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.general[each.key].id
        version            = aws_launch_template.general[each.key].latest_version
      }
      dynamic "override" {
        for_each = local.general.vm
        content {
          instance_type = override.value
        }
      }
    }
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
    for_each = merge(local.tags, { Name = "protostrap-general", "protostrap:pool" = "general", "protostrap:zone" = each.key, "protostrap:capacity" = "on-demand" })
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
