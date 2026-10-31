# Stateful components

PostgreSQL, Redis, or any upstream server that keeps data: `kind: stateful`, one replica,
pinned to a data VM, with its data on an EBS volume of its own. Ground truth, at the spec's
version: `docs/vms.md#data`, `docs/production.md#restoring-a-backup`, and the working
entries for both in ProtoStrap's `sample/protostrap.yaml`.

## The entry

```yaml
  postgres:
    kind: stateful
    image: postgres:17.6                 # upstream, a pinned tag; never latest
    vm: data-1
    request: { cpu: 1000m, memory: 3Gi }
    port: 5432
    env:
      POSTGRES_PASSWORD_FILE: /run/secrets/POSTGRES_PASSWORD
      POSTGRES_HOST_AUTH_METHOD: scram-sha-256
      PGDATA: /var/lib/postgresql/data/pgdata
    secrets: [POSTGRES_PASSWORD]
    secret_files: true
    on_secret_change: [sh, -c, '<applies the new password in place: the sample has it>']
    volume: { path: /var/lib/postgresql/data, size: 20Gi }
    backup: [pg_dumpall, -U, postgres]
    stop: 60s
```

- **`image`** runs as it is, from ECR: the deploy workflow copies it there. A server that
  needs its own files or plugins is a Dockerfile and a `service`, or its files come as
  `config`.
- **`request`** is its share of the data VM, and its memory is a hard limit. **Tune the
  program to the request, not the VM:** Redis's `--maxmemory` below it (the sample gives
  `768mb` of `1Gi`), PostgreSQL's `shared_buffers` about a quarter of it.
- **`volume`**: one per component, mounted at `path`. Its `size` is whole GiB and **only
  grows**: size PostgreSQL's for a year of data. `iops` and `throughput` are optional, for gp3.
- **`backup`** is a command run in the container once a day; its stdout goes to the
  backups bucket as `<component>/<date>.gz`. It must not put a password on a command line:
  the sample's read the secret file, or use the socket the container trusts.
- **Keys** arrive as files (`secret_files`), and a new value is applied in place by
  `on_secret_change` (protostrap-secrets). Copy the sample's PostgreSQL and Redis hooks.
- **Files it reloads**, such as `pg_hba.conf`: `config` with `on_config_change` that checks,
  then reloads (the sample's PostgreSQL does).
- **`stop`**: long enough to shut down cleanly; 60 s for PostgreSQL.
- **No `scale`, `routes` or `pool`.** Services reach it by name on the overlay.

## Fitting on the data VM

Every stateful component on a data VM, plus the telemetry replica on data-1, plus the data
pool's `reserved`, must fit that VM. `protostrap check` refuses a spec where they don't.
A larger `pools.data.vm`, or a second data VM (`count: 2`, then `vm: data-2`), makes room;
both wait for a maintenance window, since they stop what's on the VM.

## Services that use it

- **Connection details are keys,** never `env`: `Shared_ConnectionStrings__shop` holding
  `Host=postgres;Port=5432;Database=shop;Username=shop;Password=…`, listed in each user's
  `secrets` (protostrap-secrets). Moving to RDS later changes only the key.
- **`depends_on: [postgres]`** on each component that can't start without it.
- **A user per service,** not the superuser: every component's role can read the whole
  secret. The image's init scripts run only on an empty volume, so a database or user added
  later is made by something in the release, such as a configure task on PostgreSQL
  (`configure:`, `docs/config.md`), never by hand on data-1.
- **Schema changes are migrations** (protostrap-contract), compatible with the previous
  release.

## Changing one

| Change                                    | Does                                                                          |
| ----------------------------------------- | ----------------------------------------------------------------------------- |
| `image` within a major version, `request`, `env`, `command` | Stops it, then starts the new one: **brief downtime** for everything that uses it |
| A **major** PostgreSQL version            | The new server can't open the old data. A dump and restore in a maintenance window, rehearsed in the sample's test account or in prod before go-live; tell the user, don't just change the tag |
| `volume.size`                             | Terraform grows the volume. It can never shrink                               |
| Its keys                                  | Its `on_secret_change` hook, in `depends_on` order with the connection strings |
| Moving it to another data VM, or to RDS or ElastiCache | `docs/vms.md#moving-postgresql-and-redis`, protostrap-secrets's "Moving PostgreSQL or Redis to a managed service" |

**Terraform never deletes a stateful component's volume.** Removing or renaming the
component, or any change that would replace its volume, fails the infra plan
(`prevent_destroy`). That is on purpose: say so in the pull request. Removing one for good,
such as after a move to RDS, needs an Org Admin to forget the volume first, in each account,
before the infra workflow runs:

```bash
terraform state rm 'module.protostrap.aws_ebs_volume.stateful["<component>"]'
```

(`module.<name>` is the module's name in that account's root.) The volume then stays in the
account, with its data, until someone deletes it by hand. Write the command for them; never
run it. A rename is a new, empty volume beside the old one, so it is a data move, not an
edit: tell the user.

## Backups and restores

- **Every night:** the `backup` command's dump to the backups bucket, kept
  `backup_retention_days`, and an EBS snapshot of the volume, the last `snapshot_retention`
  kept (both in the platform root). Snapshots are crash-consistent and up to a day old.
- **A failed backup** is a `backup failed` record in the infra log, and the daily report
  counts it (protostrap-operate).
- **Point in time:** a logical dump restores to the night it ran. Anything finer needs WAL
  archiving in the image, or RDS.
- **Restoring** is an Org Admin's job, on data-1 through SSM, following
  `docs/production.md#restoring-a-backup`: test into a scratch container first; a real
  restore needs a maintenance window, since ProtoStrap can't yet stop the writers. Write
  the steps for them; never run them.
