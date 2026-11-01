# ProtoStrap's skills

A [Claude Code](https://claude.com/claude-code) plugin for the repositories of applications
deployed with ProtoStrap. Its skills tell an agent working in such a repository what
ProtoStrap expects of it, at the version the application uses, so the agent writes a spec
that passes `protostrap check`, code that keeps the
[application contract](../../docs/protostrap.md#application-contract), and pull requests that
say what they will restart or provision.

| Skill                                                        | For                                                                                    |
| ------------------------------------------------------------ | -------------------------------------------------------------------------------------- |
| [`protostrap-setup`](skills/protostrap-setup/SKILL.md)       | Adding ProtoStrap to a repository, adding a deployed environment such as prod, upgrading ProtoStrap |
| [`protostrap-spec`](skills/protostrap-spec/SKILL.md)         | Writing and changing `protostrap.yaml`: kinds, sources, requests, routes, queues, buckets, scaling, config files and configure tasks, stateful components and their backups |
| [`protostrap-aspire`](skills/protostrap-aspire/SKILL.md)     | Describing the application in an Aspire AppHost with ProtoStrap.Hosting, and generating the spec |
| [`protostrap-contract`](skills/protostrap-contract/SKILL.md) | Service, worker and task code: keys, readiness, draining, jobs, uploads, what the request path can't carry, scheduled work, migrations, configure tasks, for .NET, Node.js and Python |
| [`protostrap-telemetry`](skills/protostrap-telemetry/SKILL.md) | Logs, traces and metrics from code: OpenTelemetry for .NET (ServiceDefaults included), Node.js and Python, one log path, traces through queues, metrics for scaling, the spec's `telemetry` section |
| [`protostrap-deploy`](skills/protostrap-deploy/SKILL.md)     | Shipping a change: what comes before merging, what the workflow does from approval to prod, watching the run, confirming the release serves |
| [`protostrap-secrets`](skills/protostrap-secrets/SKILL.md)   | Naming, adding and rotating keys in the environment's secret, without the agent handling a value |
| [`protostrap-operate`](skills/protostrap-operate/SKILL.md)   | Finding out why a deployment behaves as it does, read-only, through the operator API and Grafana, and fixing it through the repository |
| [`protostrap-email`](skills/protostrap-email/SKILL.md)       | Sending mail with SES: the spec's `email`, sending from code with the component's role, Keycloak, local runs, production access |

They lean on each other: a new worker touches the spec (or the AppHost), the contract and
perhaps a secret, and each skill names the next one.


## Using them in an application

Install the plugin at the same version as everything else the application takes from
ProtoStrap. In the application's `.claude/settings.json`, committed, so everyone working on
it gets the same skills:

```json
{
  "extraKnownMarketplaces": {
    "protostrap": {
      "source": { "source": "github", "repo": "duncan-G/protostrap", "ref": "v0.1.4" }
    }
  },
  "enabledPlugins": {
    "protostrap@protostrap": true
  }
}
```

Or for yourself, in a Claude Code session:

```
/plugin marketplace add duncan-G/protostrap#v0.1.4
/plugin install protostrap@protostrap
```

Upgrading ProtoStrap moves this `ref` with the spec's `protostrap:`, the workflows' `@v` and
the Terraform roots' `?ref=` (the `protostrap-setup` skill lists them all).

Before a version is on GitHub, or to try changes to the skills, use a ProtoStrap checkout
instead, for yourself only. From the application's repository:

```bash
claude plugin marketplace add ../ProtoStrap --scope local
```

```bash
claude plugin install protostrap@protostrap --scope local
```

That writes `.claude/settings.local.json`, which overrides the committed marketplace; keep it
out of git, as ProtoApp's `.gitignore` does.


## What they hold, and what they don't

- **Rules and steps an agent gets wrong without them,** such as readiness that answers only
  for its own name, a keep-alive longer than the edge's, `uses` building the role, sources
  building the tag, never editing a generated spec, never handling a secret's value.
- **Pointers, not copies,** for the rest: each skill names the documents in `docs/` that are
  the ground truth, and tells the agent to read them at the application's version, from a
  checkout in `.protostrap/` or `../ProtoStrap`, or on GitHub at the tag.
- **Nothing that acts on an account.** Bootstrap, the infra apply, secret values and deploys
  stay with people and the workflows. The skills have the agent write the commands for an
  Org Admin to run.


## Keeping them true

They are part of ProtoStrap, and change with it. A pull request that changes the spec's
fields, the contract, the workflows' inputs, the Terraform modules' variables, the secret's
layout or ProtoStrap.Hosting's API changes the skill that describes it, in the same pull
request. The skill for each:

| A change to                                                  | Update                                                   |
| ------------------------------------------------------------ | -------------------------------------------------------- |
| `internal/spec`, `docs/protostrap.md#spec`, `docs/scaling-examples.md`, `docs/vms.md#data` | `protostrap-spec` and its references |
| `docs/protostrap.md#application-contract`, `internal/edge`, `internal/probe`, the sample services | `protostrap-contract` and its references |
| `hosting/`, `docs/protostrap-aspire.md`                      | `protostrap-aspire`                                      |
| `docs/secrets.md`, `internal/secret`, `scripts/set-secret.sh` | `protostrap-secrets`                                    |
| `.github/workflows/{plan,infra,deploy}.yml`, `infra/*/variables.tf`, `infra/people/main.tf`'s variables, `docs/production.md` | `protostrap-setup` and its templates, `protostrap-deploy` |
| `docs/telemetry.md`, `internal/telemetry`, the `OTEL_` variables the scheduler sets, the sample services' telemetry | `protostrap-telemetry`, and `protostrap-operate`'s Grafana section |
| `internal/ui`, `scripts/operator-ui.sh`, `scripts/telemetry.sh`, `infra/people/main.tf`'s permission sets, the scheduler's reasons | `protostrap-operate` |
| `cmd/protostrap/deploy.go`, `internal/release`, `docs/protostrap.md#deploy-a-release` | `protostrap-deploy`                          |
| `docs/email.md`, `internal/spec/email.go`, `infra/platform/email.tf`, `hosting/src/ProtoStrap.Hosting/ProtoStrapEmail.cs` | `protostrap-email` |

Check the manifests with `claude plugin validate .` and `claude plugin validate plugins/protostrap`.
