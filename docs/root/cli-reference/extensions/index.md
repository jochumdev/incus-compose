---
date: 2026-08-28T04:18:09Z
dateCreated: 2026-08-27T23:56:29Z
leafwiki_id: 488Qx7QvgM
leafwiki_title: Extensions
leafwiki_created_at: "2026-08-27T23:56:29.231878374Z"
leafwiki_updated_at: "2026-08-28T04:18:09Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Extensions

`incus-compose` groups its commands into two categories, and `--help` shows the
split. The **compose** category is the `docker compose` verbs. Everything here
is in the **extensions** category: it has no `docker compose` counterpart, or it
does the job better than the counterpart does.

| Command                         | Does                                                |
| ------------------------------- | --------------------------------------------------- |
| [`backup`](backup/)             | Snapshot project data volumes into a backup project |
| [`list`](#list)                 | List resources - the one to reach for over `ps`     |
| [`port-forward`](#port-forward) | Forward a local TCP port into an instance           |
| [`incus`](#incus)               | Run an `incus` command in the project context       |
| [`healthd`](#healthd)           | Manage the ic-healthd sidecar                       |
| [`dns`](#dns)                   | Manage the ic-dns sidecar                           |
| [`self-update`](#self-update)   | Update incus-compose to the latest release          |

## list

List project resources. This is the one to reach for: `ps` is the
`docker compose`-compatible variant and reports instances only, where `list`
reports the project as a whole and knows about the ic-healthd sidecar.

```
incus-compose list [SERVICE...]
```

| Option         | Description                                    |
| -------------- | ---------------------------------------------- |
| `--format`     | table (default), yaml, json                    |
| `--no-healthd` | Exclude the ic-healthd sidecar from the output |

The `IMAGE` column shows the compose image for each service. The ic-healthd
sidecar is listed by default; its image is resolved from the instance's stored
metadata. Pass `--no-healthd` to omit it.

_Changed in 1.0.0-rc.1_: healthd is listed by default.

## port-forward

Forward a local TCP port into an instance, published or not.

```
incus-compose port-forward [options] SERVICE TARGET_PORT [LISTEN_PORT]
```

| Option      | Description                                                          |
| ----------- | -------------------------------------------------------------------- |
| `--index`   | Index of the container if service has multiple replicas (default: 0) |
| `--dry-run` | Print the `incus` command instead of running it                      |

This has no `docker compose` counterpart. It runs a local TCP listener and
forwards every connection made to it into the instance, so it reaches a port
that was never published - a database you did not want on the host, for
instance. `LISTEN_PORT` defaults to `TARGET_PORT`, and either port can be
prefixed with an address to use, IPv6 in square brackets:

```bash
incus-compose port-forward db 5432          # 127.0.0.1:5432 -> 5432 in db
incus-compose port-forward db 5432 15432    # 127.0.0.1:15432 -> 5432 in db
incus-compose port-forward db 5432 0.0.0.0:15432
```

Like `exec`, it shells out to your local `incus` client and targets the instance
via `INCUS_PROJECT`.

_Since: v1.3.0_

## incus

Run any `incus` command scoped to the current compose project. All flags and
arguments are passed through verbatim; only `INCUS_PROJECT` is injected.

```
incus-compose incus COMMAND [ARGS...]
```

Examples:

```bash
incus-compose incus list                        # list instances in this project
incus-compose incus config show web-1           # show instance config
incus-compose incus config set web-1 limits.memory 512MiB
incus-compose incus exec web-1 -- bash
```

Equivalent to `INCUS_PROJECT=<project> incus COMMAND [ARGS...]`.

## healthd

Manage the ic-healthd sidecar. See [Health Checking](/healthd) for full details.

```
incus-compose healthd <subcommand>
```

| Subcommand        | Description                                               |
| ----------------- | --------------------------------------------------------- |
| `logs [--follow]` | Stream the ic-healthd container log                       |
| `reload`          | Send SIGHUP to the ic-healthd process                     |
| `restart`         | Restart the ic-healthd container                          |
| `status`          | Print the status, IP addresses, and metrics of healthd    |
| `up`              | Create the sidecar, or replace one running an older image |
| `down [--force]`  | Stop and remove the sidecar                               |

Each follows the project's scope, so in a `global`-scope project they act on the
shared daemon in the Incus `incus-compose` project. With no compose file present
they all act on the shared daemon directly: `healthd up` creates one, the rest
fail with `no ic-healthd is running` when there is none. `healthd down` asks
first when other projects rely on that daemon; `--force` skips the question and
is required without a terminal.

_Since: v1.3.0_: `healthd status`.

`healthd status` prints the healthd sidecar's status, IP addresses, and
optionally Prometheus metrics:

| Option      | Description                                     | Default |
| ----------- | ----------------------------------------------- | ------- |
| `--format`  | Output format: `text` or `json`                 | `text`  |
| `--port`    | HTTP port to query on the sidecar               | `9153`  |
| `--metrics` | Include Prometheus metrics in the status report | `false` |

`healthd up` also accepts `--image`, `--binary`, `--incus`, `--network`,
`--scope`, `--pull` and `--timeout`. See
[Health Checking - Scope](/healthd#scope-one-daemon-or-one-per-project) and
[Network Configuration](/healthd#network-configuration).

## dns

Manage the ic-dns sidecar. See [DNS](/dns) for full details.

```
incus-compose dns <subcommand>
```

| Subcommand        | Description                                           |
| ----------------- | ----------------------------------------------------- |
| `logs [--follow]` | Stream the ic-dns container log                       |
| `status`          | Print the status, IP addresses, and metrics of ic-dns |
| `up`              | Create or recreate the ic-dns sidecar                 |
| `down [--force]`  | Stop and remove the ic-dns sidecar                    |

`dns status` prints the ic-dns sidecar's status, IP addresses, and optionally
Prometheus metrics:

| Option      | Description                                     | Default |
| ----------- | ----------------------------------------------- | ------- |
| `--format`  | Output format: `text` or `json`                 | `text`  |
| `--port`    | HTTP port to query on the sidecar               | `9153`  |
| `--metrics` | Include Prometheus metrics in the status report | `false` |

## self-update

Update incus-compose to the latest release from GitHub.

```
incus-compose self-update
```

| Option          | Description                                                                               |
| --------------- | ----------------------------------------------------------------------------------------- |
| `--draft`       | Also consider draft releases when checking for updates (works only with GITHUB_TOKEN set) |
| `--pre-release` | Also consider pre-releases when checking for updates                                      |

This command is only available when both conditions are met:

1. The binary was built with a release version (not `latest` / development
   builds)
2. The binary file is writable by the current user

When available, `self-update` checks the
[lxc/incus-compose](https://github.com/lxc/incus-compose) GitHub releases for a
newer version matching the current OS and architecture. If a newer version is
found, the binary is replaced in-place. If you are already on the latest
version, no action is taken.

_Changed v1.0.0: the `--drafts` option has been added_

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command           | Variable                                | Flag             | Description                              |
| ----------------- | --------------------------------------- | ---------------- | ---------------------------------------- |
| `list`            | `INCUS_COMPOSE_LIST_FORMAT`             | `--format`       | Output format: `table`, `yaml` or `json` |
| `list`            | `INCUS_COMPOSE_NO_HEALTHD`              | `--no-healthd`   | Don't list the healthd sidecar           |
| `port-forward`    | `INCUS_COMPOSE_PORT_FORWARD_INDEX`      | `--index`        | Replica index if service is scaled       |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_IMAGE`           | `--image`        | Healthd OCI image                        |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_BINARY`          | `--binary`       | Local ic-healthd binary path             |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_INCUS`           | `--incus`        | Incus API URL for the sidecar            |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_NETWORK`         | `--network`      | Network for the sidecar (project scope)  |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_SCOPE`           | `--scope`        | `global` or `project`                    |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_PULL`            | `--pull`         | Pull policy                              |
| `healthd up`      | `INCUS_COMPOSE_HEALTHD_TIMEOUT`         | `--timeout`      | Timeout for stopping                     |
| `healthd down`    | `INCUS_COMPOSE_HEALTHD_IMAGE`           | `--image`        | Healthd OCI image                        |
| `healthd down`    | `INCUS_COMPOSE_HEALTHD_DOWN_FORCE`      | `--force`        | Stop a shared daemon without asking      |
| `healthd down`    | `INCUS_COMPOSE_HEALTHD_TIMEOUT`         | `--timeout`      | Timeout for stopping                     |
| `healthd logs`    | `INCUS_COMPOSE_HEALTHD_LOGS_FOLLOW`     | `--follow`, `-f` | Follow log output                        |
| `healthd restart` | `INCUS_COMPOSE_HEALTHD_RESTART_TIMEOUT` | `--timeout`      | Timeout for stopping                     |
| `healthd status`  | `INCUS_COMPOSE_HEALTHD_STATUS_FORMAT`   | `--format`       | Output format: `text` or `json`          |
| `healthd status`  | `INCUS_COMPOSE_HEALTHD_HTTP_PORT`       | `--port`         | HTTP port to query (default: 9153)       |
| `healthd status`  | `INCUS_COMPOSE_HEALTHD_STATUS_METRICS`  | `--metrics`      | Include Prometheus metrics in output     |
| `dns status`      | `INCUS_COMPOSE_DNS_STATUS_FORMAT`       | `--format`       | Output format: `text` or `json`          |
| `dns status`      | `INCUS_COMPOSE_DNS_HTTP_PORT`           | `--port`         | HTTP port to query (default: 9153)       |
| `dns status`      | `INCUS_COMPOSE_DNS_STATUS_METRICS`      | `--metrics`      | Include Prometheus metrics in output     |
| `self-update`     | `INCUS_COMPOSE_SELF_UPDATE_DRAFT`       | `--draft`        | Also consider draft releases             |
| `self-update`     | `INCUS_COMPOSE_SELF_UPDATE_PRE_RELEASE` | `--pre-release`  | Also consider pre-releases               |

`port-forward --dry-run` has no variable - see the exceptions table in
[Environment Variables](/environment-variables#cli-configuration).

The `healthd` rows are the `incus-compose healthd <subcommand>` management
commands, distinct from `up`'s own `--healthd-*` flags. The daemon itself reads
a further set of variables - see
[The ic-healthd daemon](/environment-variables#the-ic-healthd-daemon).
