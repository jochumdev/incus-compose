---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
tags: []
leafwiki_id: 1iUA17QDR
leafwiki_title: Up and Down
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# Up and Down

## up

Create and start containers.

```
incus-compose up [SERVICE...]
```

| Option                 | Description                                                                                                                                                                                                                                        |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `-d`, `--detach`       | Detached mode: run containers in the background                                                                                                                                                                                                    |
| `--recreate`           | Recreate containers even if they exist                                                                                                                                                                                                             |
| `--no-start`           | Don't start containers after creating; implies `--detach`                                                                                                                                                                                          |
| `--pull`               | Pull policy: `always` (refresh from the registry, recreating instances the refresh moved off their image), `missing`/`policy` (use the store if present), `never` (never contact a registry; fail when the image is not stored); default: `policy` |
| `--build`              | Rebuild build-configured service images before starting containers, recreating the instances that use them                                                                                                                                         |
| `--no-build`           | Do not build images; fail if a required built image is missing                                                                                                                                                                                     |
| `--builder`            | Preferred builder, binary name or absolute path; empty for auto-detect                                                                                                                                                                             |
| `--no-deps`            | Don't start linked services (depends_on)                                                                                                                                                                                                           |
| `--timeout`            | Stop/start timeout (default: 1m)                                                                                                                                                                                                                   |
| `--dependency-timeout` | Max time to wait for `service_healthy` depends_on (default: 5m; `0` = no limit)                                                                                                                                                                    |
| `--scale`              | Scale service: `web=3` (repeatable)                                                                                                                                                                                                                |
| `--no-dns`             | Don't start or configure DNS for the project                                                                                                                                                                                                       |
| `--dns-image`          | ic-dns image (default: `ghcr.io/lxc/incus-compose/ic-dns:latest`)                                                                                                                                                                                  |
| `--no-healthd`         | Don't create healthd sidecar for healthchecks                                                                                                                                                                                                      |
| `--external-healthd`   | Use an existing (unmanaged) healthd; don't create or look one up                                                                                                                                                                                   |
| `--healthd-image`      | Healthd OCI image; `{version}` is replaced with the incus-compose version                                                                                                                                                                          |
| `--init`               | Image the `run` helper comes from, fetched here so a one-off works later; `{version}` is replaced with the incus-compose version                                                                                                                   |
| `--healthd-binary`     | Path to local ic-healthd binary (uses images:alpine/edge instead of OCI image)                                                                                                                                                                     |
| `--healthd-incus`      | Incus API URL healthd connects to; overrides `x-incus-compose.healthd.incus`; unset uses `core.https_address`, else the bridge IP                                                                                                                  |
| `--healthd-network`    | Network for healthd; overrides `x-incus-compose.healthd.network`; the bridge of the project it runs in if unset                                                                                                                                    |
| `--healthd-scope`      | `global` (shared daemon in the Incus `incus-compose` project, the default) or `project`; loses to a scope the project already carries                                                                                                              |
| `--network-driver`     | Network driver to use for project networks: `auto` (default), `ovn`, or `bridge`                                                                                                                                                                   |
| `--network-uplink`     | Uplink network for OVN networks (e.g. `incusbr0`)                                                                                                                                                                                                  |

Without `--detach`, `up` streams logs from all started services (equivalent to
running `logs --follow` immediately after). Use `--detach` to return as soon as
containers are started. `--no-start` implies it: there is nothing to stream logs
from.

For services with `build:`, `up` builds missing images by default. Use `--build`
to force a rebuild or `--no-build` to require the image to already exist.
`--build` also recreates the instances of the services whose image it rebuilt -
a rebuilt image only reaches an instance created from it again. Every other
service is left alone; `--recreate` is how you recreate the whole project. See
[Builds](/builds) for details.

`--pull always` recreates on the same rule: an instance whose image the refresh
replaced is torn down and made again from the new one. Only what the pull itself
replaced counts - under every other policy a running instance keeps the image it
was created from, even when the compose file now names a different one.

_Since: v1.3.0_

## down

Stop and remove containers. Per-project image copies are removed too; volumes
and the image cache are kept. Use `--volumes` to also delete volumes while
keeping the project, or `--project` to remove everything (project and volumes).

```
incus-compose down [SERVICE...]
```

| Option               | Description                                                              |
| -------------------- | ------------------------------------------------------------------------ |
| `--project`          | Remove the project (and its volumes)                                     |
| `--volumes`          | Also delete volumes, but keep the project                                |
| `--rmi`              | Remove images used by services: `local` or `all` (docker compose compat) |
| `--images`           | Remove known images from the project (equivalent to `--rmi local`)       |
| `--timeout`          | Stop timeout (default: 10s)                                              |
| `--no-deps`          | Don't stop linked services (depends_on)                                  |
| `--no-networks`      | Don't touch networks                                                     |
| `--external-healthd` | Use an existing (unmanaged) healthd; don't look one up                   |
| `--no-healthd`       | Don't stop/remove healthd sidecar                                        |
| `--no-dns`           | Don't stop/remove dns sidecar (project-scoped)                           |

An instance takes its own volumes down with it, so `--volumes` reaches the ones
[an image declared](/compose-compatibility/volumes#image-volumes) as well. After
a plain `down` the instance is gone and there is nothing left to ask: those
volumes stay until `--project`, or until the next `up` recreates the instance
that owns them.

_Changed in 1.0.0-rc.1_: `--volumes` is now no more an alias for `--project` but
deletes volumes.

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command   | Variable                              | Flag                   | Description                               |
| --------- | ------------------------------------- | ---------------------- | ----------------------------------------- |
| `up`      | `INCUS_COMPOSE_UP_NO_START`           | `--no-start`           | Don't start containers after creating     |
| `up`      | `INCUS_COMPOSE_UP_TIMEOUT`            | `--timeout`            | Timeout for stopping/starting a service   |
| `up`      | `INCUS_COMPOSE_UP_DEPENDENCY_TIMEOUT` | `--dependency-timeout` | Max wait for `service_healthy` depends_on |
| `up/down` | `INCUS_COMPOSE_SCALE`                 | `--scale`              | Scale SERVICE to NUM instances            |
| `up`      | `INCUS_COMPOSE_UP_PULL`               | `--pull`               | Pull policy                               |
| `up`      | `INCUS_COMPOSE_UP_BUILD`              | `--build`              | Build images before starting              |
| `up`      | `INCUS_COMPOSE_UP_BUILDER`            | `--builder`            | Preferred builder binary or path          |
| `up`      | `INCUS_COMPOSE_UP_NO_BUILD`           | `--no-build`           | Do not build images even if missing       |
| `up`      | `INCUS_COMPOSE_UP_NO_DEPS`            | `--no-deps`            | Don't start linked services               |
| `up`      | `INCUS_COMPOSE_UP_DETACH`             | `--detach`, `-d`       | Run containers in the background          |
| `up/down` | `INCUS_COMPOSE_NO_HEALTHD`            | `--no-healthd`         | Don't create the healthd sidecar          |
| `up/down` | `INCUS_COMPOSE_NO_DNS`                | `--no-dns`             | Don't start/configure or remove DNS       |
| `up`      | `INCUS_COMPOSE_DNS_IMAGE`             | `--dns-image`          | ic-dns image                              |
| `up`      | `INCUS_COMPOSE_EXTERNAL_HEALTHD`      | `--external-healthd`   | Use healthd but don't create/look it up   |
| `up`      | `INCUS_COMPOSE_HEALTHD_IMAGE`         | `--healthd-image`      | Healthd OCI image                         |
| `up`      | `INCUS_COMPOSE_HEALTHD_BINARY`        | `--healthd-binary`     | Local ic-healthd binary path              |
| `up`      | `INCUS_COMPOSE_HEALTHD_INCUS`         | `--healthd-incus`      | Incus API URL for the sidecar             |
| `up`      | `INCUS_COMPOSE_HEALTHD_NETWORK`       | `--healthd-network`    | Network for the sidecar                   |
| `up`      | `INCUS_COMPOSE_HEALTHD_SCOPE`         | `--healthd-scope`      | `global` or `project`                     |
| `up`      | `INCUS_COMPOSE_NETWORK_DRIVER`        | `--network-driver`     | Network driver (`auto`, `ovn`, `bridge`)  |
| `up`      | `INCUS_COMPOSE_NETWORK_UPLINK`        | `--network-uplink`     | Uplink network for OVN networks           |
| `down`    | `INCUS_COMPOSE_DOWN_RMI`              | `--rmi`                | Remove images used by services            |
| `down`    | `INCUS_COMPOSE_DOWN_IMAGES`           | `--images`             | Remove known images from the project      |
| `down`    | `INCUS_COMPOSE_DOWN_TIMEOUT`          | `--timeout`            | Timeout for stopping                      |
| `down`    | `INCUS_COMPOSE_DOWN_NO_DEPS`          | `--no-deps`            | Don't stop linked services                |
| `down`    | `INCUS_COMPOSE_EXTERNAL_HEALTHD`      | `--external-healthd`   | Use healthd but don't look it up          |
| `down`    | `INCUS_COMPOSE_DOWN_NO_NETWORKS`      | `--no-networks`        | Don't touch networks                      |

`up --recreate` and `down --project`/`--volumes` have no variable - see the
exceptions table in
[Environment Variables](/environment-variables#cli-configuration).
