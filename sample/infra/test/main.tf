# This root builds the sample's test account: the platform (infra/platform) for
# sample/protostrap.yaml, with this account's settings. The infra workflow applies it with
# the Infra role (.github/workflows/sample-infra.yml); its inputs come from the test-infra
# GitHub environment, which bootstrap filled in.
#
# To plan it by hand, run these with credentials that can read the state bucket:
#
#   export AWS_REGION=us-east-1 CLOUDFLARE_API_TOKEN=...
#   terraform init -backend-config=bucket=protostrap-<account>-<region>-tfstate -backend-config=region=$AWS_REGION
#   terraform plan -var-file=test.tfvars

terraform {
  required_version = ">= 1.10"

  # The bucket and region are given at init: a backend can't read variables.
  backend "s3" {
    key          = "platform/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "cloudflare_account_id" {
  type = string
}

variable "cloudflare_zone" {
  type = string
}

variable "hosts" {
  type = map(string)
}

variable "alert_emails" {
  type    = list(string)
  default = []
}

# The region comes from AWS_REGION, and Cloudflare's token from CLOUDFLARE_API_TOKEN.
provider "aws" {
  default_tags {
    tags = { "protostrap:component" = "platform" }
  }
}

provider "cloudflare" {}

module "protostrap" {
  source = "../../../infra/platform"

  spec                  = file("${path.module}/../../protostrap.yaml")
  environment           = "test"
  hosts                 = var.hosts
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone       = var.cloudflare_zone
  alert_emails          = var.alert_emails

  # These settings suit a test environment, which is torn down often.
  protect_data_vms      = false
  force_destroy         = true
  log_retention_days    = 14
  backup_retention_days = 7
}

output "platform" {
  value = module.protostrap
}
