# This root bootstraps the sample's dev account with infra/bootstrap, which makes the state
# bucket, GitHub's OpenID Connect provider, the Infra and Deploy roles, their boundaries,
# and the two GitHub environments the workflows run in. An Org Admin applies it once, from
# their own machine, with admin credentials for the account (see sample/README.md):
#
#   cp terraform.tfvars.example terraform.tfvars    # Then fill it in.
#   export AWS_PROFILE=... GITHUB_TOKEN=$(gh auth token)
#   terraform init && terraform apply
#
# Its state stays on that machine (it is gitignored): everything here can be imported
# again if it is lost.

terraform {
  required_version = ">= 1.10"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    github = {
      source  = "integrations/github"
      version = "~> 6.2"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

variable "region" {
  description = "The account's region."
  type        = string
  default     = "us-east-1"
}

variable "environment" {
  type    = string
  default = "dev"
}

variable "github_repository" {
  description = "This repository, owner/repo: gh repo view --json nameWithOwner -q .nameWithOwner"
  type        = string
}

variable "cloudflare_account_id" {
  type = string
}

variable "cloudflare_zone" {
  description = "The domain on Cloudflare the sample's hosts are in, such as example.com."
  type        = string
}

variable "hosts" {
  description = "The hostname of each of the spec's hosts in this account, such as { sample = \"sample-dev.example.com\" }."
  type        = map(string)
}

variable "cloudflare_api_token" {
  description = "A Cloudflare API token for the infra workflow (see sample/README.md for its permissions). Empty: set it yourself with `gh secret set CLOUDFLARE_API_TOKEN --env dev-infra`."
  type        = string
  default     = ""
  sensitive   = true
}

variable "alert_emails" {
  type    = list(string)
  default = []
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { "protostrap:component" = "bootstrap" }
  }
}

provider "github" {
  owner = split("/", var.github_repository)[0]
}

module "bootstrap" {
  source = "../../../infra/bootstrap"

  app               = "sample"
  environment       = var.environment
  github_repository = var.github_repository

  # These are the platform root's inputs in this account (sample/infra/dev), set as
  # TF_VAR_* variables of the infra GitHub environment.
  infra_variables = {
    cloudflare_account_id = var.cloudflare_account_id
    cloudflare_zone       = var.cloudflare_zone
    hosts                 = jsonencode(var.hosts)
    alert_emails          = jsonencode(var.alert_emails)
  }
  infra_secrets = var.cloudflare_api_token == "" ? {} : { CLOUDFLARE_API_TOKEN = var.cloudflare_api_token }
}

output "state_bucket" {
  value = module.bootstrap.state_bucket
}

output "infra_role_arn" {
  value = module.bootstrap.infra_role_arn
}

output "deploy_role_arn" {
  value = module.bootstrap.deploy_role_arn
}

output "github_environments" {
  value = module.bootstrap.github_environments
}
