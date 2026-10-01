---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: ki8A17QDgz
leafwiki_title: Inspecting a project
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Inspecting a project

## ps

List containers (instances). This is the `docker compose`-compatible variant;
[`list`](/cli-reference/extensions#list) reports the project as a whole and is
the one to reach for unless you need docker parity.

```
incus-compose ps [SERVICE...]
```

| Option          | Description                                                      |
| --------------- | ---------------------------------------------------------------- |
| `-a`, `--all`   | Show all containers (including stopped ones)                     |
| `-q`, `--quiet` | Only display Incus instance names                                |
| `--services`    | Display compose service names instead of instances               |
| `--format`      | table (default) or json                                          |
| `--with-deps`   | Also list linked services (depends_on) - incus-compose extension |

## config

Validate and render compose file.

```
incus-compose config [SERVICE...]
```

| Option           | Description                              |
| ---------------- | ---------------------------------------- |
| `--format`       | yaml (default) or json                   |
| `-q`, `--quiet`  | Validate only                            |
| `--services`     | List services                            |
| `--volumes`      | List volumes                             |
| `--networks`     | List networks                            |
| `--profiles`     | List profiles                            |
| `--images`       | List images                              |
| `--environment`  | Print environment used for interpolation |
| `--variables`    | Print model variables and default values |
| `-o`, `--output` | Save to file                             |

## logs

View container output.

```
incus-compose logs [SERVICE...]
```

| Option           | Description                                                                |
| ---------------- | -------------------------------------------------------------------------- |
| `-f`, `--follow` | Follow output                                                              |
| `--with-deps`    | Also show logs from linked services (depends_on) - incus-compose extension |

Missing instances are skipped with a warning; logs from available instances are
still shown.

## top

Display resource usage per instance.

```
incus-compose top
```

| Option            | Description                                    |
| ----------------- | ---------------------------------------------- |
| `-c`, `--columns` | Columns to display, as `incus top` spells them |
| `--format`        | table (default) or compact                     |
| `--refresh`       | Refresh delay in seconds, 10 at the lowest     |

This is `incus top` scoped to the project: a live table of CPU time, memory and
disk **per instance**, refreshing until Ctrl-C. `docker compose top` reports per
_process_ instead, which Incus has no API for, so the two do not print the same
thing.

It takes no service arguments - `incus top` has no name filter - and a
project-scoped ic-healthd sidecar is listed along with the services.

_Since: v1.3.0_

## events

Receive real time events from the project.

```
incus-compose events
```

| Option         | Description                                                   |
| -------------- | ------------------------------------------------------------- |
| `-t`, `--type` | Event type to listen for (repeatable; default: `lifecycle`)   |
| `--format`     | `pretty` (default), `yaml` or `json`                          |
| `--json`       | Short for `--format=json`, for `docker compose events` parity |

This is `incus monitor` scoped to the project. The default `--type lifecycle` is
the closest thing to docker's container events; pass `-t logging` or
`-t operation` to widen it.

```bash
incus-compose events
incus-compose events --json | jq -r .action
```

It takes no service arguments - `incus monitor` filters by project, not by
instance - and has no `--since`/`--until`, which Incus does not offer.

_Since: v1.3.0_

## port

Print the host binding of a published port.

```
incus-compose port [options] SERVICE PRIVATE_PORT
```

| Option       | Description                                                          |
| ------------ | -------------------------------------------------------------------- |
| `--index`    | Index of the container if service has multiple replicas (default: 0) |
| `--protocol` | Protocol of the port, `tcp` (default) or `udp`                       |

`PRIVATE_PORT` is the port inside the instance - the right-hand side of a
`ports:` entry - and the answer is the address and port the host publishes it
on:

```bash
$ incus-compose port web 80
0.0.0.0:8080
```

Unlike `docker compose port`, a stopped instance answers too: the binding is a
device on the instance, not something the running process holds. A port that is
not published is an error naming the ports the instance does have.

_Since: v1.3.0_

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command  | Variable                           | Flag              | Description                              |
| -------- | ---------------------------------- | ----------------- | ---------------------------------------- |
| `ps`     | `INCUS_COMPOSE_PS_ALL`             | `--all`, `-a`     | Show all containers, including stopped   |
| `ps`     | `INCUS_COMPOSE_PS_QUIET`           | `--quiet`, `-q`   | Only display Incus instance names        |
| `ps`     | `INCUS_COMPOSE_PS_SERVICES`        | `--services`      | Display services instead of instances    |
| `ps`     | `INCUS_COMPOSE_PS_FORMAT`          | `--format`        | Output format: `table` or `json`         |
| `ps`     | `INCUS_COMPOSE_PS_WITH_DEPS`       | `--with-deps`     | Also list linked services                |
| `logs`   | `INCUS_COMPOSE_LOGS_FOLLOW`        | `--follow`, `-f`  | Follow log output                        |
| `config` | `INCUS_COMPOSE_CONFIG_FORMAT`      | `--format`        | Output format: `yaml` or `json`          |
| `config` | `INCUS_COMPOSE_CONFIG_SERVICES`    | `--services`      | Print the service names, one per line    |
| `config` | `INCUS_COMPOSE_CONFIG_VOLUMES`     | `--volumes`       | Print the volume names, one per line     |
| `config` | `INCUS_COMPOSE_CONFIG_NETWORKS`    | `--networks`      | Print the network names, one per line    |
| `config` | `INCUS_COMPOSE_CONFIG_PROFILES`    | `--profiles`      | Print the profile names, one per line    |
| `config` | `INCUS_COMPOSE_CONFIG_QUIET`       | `--quiet`, `-q`   | Only validate, don't print anything      |
| `config` | `INCUS_COMPOSE_CONFIG_IMAGES`      | `--images`        | Print the image names, one per line      |
| `config` | `INCUS_COMPOSE_CONFIG_ENVIRONMENT` | `--environment`   | Print environment used for interpolation |
| `config` | `INCUS_COMPOSE_CONFIG_VARIABLES`   | `--variables`     | Print model variables and default values |
| `config` | `INCUS_COMPOSE_CONFIG_OUTPUT`      | `--output`, `-o`  | Save to file (default: stdout)           |
| `top`    | `INCUS_COMPOSE_TOP_COLUMNS`        | `--columns`, `-c` | Columns to display                       |
| `top`    | `INCUS_COMPOSE_TOP_FORMAT`         | `--format`        | Output format: table or compact          |
| `top`    | `INCUS_COMPOSE_TOP_REFRESH`        | `--refresh`       | Refresh delay in seconds                 |
| `events` | `INCUS_COMPOSE_EVENTS_TYPE`        | `--type`, `-t`    | Event types to listen for                |
| `events` | `INCUS_COMPOSE_EVENTS_FORMAT`      | `--format`        | Output format: pretty, yaml or json      |
| `events` | `INCUS_COMPOSE_EVENTS_JSON`        | `--json`          | Short for `--format=json`                |
| `port`   | `INCUS_COMPOSE_PORT_INDEX`         | `--index`         | Replica index if service is scaled       |
| `port`   | `INCUS_COMPOSE_PORT_PROTOCOL`      | `--protocol`      | Protocol of the port, tcp or udp         |
