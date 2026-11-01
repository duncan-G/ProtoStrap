---
name: protostrap-deploy
description: Ship a change to an application deployed with ProtoStrap and confirm it landed - what must happen before merging (secret values, retired lists, a regenerated spec, an Org Admin's bootstrap change), what the pull request says, whether it runs Terraform, how the deploy workflow's plan, infra, build and deploy jobs run in prod after approval, watching the run and reading its result, confirming the new release serves, a new component's first deploy, deploying again, and where to go when it fails. Use when asked to deploy, release or ship a change, to get a new component, queue or bucket running in an environment, when a merged change doesn't seem to be live, or to check whether a deploy worked.
---

# Deploying

A deploy is a commit on `main`. The application's deploy workflow turns it into a release,
the whole application at that commit, and ProtoStrap's scheduler rolls it out. Nobody deploys
from a laptop, and nothing reaches an account any other way.

The design is `docs/protostrap.md#deploy-workflow` and `#deploy-a-release`, and
`docs/production.md#9-running-it`, at the spec's `protostrap:` version.

## Rules for you

- **Merging and approving are people's.** Don't merge, push to `main`, approve a
  `prod-approval` or `prod-infra` job, or re-run a prod deploy unless the user asks for that
  exact action.
- **Never deploy around the workflow:** no `protostrap deploy`, no SSM command to data-1, no
  `terraform apply` from your machine, no image pushed by hand.
- **Never skip a failing check** to get a change out: fix what it names.

## The path of a change

```
pull request ── ci: tests, spec drift, protostrap check, terraform fmt and validate
merge to main
  approve  prod-approval: a person approves the release for prod
  plan     protostrap check; the spec regenerated if an AppHost writes it; the images
           missing from ECR; whether the infra projection changed since the last apply
  infra    only if it changed, after prod-infra's own approval: Terraform applies
           infra/prod (ECR repositories, roles, queues, buckets, DNS, VMs)
  deploy   builds the missing images (arm64), copies ProtoStrap's binary, the edge and
           telemetry images the spec runs, and upstream images, writes
           releases/<commit>.json, and has data-1's scheduler roll it out, waiting for the
           result
```

The rollout: changed components replace a quarter of their replicas at a time, migrations
run before the first new replica, configure tasks after the component is ready, and a
stateful component stops before its replacement starts. **Any failure rolls the whole
release back** to the last good one, and the workflow fails with the reason. Unchanged
components aren't touched.

## Before merging

| The change                                          | First                                                                          |
| --------------------------------------------------- | ------------------------------------------------------------------------------ |
| A new key in a component's `secrets`                | An Org Admin sets it in every environment (protostrap-secrets). Otherwise the new replicas aren't ready and the release rolls back |
| A component or queue removed                        | `retired_components` or `retired_queues` in every account's root (protostrap-spec) |
| A bucket removed                                    | `retired_buckets`, or it is deleted; in prod the apply fails unless it's empty  |
| A stateful component removed or renamed             | An Org Admin's `terraform state rm` of its volume in each account; until then the infra plan fails, by design (protostrap-spec's `references/stateful.md`) |
| The AppHost, a project reference, `Directory.*.props` | `aspire do protostrap-spec`, and the spec committed with it (protostrap-aspire) |
| Something outside the Infra boundary, such as a managed database | An Org Admin widens bootstrap's `extra_infra_actions` and applies it      |
| A migration                                         | It works with the previous release's code, which runs during the rollout and after a rollback |
| A new ProtoStrap version                            | Every reference moved together (protostrap-setup's upgrade steps)              |
| `tunnel: {}` added                                  | Plan the tunnel's token: once Terraform has made the tunnel, an Org Admin sets `Edge_TUNNEL_TOKEN` with `scripts/set-edge-token.sh` (protostrap-secrets), before the deploy. Without it the new edge waits for the token and never gets ready, so the release rolls back after 5 minutes and the old edge keeps serving |

Write each first step in the pull request, with its commands, for whoever does it.

## What it will do

Say in the pull request what merging restarts or provisions (protostrap-spec's "What a change
does"), and whether it runs Terraform, which in prod waits for a second approval. To see both,
compare `protostrap plan` on `main` and on the branch, with the CLI at the spec's version
(protostrap-spec's "Check it"):

```bash
git worktree add /tmp/plan-main main && (cd /tmp/plan-main && /tmp/protostrap plan --spec protostrap.yaml) > /tmp/plan-main.txt; git worktree remove /tmp/plan-main
```

```bash
/tmp/protostrap plan --spec protostrap.yaml | diff /tmp/plan-main.txt -
```

- A changed `infra` hash on the first line: **the infra job runs.**
- A changed tag on a component's line: **its image is built** and its replicas replaced.
- `plan` hashes what git tracks (`git ls-files -s`): stage or commit the change first, or
  it isn't counted. A file that changed and whose tag didn't is missing from `sources`.

Changes that replace replicas without a new image (`env`, `request`, `secrets`…) don't show
in the tags: read the spec's diff against the table.

## Watching it

```bash
gh run list --workflow deploy.yml --branch main --limit 5
```

```bash
gh run watch <run id> --exit-status
```

```bash
gh run view <run id> --log-failed
```

The deploy job's "Deploy it" step prints the scheduler's progress, then one of:

| It ends with                                                   | Means                                                         |
| -------------------------------------------------------------- | ------------------------------------------------------------- |
| `done: <commit> runs`, then `status: Success`                  | Every changed component rolled out and is ready               |
| `protostrap deploy: failed and rolled back to "<commit>": <reason>` | The release is undone. The reason names the component and what failed |
| `protostrap deploy: superseded: <reason>`                      | A newer commit's release took over. Not an error              |
| `data-1 isn't running: apply the infra workflow first`         | A new environment, or data-1 is stopped                       |

The full output is in the CloudWatch log group `/protostrap/<app>/deploy`.

## Confirming it landed

A green run means the release rolled out. To see that it serves:

- **Its replicas run the commit:** in Grafana (protostrap-operate says how to open it),
  `{service_name="<component>"} | service_version="<commit>"` has lines, and none with
  errors since the rollout. (`service_version` is a filter after the braces, not a label
  inside them.) Without the spec's `telemetry`, an Org Admin reads its CloudWatch log group
  instead.
- **The scheduler agrees** (Platform and Org Admins):
  `curl -s 'http://localhost:7480/api/logs?source=scheduler&q=release&since=2h&format=text'`
  shows `release finished`, and `/api/status` the commit as the release.
- **It answers:** a request to a route it serves, from outside, when the spec has a tunnel.
  Not its `ready` path: that answers only for its own name on the overlay, so from outside
  it returns 503.

## A new component's first deploy

- The spec change makes **infra run first**: its ECR repository, IAM role, and any new
  queue or bucket. Then its image builds, and the scheduler starts it once everything in
  its `depends_on` has a ready replica.
- Its keys are set before the merge (above). If it needs a database or user of its own in
  PostgreSQL, something in the release creates it, such as a configure task on PostgreSQL,
  never a manual step on data-1.
- **A worker with `min: 0` stays at zero until its queue has jobs.** That is deployed, not
  broken.
- Its `gateway.upstreams` routes reach it once it is ready; the gateway reloads them without
  a restart.

## Deploying again

- **The same commit**, after a fix outside the repository (a secret value, an Org Admin's
  bootstrap change): `gh workflow run deploy.yml`, or `gh run rerun <run id> --failed`.
  Nothing is rebuilt when its images are already in ECR.
- **An earlier commit**, to roll back: protostrap-operate's "Rolling back".

Prod waits for its approval every time.

## When it fails

| Job       | Usually                                                                        | Go to                                  |
| --------- | ------------------------------------------------------------------------------ | -------------------------------------- |
| `plan`    | `protostrap check` names a rule the spec breaks, or the committed spec isn't what the AppHost writes | protostrap-spec, protostrap-aspire |
| `infra`   | AccessDenied: the change needs an Org Admin's bootstrap change; or Cloudflare's token | protostrap-operate's symptoms   |
| `build`   | The Dockerfile, a native dependency without an arm64 build, a missing `global.json`, a file outside `sources` | protostrap-contract's "The image" |
| `deploy`  | Rolled back: a replica not ready in 5 minutes, a migration or configure task that failed, a missing key | protostrap-operate |
