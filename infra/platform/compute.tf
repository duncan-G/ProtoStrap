# The compute pool (docs/vms.md#compute) holds the VMs that drain the job queue, on Spot
# first. Each zone has a Spot group across many instance types, each a separate pool of
# Spot capacity, and an On-Demand group that stays empty unless Spot has none
# (docs/vms.md#when-a-launch-fails). Both can sit at zero, and Capacity Rebalancing
# replaces a Spot VM that EC2 says is at risk, through the terminate hook, before its two
# minutes' notice.
#
# The pool is only created when the spec has a compute pool. Its VMs use the node role
# (general.tf).

locals {
  compute_groups = merge([
    for i, zone in(local.compute == null ? [] : local.zones) : {
      "${zone}/spot" = {
        zone = zone
        name = "protostrap-compute-spot-${zone}"
        spot = true
        min  = floor(local.compute.min / length(local.zones)) + (i < local.compute.min % length(local.zones) ? 1 : 0)
      }
      "${zone}/on-demand" = {
        zone = zone
        name = "protostrap-compute-ondemand-${zone}"
        spot = false
        min  = 0
      }
    }
  ]...)
}

resource "aws_launch_template" "compute" {
  for_each      = local.compute_groups
  name          = each.value.name
  image_id      = var.ami
  instance_type = local.compute.vm[0]
  user_data = base64encode(templatefile("${path.module}/templates/vm.sh.tftpl", merge(local.vm_settings, local.vm_files, {
    pool = "compute"
    asg  = each.value.name
    agent_config = jsonencode(merge(local.agent_base, local.agent_hooks, {
      pool               = "compute"
      auto_scaling_group = each.value.name
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
    tags = merge(local.vm_tags, {
      Name                  = "protostrap-compute"
      "protostrap:pool"     = "compute"
      "protostrap:zone"     = each.value.zone
      "protostrap:capacity" = each.value.spot ? "spot" : "on-demand"
    })
  }

  tag_specifications {
    resource_type = "volume"
    tags          = merge(local.vm_tags, { Name = "protostrap-compute" })
  }

  tags = local.tags
}

resource "aws_autoscaling_group" "compute" {
  for_each                  = local.compute_groups
  name                      = each.value.name
  min_size                  = each.value.min
  max_size                  = local.compute.max + 1
  desired_capacity          = each.value.min
  vpc_zone_identifier       = [aws_subnet.nodes[each.value.zone].id]
  protect_from_scale_in     = true
  health_check_type         = "EC2"
  health_check_grace_period = 300
  suspended_processes       = ["AZRebalance"]
  capacity_rebalance        = each.value.spot
  wait_for_capacity_timeout = "0"

  mixed_instances_policy {
    instances_distribution {
      on_demand_base_capacity                  = 0
      on_demand_percentage_above_base_capacity = each.value.spot ? 0 : 100
      on_demand_allocation_strategy            = "prioritized"
      spot_allocation_strategy                 = "price-capacity-optimized"
    }
    launch_template {
      launch_template_specification {
        launch_template_id = aws_launch_template.compute[each.key].id
        version            = aws_launch_template.compute[each.key].latest_version
      }
      dynamic "override" {
        for_each = local.compute.vm
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
    for_each = merge(local.tags, {
      Name                  = "protostrap-compute"
      "protostrap:pool"     = "compute"
      "protostrap:zone"     = each.value.zone
      "protostrap:capacity" = each.value.spot ? "spot" : "on-demand"
    })
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
