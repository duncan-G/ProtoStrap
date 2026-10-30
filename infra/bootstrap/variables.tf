variable "environment" {
  description = "The environment this AWS account runs, such as dev or prod. Names the GitHub environments the workflows run in."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,15}$", var.environment))
    error_message = "environment must be up to 16 lowercase letters, digits and dashes, starting with a letter."
  }
}

variable "app" {
  description = "The application this account runs: the spec's app. Its secret is <app>/<environment>."
  type        = string

  validation {
    condition     = can(regex("^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$", var.app))
    error_message = "app must be the spec's app: up to 30 lowercase letters, digits and dashes."
  }
}

variable "github_repository" {
  description = "The application's repository, owner/repo. Only its workflows can take on the Infra and Deploy roles."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be owner/repo."
  }
}

variable "github_deploy_environment" {
  description = "GitHub environment the deploy workflow runs in. Empty: the environment's name, such as dev."
  type        = string
  default     = ""
}

variable "github_infra_environment" {
  description = "GitHub environment the infra workflow runs in. Empty: <environment>-infra. Put required reviewers on it to gate infrastructure changes."
  type        = string
  default     = ""
}

variable "manage_github" {
  description = "Create the two GitHub environments and their variables and secrets. Needs a GitHub token with admin rights on the repository (GITHUB_TOKEN or `gh auth token`)."
  type        = bool
  default     = true
}

variable "create_github_oidc_provider" {
  description = "Create GitHub's OpenID Connect provider in IAM. An account has at most one for token.actions.githubusercontent.com: set false if it already exists."
  type        = bool
  default     = true
}

variable "infra_variables" {
  description = "Inputs to the platform's Terraform root that differ per account but aren't secret, such as cloudflare_account_id, cloudflare_zone or hosts (as JSON). Each becomes a TF_VAR_<name> variable in the infra GitHub environment."
  type        = map(string)
  default     = {}
}

variable "infra_secrets" {
  description = "Secrets the infra workflow needs, such as CLOUDFLARE_API_TOKEN, set as secrets of the infra GitHub environment. They are written to this root's state: keep it on your machine, or set them with `gh secret set --env` instead."
  type        = map(string)
  default     = {}
  sensitive   = true
}

variable "extra_infra_actions" {
  description = "Actions the Infra boundary allows besides building ProtoStrap, such as [\"elasticache:*\"] when Redis moves to ElastiCache. Widening a boundary is an Org Admin's change (docs/identity.md#adding-to-protostrap)."
  type        = list(string)
  default     = []
}

variable "extra_role_actions" {
  description = "Actions the Role boundary allows besides running ProtoStrap, such as [\"ses:SendEmail\"], so component roles can be given them."
  type        = list(string)
  default     = []
}
