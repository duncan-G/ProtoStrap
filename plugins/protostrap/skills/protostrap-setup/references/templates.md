# Templates

Replace `shop` with the spec's `app` and `v0.1.2` with the ProtoStrap version. Every input
is in ProtoStrap's `infra/bootstrap/variables.tf`, `infra/platform/variables.tf` and
`infra/people/main.tf` at that version; check them there before relying on these.

## infra/bootstrap/main.tf

```hcl
# Bootstraps the application's account: the state and deploy buckets, GitHub's OIDC provider, the Infra
# and Deploy roles and their boundaries, and the prod and prod-infra GitHub environments. An
# Org Admin applies it once, from their own machine:
#
#   cp terraform.tfvars.example terraform.tfvars    # Then fill it in.
#   export AWS_PROFILE=... GITHUB_TOKEN=$(gh auth token)
#   terraform init && terraform apply
#
# Its state stays on that machine (gitignored); everything here can be imported again.
terraform {
  required_version = ">= 1.10"
  required_providers {
    aws    = { source = "hashicorp/aws", version = "~> 5.60" }
    github = { source = "integrations/github", version = "~> 6.2" }
    tls    = { source = "hashicorp/tls", version = "~> 4.0" }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}
variable "environment" {
  type    = string
  default = "prod"
}
variable "github_repository" {
  description = "owner/repo: gh repo view --json nameWithOwner -q .nameWithOwner"
  type        = string
}
variable "cloudflare_account_id" { type = string }
variable "cloudflare_zone" { type = string }
variable "hosts" { type = map(string) } # each of the spec's gateway.hosts' hostname in this account
variable "alert_emails" {
  type    = list(string)
  default = []
}

provider "aws" {
  region = var.region
  default_tags { tags = { "protostrap:component" = "bootstrap" } }
}
provider "github" {
  owner = split("/", var.github_repository)[0]
}

module "bootstrap" {
  # The same version as the spec's protostrap: and the workflows' @v0.1.2.
  source = "github.com/duncan-G/protostrap//infra/bootstrap?ref=v0.1.2"

  app               = "shop" # the spec's app; the secret is shop/<environment>
  environment       = var.environment
  github_repository = var.github_repository

  # The platform root's inputs in this account, as TF_VAR_* variables of prod-infra.
  infra_variables = {
    cloudflare_account_id = var.cloudflare_account_id
    cloudflare_zone       = var.cloudflare_zone
    hosts                 = jsonencode(var.hosts)
    alert_emails          = jsonencode(var.alert_emails)
  }
}

output "state_bucket" { value = module.bootstrap.state_bucket }
output "infra_role_arn" { value = module.bootstrap.infra_role_arn }
output "deploy_role_arn" { value = module.bootstrap.deploy_role_arn }
```

`terraform.tfvars.example` beside it, with placeholders only:

```hcl
github_repository     = "owner/repo"
region                = "us-east-1"
cloudflare_account_id = "0123456789abcdef0123456789abcdef"
cloudflare_zone       = "example.com"
hosts                 = { shop = "shop.example.com" }
```

The Cloudflare token goes in with `gh secret set CLOUDFLARE_API_TOKEN --env prod-infra`, not
the tfvars, so it stays out of state.

## infra/prod/main.tf

```hcl
# Prod's platform: ProtoStrap's platform module for protostrap.yaml. The infra workflow
# applies it with the Infra role; its inputs come from the prod-infra environment.
terraform {
  required_version = ">= 1.10"

  # The bucket and region are given at init.
  backend "s3" {
    key          = "platform/terraform.tfstate"
    encrypt      = true
    use_lockfile = true
  }

  required_providers {
    aws        = { source = "hashicorp/aws", version = "~> 5.60" }
    cloudflare = { source = "cloudflare/cloudflare", version = "~> 5.0" }
    random     = { source = "hashicorp/random", version = "~> 3.6" }
  }
}

variable "cloudflare_account_id" { type = string }
variable "cloudflare_zone" { type = string }
variable "hosts" { type = map(string) } # each of the spec's gateway.hosts' hostname in this account
variable "alert_emails" {
  type    = list(string)
  default = []
}

provider "aws" {
  default_tags { tags = { "protostrap:component" = "platform" } }
}
provider "cloudflare" {}

module "protostrap" {
  source = "github.com/duncan-G/protostrap//infra/platform?ref=v0.1.2"

  spec                  = file("${path.module}/../../protostrap.yaml")
  environment           = "prod"
  hosts                 = var.hosts
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone       = var.cloudflare_zone
  alert_emails          = var.alert_emails

  # protect_data_vms and force_destroy keep their defaults, true and false.
  log_retention_days    = 90
  backup_retention_days = 35
  snapshot_retention    = 14
}
```

Other inputs worth knowing: `zones`, `vpc_cidr` (can't change later), `cors_origins`,
`retired_components`, `retired_queues`, `retired_buckets`, `extra_egress`, `operator_hosts`
(only with the spec's `tunnel`). Cloudflare's rate limit, WAF rules and cache rules aren't
inputs: they are the spec's `cdn` (protostrap-spec), which needs a tunnel.

## infra/people/main.tf

```hcl
# Who signs in to the application's account, and what they may do: ProtoStrap's people
# module (its docs/identity.md#people). An Org Admin applies it once, from that account,
# its organization's management account, after enabling Identity Center in the console,
# and adds people to the groups in Identity Center:
#
#   cp terraform.tfvars.example terraform.tfvars    # Then fill it in.
#   export AWS_PROFILE=...
#   terraform init && terraform apply
#
# Its state stays on that machine (gitignored).
terraform {
  required_version = ">= 1.10"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

variable "region" {
  description = "Identity Center's region."
  type        = string
  default     = "us-east-1"
}
variable "accounts" {
  description = "Each deployed environment's AWS account, such as { prod = \"222222222222\" }. Dev, the local run, has none: its secret is in prod's."
  type        = map(string)
}

provider "aws" {
  region = var.region
}

module "people" {
  source = "github.com/duncan-G/protostrap//infra/people?ref=v0.1.2"

  # Developers see the account read-only, manage dev's secret, shop/dev, beside prod's, and
  # open Grafana when the spec has telemetry.
  accounts = var.accounts
}

output "permission_sets" { value = module.people.permission_sets }
output "groups" { value = module.people.groups }
```

`terraform.tfvars.example` beside it:

```hcl
region   = "us-east-1"
accounts = { prod = "222222222222" }
```

## .github/workflows/deploy.yml

```yaml
name: deploy

on:
  push:
    branches: [main]
  workflow_dispatch:

# The reusable workflows sign in to AWS with GitHub's OIDC token.
permissions:
  contents: read
  id-token: write

# Not deploy-prod: the reusable deploy job holds deploy-<environment>, and GitHub cancels a
# run whose workflow and job concurrency groups collide.
concurrency:
  group: deploy
  cancel-in-progress: false

jobs:
  # One approval per release, by prod-approval's required reviewers.
  approve:
    runs-on: ubuntu-latest
    environment: prod-approval
    steps:
      - run: echo "Deploying ${{ github.sha }} to prod"

  plan:
    needs: approve
    uses: duncan-G/protostrap/.github/workflows/plan.yml@v0.1.2
    with:
      environment: prod
      # apphost: src/Shop.AppHost/Shop.AppHost.csproj   # when an AppHost writes the spec
      protostrap-ref: v0.1.2

  infra:
    needs: plan
    if: needs.plan.outputs.infra-changed == 'true'
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.2
    with: { environment: prod, directory: infra/prod, action: apply, protostrap-ref: v0.1.2 }
    secrets: inherit

  deploy:
    needs: [plan, infra]
    if: always() && needs.plan.result == 'success' && (needs.infra.result == 'success' || needs.infra.result == 'skipped')
    uses: duncan-G/protostrap/.github/workflows/deploy.yml@v0.1.2
    with:
      environment: prod
      version: ${{ needs.plan.outputs.version }}
      builds: ${{ needs.plan.outputs.builds }}
      protostrap-ref: v0.1.2
```

The plan and deploy jobs run in the `prod` GitHub environment and infra in `prod-infra`, both
created by bootstrap; create `prod-approval` by hand, as in ProtoStrap's
`docs/production.md#5-the-applications-repository`. Required reviewers go on `prod-approval`,
for one approval per release, and on `prod-infra`; all three are limited to `main`.

## .github/workflows/infra.yml

```yaml
name: infra

on:
  push:
    branches: [main]
    paths: [infra/prod/**]
  workflow_dispatch:
    inputs:
      action:
        type: choice
        options: [plan, apply, destroy]
        default: plan

permissions:
  contents: read
  id-token: write

jobs:
  infra:
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.2
    with:
      environment: prod
      directory: infra/prod
      action: ${{ github.event_name == 'push' && 'apply' || inputs.action }}
      protostrap-ref: v0.1.2
    secrets: inherit
```

## .github/workflows/ci.yml

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

jobs:
  spec:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/checkout@v5
        with:
          repository: duncan-G/protostrap
          ref: v0.1.2
          path: .protostrap
      - uses: ./.protostrap/.github/actions/protostrap   # builds the protostrap CLI
        with:
          source: .protostrap
      # With an AppHost: set up .NET, run the tests, regenerate the spec, and fail if
      # `git status --porcelain -- protostrap.yaml` isn't empty.
      - run: protostrap check --spec protostrap.yaml

  terraform:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: hashicorp/setup-terraform@v4
        with: { terraform_version: 1.10.5 }
      - run: terraform fmt -check -recursive infra
      - run: |
          for dir in infra/bootstrap infra/prod infra/people; do
            terraform -chdir="$dir" init -backend=false -input=false
            terraform -chdir="$dir" validate
          done
```

## .gitignore

```
.terraform/
*.tfstate
*.tfstate.*
terraform.tfvars
/.protostrap/
```
