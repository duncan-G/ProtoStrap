output "state_bucket" {
  description = "The bucket for the platform's state. Pass it to the platform root's `terraform init -backend-config=bucket=...`."
  value       = aws_s3_bucket.state.id
}

output "deploy_bucket" {
  description = "The deploy bucket's name. The platform creates the bucket itself."
  value       = local.deploy_bucket
}

output "registry" {
  description = "The ECR registry images are pushed to."
  value       = local.registry
}

output "infra_role_arn" {
  description = "The role the infra workflow takes on, in the infra GitHub environment."
  value       = aws_iam_role.infra.arn
}

output "deploy_role_arn" {
  description = "The role the deploy workflow takes on, in the deploy GitHub environment."
  value       = aws_iam_role.deploy.arn
}

output "infra_boundary_arn" {
  description = "The Infra boundary: the most a CI role may do."
  value       = aws_iam_policy.infra_boundary.arn
}

output "role_boundary_arn" {
  description = "The Role boundary, which every role the infra workflow creates must carry."
  value       = aws_iam_policy.role_boundary.arn
}

output "github_environments" {
  description = "The GitHub environments the workflows run in."
  value = {
    deploy = local.deploy_environment
    infra  = local.infra_environment
  }
}
