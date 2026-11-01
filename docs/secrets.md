# Secrets

Each environment has one secret in AWS Secrets Manager, `<app>/<environment>`, such as
`shop/prod`. It holds every component's keys: connection strings, API keys, passwords, the
edge's tunnel token and the reporter's LLM key. Terraform creates it empty, and an Org
Admin sets the keys, so no value ever passes through Terraform's state, CI or an image.

When a value changes, ProtoStrap notices within a minute and updates only the components
whose keys changed.

| To                                        | See                                                         |
| ----------------------------------------- | ----------------------------------------------------------- |
| Name a component's keys                   | [Keys](#keys)                                               |
| Read them in a service                    | [How a replica gets its keys](#how-a-replica-gets-its-keys) |
| Give them to an image that can't read AWS | [Secret files](#secret-files)                               |
| Set or rotate a value                     | [Changing a value](#changing-a-value)                       |
| Move from a secret per component          | [Upgrading](#upgrading)                                     |


## Keys

The secret is one JSON object of keys, each starting with who it is for:

| Prefix                   | Read by                                                                   |
| ------------------------ | ------------------------------------------------------------------------- |
| The component's own      | That component. `document-import`'s is `DocumentImport_`; set `secret_prefix` in the spec to choose another, or to share one, such as a migration task that uses `Api_` |
| `Shared_`                | Any component that lists the key in `secrets`                             |
| `Edge_`                  | The edge, when the spec has a tunnel: `Edge_TUNNEL_TOKEN`                 |
| `Reporter_`              | The reporter: `Reporter_ANTHROPIC_API_KEY`                                |

For example:

```json
{
  "Api_StripeKey": "sk_live_...",
  "Shared_ConnectionStrings__postgres": "Host=postgres;Port=5432;Username=postgres;Password=...;Database=postgres",
  "Postgres_POSTGRES_PASSWORD": "...",
  "Edge_TUNNEL_TOKEN": "..."
}
```

A component lists the keys it reads, without their prefix, in `secrets`:

```yaml
  api:
    secrets: [ConnectionStrings__postgres, StripeKey]
```

The list does three things:

- **It says which `Shared_` keys are the component's.** A change to one of them updates
  the component. Its own keys always count, listed or not.
- **It names the files** a component with `secret_files` gets.
- **It is what `protostrap check` checks** the names of.

Put a value in `Shared_` when several components must agree on it, such as a connection
string. Then one write changes it everywhere.


## How a replica gets its keys

Every replica gets two variables:

| Variable        | Value                                              |
| --------------- | -------------------------------------------------- |
| `SECRET_ID`     | The environment's secret, `<app>/<environment>`    |
| `SECRET_PREFIX` | The component's prefix, such as `Api_`             |

A service reads the secret when it starts, with its component's role, which the AWS SDK
finds on its own. It keeps the keys that start with `SECRET_PREFIX` and the `Shared_`
ones, drops the prefixes, and lets its own keys win. The sample's
[.NET](../sample/services/dotnet/Shared/ProtoStrapHosting.cs) and
[Python](../sample/services/py/main.py) services each do this in about 20 lines. Under
`aspire run`, a service reads dev's secret the same way, with the developer's sign-in (see
[Dev](#dev)).

A service reads its keys only when it starts. When they change, ProtoStrap replaces its
replicas, a quarter at a time, as a release does. Each new replica reads everything again,
including the connections it opens at startup, and the service keeps serving throughout.

### Secret files

PostgreSQL, Redis, Keycloak and other images from Docker Hub can't read Secrets Manager.
For them, the agent reads the keys and writes each one listed in `secrets` to a file:

```yaml
  postgres:
    env:
      POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD
    secrets: [POSTGRES_PASSWORD]
    secret_files: true
```

- **The files are on tmpfs,** so no value ever reaches a disk. They are mounted read-only
  at `/run/secrets`, readable by any user in the container, and have no trailing newline.
- **The agent reads them with the component's own role,** at the version the scheduler
  chose, before the container starts. A key that's missing fails the replica, and the
  reason names the key, never a value.
- **They come back after a reboot.** A reboot empties tmpfs, and Docker can't start a
  container whose files are gone, so the agent writes them again and starts it.


## Dev

Dev is the local run, `aspire run`, and its values live in dev's secret, `<app>/dev`, beside
prod's in the application's account and with the same key names. Components read it the way
they read prod's: only where it lives, and who reads it, differ. Under `aspire run` the
ProtoStrap environment (ProtoStrap.Hosting) gives every process a component runs the variables
a replica gets, `SECRET_ID` (`<app>/dev`), `SECRET_PREFIX` and `AWS_REGION`, and `AWS_PROFILE`,
the developer's AWS SSO profile (the AppHost's, or `developer`), in place of the component's
role. The SDK's default chain finds the profile as it finds the role in an account, so the
same code reads it.

- **Sign in first,** with `aws sso login --profile developer`, an SSO profile made once with
  `aws configure sso --profile developer` and the Developer permission set. Without a session,
  a service fails at startup with the SDK's error. A .NET service needs `AWSSDK.SSO` and
  `AWSSDK.SSOOIDC` for an SSO profile.
- **The region** is the AppHost's `AWS_REGION`, which LocalStack uses too: set it to the
  account's, where dev's secret is.
- **Don't give a process LocalStack's test keys.** Environment keys win over `AWS_PROFILE`, so
  the process would read dev's secret with them. LocalStack takes the developer's credentials
  as its one account, `000000000000`. Containers, which have no sign-in, keep test keys.
- **Containers read nothing from it.** An image that takes secret files in an account, such as
  PostgreSQL or Keycloak, is a local resource under `aspire run`, and takes Aspire's parameters,
  generated and kept in the AppHost's user secrets, as connection strings do. Dev's secret
  holds none of their keys: a process that shares one, such as a client secret, would take the
  secret's value over Aspire's.
- **Developers make it once,** with `aws secretsmanager create-secret --name <app>/dev`, and
  fill it with `scripts/set-secret.sh` or the application's own script. Nothing deployed reads
  it, and no workflow touches it. A component reads it when it starts: restart one to take a
  new value.

## Changing a value

Set keys with [`scripts/set-secret.sh`](../scripts/set-secret.sh). It keeps the
secret's other keys:

```bash
scripts/set-secret.sh shop/prod Api_StripeKey=-
```

`KEY=-` asks for the value without echoing it, and `KEY=@file` reads it from a file, such
as a PEM key. Either way the value never appears on a command line. `-KEY` removes a key.

Everything set in one run is one new version of the secret. ProtoStrap applies it in
about three minutes:

1. **It notices.** Once a minute, the scheduler reads the secret's version. When it is
   new, the scheduler reads the value once and hashes each component's keys, so it knows
   whose changed. It keeps only the hashes, made with a key that never leaves data-1, and
   never logs or saves a value.
2. **It waits for the secret to settle.** Nothing is applied until the secret has gone two
   minutes without a new version, so keys written in a few runs go out together.
3. **It applies them in order.** A component waits for the components it `depends_on` to
   apply theirs first: PostgreSQL takes its new password before the services that connect
   to it get the new connection string. Nothing starts while a release is rolling out.
4. **Each component takes its keys** in the way its `on_secret_change` says:

| `on_secret_change`  | What happens                                                             |
| ------------------- | ------------------------------------------------------------------------ |
| `restart` (default) | Its replicas are replaced, as in a release. A stateful one stops before its replacement starts |
| `reload`            | The agent rewrites its secret files in place, all at once, and the replica reads them again itself |
| A command           | The agent rewrites its secret files, then runs the command in the replica, such as PostgreSQL's `ALTER ROLE`. The replica keeps running |

`reload` and a command need `secret_files`. A command must be safe to run twice: it runs
once when the replica is first ready, and again after a reboot. It has two minutes. The
previous values stay readable in `/run/secrets/previous/` for it to log in with. What it
prints on failure is reported with every value replaced by `***`.

A [configure task](config.md#configure-tasks)'s keys count too: a change to them runs the
task again, after the component it configures has taken its own keys, and before the
components that depend on that one take theirs.

`protostrap status` shows each component's version, a newer one waiting and what it
waits for, and why a change is stuck.

### When a new value is wrong

There is no going back to an old value. By the time anything fails, the old value may no
longer work: PostgreSQL has a new password, or the old API key is revoked. So ProtoStrap
moves forward:

- **The replicas that still work keep running.** New replicas that fail with the new keys
  aren't ready, so no old replica is stopped for them.
- **It keeps trying, a little slower each time,** and says why in `protostrap status`.
- **The next value written is applied.** That is the way out: write the fix.

A failing hook is retried every minute. A release is blamed only for the components it
changes, so a broken key never rolls one back.


## PostgreSQL and Redis

The Aspire generator gives both a password by default, and the sample spec does the same.

| Component  | Gets its password from                                   | A new password is                                                    |
| ---------- | -------------------------------------------------------- | -------------------------------------------------------------------- |
| PostgreSQL | `POSTGRES_PASSWORD_FILE`, when it first makes its data   | Set with `ALTER ROLE` by its hook. PostgreSQL never restarts         |
| Redis      | Its command: `--requirepass "$(cat /run/secrets/REDIS_PASSWORD)"` | Set with `CONFIG SET requirepass` by its hook, logged in with the old one |

Inside its container, PostgreSQL still trusts its socket, which the backup and the hook
use. Over the network, it needs the password.

To rotate PostgreSQL's password, write it and the connection string in one run:

```bash
scripts/set-secret.sh shop/prod Postgres_POSTGRES_PASSWORD=@pw Shared_ConnectionStrings__postgres=@connection
```

PostgreSQL applies it first, then the services that connect are replaced. Connections they
already have stay open; only new ones need the new password.


## Who can read it

Secrets Manager can't grant one key alone. Every role that reads the secret can read every
key in it:

| Reads the secret     | Why                                                        |
| -------------------- | ---------------------------------------------------------- |
| Every component role | Its own keys and the `Shared_` ones                        |
| The edge's role      | Its tunnel token. The edge runs on the internet-facing ingress VMs |
| The data VMs' role   | The scheduler hashes the keys; the reporter reads its LLM key |
| Org Admins           | They set the keys                                          |

Platform Admins can't read it, and neither can Developers, since the platform tags it:
they have dev's secret, `<app>/dev`, beside it in the same account, for running services
locally. This is an accepted risk (see
[What you are accepting](production.md#1-know-what-you-are-accepting)): a compromised component can read every
other component's keys. Keep what each component holds as narrow as it can be, such as a
database user per service rather than the superuser.


## Upgrading

ProtoStrap before the environment's secret had a secret per component,
`protostrap/<component>`, plus `protostrap/edge` and `protostrap/reporter`. To move:

1. **Bootstrap.** Add `app` to the bootstrap module, the spec's `app`, and an Org Admin
   applies it. The boundaries then cover `<app>/<environment>`.
2. **Platform.** Apply the platform. It creates `<app>/<environment>` and gives every role
   access to it. The old secrets are kept, untouched, and each role can still read its old
   one until the next ProtoStrap version.
3. **Copy the keys.** Run [`scripts/merge-secrets.sh`](../scripts/merge-secrets.sh)
   `<app> <environment>`. It copies each old secret's keys under its owner's prefix:
   `protostrap/api`'s `StripeKey` becomes `Api_StripeKey`, `protostrap/edge`'s
   `TUNNEL_TOKEN` becomes `Edge_TUNNEL_TOKEN`. `--dry-run` shows what it would copy.
4. **Services.** Release services that read `SECRET_PREFIX`, as above. One that reads
   every key in `SECRET_ID` would see every component's.
5. **Release ProtoStrap.** Its replicas read the environment's secret. Nothing is replaced
   for it: what runs is taken as it is until a key really changes.
6. **Clean up.** Once everything runs, delete the old secrets by hand.

PostgreSQL and Redis that ran without a password need two more steps:

- **Set their keys before deploying,** `Postgres_POSTGRES_PASSWORD` and
  `Redis_REDIS_PASSWORD`, with connection strings that carry them. A replica whose key is
  missing fails. Each restarts once, when the release that gives it a password rolls out.
- **PostgreSQL keeps trusting the network** until you change it: `POSTGRES_HOST_AUTH_METHOD`
  only applies when it first makes its data. Give it a `pg_hba.conf` that uses
  `scram-sha-256` for `host` lines as [config files](config.md#postgresqls-pg_hbaconf), which
  it reloads without restarting.
