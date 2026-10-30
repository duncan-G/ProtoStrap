# Templates

Replace `shop` with the spec's `app`, `v0.1.0` with the ProtoStrap version, and `dev` with
the environment. Every input is in ProtoStrap's `infra/bootstrap/variables.tf` and
`infra/platform/variables.tf` at that version; check them there before relying on these.

## infra/bootstrap/dev/main.tf

```hcl
# Bootstraps the dev account: the state bucket, GitHub's OIDC provider, the Infra and Deploy
# roles and their boundaries, and the dev and dev-infra GitHub environments. An Org Admin
# applies it once, from their own machine:
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
  default = "dev"
}
variable "github_repository" {
  description = "owner/repo: gh repo view --json nameWithOwner -q .nameWithOwner"
  type        = string
}
variable "cloudflare_account_id" { type = string }
variable "cloudflare_zone" { type = string }
variable "hosts" { type = map(string) } # each of the spec's hosts' hostname in this account
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
  # The same version as the spec's protostrap: and the workflows' @v0.1.0.
  source = "github.com/duncan-G/protostrap//infra/bootstrap?ref=v0.1.0"

  app               = "shop" # the spec's app; the secret is shop/<environment>
  environment       = var.environment
  github_repository = var.github_repository

  # The platform root's inputs in this account, as TF_VAR_* variables of dev-infra.
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
hosts                 = { shop = "shop-dev.example.com" }
```

The Cloudflare token goes in with `gh secret set CLOUDFLARE_API_TOKEN --env dev-infra`, not
the tfvars, so it stays out of state.

## infra/dev/main.tf

```hcl
# The dev account's platform: ProtoStrap's platform module for protostrap.yaml. The infra
# workflow applies it with the Infra role; its inputs come from the dev-infra environment.
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
variable "hosts" { type = map(string) } # each of the spec's hosts' hostname in this account
variable "alert_emails" {
  type    = list(string)
  default = []
}

provider "aws" {
  default_tags { tags = { "protostrap:component" = "platform" } }
}
provider "cloudflare" {}

module "protostrap" {
  source = "github.com/duncan-G/protostrap//infra/platform?ref=v0.1.0"

  spec                  = file("${path.module}/../../protostrap.yaml")
  environment           = "dev"
  hosts                 = var.hosts
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone       = var.cloudflare_zone
  alert_emails          = var.alert_emails

  # A test environment, torn down often. Prod keeps the defaults: true and false.
  protect_data_vms      = false
  force_destroy         = true
  log_retention_days    = 14
  backup_retention_days = 7
}
```

Prod's root differs only in settings: no `protect_data_vms` or `force_destroy`, and
`log_retention_days = 90`, `backup_retention_days = 35`, `snapshot_retention = 14`,
`client_rate_limit` sized from real traffic. Other inputs worth knowing: `zones`, `vpc_cidr`
(can't change later), `cors_origins`, `retired_components`, `retired_queues`,
`retired_buckets`, `extra_egress`, `operator_hosts`.

## .github/workflows/deploy.yml (dev)

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

# Not deploy-dev: the reusable deploy job holds deploy-<environment>, and GitHub cancels a
# run whose workflow and job concurrency groups collide.
concurrency:
  group: deploy
  cancel-in-progress: false

jobs:
  plan:
    uses: duncan-G/protostrap/.github/workflows/plan.yml@v0.1.0
    with:
      environment: dev
      # apphost: src/Shop.AppHost/Shop.AppHost.csproj   # when an AppHost writes the spec
      protostrap-ref: v0.1.0

  infra:
    needs: plan
    if: needs.plan.outputs.infra-changed == 'true'
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.0
    with: { environment: dev, directory: infra/dev, action: apply, protostrap-ref: v0.1.0 }
    secrets: inherit

  deploy:
    needs: [plan, infra]
    if: always() && needs.plan.result == 'success' && (needs.infra.result == 'success' || needs.infra.result == 'skipped')
    uses: duncan-G/protostrap/.github/workflows/deploy.yml@v0.1.0
    with:
      environment: dev
      version: ${{ needs.plan.outputs.version }}
      builds: ${{ needs.plan.outputs.builds }}
      protostrap-ref: v0.1.0
```

With prod, add after `deploy`, as in ProtoStrap's `docs/production.md#5-the-applications-repository`:

```yaml
  approve:
    needs: deploy
    runs-on: ubuntu-latest
    environment: prod-approval
    steps:
      - run: echo "Deploying ${{ github.sha }} to prod"

  prod-plan:
    needs: approve
    uses: duncan-G/protostrap/.github/workflows/plan.yml@v0.1.0
    with: { environment: prod, protostrap-ref: v0.1.0 }

  prod-infra:
    needs: prod-plan
    if: needs.prod-plan.outputs.infra-changed == 'true'
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.0
    with: { environment: prod, directory: infra/prod, action: apply, protostrap-ref: v0.1.0 }
    secrets: inherit

  prod:
    needs: [prod-plan, prod-infra]
    if: always() && needs.prod-plan.result == 'success' && (needs.prod-infra.result == 'success' || needs.prod-infra.result == 'skipped')
    uses: duncan-G/protostrap/.github/workflows/deploy.yml@v0.1.0
    with:
      environment: prod
      version: ${{ needs.prod-plan.outputs.version }}
      builds: ${{ needs.prod-plan.outputs.builds }}
      protostrap-ref: v0.1.0
```

and copy `infra.yml` as `infra-prod.yml` for prod, with `environment: prod` and
`directory: infra/prod`.

## .github/workflows/infra.yml

```yaml
name: infra

on:
  push:
    branches: [main]
    paths: [infra/dev/**]
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
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.0
    with:
      environment: dev
      directory: infra/dev
      action: ${{ github.event_name == 'push' && 'apply' || inputs.action }}
      protostrap-ref: v0.1.0
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
          ref: v0.1.0
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
          for dir in infra/bootstrap/dev infra/dev; do
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
