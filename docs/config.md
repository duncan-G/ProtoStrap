# Config files

Some components are run by a config file rather than by code: an Envoy of the
application's own, nginx, PostgreSQL's `pg_hba.conf`, or an identity provider's realms,
which live in its database and change only through its API. ProtoStrap delivers the files
from the application's repository with each release, and each component says how it takes
new ones. Changing a component's config never needs anyone on a VM.

| To                                              | See                                                       |
| ----------------------------------------------- | --------------------------------------------------------- |
| Choose how a component takes its config         | [Three ways to take config](#three-ways-to-take-config)   |
| Give a component files                          | [Config files](#config-files-1)                           |
| Take new files without a restart                | [Taking new files](#taking-new-files)                     |
| Configure a component through its API           | [Configure tasks](#configure-tasks)                       |
| See worked examples                             | [Examples](#examples)                                     |
| See what's pending or failing                   | [Status](#status)                                         |


## Three ways to take config

How a program reads its config decides how ProtoStrap should hand it over:

| The program reads it                         | Such as                                                         | Give it                                                    |
| -------------------------------------------- | --------------------------------------------------------------- | ---------------------------------------------------------- |
| Once, when it starts or is built             | An identity provider's providers, themes and build options; Envoy's bootstrap | The image. A change replaces the replicas, as any image does |
| From files it watches, or again on a signal  | Envoy's dynamic config, nginx, `pg_hba.conf`                    | [Config files](#config-files-1), with `on_config_change: reload` or a command |
| Through its API, kept in its database        | An identity provider's realms, clients, flows and mail settings | A [configure task](#configure-tasks)                       |

A program that reads a file once at start can still take it as config files with the
default, `restart`: the file then changes without a new image, and the replicas are
replaced as for one.


## Config files

`config` names a directory in the repository, and where its files appear in the replicas:

```yaml
  postgres:
    config: { from: config/postgres, path: /etc/postgresql/conf }
```

- **The files are what git tracks** in `from`, as the release's commit has them, nested
  directories included. An edit that isn't committed isn't released.
- **They are mounted read-only** at `path`, a directory, readable by any user in the
  container. `path` can't cover or sit in `/run/secrets` or the component's volume.
- **The release carries them**, so they roll back with it. Every environment gets the same
  files: anything that differs between accounts comes from an environment variable the
  program reads, or from the [environment's secret](secrets.md).
- **They hold no secret.** They are in git, in the release in the deploy bucket, and on the
  VM's disk. A file that needs a secret names its [secret file](secrets.md#secret-files),
  such as `/run/secrets/SMTP_PASSWORD`, and the program reads it from there.
- **They are small**: at most 1 MiB and 256 files, with no links or submodules, and no name
  starting with `..`, which the agent keeps for itself. Anything larger belongs in the image.

The agent fetches the files from the scheduler, checks them against their digest, and
writes them before the replica starts. As with secret files, each set is a directory of its
own and `..data` points at the current one, so a change swaps every file at once: a
program watching the directory, such as Envoy with `watched_directory`, sees one change,
never half of one.


## Taking new files

`on_config_change` says what new files do to a component's replicas:

| `on_config_change`  | What happens                                                                         |
| ------------------- | ------------------------------------------------------------------------------------ |
| `restart` (default) | The replicas are replaced, as for a new image. A stateful one stops before its replacement starts |
| `reload`            | The agent swaps the new files in, and the replica reads them again itself            |
| A command           | The agent swaps the new files in, then runs the command in the replica, such as `nginx -s reload`. The replica keeps running |

A release takes new files a quarter of a component's replicas at a time, at least one,
as it replaces replicas: the next ones get them once those before have them and are ready.

- **A command has two minutes**, and runs once per new set. It doesn't run when a replica
  starts, since the replica reads its files then. What it writes to stderr on failure is
  reported, with every value of the replica's secret files replaced by `***`.
- **A command is the place to check the files.** `reload` can't tell whether the program
  liked them: a program that keeps its old config when the new one is bad, as Envoy and
  PostgreSQL do, would leave the release looking good. A command that checks first, then
  reloads, and exits non-zero when the files are bad, fails the release instead.
- **A follower that can't take a release's files fails the release**, which rolls back: the
  old files are swapped in again and the command runs again. Its replicas are never
  replaced for it.
- **A new replica starts with the files the release has**, even mid-rollout.


## Configure tasks

Some programs keep their config in their own database and change it only through an API.
Restarting them with a new file changes nothing that's already there: an identity provider
that imports a realm file at startup skips a realm that exists. For these, a task applies
the config through the API:

```yaml
  idp:
    kind: service
    build: services/idp
    configure: idp-config

  idp-config:
    kind: task
    build: services/idp-config
    pool: general
    request: { cpu: 250m, memory: 512Mi }
    depends_on: [idp]
    secret_prefix: Idp_ # It reads idp's own keys.
    secrets: [ADMIN_CLIENT_SECRET, SMTP_PASSWORD]
    secret_files: true
    config: { from: config/idp, path: /config }
```

`configure` names a task that runs **after** the component is ready, as `migrate` names one
that runs before. It runs:

| When                                                                     | Why                                   |
| ------------------------------------------------------------------------ | ------------------------------------- |
| The component first has a ready replica                                  | The first deploy                      |
| The task changes in a release: its image, config files, `env`, `command` or other settings | The config changed |
| One of the task's keys changes in the environment's secret               | A client secret or password it sets   |
| The component itself changes in a release: its image, `env` or anything else that replaces its replicas | An upgrade may need it applied again |

Nothing else runs it: not scaling, not another component's release, not a new route. A
release that changes nothing it applies doesn't run it.

- **It waits for the component to roll out.** It runs once every replica of the component
  is of the release, has its keys and config files, and one is ready.
- **What depends on the component waits for it.** A component that `depends_on` it starts
  its first replica, and in a release its new replicas, only once the task has run. A new
  key for both waits for the task too, so the component sets a client secret before the
  services that use it are replaced.
- **One run at a time.** A run never starts while another of the same task is in progress,
  even across a scheduler restart. A configure task is never a migration.
- **What it applied is kept** in the scheduler's `state.json`, so a restart doesn't run it
  again.

### When a run fails

- **In a release, the release fails** and rolls back, and the last good release's task runs
  again. A run that failed halfway may have changed some of the config, so what's
  configured is applied over again to match what runs.
- **For new keys, it moves forward**, as [secrets](secrets.md#when-a-new-value-is-wrong) do:
  the task runs again after a minute, then a little slower each time, up to every ten
  minutes, and `protostrap status` says why. Writing the fix is the way out.
- **The same goes for a release that fails for another reason** after the task ran: the
  last good release's task runs again.

### Writing a configure task

The task is the application's: ProtoStrap only decides when it runs. It must:

1. **Apply the whole config, every run, the same way twice.** It runs again after a
   failure, a rollback or a restart. A declarative tool, which makes the program's config
   match the files, does this; a script of one-off changes doesn't.
2. **Exit with 0 only when everything is applied.** Any other exit counts as a failure.
3. **Log in with a credential from the environment's secret**, read from its
   [secret files](secrets.md#secret-files): a service account it keeps, never one made by
   hand for the occasion. Like every key in the secret, every component's role can read it
   (see [Who can read it](secrets.md#who-can-read-it)).
4. **Reach the component by its name** on the overlay, such as `http://idp:8080`, and leave
   anything the edge doesn't route, such as an admin API, unrouted.
5. **Write its logs to stdout.**


## Examples

These are recipes, not part of ProtoStrap: each program and tool named here is the
application's choice.

### An identity provider

Keycloak keeps its realms in its database. Its `--import-realm` only ever creates a realm,
so a change to the realm file never reaches one that exists.

- **The image** holds what Keycloak reads when it starts or builds: its version, providers,
  themes and `kc.sh build` options. A change replaces its replicas. It doesn't use
  `--import-realm`.
- **A configure task** holds the realms, as files for a declarative tool such as
  keycloak-config-cli, which makes a realm match its files through the admin API and
  creates it when it's missing. It logs in with the client credentials grant, and its
  `$(file:UTF-8:/run/secrets/SMTP_PASSWORD)` substitution reads secrets from the task's
  secret files, so client secrets, mail and identity provider settings are applied too, and
  applied again when they change.
- **Its admin credential** is a service-account client whose secret is a key in the
  environment's secret, such as `Idp_ADMIN_CLIENT_SECRET`, listed by both the service and
  the task, which shares the service's prefix. Keycloak creates it on its first start from `KC_BOOTSTRAP_ADMIN_CLIENT_ID` and
  `KC_BOOTSTRAP_ADMIN_CLIENT_SECRET`, which its entrypoint reads from the secret file.
  Keycloak that already runs without one needs that client created once.

### An Envoy of the application's own

ProtoStrap's [edge](protostrap.md#edge) already reloads its routes without restarting. An
Envoy the application runs itself, behind the edge, does the same with config files:

```yaml
  gateway:
    config: { from: config/gateway, path: /etc/envoy/xds }
    on_config_change: reload
```

Its image holds the bootstrap, which names `/etc/envoy/xds/lds.yaml` and `cds.yaml` with
`path_config_source` and `watched_directory: { path: /etc/envoy/xds }`.

### PostgreSQL's pg_hba.conf

```yaml
  postgres:
    command: [postgres, -c, hba_file=/etc/postgresql/conf/pg_hba.conf]
    config: { from: config/postgres, path: /etc/postgresql/conf }
    on_config_change: [sh, -c, 'psql -X -q -U postgres -tAc "SELECT count(*) FROM pg_hba_file_rules WHERE error IS NOT NULL" | grep -qx 0 && psql -X -q -U postgres -c "SELECT pg_reload_conf()"']
```

The image's entrypoint still runs, and starts PostgreSQL with the file. The command refuses
a file PostgreSQL can't read, which `pg_reload_conf()` alone would ignore, then reloads it.
PostgreSQL never restarts. Keep a `local` line that trusts the socket: the command and the
backup use it.


## With Aspire

An AppHost that writes the spec with [ProtoStrap.Hosting](protostrap-aspire.md) says the same:

```csharp
var idpConfig = builder.AddProject<Projects.Idp_Config>("idp-config")
    .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", adminClientSecret)
    .PublishAsProtoStrapTask(t => t.Request(cpu: "250m", memory: "512Mi").SecretPrefix("Idp_"));

builder.AddDockerfile("idp", "../services/idp")
    .WithEnvironment("KC_BOOTSTRAP_ADMIN_CLIENT_SECRET", adminClientSecret)
    .WithProtoStrapConfigure(idpConfig)
    .PublishAsProtoStrapService(s => s.Request(cpu: "1000m", memory: "1Gi").SecretFiles());

builder.AddPostgres("postgres")
    .WithProtoStrapConfig("../config/postgres", "/etc/postgresql/conf")
    .PublishAsProtoStrapStateful(s => s.OnConfigChange(ConfigChange.Run("sh", "-c", "...")) /* ... */);
```

Under `aspire run` the config files are a read-only bind mount, and the configure task runs
once the component is healthy, before anything that waits for the component starts.


## Status

`protostrap status` shows a follower whose replicas haven't all taken a release's files and
why one can't, and a configure task that is pending, running or failed, with why it waits.
The [operator UI](protostrap.md#operator-ui) lists the same under what needs attention.


## Upgrading

A spec with `config` or `configure` needs a ProtoStrap version that has them. Agents update
themselves before they start a replica of a release, so no replica ever starts without its
files.
