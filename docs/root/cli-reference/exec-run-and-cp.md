---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
tags: []
leafwiki_id: yk8A1nQvR
leafwiki_title: Exec, Run and CP
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# Exec, Run and CP

## exec

Execute a command in a running instance.

```
incus-compose exec [options] SERVICE COMMAND [ARGS...]
```

| Option            | Description                                                            |
| ----------------- | ---------------------------------------------------------------------- |
| `-d`, `--detach`  | Run command in the background                                          |
| `--dry-run`       | Execute command in dry run mode                                        |
| `-e`, `--env`     | Set environment variables `KEY=VALUE` (repeatable)                     |
| `--index`         | Index of the container if service has multiple replicas (default: 0)   |
| `-T`, `--no-tty`  | Disable pseudo-TTY allocation                                          |
| `--privileged`    | Give extended privileges to the process (accepted but not implemented) |
| `-u`, `--user`    | Run the command as this user (default: the instance's UID)             |
| `-g`, `--group`   | Run the command as this group (default: the instance's GID)            |
| `-w`, `--workdir` | Path to workdir directory for this command                             |

`exec` shells out to your local `incus` client and targets the instance via
`INCUS_PROJECT`. It uses your local Incus remote configuration (`incus remote`),
not incus-compose's own connection settings.

Like `docker compose exec`, the command runs as the instance's user and group by
default: the image's `oci.uid` / `oci.gid`, or the numeric IDs from the service
[`user:`](/compose-compatibility/services#user) override. Pass `--user` /
`--group` to run as someone else:

```bash
incus-compose exec web id             # runs as the instance's user (e.g. 1000:1000)
incus-compose exec --user 0 web id    # runs as root
```

The command and its arguments are passed to Incus verbatim, so flags with
leading dashes work without escaping:

```bash
incus-compose exec web ls -ln /data
incus-compose exec web sh -c 'echo hello > /data/test.txt'
```

_Changed in 1.0.0-beta.22_: exec uses the instances UID/GID by default.

## run

Run a one-off command on a service, as `docker compose run` does.

```
incus-compose run [options] SERVICE [COMMAND] [ARGS...]
```

| Option                   | Description                                                 |
| ------------------------ | ----------------------------------------------------------- |
| `--rm`                   | Remove the instance after the command exits                 |
| `-d`, `--detach`         | Print the instance name and return                          |
| `-e`, `--env`            | Set environment variables `KEY=VALUE` (repeatable)          |
| `-l`, `--label`          | Add a label `KEY=VALUE` (repeatable)                        |
| `-v`, `--volume`         | Bind mount a volume (repeatable)                            |
| `-p`, `--publish`        | Publish a port (repeatable)                                 |
| `-P`, `--service-ports`  | Keep the ports the service declares                         |
| `--entrypoint`           | Override the image entrypoint                               |
| `-u`, `--user`           | Run as this user (default: the instance's UID)              |
| `--group`                | Run as this group (default: the instance's GID)             |
| `-w`, `--workdir`        | Working directory for the command                           |
| `--name`                 | Name for the one-off instance                               |
| `-T`, `--no-tty`         | Disable pseudo-TTY allocation                               |
| `--no-deps`              | Don't start the services this one depends on                |
| `--build` / `--no-build` | Build the image first, or never build                       |
| `--builder`              | Preferred builder, binary name or absolute path             |
| `--pull`                 | `always` / `missing` (default) / `never`                    |
| `--init`                 | Image the blocking helper comes from                        |
| `--timeout`              | Timeout for creating and stopping the one-off (default: 2m) |

Everything after SERVICE belongs to the command, so its own flags need no
escaping:

```bash
incus-compose run --rm web sh -c 'echo hello'
incus-compose run --rm db psql -U postgres -c 'select 1'
```

`run` exits with the command's own status:

```bash
incus-compose run --rm web sh -c 'exit 42'; echo $?   # 42
```

The one-off is named `<service>-run-<8 hex>` unless `--name` says otherwise, and
it carries `user.incus-compose.oneoff=true`. It is not one of the service's
instances: `up` never reconciles it, `ps` lists it under its service name, and
`down` removes it even without `--rm`. Health checks are off on it, so
ic-healthd never restarts one.

Ports are dropped unless `-p` or `-P` is given, because a proxy device would
fight the running service for the same listener.

### How the exit code is obtained

Incus reports no exit status for an instance that stopped, and `incus exec`
reports an exact one. So the instance runs a helper that does nothing but block,
and the service's own entrypoint and command run through an exec into it.

That helper comes from `ghcr.io/lxc/incus-compose/ic-sleep`, read once per
server into an `incus-compose-tools` volume in the `incus-compose` project and
copied from there into each compose project that runs a one-off. Point `--init`,
or `x-incus-compose.init`, at another image for a private mirror.

Fetching that image is the only step of a one-off that needs the network, and
`pull` does it - so does `up`, which runs `pull`. Both take `--init` for the
same reason, and both only warn when it cannot be fetched: most projects never
run a one-off, so an unreachable tools image must not fail the whole command.
`run` is where it becomes an error. That is what makes an
[air-gapped install](/air-gapped) work: `pull` while connected, `run` later.

Two consequences:

- **The command is not PID 1**, where docker's is. Nothing user-visible depends
  on it, and Incus runs an OCI entrypoint under an init of its own either way.
- **A cluster mixing CPU architectures is not supported.** The helper is pulled
  for the architecture of the server that fetched it, and an OCI remote serves
  no other. On a single-architecture cluster, or a standalone server, this never
  comes up.

`run` shells out to your local `incus` client for the exec, as [`exec`](#exec)
does.

_Since: v1.3.0_

## cp

Copy files between a service's instance and the local filesystem.

```
incus-compose cp [options] SERVICE:SRC_PATH DEST_PATH|-
incus-compose cp [options] SRC_PATH|- SERVICE:DEST_PATH
```

| Option                | Description                                                          |
| --------------------- | -------------------------------------------------------------------- |
| `--index`             | Index of the container if service has multiple replicas (default: 0) |
| `-a`, `--archive`     | Keep the source's uid/gid instead of the instance's                  |
| `-L`, `--follow-link` | Always follow symbolic links in `SRC_PATH`                           |
| `--dry-run`           | Print the `incus` command instead of running it                      |

Which side is the instance is decided by the name before the colon: it has to
name a service in the compose file. So `C:\data`, `./a:b` and `-` stay local
paths, and a typo'd service name is a local path that does not exist rather than
a copy into the wrong place.

```bash
incus-compose cp ./nginx.conf web:/etc/nginx/nginx.conf
incus-compose cp web:/var/log/nginx ./logs
```

A path inside the instance resolves from its root, so `web:etc/hosts` and
`web:/etc/hosts` are the same file. Directories are copied recursively without
asking, and a symlink stays a symlink unless `-L` is given.

Like `exec`, and unlike `docker compose cp`, a pushed file is owned by the
instance's user rather than root - a 0400 file owned by root is unreadable to
the non-root user most OCI images run as. `--archive` keeps whatever the source
had.

`cp` shells out to your local `incus` client and targets the instance via
`INCUS_PROJECT`.

_Since: v1.3.0_

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command             | Variable                          | Flag                    | Description                                   |
| ------------------- | --------------------------------- | ----------------------- | --------------------------------------------- |
| `run`               | `INCUS_COMPOSE_RUN_RM`            | `--rm`                  | Remove the instance after the command exits   |
| `run`               | `INCUS_COMPOSE_RUN_DETACH`        | `--detach`, `-d`        | Print the instance name and return            |
| `run`               | `INCUS_COMPOSE_RUN_ENV`           | `--env`, `-e`           | Set environment variables (KEY=VALUE)         |
| `run`               | `INCUS_COMPOSE_RUN_LABEL`         | `--label`, `-l`         | Add a label (KEY=VALUE)                       |
| `run`               | `INCUS_COMPOSE_RUN_VOLUME`        | `--volume`, `-v`        | Bind mount a volume                           |
| `run`               | `INCUS_COMPOSE_RUN_PUBLISH`       | `--publish`, `-p`       | Publish a port                                |
| `run`               | `INCUS_COMPOSE_RUN_SERVICE_PORTS` | `--service-ports`, `-P` | Keep the ports the service declares           |
| `run`               | `INCUS_COMPOSE_RUN_ENTRYPOINT`    | `--entrypoint`          | Override the image entrypoint                 |
| `run`               | `INCUS_COMPOSE_RUN_USER`          | `--user`, `-u`          | Run as this user                              |
| `run`               | `INCUS_COMPOSE_RUN_GROUP`         | `--group`               | Run as this group                             |
| `run`               | `INCUS_COMPOSE_RUN_WORKDIR`       | `--workdir`, `-w`       | Working directory for the command             |
| `run`               | `INCUS_COMPOSE_RUN_NAME`          | `--name`                | Name for the one-off instance                 |
| `run`               | `INCUS_COMPOSE_RUN_NO_TTY`        | `--no-tty`, `-T`        | Disable pseudo-TTY allocation                 |
| `run`               | `INCUS_COMPOSE_RUN_NO_DEPS`       | `--no-deps`             | Don't start the services this one depends on  |
| `run`               | `INCUS_COMPOSE_RUN_BUILD`         | `--build`               | Build the image before running                |
| `run`               | `INCUS_COMPOSE_RUN_NO_BUILD`      | `--no-build`            | Never build                                   |
| `run`               | `INCUS_COMPOSE_RUN_BUILDER`       | `--builder`             | Preferred builder                             |
| `run`               | `INCUS_COMPOSE_RUN_PULL`          | `--pull`                | always / missing / never                      |
| `run`, `pull`, `up` | `INCUS_COMPOSE_SLEEP_IMAGE`       | `--sleep-image`         | Image the blocking helper comes from          |
| `run`               | `INCUS_COMPOSE_RUN_TIMEOUT`       | `--timeout`             | Timeout for creating and stopping the one-off |
| `exec`              | `INCUS_COMPOSE_EXEC_DETACH`       | `--detach`, `-d`        | Run command in the background                 |
| `exec`              | `INCUS_COMPOSE_EXEC_ENV`          | `--env`, `-e`           | Set environment variables (KEY=VALUE)         |
| `exec`              | `INCUS_COMPOSE_EXEC_INDEX`        | `--index`               | Replica index if service is scaled            |
| `exec`              | `INCUS_COMPOSE_EXEC_NO_TTY`       | `--no-tty`, `-T`        | Disable pseudo-TTY allocation                 |
| `exec`              | `INCUS_COMPOSE_EXEC_PRIVILEGED`   | `--privileged`          | Accepted but not implemented                  |
| `exec`              | `INCUS_COMPOSE_EXEC_USER`         | `--user`, `-u`          | Run the command as this user                  |
| `exec`              | `INCUS_COMPOSE_EXEC_GROUP`        | `--group`, `-g`         | Run the command as this group                 |
| `exec`              | `INCUS_COMPOSE_EXEC_WORKDIR`      | `--workdir`, `-w`       | Path to workdir directory                     |
| `cp`                | `INCUS_COMPOSE_CP_INDEX`          | `--index`               | Replica index if service is scaled            |
| `cp`                | `INCUS_COMPOSE_CP_ARCHIVE`        | `--archive`, `-a`       | Keep the source's uid/gid                     |
| `cp`                | `INCUS_COMPOSE_CP_FOLLOW_LINK`    | `--follow-link`, `-L`   | Always follow symlinks in SRC_PATH            |

`exec --dry-run` and `cp --dry-run` have no variable - see the exceptions table
in [Environment Variables](/environment-variables#cli-configuration).
