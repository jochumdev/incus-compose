---
date: 2026-08-28T00:09:59Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: VWUAJ7wvR
leafwiki_title: Services
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:59Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Services

- `image` - OCI images from any registry
- `command` - Override container command (replaces the image's, see below)
- `entrypoint` - Override the container entrypoint (see below)
- `working_dir` - Set working directory
- `user` - Run the container process as a specific UID/GID (numeric only, see
  below)
- `dns` / `dns_search` / `domainname` - DNS resolver configuration (see below)
- `sysctls` - Kernel parameters, set as `linux.sysctl.*` config (see below)
- `environment` - Environment variables
- `labels` - Metadata (stored as `user.label.*` config, see below)
- `depends_on` - Service dependency order
- `networks` - Multiple networks per service
- `ports` - Port publishing
- `volumes` - Named volumes and bind mounts
- `deploy.replicas` - Service scaling (instances named `{service}-{index}`)
- `restart` - Restart policies (`no`, `always`, `on-failure`, `unless-stopped`)
- `pre_start` - Init containers run before container start (see below)
- `post_start` - Lifecycle hooks run after container start (see below)
- `x-incus` extension - pass any Incus project, network and instance option
  directly (see [Extras](/extras#x-incus))
- Top-level `x-incus-compose.healthd` - configure the ic-healthd sidecar's
  network and Incus endpoint (see [Extras](/extras#healthd))
- Top-level `x-incus-compose.backup` - where `incus-compose backup` puts the
  copies (see [Extras](/extras#backup))

## Labels

Compose `labels` are stored on the instance as `user.label.<key>` config keys.
Both the map and list forms work:

```yaml
services:
  app:
    image: docker.io/nginx:alpine
    labels:
      caddy: whoami.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"
  api:
    image: docker.io/nginx:alpine
    labels:
      - "traefik.http.routers.api.rule=Host(`api.example.com`)"
```

becomes:

```yaml
config:
  user.label.caddy: whoami.example.com
  user.label.caddy.reverse_proxy: "{{upstreams 80}}"
  user.label.traefik.http.routers.api.rule: "Host(`api.example.com`)"
```

Two labels are always added:

| Key                                | Value                    |
| ---------------------------------- | ------------------------ |
| `user.label.incus-compose.project` | the compose project name |
| `user.label.incus-compose.service` | the compose service name |

Read them back with the `incus` passthrough:

```bash
incus-compose incus config get app-1 user.label.caddy
```

**Service discovery** - the `user.label.` prefix keeps compose labels out of the
`user.*` namespace incus-compose uses for its own keys, and mirrors the label
conventions of reverse proxies and DNS managers:

- [Traefik](https://doc.traefik.io/traefik/) - `traefik.enable`,
  `traefik.http.routers.<name>.rule`, ...
- [caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy) -
  `caddy`, `caddy.reverse_proxy`
- [dnsweaver](https://maxfield-allison.github.io/dnsweaver/) - reads the Traefik
  router labels above

> None of these tools support incus-compose yet: they discover services over the
> Docker socket, not the Incus API. incus-compose only exposes the labels as
> `user.label.*` instance config; consuming them needs an Incus-aware discovery
> integration.

_Changed in 1.0.0-rc.2_: labels moved from `user.<key>` to `user.label.<key>`,
and the `incus-compose.project` / `incus-compose.service` labels were added.

## User

The `user` attribute overrides the user the container process runs as, mapping
to the image's `oci.uid` / `oci.gid`:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    user: "1000:1001" # UID:GID; the GID is optional
```

Either side may be a number or a name the image's own `/etc/passwd` and
`/etc/group` define, so `1000`, `1000:root`, `nobody` and `netbox:root` all
work:

```yaml
services:
  netbox:
    image: docker.io/netboxcommunity/netbox:v4.6-5.0.2
    user: "netbox:root"
```

A name the image does not define is an error, not a fall back to root. The same
resolution applies to the image's own `USER`, so an image built with
`USER nginx` runs as `nginx` without a `user:` of its own.

A **name** with no group takes that user's own group, as `login` would - busybox
runs `nobody` as `65534:65534`. A **number** with no group keeps GID 0, because
resolving it would mean reading the image for every service that sets `user:`.

An image has no file API, so resolving a name starts a stopped instance from the
image and reads it over SFTP. That is one instance per image per command, shared
by every service using that image and removed when the command ends. A `user:`
that is numeric on both sides never reads the image at all.

> The
> [Compose Specification](https://github.com/compose-spec/compose-spec/blob/main/05-services.md#user)
> only says `user` "overrides the user used to run the container process" and
> does not document a value format. The `UID:GID` form is Docker's convention,
> which we follow.

_Since: 1.0.0-beta.22_

_Changed in 1.3.0_: user and group names resolve against the image; only numbers
were accepted before.

## Entrypoint and Command

`entrypoint:` behaves as the compose spec describes: it replaces the image's
entrypoint, and the image's default command is discarded, so the container runs
exactly `entrypoint:` followed by `command:`.

```yaml
services:
  web:
    image: docker.io/library/busybox:glibc
    entrypoint: ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
```

| `entrypoint:` | `command:` | The container runs           |
| ------------- | ---------- | ---------------------------- |
| set           | unset      | `entrypoint`                 |
| set           | set        | `entrypoint` + `command`     |
| set           | `[]`       | `entrypoint`                 |
| `[]`          | set        | `command`                    |
| `[]`          | unset      | rejected - nothing to run    |
| unset         | set        | image entrypoint + `command` |

`command:` on its own **replaces the image's `CMD` and keeps its `ENTRYPOINT`**,
as Docker does. An image with `ENTRYPOINT ["caddy"]` and `CMD ["run"]` plus
`command: ["version"]` runs `caddy version`.

Incus cannot answer which part of an image's argv was the `ENTRYPOINT`: it
reports the two already concatenated. incus-compose reads the split from the
image's own config in the registry instead, before the image is pulled, and
keeps it in the image's `oci.entrypoint` and `oci.cmd` properties. A locally
built image gets the same from the builder.

Reading that config is the one thing incus-compose asks of a registry directly
rather than pointing the server at it, so a client that cannot reach the
registry its server pulls from logs a warning and stores no split. `command:`
then runs on its own, without the image's entrypoint in front of it - set
`entrypoint:` to say exactly what should run, which needs nothing from the
image.

_Since: v1.3.0_ - until v1.2.0 `command:` appended to the image's whole argv, so
the example above ran `caddy run version`.

## DNS

`dns`, `dns_search`, and `domainname` map to Incus's `oci.dns.*` instance config
keys, which seed the container's initial `/etc/resolv.conf`:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    dns:
      - 8.8.8.8
      - 1.1.1.1
    dns_search:
      - example.com
    domainname: example.com
```

becomes:

```yaml
config:
  oci.dns.nameservers: 8.8.8.8,1.1.1.1
  oci.dns.search: example.com
  oci.dns.domain: example.com
```

Each key is only set when the corresponding compose field is non-empty.
`dns_opt` has no Incus equivalent and is not mapped.

_Since: v1.1.0_

## Sysctls

`sysctls` sets kernel parameters on the instance, mapping each key to
`linux.sysctl.<key>`. Both the map and list forms work:

```yaml
services:
  vpn:
    image: docker.io/nginx:alpine
    sysctls:
      net.ipv4.conf.all.src_valid_mark: 1
      net.ipv6.conf.all.disable_ipv6: 0
  web:
    image: docker.io/nginx:alpine
    sysctls:
      - net.core.somaxconn=1024
```

becomes:

```yaml
config:
  linux.sysctl.net.ipv4.conf.all.src_valid_mark: "1"
  linux.sysctl.net.ipv6.conf.all.disable_ipv6: "0"
  linux.sysctl.net.core.somaxconn: "1024"
```

The value applies when the instance starts and survives a restart, on both
privileged and unprivileged containers. Which parameters are writable from
inside an unprivileged container is Incus's business, not ours: a key the kernel
refuses in that namespace fails at start rather than being ignored.

_Since: v1.2.0_

## Environment

- `.env` file loading
- `env_file` directive
- Variable interpolation
- Default values: `${VAR:-default}`
- Required variables: `${VAR?error message}`

The bare `${VAR}` form, with no `:-`/`?` operator, does not fail on a missing or
empty value - it silently interpolates to an empty string. Only the
`${VAR?message}` form hard-fails.

## Build

See [Builds](/builds) for supported options, builder selection, and platform
handling.

## Health Checks

Supported via the `ic-healthd` sidecar. See [Health Checking](/healthd) for full
details, including config keys, defaults, security model, and `healthd`
management commands.

The healthcheck status (`starting`, `healthy`, `unhealthy`) is reported in the
`Status` column of `incus-compose list` and `incus-compose ps` when healthchecks
are configured.

A service with no `healthcheck:` block inherits the one its image declares, and
`disable: true` opts out of both - see
[Dockerfile HEALTHCHECK](/healthd#dockerfile-healthcheck).

## Resource Limits

`deploy.resources` is not mapped. Use `x-incus` to set Incus instance limits
directly:

```yaml
services:
  app:
    x-incus:
      limits.cpu: "1"
      limits.memory: 512MiB
```

Any Incus instance config key is accepted. See [Architecture](/extras#x-incus)
for full details.

## Restart Policies

Restart policies map to Incus boot configuration:

| Compose `restart` | Incus Config                                   |
| ----------------- | ---------------------------------------------- |
| `no` (default)    | `boot.autostart=false`                         |
| `always`          | `boot.autostart=true`                          |
| `on-failure`      | `boot.autostart=true`, `boot.autorestart=true` |
| `unless-stopped`  | Uses last-state behavior (Incus default)       |

```yaml
services:
  app:
    image: docker.io/nginx:alpine
    restart: always
```

Restart enforcement is handled by the ic-healthd sidecar, including `restart`
without a healthcheck - see [Health Checking](/healthd#restart-without-a-test).

## Lifecycle Hooks (pre_start and post_start)

incus-compose supports Compose Specification `pre_start` and `post_start`
lifecycle hooks to run initialization tasks before or after the main service
container starts.

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    volumes:
      - data:/data
    pre_start:
      - command: ["sh", "-c", "echo ready > /data/initialized"]
    post_start:
      - command: ["touch", "/var/run/web.ready"]
        privileged: true
volumes:
  data:
```

### pre_start

`pre_start` runs one or more ephemeral init containers in order before the main
service container boots:

```yaml
services:
  app:
    image: docker.io/node:20-alpine
    volumes:
      - app-data:/app/data
    pre_start:
      - command: ["npm", "run", "migrate"]
      - image: docker.io/alpine:edge
        command: ["sh", "-c", "chown -R 1000:1000 /app/data"]
        privileged: true
```

- **Shared storage & tmpfs:** The runner instance inherits the service's volumes
  and tmpfs mounts so init scripts can prepare schemas, initialize files, or fix
  permissions. Published ports and proxy devices are stripped from the runner so
  host port bindings do not conflict.
- **Volume seeding (`seed: true`):** When a volume configuration sets
  `x-incus-compose: { seed: true }`, seed files are copied into the `pre_start`
  runner rather than the main instance, allowing migration or init scripts to
  consume or move them.
- **Custom images:** If `image:` is specified on a hook item, the runner uses
  that image; otherwise it defaults to the service's image. Custom hook images
  are recognized and pulled by `incus-compose pull` and listed by
  `incus-compose config --images`.
- **Scaling (`per_replica`):**
  - When `per_replica: false` (the default), the init container runs once before
    replica 1 starts.
  - When `per_replica: true`, each replica executes its own init container
    before that replica starts.
  - Runner instances are named `{service}-pre_start-{index}` (or
    `{service}-{replica}-pre_start-{index}` when `per_replica: true`).
- **Success and failure behavior:**
  - On exit code `0`, the runner container is automatically stopped and removed.
  - If a hook exits non-zero, startup aborts immediately and the main container
    is never started. The failed runner container is kept stopped for inspection
    with `incus-compose incus exec` or `incus-compose logs`.

| Field         | Type          | Description                                                          |
| ------------- | ------------- | -------------------------------------------------------------------- |
| `command`     | string / list | Command to execute in the init container                             |
| `image`       | string        | Optional image override (defaults to service `image`)                |
| `user`        | string        | User / UID to run as                                                 |
| `privileged`  | boolean       | Run container as privileged root                                     |
| `working_dir` | string        | Working directory                                                    |
| `environment` | map / list    | Environment variables (overrides/extends service env)                |
| `per_replica` | boolean       | Run for each replica (`true`) or once per service (`false`, default) |

_Since: v1.4.0_

### post_start

`post_start` executes commands inside the running service container immediately
after container startup, before waiting for health checks:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    user: "1000"
    post_start:
      - command: ["nginx", "-t"]
      - command: ["chmod", "600", "/etc/ssl/certs/app.key"]
        privileged: true
```

- **Execution:** Runs non-interactively inside the started container using the
  Incus exec API.
- **Permissions:** Unprivileged hooks must run as the instance's configured
  `user` (or the image's default `USER`); any mismatch is rejected. Setting
  `privileged: true` allows running the command as `root` (`0:0`).
- **Failure:** If any `post_start` command exits with a non-zero exit code, the
  service startup fails.

| Field         | Type          | Description                                                         |
| ------------- | ------------- | ------------------------------------------------------------------- |
| `command`     | string / list | Command to execute inside the started instance                      |
| `user`        | string        | User to run as (must match instance user unless `privileged: true`) |
| `privileged`  | boolean       | Run command as root (`0:0`)                                         |
| `working_dir` | string        | Working directory                                                   |
| `environment` | map / list    | Environment variables                                               |

_Since: v1.4.0_
