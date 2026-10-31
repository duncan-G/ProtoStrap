---
name: protostrap-secrets
description: Add, name, share, set or rotate a secret in an application deployed with ProtoStrap - API keys, connection strings, database and Redis passwords, the edge's tunnel token, the reporter's LLM key. Covers the one-secret-per-environment layout (<app>/<environment>) with prefixed keys (Api_, Shared_, Edge_, Reporter_), the spec's secrets, secret_prefix, secret_files and on_secret_change, how ProtoStrap applies a change, and moving PostgreSQL or Redis to a managed service. Use whenever code needs a new secret value or a credential changes.
---

# Secrets

Each deployed environment has one Secrets Manager secret, `<app>/<environment>` (such as
`shop/prod`), a JSON object of every component's keys. Terraform creates it empty; **an Org
Admin sets the values**, so no value passes through Terraform, CI, an image, or you.

The design is `docs/secrets.md` at the spec's `protostrap:` version.

## Rules for you

- **Never handle a value.** Don't ask for one, generate one into a file, put one in a
  command line, the spec, `env`, a commit, a test fixture or a log. Write the commands an Org
  Admin runs, with `KEY=-` (prompted, not echoed) or `KEY=@file`.
- **Developers can't read the secret,** only Org Admins and the components' roles. Don't
  try to read it to debug; read `protostrap status` or the operator UI, which never show values.
- **There is no rollback.** A written value is applied; the way out of a bad one is writing
  the fix.

## Keys

| Prefix              | Read by                                                               |
| ------------------- | --------------------------------------------------------------------- |
| The component's own | That component. Its name in PascalCase plus `_`: `document-import` → `DocumentImport_`, `api` → `Api_`. A name whose prefix would be reserved or invalid (`shared`, `reporter`, a leading digit) needs `secret_prefix` |
| `Shared_`           | The components that list the key in their `secrets`. Only a listed key replaces a component when it changes, or becomes a secret file |
| `Edge_`             | The edge: `Edge_TUNNEL_TOKEN`                                         |
| `Reporter_`         | The reporter: `Reporter_ANTHROPIC_API_KEY`                            |

- Key names are in environment-variable form, `ConnectionStrings__postgres`,
  `Stripe__ApiKey`, so a local run sets the same names, without the prefix, as variables.
- `secret_prefix: Api_` in the spec makes a component read another's keys, such as a
  migrations task that needs the API's connection string.
- Put a value in `Shared_` when several components must agree on it, such as a connection
  string: one write then changes it everywhere.
- **Keep each component's keys narrow**, such as a database user per service, not the
  superuser: every component's role can read the whole secret.

## Adding a secret

1. **The code** reads the key by its unprefixed name from configuration
   (protostrap-contract skill). Locally it gets an environment variable: with
   `WithDevSecret()`, the AppHost takes its value from dev's secret by the same key.
2. **The spec** lists it in the component's `secrets`, without prefix:
   `secrets: [ConnectionStrings__postgres, Stripe__ApiKey]`. With an Aspire AppHost, a
   `builder.AddParameter("stripe-key", secret: true)` passed to the component does this
   (protostrap-aspire skill). Its own keys count even unlisted, but list them: `protostrap check`
   checks the names, and `secret_files` writes only listed keys.
3. **The value is set before the release that reads it.** A release whose new replicas
   can't find a key they need leaves them unready, and that release rolls back. Hand the Org
   Admin the command for each deployed environment:

   ```bash
   scripts/set-secret.sh shop/prod Api_Stripe__ApiKey=-
   ```

   `scripts/set-secret.sh` is in ProtoStrap, at the spec's version (`.protostrap/scripts/` or
   `../ProtoStrap/scripts/`). `-KEY` removes a key. Say this ordering in the pull request.
   Dev is the local run: its secret, `shop/dev`, sits in the application's one account beside
   `shop/prod`, and Developers set it with their own sign-in. They can't read `shop/prod`:
   the platform tags it. No Terraform makes `shop/dev`, so it is made once with
   `aws secretsmanager create-secret --name shop/dev` before `scripts/set-secret.sh` fills it.
   With `WithDevSecret()`, `aspire run` stops until dev's secret has the new key too, so add
   it there in the same change (docs/secrets.md#dev).

## Images that can't read Secrets Manager

Upstream images (PostgreSQL, Redis, Keycloak) get keys as files instead:

```yaml
  postgres:
    env:
      POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD
    secrets: [POSTGRES_PASSWORD]
    secret_files: true
    on_secret_change: [sh, -c, '<one line that applies the new value in place>']
```

- Each listed key is a file in `/run/secrets/<KEY>`, on tmpfs, read-only, with no trailing newline.
- `on_secret_change`: `restart` (the default; a stateful component stops before its
  replacement starts), `reload` (files rewritten, the process re-reads them), or a command run
  in the replica. `reload` and a command need `secret_files: true` and at least one key in
  `secrets`. A command must be safe to run twice, finish within 2 minutes, never put a value
  on a command line, and can log in with the old value from `/run/secrets/previous/`.
  The sample's `protostrap.yaml` has working hooks for PostgreSQL (`ALTER ROLE`) and Redis
  (`CONFIG SET requirepass`).

## How a change is applied

1. The scheduler sees a new version within a minute, hashes each component's keys, and
   keeps only the hashes.
2. It waits until the secret has had no new version for 2 minutes, so several runs go out together.
3. It applies in `depends_on` order: PostgreSQL takes its new password before the services
   that connect get the new connection string. Nothing starts during a release rollout.
4. Each changed component takes it per its `on_secret_change`. Services are replaced a
   quarter at a time.

About 3 minutes in all. **Write keys that must change together in one run:**

```bash
scripts/set-secret.sh shop/prod Postgres_POSTGRES_PASSWORD=@pw Shared_ConnectionStrings__postgres=@connection
```

If new replicas fail with the new value, the old ones keep serving, ProtoStrap retries more
slowly, and `protostrap status` says why. A secret change never rolls back a release: a
release is blamed only for the components it changes.

## Moving PostgreSQL or Redis to a managed service

1. Create the RDS or ElastiCache resource in the account's Terraform root (an Org Admin may
   first need to widen bootstrap's `extra_infra_actions`).
2. Copy the data in a maintenance window.
3. Write the new connection strings, `Shared_ConnectionStrings__postgres` and the like, in one run.
4. Remove the stateful component from the spec, and from every `depends_on`.

The code doesn't change, because its connection details were always keys.

## Older layouts

Before the environment's secret, each component had `protostrap/<component>`, and services
read every key in it. An application still writing `protostrap/<name>` secrets, or loading
the secret without filtering by `SECRET_PREFIX`, must move: `docs/secrets.md#upgrading` and
`scripts/merge-secrets.sh <app> <environment> --dry-run`.
