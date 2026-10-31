output "app" {
  description = "The application, from the spec."
  value       = local.app
}

output "vpc_id" {
  value = aws_vpc.main.id
}

output "zones" {
  value = local.zones
}

output "registry" {
  description = "The ECR registry the deploy workflow pushes to."
  value       = local.registry
}

output "buckets" {
  description = "The deploy, files and backups buckets, and the application's own, by name."
  value = merge({ for name, b in aws_s3_bucket.app : name => b.bucket }, {
    deploy  = data.aws_s3_bucket.deploy.bucket
    files   = aws_s3_bucket.files.bucket
    backups = aws_s3_bucket.backups.bucket
  })
}

output "data_vms" {
  description = "Each data VM's instance and fixed address. data-1 runs the scheduler."
  value       = { for name, vm in aws_instance.data : name => { instance = vm.id, address = vm.private_ip } }
}

output "scheduler" {
  description = "Where the agents reach the scheduler."
  value       = local.scheduler_addr
}

output "groups" {
  description = "The Auto Scaling groups of each pool."
  value = {
    ingress = values(local.ingress_groups)
    general = values(local.general_groups)
    compute = [for g in local.compute_groups : g.name]
  }
}

output "egress_ips" {
  description = "Each zone's Elastic IP: all outbound traffic comes from these, so third parties can allowlist them."
  value       = { for zone, eip in aws_eip.ingress : zone => eip.public_ip }
}

output "tunnel_id" {
  description = "The Cloudflare tunnel. scripts/set-edge-token.sh puts its token in the environment's secret."
  value       = cloudflare_zero_trust_tunnel_cloudflared.edge.id
}

output "hosts" {
  value = local.tunnel_hosts
}

output "queues" {
  description = "Each queue's URL."
  value       = { for name, q in aws_sqs_queue.main : name => q.url }
}

output "secret" {
  description = "The environment's secret, which an Org Admin fills in: every component's keys, the edge's tunnel token and the reporter's LLM key."
  value       = aws_secretsmanager_secret.app.name
}

output "deploy_document" {
  description = "The SSM document the deploy workflow sends to data-1."
  value       = aws_ssm_document.deploy.name
}

output "log_groups" {
  description = "Where to look: the scheduler's decisions, the deploy command's output, each component's logs."
  value = merge(
    { scheduler = aws_cloudwatch_log_group.scheduler.name, deploy = aws_cloudwatch_log_group.deploy.name, edge = aws_cloudwatch_log_group.edge.name },
    { for name, g in aws_cloudwatch_log_group.component : name => g.name },
  )
}

output "alerts_topic" {
  value = aws_sns_topic.alerts.arn
}
