---
name: protostrap-operate
description: Find out why a ProtoStrap deployment behaves as it does, and fix it through the repository - a deploy that failed or rolled back, replicas that aren't ready or wait as pending, a component that won't scale or is stuck at its maximum, a secret change that's stuck, errors in a component's logs, slow requests, jobs in a dead-letter queue, or the daily report's recommendations. Covers opening the forwards and checking they're open, an order for finding the cause, the deploy workflow's logs, the operator UI and its logs API (localhost:7480/api/help), Grafana's logs, traces and metrics (localhost:7482/protostrap/help), dead-letter queues, protostrap status, the daily report, and rolling back. Use whenever someone asks what is running, why, or what went wrong in a ProtoStrap environment.
---

# Operating a ProtoStrap application

ProtoStrap's scheduler makes every decision and logs each one with its reason. Read those
reasons before guessing. **Everything here is read-only.** Fixes are commits: an edit to the
spec, the code or a Terraform root, through a pull request and the deploy workflow.

## Rules for you

- **Look, don't touch.** Don't terminate instances, change Auto Scaling groups, run
  `protostrap deploy`, write secrets, or re-run a production deploy unless the user asks for
  that exact action. Prod's approval stays with the people who hold it.
- **Credentials are the user's.** The operator UI and AWS reads need their Identity Center
  sign-in (`AWS_PROFILE`). Ask them to open the forward, or run it only if they say so.
- **Never print secret values.** No tool here shows them; don't go looking.

## Where to look

| Question                                           | Look at                                                          | Who can          |
| -------------------------------------------------- | ---------------------------------------------------------------- | ---------------- |
| Did the deploy fail, and why?                      | The deploy workflow's run: `gh run list --workflow deploy.yml`, then `gh run view <id> --log-failed` | Anyone with the repository |
| What runs where, what each component wants, why    | The operator UI, `/api/status`                                   | Platform Admins, Org Admins |
| What the scheduler and agents decided and saw      | The infra log, `/api/logs`                                       | Platform Admins, Org Admins |
| What happened yesterday, what to change            | The daily report, `/api/reports`                                 | Platform Admins, Org Admins |
| What a component itself printed, its traces and metrics | Grafana: Loki, Tempo and Prometheus                          | Developers, Platform Admins, Org Admins |
| The scheduler's own view, on the VM                | `sudo protostrap status` on data-1, through an SSM session        | Org Admins       |

Only Platform Admins and Org Admins can open the operator UI. Platform Admins sign in to
every environment with view-only access and the UI's read-only forward; Developers can't
open it, so a Developer asks a Platform Admin. Grafana is the other way in: it holds the
services' own logs, traces and metrics, never ProtoStrap's records, and Developers open it
in each deployed environment with their Developer sign-in, which also has view-only access.
Ask an Org Admin for the rest.

### Opening the forwards

The forwards are scripts in ProtoStrap, at the spec's version (`.protostrap/scripts/` or
`../ProtoStrap/scripts/`). They need the AWS CLI and its Session Manager plugin, and the
user's Identity Center sign-in. The user runs them, in a terminal of their own, and leaves
them open; run one yourself only if they ask.

| Script                          | Serves                    | For                                              |
| ------------------------------- | ------------------------- | ------------------------------------------------ |
| `scripts/operator-ui.sh`        | `http://localhost:7480`   | The operator UI and its API: Platform Admins and Org Admins |
| `scripts/operator-ui.sh --admin` | `http://localhost:7481`  | The same, on the port that will hold Org Admins' actions: Org Admins |
| `scripts/telemetry.sh`          | `http://localhost:7482`   | Grafana: Developers, Platform Admins, Org Admins |

```bash
AWS_PROFILE=<their profile> AWS_REGION=<region> ../ProtoStrap/scripts/operator-ui.sh
```

If the account runs more than one application, they also set `PROTOSTRAP_APP`. **Before
querying, check what is open:**

```bash
curl -s --max-time 3 http://localhost:7480/api/whoami
```

```bash
curl -s --max-time 3 -o /dev/null -w '%{http_code}\n' http://localhost:7482/protostrap/help
```

`whoami` answers `{"role":"platform_admin"}` or `org_admin`. A refused connection means
that forward isn't open: ask the user to open it, and say which one.

### The operator UI and its logs API

**Read the API's help first**, since it describes the filters of the version that's running:

```bash
curl -s http://localhost:7480/api/help
```

```bash
curl -s 'http://localhost:7480/api/logs?component=api&level=warn&since=1h&format=text'
```

`/api/status` is the scheduler's status as JSON: pools, VMs, replicas, pending replicas,
rollout, each component's desired count with its signals and limits. `/api/reports` lists
the daily reports, `/api/reports/<date>` returns one. `snapshots=true` on `/api/logs` adds
the scheduler's signal readings, one a minute, to see why a count changed when it did.

### Component logs, traces and metrics

**Read Grafana's help first:**

```bash
curl -s http://localhost:7482/protostrap/help
```

```bash
curl -sG http://localhost:7482/api/datasources/proxy/uid/loki/loki/api/v1/query_range \
  --data-urlencode 'query={service_name="api"} |~ "(?i)error|exception"' --data-urlencode since=1h
```

Every line carries its component as `service_name` and its replica as
`service_instance_id`, so one that failed at startup can be found from the replica id in the
infra log. The release is `service_version`, a filter after the braces in Loki
(`{service_name="api"} | service_version="<commit>"`), not a label inside them. Replicas
started before the environment had telemetry still log to CloudWatch,
`/protostrap/<app>/<component>`, which only Org Admins can read.

## Finding out what's wrong

Go from what ProtoStrap decided to what the component said, in this order, and stop when
the cause is plain:

1. **The deploy, if one was involved:** `gh run view <id> --log-failed`. A rollback's
   reason names the component and what failed (protostrap-deploy).
2. **What the scheduler sees:** `/api/status`. Which components have fewer ready replicas
   than desired, which replicas are pending, starting or failing, what each signal reads.
3. **What it decided and why:** `/api/logs?component=<name>&level=warn&since=6h&format=text`.
   Note each failed replica's id and reason: exit code 137 is out of memory, "not ready
   within" is the readiness check, and an exit soon after start is the program.
4. **What the replica said:** in Loki, `{service_instance_id="<replica id>"}` around that
   time, stderr included. A crash's stack trace is there.
5. **Where a slow or failing request went:** a trace ID from a log line, or a Tempo search
   on the component, then `/api/traces/<id>`.
6. **What changed:** `git log` since the last good release; the commit is the release.

A Developer has steps 1, 4, 5 and 6, and asks a Platform Admin for 2 and 3. Say what you
found with the records that show it, then the fix as a change to the repository.

## Dead-letter queues

A job that failed `max_receives` times is in `protostrap-<queue>-dlq`, or
`protostrap-<queue>-dlq.fifo` for a FIFO queue. With a sign-in that can read queue
attributes:

```bash
aws sqs get-queue-attributes --queue-url "$(aws sqs get-queue-url --queue-name protostrap-<queue>-dlq --query QueueUrl --output text)" --attribute-names ApproximateNumberOfMessagesVisible
```

- **Don't receive from it to look:** receiving changes the jobs, and its bodies may hold
  people's data. The worker's errors for those jobs are in Loki.
- **Moving them back** to the queue, once a fix is deployed, is an Org Admin's action:
  `aws sqs start-message-move-task --source-arn <dead-letter queue's ARN>`. Write it for
  them; the jobs must be safe to run again (protostrap-contract).
- Production should alarm on it (protostrap-setup's production list).

## Symptoms

| Symptom                                                         | Usually                                                                                              | Fix in                                    |
| --------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- | ----------------------------------------- |
| Deploy "failed and rolled back": a replica not ready in 5 minutes | The readiness check: wrong `port` or `ready` path or port, the check not answering for its own name (ASP.NET's gRPC health maps only `""` by default), h2c missing for `ready: grpc` or `protocol: http2`, a `ready` command failing or over 5 s; or the replica crashing at start. The infra log names the replica; its lines in Loki say why | Code (protostrap-contract) or spec        |
| Same, and the log says a key is missing                          | A key in `secrets` that nobody set in this environment                                                | An Org Admin sets it (protostrap-secrets), then redeploy |
| Deploy failed in the migrate task                               | The migration's own error, in the task's lines in Loki                                                | The migration; fix forward                |
| Deploy "superseded"                                             | A newer release arrived; not an error                                                                 | —                                         |
| `plan` fails: spec isn't what the AppHost writes                | Someone changed the AppHost or a project reference without regenerating                               | `aspire do protostrap-spec`, commit       |
| `plan` or `infra` fails `protostrap check`                      | The spec breaks a rule; the message names it                                                         | Spec (protostrap-spec)                    |
| `infra` fails AccessDenied in Terraform                         | The change needs something outside the Infra boundary                                                 | An Org Admin widens bootstrap's `extra_infra_actions` |
| AccessDenied at runtime                                         | The queue or bucket isn't in the component's `uses`                                                   | Spec `uses`, then the infra change applies |
| Replicas pending, no VM added                                   | The pool is at `max`, or the request doesn't fit any VM type in it                                    | Pool `max`, VM type, or the request       |
| Replicas pending while totals fit                               | Fragmentation: requests too big for the VM size                                                       | Smaller requests or a larger VM type      |
| A worker stays at 0 with jobs queued                            | A limit at 0 (budget), the `queue` signal naming another queue, or the worker crash-looping           | Read `/api/status` for its signals and caps |
| A component pinned at `max`, latency up                         | Its demand exceeds `max`, or its request is too small                                                 | `max` or `request`, as the report says     |
| A `metric` limit ignored                                        | No value in the last 5 minutes: the exporting component isn't running, or `metrics` is wrong         | Export it from a component that always runs |
| Errors during every deploy                                       | No draining on SIGTERM, `stop` left at its 10 s default, or a keep-alive timeout under 60 s           | Code (protostrap-contract), or the spec's `stop` |
| Jobs processed twice or lost                                    | Not idempotent, deleted before done, or `visibility` shorter than the job                             | Code, or the queue's `visibility`         |
| Secret change not applied                                       | It waits 2 minutes for the secret to settle, a rollout in progress, or new replicas failing with it; `/api/status` says which | Write the fix; there is no going back |
| An image didn't rebuild after a change                          | The changed path isn't in the component's `sources`                                                   | `sources`, or the AppHost's `WithProtoStrapSources` |
| Every log line appears twice                                    | The component logs to stdout and exports logs over OTLP                                               | Code (protostrap-telemetry)               |
| A custom metric or span never shows in Grafana                  | Its `Meter` or `ActivitySource` isn't registered (.NET), or it was only ever sent locally             | Code (protostrap-telemetry)               |
| A trace stops at a queue                                        | The sender doesn't inject the context into the message, or the worker doesn't ask for its attributes | Code (protostrap-telemetry)               |
| Jobs piling up in a dead-letter queue                           | The worker fails on them: its errors in Loki say why                                                  | Code, then an Org Admin moves them back (above) |

## Rolling back

A rolled-out release that's wrong is replaced by deploying an earlier commit. Ask the user
which they want:

- **`git revert`** the change and merge. It goes through approval like any release.
- **Faster:** re-run the deploy workflow's run for the last good commit. Its images are still
  in ECR, so nothing is built. Prod still waits for its approval.

  ```bash
  gh run list --workflow deploy.yml --commit <last good commit>
  ```

Migrations aren't undone, which is why they stay compatible with the previous release. A
secret is never rolled back: write the right value.

## The daily report

Its "What to change" items are spec edits: a component stuck at its maximum, a request too
small or too large, a data VM running short, a component meeting a "Move when" on the path to
managed services. Make each one as a pull request with the report's numbers in its
description, and say what the change restarts (protostrap-spec's "What a change does").
