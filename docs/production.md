# Going to production

How to get from this repository to an application serving real traffic from its own
production AWS account, and how to run it once it does. In order:

1. [Know what you are accepting](#1-know-what-you-are-accepting): the limits and the gaps.
2. [Prove it with the sample](#2-prove-it-with-the-sample): nothing has run against real AWS yet.
3. [Publish ProtoStrap](#3-publish-protostrap) at a version.
4. [Set up the accounts and people](#4-accounts-and-people).
5. [Set up the application's repository](#5-the-applications-repository).
6. [Choose production settings](#6-production-settings).
7. [Bootstrap and build prod](#7-bootstrap-and-build-prod).
8. [Go live](#8-go-live).
9. [Run it](#9-running-it).

The [sample's walkthrough](../sample/README.md) is the same path for a test account; this
document adds what changes for production.


## 1. Know what you are accepting

**Limits, by design.** These are the price of ProtoStrap's low cost. Each one is a step on
the [Path to managed services](../README.md#path-to-managed-services).

| Limit                               | What it means in production                                                                              |
| ----------------------------------- | -------------------------------------------------------------------------------------------------------- |
| One availability zone               | A zone outage takes the whole application offline                                                        |
| One edge per zone                   | Replacing the ingress VM pauses requests and outbound traffic until its replacement takes over           |
| One data VM                         | PostgreSQL, Redis, the swarm manager and the scheduler share it. EC2 recovers it from hardware failure; losing the instance is a manual restore ([Recovery](vms.md#recovery)) |
| Daily backups, in the same account  | Up to a day of data can be lost. Losing the account or the region loses the backups too                  |
| One secret per environment          | Every component, the edge and the data VMs can read every key in it: a compromised component reads the others' too ([Who can read it](secrets.md#who-can-read-it)) |

**Gaps, not built yet.** Decide for each whether to accept it, work around it, or have it
built before go-live.

| Gap                                                          | Until it is closed                                                                                                    |
| ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- |
| VMs don't roll onto a new image ([New images](vms.md#new-images)) | New VMs boot the latest Amazon Linux; running ones keep theirs. [Replace them by hand](#patching-vms)            |
| No restore tooling                                           | Backups are taken every night, but restoring one into the live database is manual: nothing stops the components that write to it. [Test restores](#restoring-a-backup) work today |
| Alerts cover AWS events only                                 | Nothing alerts when the application is down, a backup fails, a volume fills, or jobs reach a dead-letter queue. [Add them](#alerts) |
| Each account builds its own images                           | A second deployed environment rebuilds each image from the same commit. Pin base images by digest, so both builds are the same |
| One spec for every environment                               | A second deployed environment runs prod's pool sizes, unless each environment gets [a spec of its own](#one-spec-or-one-per-environment) |


## 2. Prove it with the sample

ProtoStrap's code is tested, but none of it has run against real AWS, Cloudflare or
GitHub, and no image has been built. The image versions in [`edge/Dockerfile`](../edge/Dockerfile),
[`telemetry/Dockerfile`](../telemetry/Dockerfile) and the sample's Dockerfiles are
unverified, as are the telemetry programs' configs (`internal/telemetry`). Before any production account exists, take
the sample, ProtoStrap's own end-to-end test, through [its walkthrough](../sample/README.md)
in a test account, the environment `test`, and check each of these:

- [ ] Bootstrap, infra and the first deploy work from nothing, with no hand fixes.
- [ ] Every scaling test in the walkthrough behaves as its table says.
- [ ] A release that fails rolls back, and one that succeeds replaces only what changed.
- [ ] A ProtoStrap upgrade: tag a second version, bump the spec, and deploy.
- [ ] Losing VMs: terminate a general VM, a compute VM mid-job and the ingress VM. Each is
      replaced, and no job is lost.
- [ ] data-1: reboot it, then stop and start it. Everything comes back by itself.
- [ ] [Restore a backup](#restoring-a-backup) into a scratch database.
- [ ] Destroy the environment and build it again.

Then do the same with your own application, with its real `protostrap.yaml`, in prod: its
first deploys go there, and it serves no one until it [goes live](#8-go-live). Skip the
last check there: prod keeps its data VMs protected and its buckets undeletable.


## 3. Publish ProtoStrap

Applications use ProtoStrap at a version, from GitHub: its Terraform modules, its reusable
workflows, and the source the workflows build `protostrap` and the edge image from.

1. **Put this repository on GitHub** at `duncan-G/protostrap`. That name is
   assumed in `go.mod`, the workflows' `protostrap-repository` default and the docs. For
   another name, change every file this lists:

   ```bash
   grep -rln duncan-G/protostrap --exclude-dir=.git .
   ```

2. **Make it public.** An application's workflows check out ProtoStrap's source with
   their default token, which can't read another private repository. A private ProtoStrap
   needs a token input added to the workflows.
3. **Get CI green:** [`ci.yml`](../.github/workflows/ci.yml) runs the Go tests with the race
   detector, checks the sample spec, and validates and tests the Terraform.
4. **Tag a version,** and protect tags with a ruleset so they can't move:

   ```bash
   git tag v0.1.0 && git push origin v0.1.0
   ```

**Never move or reuse a tag.** Each account keeps the `protostrap` binary and the edge image
by version, and never builds a version it already has. A retagged `v0.1.0` would leave
accounts running different code under one name. Every reference to the version must
match:

| Where                                     | Form                                                             |
| ----------------------------------------- | ---------------------------------------------------------------- |
| The spec                                  | `protostrap: 0.1.1`                                              |
| The application's workflows               | `uses: duncan-G/protostrap/.github/workflows/deploy.yml@v0.1.1`, with `protostrap-ref: v0.1.1` |
| The application's Terraform roots         | `source = "github.com/duncan-G/protostrap//infra/platform?ref=v0.1.1"` |


## 4. Accounts and people

- **One account per application.** It holds everything: IAM Identity Center, as its
  organization's management account, prod's platform (its bootstrap and platform roots),
  and dev's secret. Dev is the local run, so it has no account: its secret, `<app>/dev`,
  named like every environment's, sits beside prod's, `<app>/prod`. No Terraform makes it:
  Developers create it once (`aws secretsmanager create-secret --name <app>/dev`), fill it
  with `scripts/set-secret.sh`, and their services read it with their sign-in
  ([Dev](secrets.md#dev)). Nothing is deployed for dev,
  and no workflow touches its secret. ProtoStrap names its resources on the assumption of
  one application per account, so a second application gets an account of its own, as
  does a second deployed environment if the application has one. Use one region for all
  of it.
- **People sign in through Identity Center.** Enable it in the account's console, then
  apply [`infra/people`](../infra/people/main.tf) there with the accounts map,
  `{ prod = "<the account's ID>" }`, and add people to `Org-Admins`, `Platform-Admins` and
  `Developers`. No IAM users, no access keys ([People](identity.md#people)).
- **The root user** of every account gets MFA and no access keys.
- **A budget.** ProtoStrap creates no billing alerts; add an AWS Budget in each account.
- **Service quotas.** A new account's limits are low. Check that the account allows the
  pools' maxima, in vCPUs, for On-Demand (general, ingress, data, and compute's fallback)
  and for Spot (compute):

  ```bash
  aws service-quotas get-service-quota --service-code ec2 --quota-code L-1216C47A   # On-Demand Standard vCPUs
  ```

  ```bash
  aws service-quotas get-service-quota --service-code ec2 --quota-code L-34B43A08   # Spot Standard vCPUs
  ```


## 5. The application's repository

The application's repository holds its spec, its code, its account's Terraform roots,
and workflows that call ProtoStrap's at a version:

```
protostrap.yaml
services/…
infra/
  bootstrap/main.tf         a copy of sample/infra/bootstrap: the account's bootstrap
  prod/main.tf              a copy of sample/infra/test: prod's platform
  people/                   the people module, applied once in the same account
.github/workflows/
  ci.yml                    the checks a pull request must pass
  infra.yml                 like sample-infra.yml, once per environment
  deploy.yml                below
```

In each copied root, point the module `source` at the version, as in
[the table above](#3-publish-protostrap), and set `environment`. In the bootstrap root,
`app` is the spec's `app`, which names the environment's secret, and `github_repository` is
the application's repository.

**The deploy workflow** runs on every push to `main`, once CI has passed on its pull
request. It waits for one approval, then plans, applies infrastructure if it changed, and
deploys to prod:

```yaml
name: deploy

on:
  push:
    branches: [main]

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
    uses: duncan-G/protostrap/.github/workflows/plan.yml@v0.1.1
    with: { environment: prod, protostrap-ref: v0.1.1 }

  infra:
    needs: plan
    if: needs.plan.outputs.infra-changed == 'true'
    uses: duncan-G/protostrap/.github/workflows/infra.yml@v0.1.1
    with: { environment: prod, directory: infra/prod, action: apply, protostrap-ref: v0.1.1 }
    secrets: inherit

  deploy:
    needs: [plan, infra]
    if: always() && needs.plan.result == 'success' && (needs.infra.result == 'success' || needs.infra.result == 'skipped')
    uses: duncan-G/protostrap/.github/workflows/deploy.yml@v0.1.1
    with:
      environment: prod
      version: ${{ needs.plan.outputs.version }}
      builds: ${{ needs.plan.outputs.builds }}
      protostrap-ref: v0.1.1
```

The plan and deploy jobs run in the `prod` GitHub environment, and infra in `prod-infra`.
The approval is its own environment, `prod-approval`, because every job that uses a
protected environment waits for approval separately. Gating once, before the plan, means
one approval per release. The `prod` environment itself only needs to be limited to
`main`.

**GitHub settings.** Bootstrap creates `prod` and `prod-infra`; create `prod-approval` by
hand.

| Setting                                          | Value                                                                            |
| ------------------------------------------------ | -------------------------------------------------------------------------------- |
| `prod-approval`: required reviewers              | Whoever may release to prod                                                      |
| `prod-infra`: required reviewers                 | Whoever may change prod's infrastructure. Applies only when the infra projection changed |
| `prod`, `prod-infra`: deployment branches        | `main` only. The roles trust these environments, so this decides what code can take them on |
| `main`: branch ruleset                           | Pull requests with review, and `ci` passing                                      |
| `CODEOWNERS`                                     | `protostrap.yaml` and `infra/` owned by whoever approves infrastructure          |

**Pin base images by digest** in every Dockerfile, such as
`FROM mcr.microsoft.com/dotnet/aspnet:10.0@sha256:…`. An image's tag is a hash of its
sources, so a second deployed environment builds the same tag prod did, but an unpinned
base image may have changed in between.


## 6. Production settings

### The platform root

`infra/prod/main.tf` differs from `sample/infra/test`, which it copies, only in its
settings. Leave `protect_data_vms` and `force_destroy` at their defaults, true and false.

```hcl
module "protostrap" {
  source = "github.com/duncan-G/protostrap//infra/platform?ref=v0.1.1"

  spec                  = file("${path.module}/../../protostrap.yaml")
  environment           = "prod"
  hosts                 = var.hosts
  cloudflare_account_id = var.cloudflare_account_id
  cloudflare_zone       = var.cloudflare_zone
  alert_emails          = var.alert_emails

  log_retention_days    = 90
  backup_retention_days = 35
  snapshot_retention    = 14
  client_rate_limit     = 300 # requests per client per 10 seconds; size it from real traffic
}
```

Every setting is in [`infra/platform/variables.tf`](../infra/platform/variables.tf). Most
are safe to change later. These are not:

- **`vpc_cidr`** can't change without rebuilding the network. Pick one that won't overlap
  any network prod may ever connect to.
- **Volume sizes** in the spec only grow. Size `postgres`'s `volume` for a year of data.
- **The Cloudflare zone settings** the platform sets, Always Use HTTPS and the minimum TLS
  version, apply to the whole zone, not just the application's hosts.
- **Email domains** can't be shared between accounts, since both would write the same
  `bounce.` records: if a second deployed environment sends mail, its root sets
  `email_domains` to a domain of its own, and prod keeps the spec's. Set `email_dmarc` in
  prod once you know no other mail service publishes DMARC for the domain
  ([Email](email.md#each-account)).

### The spec

- **Survive a VM loss.** A service that must stay up needs `scale.min: 2`, and its pool at
  least two VMs (`min` plus `spare`), so its replicas spread across them.
- **Size the data VM** so PostgreSQL's and Redis's requests fit with room to grow, beside
  the telemetry replica's. Resizing it later takes them offline for a few minutes, in a
  maintenance window.
- **Set telemetry's retention** for what prod must keep, and its `volume` for that many
  days of metrics ([Telemetry settings](telemetry.md#settings)). Retention can change
  later; the volume, like every volume, only grows.
- **Set every maximum** knowingly: a pool's `max` is also the most it can cost.
- **Pin `image` tags** for PostgreSQL and Redis to exact versions.

### One spec, or one per environment

By default every deployed environment runs the same `protostrap.yaml`, so a second one
pays for prod's pool minimums. To size them apart, keep a spec per environment, such as
`protostrap.yaml` for prod and `protostrap.<environment>.yaml` for each other one. Each
environment's platform root reads its own file, and its workflow jobs pass it as `spec`.
The cost is two files to keep in step: run `protostrap check` on both in CI, and change
components in both in the same pull request.

### The reporter

The daily report sends the facts it extracts from the logs, including the first line of
each common error from telemetry's Loki, to Anthropic's API. Error lines can carry user
data. Decide whether
that is acceptable before setting `ANTHROPIC_API_KEY` in prod's `protostrap/reporter`;
without the key, nothing leaves the account and the report is the facts alone.


## 7. Bootstrap and build prod

The same steps as the sample's walkthrough, in the application's account, by an Org Admin:

1. **Bootstrap**, from your machine, with the account's admin profile:

   ```bash
   terraform -chdir=infra/bootstrap init
   ```

   ```bash
   terraform -chdir=infra/bootstrap apply
   ```

2. **The Cloudflare token** for prod's infra workflow, scoped to the prod zone only, with
   the permissions in the [sample's table](../sample/README.md#what-you-need):

   ```bash
   gh secret set CLOUDFLARE_API_TOKEN --env prod-infra
   ```

3. **Infra:** run the application's infra workflow for prod with `action=plan`, read the
   plan, then run it with `action=apply`. Approval on `prod-infra` comes before the job
   runs, so the plan run is the review; the apply plans again, so check it adds what the
   plan said.
4. **Confirm the alert emails.** Each address gets a confirmation email from SNS and
   receives nothing until it is confirmed.
5. **Secrets:** the tunnel token, every component's keys, including PostgreSQL's and
   Redis's passwords, and the reporter's key if wanted, all in the environment's secret
   ([Secrets](secrets.md)):

   ```bash
   scripts/set-edge-token.sh <app> prod
   ```

   ```bash
   scripts/set-secret.sh <app>/prod Api_StripeKey=- Reporter_ANTHROPIC_API_KEY=-
   ```


## 8. Go live

1. **Deploy** by merging to `main` and approving the prod release. The first one installs
   ProtoStrap on every VM, starts the scheduler, runs the migrations and starts the
   services.
2. **Load existing data,** if the application has any elsewhere, before traffic arrives.
   The first release has already run your migrations, so take a data-only dump of the old
   database (`pg_dump --data-only`), upload it to the backups bucket, and pipe it into the
   live PostgreSQL the way [Restoring a backup](#restoring-a-backup) pipes one into a
   scratch one.
3. **Test it on a host of its own first,** such as a `next` host in the spec, which prod's
   `hosts` maps to `new.example.com`: smoke tests,
   then a load test up to the traffic you expect, which also proves the service quotas.
4. **Move the real host.** The platform creates each host's DNS record, and Cloudflare
   refuses a second record with the same name. Either delete the old record and apply at
   once, which leaves the host down until the apply finishes, or have an Org Admin import
   the record, so the apply changes it in place with no gap. With the prod root initialized
   against its state bucket, as its header comment shows, and `CLOUDFLARE_API_TOKEN` set:

   ```bash
   terraform -chdir=infra/prod import 'module.protostrap.cloudflare_dns_record.host["www.example.com"]' <zone id>/<record id>
   ```

   Then give the real host's name the real hostname in `hosts` and apply.
5. **Watch the first day:** the [operator UI](protostrap.md#operator-ui)
   (`scripts/operator-ui.sh`), its Logs view in particular, or `protostrap status` on data-1;
   the services' own logs, traces and metrics in [Grafana](telemetry.md#reading-telemetry)
   (`scripts/telemetry.sh`); and the first daily report.

**Before you open it to traffic:**

- [ ] Every item in [Prove it with the sample](#2-prove-it-with-the-sample) passed, for the
      sample in its test account and for the application in prod.
- [ ] Each gap in [Know what you are accepting](#1-know-what-you-are-accepting) is accepted or closed.
- [ ] Alert emails confirmed, and the [extra alerts](#alerts) in place.
- [ ] If the application sends mail: SES production access granted in prod's account, and a
      test message from prod delivered to an outside mailbox ([Email](email.md#each-account)).
- [ ] A test restore of one of prod's own backups worked.
- [ ] Reviewers set on `prod-approval` and `prod-infra`, and `main` protected.
- [ ] Someone knows how to [roll back](#rolling-back), and has done it in the sample's test
      account, or in prod before go-live.


## 9. Running it

### Releasing

Merge to `main`. The release waits for approval, then goes to prod. A failed rollout rolls
itself back to the last good release; the workflow fails and says why.

### Rolling back

A release that rolled out but is wrong is replaced by deploying an earlier commit. The usual
way is `git revert` and a merge, which goes through approval like any release. To go
faster, re-run the deploy workflow's run for the last good commit: its images are still in
ECR, so nothing is built.

```bash
gh run list --workflow deploy.yml --commit <last good commit>
```

```bash
gh run rerun <run id>
```

Migrations aren't undone. That is why they must stay compatible with the previous release
([Application contract](protostrap.md#application-contract)).

### Changing infrastructure

Change the spec's infrastructure or a root, open a pull request, and merge. The deploy
workflow's plan sees that the infra projection changed and runs the infra workflow first,
which waits for approval on `prod-infra`. Widening what Infra may do is an Org Admin's
change to bootstrap ([Adding to ProtoStrap](identity.md#adding-to-protostrap)).

### Upgrading ProtoStrap

Prove the new version with the sample in its test account
([Prove it with the sample](#2-prove-it-with-the-sample)) and tag it, then in one pull
request change the spec's `protostrap:`, every workflow's `@v…` and `protostrap-ref`, and
every module's `?ref=`. The release updates the scheduler first, then every agent. Like any
release, it waits for approval before prod.

### Rotating a secret

Write the new value with `scripts/set-secret.sh`. ProtoStrap applies it within about three
minutes, only to the components whose keys changed, and in `depends_on` order. Write keys
that must change together, such as PostgreSQL's password and the connection strings that
carry it, in one run. If the new value is wrong, the replicas that still work keep running
until you write the fix ([Changing a value](secrets.md#changing-a-value)).

### Patching VMs

Pool VMs boot the latest Amazon Linux 2023 when they launch, but keep it until they are
replaced. Until the scheduler rolls VMs itself, replace each long-lived VM by hand, one at
a time, a monthly job:

```bash
aws autoscaling terminate-instance-in-auto-scaling-group --instance-id <instance id> --no-should-decrement-desired-capacity
```

The VM is drained before it goes ([When AWS removes a VM](vms.md#when-aws-removes-a-vm)), and
its group launches a replacement. Wait until it shows in `protostrap status` before the
next one. The ingress VM pauses its zone while it is replaced, so do it at a quiet time.

data-1 isn't in a group. Patch it in a maintenance window, from an SSM session, which takes
PostgreSQL, Redis and the scheduler offline for a few minutes:

```bash
sudo dnf upgrade --releasever=latest -y && sudo reboot
```

### Restoring a backup

Every night each stateful component's dump goes to `<component>/<date>.gz` in the backups
bucket, and each volume gets an EBS snapshot, kept for `snapshot_retention` days.

**Test a restore** in a scratch PostgreSQL beside the real one, on data-1. The agent leaves
containers it didn't start alone. A large dump competes with the real PostgreSQL for
data-1's memory and disk; restore it on another machine then.

```bash
image=$(sudo docker inspect --format '{{.Config.Image}}' $(sudo docker ps -q --filter label=protostrap.component=postgres))
sudo docker run -d --name restore-test -e POSTGRES_HOST_AUTH_METHOD=trust "$image"
until sudo docker exec restore-test pg_isready -q; do sleep 1; done
```

```bash
aws s3 cp s3://<backups bucket>/postgres/<date>.gz - | gunzip | sudo docker exec -i restore-test psql -q -U postgres
```

```bash
sudo docker exec restore-test psql -U postgres -c '\l' && sudo docker rm -f restore-test
```

**A real restore** pipes the same dump into the live PostgreSQL, after dropping the
application's database, while nothing writes to it. ProtoStrap can't yet stop the
components that write, and has no procedure for swapping in a snapshot's volume, so a real
restore needs a maintenance window and a plan rehearsed in the sample's test account, or in
prod before go-live.

**Copy the backups** somewhere the account can't delete them, such as another account's
bucket with object lock, if losing the account must not lose the data.

### Alerts

The platform alerts on AWS events: maintenance on a data VM, a data VM failing its checks,
the ingress VM being replaced, and VMs that fail to launch. Before go-live, add:

- **An uptime check** from outside AWS, such as a Cloudflare Health Check or a hosted
  monitor, on a route that reaches the database.
- **Dead letters,** in `infra/prod/main.tf`:

  ```hcl
  resource "aws_cloudwatch_metric_alarm" "dead_letters" {
    for_each            = toset(["orders"]) # every queue in the spec
    alarm_name          = "protostrap-dead-letters-${each.key}"
    namespace           = "AWS/SQS"
    metric_name         = "ApproximateNumberOfMessagesVisible"
    dimensions          = { QueueName = "protostrap-${each.key}-dlq" }
    statistic           = "Maximum"
    period              = 300
    evaluation_periods  = 1
    threshold           = 0
    comparison_operator = "GreaterThanThreshold"
    alarm_actions       = [module.protostrap.alerts_topic]
  }
  ```

- **Errors and latency** from the services, as Grafana alert rules or a hosted monitor:
  ProtoStrap provisions none, and Grafana's own rules would need a place to send to.
- **Disk space** on the stateful and telemetry volumes, and **failed backups**, which the agent logs to
  the data VM's journal and the [infra log](protostrap.md#infra-log), where the daily
  report counts them. An alert on either needs something ProtoStrap doesn't install yet:
  the CloudWatch agent for disk space, and an alarm on the infra log for backups.

### Every day

Read the daily report, `reports/<date>.md` in the deploy bucket. It lists what happened and
what to change, such as a component stuck at its maximum or a request that is too small.
Its changes are edits to `protostrap.yaml`, made through a pull request like any other.
