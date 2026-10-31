# The sample

A small application with one component of every kind, used to test ProtoStrap end to end:
bootstrapping an account, building its infrastructure, deploying, and scaling replicas and
VMs. Nothing in ProtoStrap knows about it; it is deployed the way any application is.

| Path                                     | What it is                                                                 |
| ---------------------------------------- | -------------------------------------------------------------------------- |
| [`protostrap.yaml`](protostrap.yaml)     | The spec: api (.NET gRPC), web (Node), py (FastAPI), jobs (.NET worker), migrate (task), postgres and redis |
| [`services/`](services)                  | Their code, each with a knob that makes load ([services/README.md](services/README.md)) |
| [`load/`](load)                          | A load generator, Go standard library only                                 |
| [`infra/bootstrap/`](infra/bootstrap)    | Bootstrap for the test account: roles, state and deploy buckets, GitHub environments |
| [`infra/test/`](infra/test)              | The test account's platform, built from the spec                           |
| [`set-secrets.sh`](set-secrets.sh)       | Fills in the sample's secret, and rotates its passwords                    |
| [`compose.yaml`](compose.yaml)           | Runs the services on your machine, without AWS                             |

The workflows that build and deploy it are
[`sample-infra.yml`](../.github/workflows/sample-infra.yml) and
[`sample-deploy.yml`](../.github/workflows/sample-deploy.yml). An application's own
repository has copies of them that call ProtoStrap's reusable workflows at a version.


## What you need

- **An AWS account to test ProtoStrap in**, used for nothing else: ProtoStrap names its
  resources on the assumption of one application per account. You need administrator
  credentials for it, as an Org Admin ([People](../docs/identity.md#people)).
- **A Cloudflare account** with the domain the sample's host goes in, such as
  `example.com`, already added as a zone, and a host name for the sample, such as
  `sample-test.example.com`. Nothing needs to exist at that host yet.
- **This repository on GitHub**, with Actions enabled. The sample's workflows run from it.
- **On your machine:** Terraform 1.10 or later, the AWS CLI v2 with the Session Manager
  plugin, the GitHub CLI (`gh`), `jq`, `curl`, and Go for the load generator. Docker only
  to run the services locally.

**A Cloudflare API token** for the infra workflow. Create it under My Profile → API Tokens,
with these permissions, scoped to the account and the zone:

| Permission                              | For                                                 |
| --------------------------------------- | --------------------------------------------------- |
| Account → Cloudflare Tunnel → Edit      | The tunnel and its routes, and fetching its token   |
| Zone → DNS → Edit                       | A record per host, pointing at the tunnel           |
| Zone → Zone Settings → Edit             | Always use HTTPS, and the minimum TLS version       |
| Zone → Zone → Read                      | Finding the zone                                    |
| Zone → Zone WAF → Edit                  | Only with `client_rate_limit` set                   |
| Account → Access: Apps and Policies → Edit | Only with `operator_hosts` set                   |


## 1. Bootstrap the account

An Org Admin does this once, from their own machine. It creates the Terraform state
bucket, GitHub's OpenID Connect provider, the Infra and Deploy roles with their
[boundaries](../docs/identity.md#identity-boundaries), and the two GitHub environments the
workflows run in: `test` for deploys and `test-infra` for infrastructure, each with the
variables its workflow needs. After this, no one needs admin credentials to deploy.

```bash
cd sample/infra/bootstrap
cp terraform.tfvars.example terraform.tfvars    # fill in the repository, region, Cloudflare account, zone and host
export AWS_PROFILE=<an admin profile for the test account>
export GITHUB_TOKEN=$(gh auth token)            # admin rights on the repository
terraform init
terraform apply
```

Then give the infra workflow its Cloudflare token, as a secret of `test-infra`:

```bash
gh secret set CLOUDFLARE_API_TOKEN --env test-infra
```

Its state stays on your machine (it is gitignored). Everything it made can be imported
again if it is lost. To gate infrastructure changes, add required reviewers to the
`test-infra` environment in GitHub. To set up sign-in for people, see
[`infra/people`](../infra/people/main.tf).


## 2. Build the infrastructure

Run the infra workflow, which uses the Infra role and nothing of yours. Plan first, then
apply:

```bash
gh workflow run sample-infra.yml -f action=plan
```

```bash
gh workflow run sample-infra.yml -f action=apply
```

It builds the network, the buckets, the queue, a secret, a log group, a role and an image
repository per component, the Cloudflare tunnel and its DNS record, and the VMs: one
ingress VM, the general pool at its minimum, an empty compute pool and data-1. The
workflow runs only by hand: after a change to the sample's spec or infrastructure, run it
again.

The VMs boot and wait: there is no `protostrap` binary in the account until the first
deploy puts one there.


## 3. Fill in the secrets

Terraform makes every secret empty, so no value passes through its state. An Org Admin fills
them in, with the same admin profile:

```bash
export CLOUDFLARE_ACCOUNT_ID=<the account id> CLOUDFLARE_API_TOKEN=<the token from above>
scripts/set-edge-token.sh sample test
```

```bash
sample/set-secrets.sh
```

Both write to the environment's secret, `sample/test` ([Secrets](../docs/secrets.md)). The
first puts in the tunnel's token; the second makes passwords for PostgreSQL and Redis, and
the connection strings the components read. Run the second again to rotate both passwords:
PostgreSQL and Redis apply theirs in place, then the components that connect are replaced.
For the daily report to be written by an LLM rather than be facts alone, add an Anthropic
API key (it asks for the value, so it stays out of your shell history):

```bash
scripts/set-secret.sh sample/test Reporter_ANTHROPIC_API_KEY=-
```


## 4. Deploy

The deploy workflow runs only by hand:

```bash
gh workflow run sample-deploy.yml
```

The deploy workflow builds the images missing from ECR, copies the `protostrap` binary,
the edge image and the PostgreSQL and Redis images into the account, writes the release,
and tells the scheduler on data-1 to roll it out. The first time, every VM installs the
binary and starts its agent, data-1 starts the scheduler, the migrate task creates the
tables, and then the services start. Expect 10 to 15 minutes, most of it building images.

Then, with your host:

```bash
curl https://sample-test.example.com/api/hello
curl https://sample-test.example.com/web/hello
curl https://sample-test.example.com/py/count
```

**Watching it.** The scheduler's own view is `protostrap status` on data-1:

```bash
aws ssm start-session --target $(aws ec2 describe-instances \
  --filters Name=tag:Name,Values=protostrap-data-1 Name=instance-state-name,Values=running \
  --query 'Reservations[0].Instances[0].InstanceId' --output text)
```

```bash
sudo protostrap status
```

It lists each component's desired, ready and starting replicas, each pool's VMs, and the
release being rolled out. Every decision it makes, a snapshot of every signal once a
minute, and every agent's records are in the [infra log](../docs/protostrap.md#infra-log),
under `logs/` in the deploy bucket; each component's own logs, traces and metrics are in
[telemetry](../docs/telemetry.md), which `scripts/telemetry.sh` opens in Grafana. The daily report is `reports/<date>.md` in the deploy
bucket, and `sudo protostrap report --print` on data-1 prints one now.

The [operator UI](../docs/protostrap.md#operator-ui) shows the same in a browser, live,
with why each component wants the replicas it has and the infra log. From your machine:

```bash
scripts/operator-ui.sh
```

Then open http://localhost:7480. To have an agent such as Claude Code read the infra log,
point it at `curl -s http://localhost:7480/api/help` while the forward is open.


## 5. Test deploying and scaling

Run the load generator from the repository root against your host. The numbers below
assume the spec as it is; the reasoning behind each is in
[Exercising each scaling path](services/README.md#exercising-each-scaling-path).

| To see                         | Do                                                                                             | Expect                                                                    |
| ------------------------------ | ---------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| Replicas scaling on CPU        | `go run ./sample/load steps -url https://HOST/api/burn?ms=100 -steps 5,10,20,5 -step 5m`       | api goes 2, 3, 6, then back down after a few minutes                      |
| VMs scaling with them          | The same                                                                                       | The general pool adds VMs as api passes 3 replicas, and removes them later |
| Requests per second            | `go run ./sample/load steps -url https://HOST/py/hello -steps 20,60,120,40 -step 5m`           | py goes 1, 3, 6, 2                                                        |
| A custom metric                | `go run ./sample/load http -url https://HOST/web/block?ms=50 -rps 40 -duration 10m`            | web scales on event loop lag, with little CPU                             |
| A worker from zero, on Spot    | `go run ./sample/load jobs -url https://HOST/api/jobs -count 1200 -work-ms 1000`               | jobs goes 0 to 10, in spare room on general VMs until compute VMs boot, drains the queue, and goes back to 0 |
| A limit holding a worker back  | Set `JOBS_BUDGET: "1500"` on api and jobs, deploy, enqueue 2000                                | jobs scales to 0 at 1500 while the rest wait in the queue                 |
| A deploy without errors        | Keep a `load http` running while you deploy a change to that service                           | Replicas are replaced a quarter at a time, with no errors                 |
| A migration                    | Add a step to [`Migrations.cs`](services/dotnet/Migrate/Migrations.cs) and deploy              | migrate runs before any new api or jobs replica                           |
| A rollback                     | Deploy a change that makes api exit at startup                                                 | The release fails and the previous one is back, with no new replica serving |
| Only what changed redeploys    | Change web's code and deploy                                                                   | Only web's image is built, and only its replicas are replaced             |

Scaling down is slow on purpose: replicas come down only to the highest demand of the last
few minutes, and a VM goes only once it has been unneeded for a while. Cloudflare may
challenge a lot of traffic from one address; run the load from inside the account if it
does.


## 6. Tear down

```bash
gh workflow run sample-infra.yml -f action=destroy
```

The sample's test account sets `protect_data_vms = false` and `force_destroy = true`, so this
removes everything, the buckets and images included. Then, to remove bootstrap too, with
the admin profile:

```bash
terraform -chdir=sample/infra/bootstrap destroy
```

Empty the state and deploy buckets first, every object version included: they keep the
platform's state history, and every release and protostrap binary.


## Running it locally

`docker compose -f sample/compose.yaml up --build` runs the services, PostgreSQL and Redis
on your machine, without AWS or a queue. See
[Running locally](services/README.md#running-locally).
