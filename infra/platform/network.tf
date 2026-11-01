# This file builds the network (docs/network.md): one VPC, four kinds of subnet in every
# zone, and the rules that keep everything but the tunnel out. With the default vpc_cidr,
# the subnets are:
#
#   public   10.20.0.0/24, a /26 per zone    ingress VMs, the only ones with public IPs
#   data     10.20.1.0/24, a /26 per zone    data VMs, at fixed addresses
#   managed  10.20.2.0/24, a /26 per zone    RDS, ElastiCache, endpoints; no route out
#   nodes    a /20 per zone from 10.20.16.0  general and compute VMs
#
# The nodes and data subnets send internet traffic to their zone's ingress VM. That route
# isn't here: the zone's ingress VM points it at itself when it boots (internal/ingress), so
# Terraform leaves the private route tables' routes alone.

locals {
  subnets = {
    for i, zone in local.zones : zone => {
      public  = cidrsubnet(cidrsubnet(var.vpc_cidr, 8, 0), 2, i)
      data    = cidrsubnet(cidrsubnet(var.vpc_cidr, 8, 1), 2, i)
      managed = cidrsubnet(cidrsubnet(var.vpc_cidr, 8, 2), 2, i)
      nodes   = cidrsubnet(var.vpc_cidr, 4, i + 1)
    }
  }
  resolver = cidrhost(var.vpc_cidr, 2)

  # These are the ports VMs use to talk to each other: Swarm, the overlay, the scheduler, and,
  # with telemetry, Docker sending replicas' output to the collector on data-1 (telemetry.tf).
  vm_ports = merge({
    swarm_manager = { protocol = "tcp", port = 2377 }
    gossip_tcp    = { protocol = "tcp", port = 7946 }
    gossip_udp    = { protocol = "udp", port = 7946 }
    overlay       = { protocol = "udp", port = 4789 }
    scheduler     = { protocol = "tcp", port = 7443 }
  }, local.has_telemetry ? { telemetry = { protocol = "tcp", port = 24224 } } : {})
}

resource "aws_vpc" "main" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = merge(local.tags, { Name = "protostrap" })
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.tags, { Name = "protostrap" })
}

resource "aws_subnet" "public" {
  for_each          = local.subnets
  vpc_id            = aws_vpc.main.id
  availability_zone = each.key
  cidr_block        = each.value.public
  tags              = merge(local.tags, { Name = "protostrap-public-${each.key}", "protostrap:subnet" = "public" })
}

resource "aws_subnet" "nodes" {
  for_each          = local.subnets
  vpc_id            = aws_vpc.main.id
  availability_zone = each.key
  cidr_block        = each.value.nodes
  tags              = merge(local.tags, { Name = "protostrap-nodes-${each.key}", "protostrap:subnet" = "nodes" })
}

resource "aws_subnet" "data" {
  for_each          = local.subnets
  vpc_id            = aws_vpc.main.id
  availability_zone = each.key
  cidr_block        = each.value.data
  tags              = merge(local.tags, { Name = "protostrap-data-${each.key}", "protostrap:subnet" = "data" })
}

resource "aws_subnet" "managed" {
  for_each          = local.subnets
  vpc_id            = aws_vpc.main.id
  availability_zone = each.key
  cidr_block        = each.value.managed
  tags              = merge(local.tags, { Name = "protostrap-managed-${each.key}", "protostrap:subnet" = "managed" })
}

# Only the public subnets route to the internet gateway.
resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.tags, { Name = "protostrap-public" })
}

resource "aws_route" "public_internet" {
  route_table_id         = aws_route_table.public.id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.main.id
}

resource "aws_route_table_association" "public" {
  for_each       = aws_subnet.public
  subnet_id      = each.value.id
  route_table_id = aws_route_table.public.id
}

# The nodes and data subnets share their zone's private route table, whose way out the
# ingress VM owns.
resource "aws_route_table" "private" {
  for_each = local.subnets
  vpc_id   = aws_vpc.main.id
  tags     = merge(local.tags, { Name = "protostrap-private-${each.key}", "protostrap:zone" = each.key })
}

resource "aws_route_table_association" "nodes" {
  for_each       = aws_subnet.nodes
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private[each.key].id
}

resource "aws_route_table_association" "data" {
  for_each       = aws_subnet.data
  subnet_id      = each.value.id
  route_table_id = aws_route_table.private[each.key].id
}

# The managed subnets have no route out: nothing in them needs the internet.
resource "aws_route_table" "managed" {
  vpc_id = aws_vpc.main.id
  tags   = merge(local.tags, { Name = "protostrap-managed" })
}

resource "aws_route_table_association" "managed" {
  for_each       = aws_subnet.managed
  subnet_id      = each.value.id
  route_table_id = aws_route_table.managed.id
}

# All S3 traffic, image layers included, goes through the free gateway endpoint, which
# also keeps the scheduler's state and backups off the internet.
resource "aws_vpc_endpoint" "s3" {
  vpc_id            = aws_vpc.main.id
  service_name      = "com.amazonaws.${local.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = concat([aws_route_table.public.id, aws_route_table.managed.id], [for rt in aws_route_table.private : rt.id])
  tags              = merge(local.tags, { Name = "protostrap-s3" })
}

# This network ACL is the second of three locks on the ingress VMs
# (docs/network.md#only-ingress-vms-are-public): whatever the security groups say, the
# internet can't reach the overlay's, Swarm's, the scheduler's or telemetry's ports, or any
# port below 1024. Higher ports stay open for replies to connections the VMs opened.
resource "aws_network_acl" "public" {
  vpc_id     = aws_vpc.main.id
  subnet_ids = [for s in aws_subnet.public : s.id]
  tags       = merge(local.tags, { Name = "protostrap-public" })

  ingress {
    rule_no    = 100
    action     = "allow"
    protocol   = "-1"
    cidr_block = var.vpc_cidr
    from_port  = 0
    to_port    = 0
  }

  dynamic "ingress" {
    for_each = [
      { rule = 110, protocol = "tcp", from = 0, to = 1023 },
      { rule = 111, protocol = "udp", from = 0, to = 1023 },
      { rule = 120, protocol = "tcp", from = 2377, to = 2377 },
      { rule = 121, protocol = "tcp", from = 7946, to = 7946 },
      { rule = 122, protocol = "udp", from = 7946, to = 7946 },
      { rule = 123, protocol = "udp", from = 4789, to = 4789 },
      { rule = 124, protocol = "tcp", from = 7443, to = 7443 },
      { rule = 125, protocol = "tcp", from = 24224, to = 24224 },
    ]
    content {
      rule_no    = ingress.value.rule
      action     = "deny"
      protocol   = ingress.value.protocol
      cidr_block = "0.0.0.0/0"
      from_port  = ingress.value.from
      to_port    = ingress.value.to
    }
  }

  ingress {
    rule_no    = 200
    action     = "allow"
    protocol   = "-1"
    cidr_block = "0.0.0.0/0"
    from_port  = 0
    to_port    = 0
  }

  egress {
    rule_no    = 100
    action     = "allow"
    protocol   = "-1"
    cidr_block = "0.0.0.0/0"
    from_port  = 0
    to_port    = 0
  }
}

# Every VM has the node group: it admits only the other VMs, for Swarm, the overlay, the
# scheduler and telemetry, and lets HTTPS out. No application port needs a rule, since services
# talk inside the overlay, and there is no SSH: shells go through SSM.
resource "aws_security_group" "node" {
  name        = "protostrap-node"
  description = "Every ProtoStrap VM: Swarm, the overlay and the scheduler between VMs, HTTPS out."
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "protostrap-node" })
}

resource "aws_vpc_security_group_ingress_rule" "node" {
  for_each                     = local.vm_ports
  security_group_id            = aws_security_group.node.id
  referenced_security_group_id = aws_security_group.node.id
  ip_protocol                  = each.value.protocol
  from_port                    = each.value.port
  to_port                      = each.value.port
  description                  = "${each.key} from other ProtoStrap VMs"
}

resource "aws_vpc_security_group_egress_rule" "node" {
  for_each                     = local.vm_ports
  security_group_id            = aws_security_group.node.id
  referenced_security_group_id = aws_security_group.node.id
  ip_protocol                  = each.value.protocol
  from_port                    = each.value.port
  to_port                      = each.value.port
  description                  = "${each.key} to other ProtoStrap VMs"
}

resource "aws_vpc_security_group_egress_rule" "node_https" {
  security_group_id = aws_security_group.node.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  description       = "AWS, S3 and third-party APIs"
}

resource "aws_vpc_security_group_egress_rule" "node_managed" {
  for_each          = local.subnets
  security_group_id = aws_security_group.node.id
  cidr_ipv4         = each.value.managed
  ip_protocol       = "tcp"
  from_port         = 0
  to_port           = 65535
  description       = "Managed services in ${each.key}, once there are any"
}

resource "aws_vpc_security_group_egress_rule" "node_extra" {
  for_each          = { for r in var.extra_egress : "${r.protocol}-${r.port}-${r.cidr}" => r }
  security_group_id = aws_security_group.node.id
  cidr_ipv4         = each.value.cidr
  ip_protocol       = each.value.protocol
  from_port         = each.value.port
  to_port           = each.value.port
  description       = each.value.description
}

# Ingress VMs also have the ingress group: HTTPS in from the other VMs, for the NAT to
# forward, and, with a tunnel, the tunnel out. Only ingress VMs can reach Cloudflare's tunnel
# port.
resource "aws_security_group" "ingress" {
  name        = "protostrap-ingress"
  description = "Ingress VMs: NAT for the other VMs, and the Cloudflare tunnel."
  vpc_id      = aws_vpc.main.id
  tags        = merge(local.tags, { Name = "protostrap-ingress" })
}

resource "aws_vpc_security_group_ingress_rule" "ingress_nat" {
  security_group_id            = aws_security_group.ingress.id
  referenced_security_group_id = aws_security_group.node.id
  ip_protocol                  = "tcp"
  from_port                    = 443
  to_port                      = 443
  description                  = "The other VMs' HTTPS, to forward"
}

resource "aws_vpc_security_group_ingress_rule" "ingress_nat_extra" {
  for_each                     = { for r in var.extra_egress : "${r.protocol}-${r.port}-${r.cidr}" => r }
  security_group_id            = aws_security_group.ingress.id
  referenced_security_group_id = aws_security_group.node.id
  ip_protocol                  = each.value.protocol
  from_port                    = each.value.port
  to_port                      = each.value.port
  description                  = "To forward: ${each.value.description}"
}

resource "aws_vpc_security_group_egress_rule" "ingress_tunnel" {
  for_each          = local.has_tunnel ? toset(["tcp", "udp"]) : toset([])
  security_group_id = aws_security_group.ingress.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = each.key
  from_port         = 7844
  to_port           = 7844
  description       = "The Cloudflare tunnel (QUIC, or HTTP/2 where UDP is blocked)"
}

# This log group keeps rejected traffic for debugging.
resource "aws_cloudwatch_log_group" "flow_logs" {
  name              = "${local.log_prefix}flow-logs"
  retention_in_days = 14
  tags              = local.tags
}

data "aws_iam_policy_document" "flow_logs_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["vpc-flow-logs.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "flow_logs" {
  name                 = "protostrap-flow-logs"
  path                 = local.path
  assume_role_policy   = data.aws_iam_policy_document.flow_logs_trust.json
  permissions_boundary = local.role_boundary
  tags                 = local.tags
}

data "aws_iam_policy_document" "flow_logs" {
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents", "logs:DescribeLogStreams"]
    resources = ["${aws_cloudwatch_log_group.flow_logs.arn}:*"]
  }
  statement {
    actions   = ["logs:DescribeLogGroups"]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "flow_logs" {
  name   = "flow-logs"
  role   = aws_iam_role.flow_logs.id
  policy = data.aws_iam_policy_document.flow_logs.json
}

resource "aws_flow_log" "rejected" {
  vpc_id          = aws_vpc.main.id
  traffic_type    = "REJECT"
  log_destination = aws_cloudwatch_log_group.flow_logs.arn
  iam_role_arn    = aws_iam_role.flow_logs.arn
  tags            = local.tags
}

# This is the third lock: AWS Config flags a security group that admits the internet.
resource "aws_config_configuration_recorder" "security_groups" {
  count    = var.record_security_groups ? 1 : 0
  name     = "protostrap"
  role_arn = "arn:${local.partition}:iam::${local.account}:role/aws-service-role/config.amazonaws.com/AWSServiceRoleForConfig"
  recording_group {
    all_supported  = false
    resource_types = ["AWS::EC2::SecurityGroup"]
  }
  depends_on = [aws_iam_service_linked_role.config]
}

resource "aws_iam_service_linked_role" "config" {
  count            = var.record_security_groups ? 1 : 0
  aws_service_name = "config.amazonaws.com"
}

resource "aws_config_delivery_channel" "security_groups" {
  count          = var.record_security_groups ? 1 : 0
  name           = "protostrap"
  s3_bucket_name = aws_s3_bucket.backups.id
  s3_key_prefix  = "config"
  depends_on     = [aws_config_configuration_recorder.security_groups]
}

resource "aws_config_configuration_recorder_status" "security_groups" {
  count      = var.record_security_groups ? 1 : 0
  name       = aws_config_configuration_recorder.security_groups[0].name
  is_enabled = true
  depends_on = [aws_config_delivery_channel.security_groups]
}

resource "aws_config_config_rule" "no_open_ports" {
  count = var.record_security_groups ? 1 : 0
  name  = "protostrap-no-open-ports"
  source {
    owner             = "AWS"
    source_identifier = "VPC_SG_OPEN_ONLY_TO_AUTHORIZED_PORTS"
  }
  depends_on = [aws_config_configuration_recorder_status.security_groups]
}
