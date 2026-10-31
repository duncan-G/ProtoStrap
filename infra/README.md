# Infrastructure

ProtoStrap's Terraform is three modules. Each AWS account an application runs in calls
them from two small roots of its own, as [`sample/infra`](../sample/infra) does for the
sample's test account. Everything about the application comes from its `protostrap.yaml`;
the roots hold only what differs between accounts, such as the region, the Cloudflare zone
and the hosts.

| Module                   | Applied by                                        | Creates                                                                 |
| ------------------------ | ------------------------------------------------- | ----------------------------------------------------------------------- |
| [`bootstrap`](bootstrap) | An Org Admin, once per account, from their machine | The state and deploy buckets, GitHub's OpenID Connect provider, the Infra and Deploy roles, their boundaries, and the two GitHub environments |
| [`platform`](platform)   | The infra workflow, with the Infra role           | Every AWS and Cloudflare resource the application needs, from its spec  |
| [`people`](people)       | An Org Admin, once per organization               | IAM Identity Center groups and permission sets for Org Admins, Platform Admins and Developers |

**The order.** Bootstrap gives the account what it needs to run the infra workflow without
anyone's admin credentials: from then on, the platform changes only through GitHub
Actions, inside the boundaries bootstrap set. Then the infra workflow applies the platform,
an Org Admin fills in the secrets, and the deploy workflow ships releases. The sample's
[README](../sample/README.md) walks through all of it in a test account, and
[Going to production](../docs/production.md) covers what changes for prod.

**Who may do what** is in [Identity](../docs/identity.md). Widening what the Infra role or
a component role may ever do, such as letting the platform create ElastiCache once Redis
moves there, is an Org Admin's change to bootstrap's `extra_infra_actions` or
`extra_role_actions`.


## Files are components

Each file holds one component of ProtoStrap and everything it needs, whatever AWS service
that is, so a component's whole footprint is in one place. There is no `iam.tf` or
`s3.tf`: the scheduler's permissions are in `scheduler.tf`, beside its log group.

| `platform/`       | Component                                                                    |
| ----------------- | ---------------------------------------------------------------------------- |
| `spec.tf`         | The spec, its infra projection, and the names everything else is built from  |
| `network.tf`      | The VPC, its subnets, routes, the S3 endpoint, security groups and flow logs  |
| `agent.tf`        | What runs on every VM: user data, the agent's config and the VMs' roles       |
| `ingress.tf`      | The ingress pool: one VM per zone, its Elastic IP and NAT                     |
| `general.tf`      | The general pool's Auto Scaling groups                                        |
| `compute.tf`      | The compute pool's Spot and On-Demand groups                                  |
| `data.tf`         | The data VMs, data-1's control volume and the scheduler's config              |
| `scheduler.tf`    | The scheduler's log group and permissions                                     |
| `reporter.tf`     | The reporter's secret and permissions                                         |
| `edge.tf`         | The Cloudflare tunnel, and the edge's repository, secret, log group and role  |
| `cdn.tf`          | Cloudflare: DNS records, zone settings, rate limits and operator access      |
| `components.tf`   | Each component's repository, secret, log group, role and volume               |
| `queues.tf`       | Each queue and its dead-letter queue                                          |
| `files.tf`        | The files bucket                                                              |
| `buckets.tf`      | Each of the application's own buckets, with its settings from the spec        |
| `backups.tf`      | The backups bucket and volume snapshots                                       |
| `deploy.tf`       | The deploy bucket's lifecycle rules, with the infra log's archive, the deploy SSM document and its log group |
| `ui.tf`           | The operator UI's SSM session documents, one per role                         |
| `telemetry.tf`    | Telemetry's volume, bucket, role, repository and Grafana's SSM session document |
| `alerts.tf`       | The alerts topic and what feeds it                                            |
| `templates/`      | User data and the systemd units it installs                                   |
| `tests/`          | `terraform test` with mocked providers, for the sample's spec                 |

Bootstrap is the same: `state.tf`, `boundaries.tf`, `infra.tf` (the Infra role),
`deploy.tf` (the Deploy role) and `github.tf`.


## An application's roots

An application's repository has, per account, a bootstrap root and a platform root, like
[`sample/infra/bootstrap`](../sample/infra/bootstrap/main.tf) and
[`sample/infra/test`](../sample/infra/test/main.tf), with the module source pointing at a
ProtoStrap version:

```hcl
module "protostrap" {
  source = "github.com/duncan-G/protostrap//infra/platform?ref=v0.1.1"

  spec                  = file("${path.module}/../../protostrap.yaml")
  environment           = "prod"
  hosts                 = var.hosts
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone       = var.cloudflare_zone
}
```

The platform root's inputs that aren't secret are bootstrap's `infra_variables`, which
become `TF_VAR_*` variables of the account's infra GitHub environment. GitHub stores their
names in uppercase, and the infra workflow lowercases each one after `TF_VAR_`, so the
root's variable names must be lowercase. Its secrets, such as `CLOUDFLARE_API_TOKEN`, are
set on that environment with `gh secret set --env`.

**Settings worth knowing**, all in [`platform/variables.tf`](platform/variables.tf):

| Variable                       | Default        | Meaning                                                              |
| ------------------------------ | -------------- | -------------------------------------------------------------------- |
| `hosts`                        | the spec's     | Each of the spec's hosts' hostname in this account, by name: `{ app = "app.staging.example.com" }`. It names every host the spec has, and only those; it reaches the tunnel, DNS, CORS, the edge and every replica's `HOST_<NAME>` through `hosts.json` |
| `zones`                        | the first zone | Availability zones to run in, each with its own ingress VM and groups |
| `vpc_cidr`                     | `10.20.0.0/16` | The VPC's range                                                       |
| `operator_hosts`               | none           | Operator UIs, behind Cloudflare Access                               |
| `email_domains`                | the spec's     | The domains this account sends mail from: a second deployed environment uses its own ([Email](../docs/email.md#each-account)) |
| `email_dmarc`                  | none           | The DMARC record for each email domain, for a domain that has none   |
| `client_rate_limit`            | 0, none        | Requests per client per 10 seconds before Cloudflare blocks it        |
| `retired_components`, `retired_queues` | none   | Components and queues removed from the spec but kept, so a rollback still works |
| `retired_buckets`              | none           | Buckets removed from the spec but kept with their objects, each with the settings it had there |
| `cors_origins`                 | none           | Origins besides the hosts whose browsers may use signed URLs on files and on `browser` buckets |
| `protect_data_vms`             | true           | Stops data VMs being terminated. Turn off before destroying. Stateful components' volumes are always kept: a destroy leaves them in the account |
| `force_destroy`                | false          | Lets a destroy delete buckets and repositories that aren't empty. Test environments only |
| `log_retention_days`, `backup_retention_days` | | How long logs and backups are kept                                   |


## Checking it offline

Every root validates without AWS or Cloudflare credentials, and the platform's tests run
against mocked providers:

```bash
terraform -chdir=infra/platform init -backend=false
```

```bash
terraform -chdir=infra/platform test
```
