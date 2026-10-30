# Bootstrap creates what one AWS account needs before GitHub Actions can run the infra
# workflow (docs/identity.md). An Org Admin applies it once per account, from their own
# machine, with admin credentials; it never runs in CI. Every later change to the platform
# goes through the infra workflow, inside the boundaries created here.
#
# It is a module: the application's repository has a small root per account that calls it
# (see sample/infra/bootstrap), so the state and the inputs live with the application.

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
