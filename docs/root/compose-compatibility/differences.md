---
date: 2026-08-28T04:18:09Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: PmU0J7wDRz
leafwiki_title: Behavioral Differences
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T04:18:09Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Behavioral Differences

## Images

**Registries:**

Image names work just like Docker: a bare `nginx:alpine` resolves to
`docker.io/library/nginx:alpine`, and an explicit registry prefix
(`ghcr.io/...`) is honored as-is.

`docker.io`, `ghcr.io`, `quay.io`, `mcr.microsoft.com`, `registry.gitlab.com`
and `codeberg.org` need no setup. Any other registry has to be an Incus remote,
and adding one of the six above overrides its built-in address — which is how
you point at a pull-through cache:

```bash
incus remote add --protocol oci registry.example.com https://registry.example.com
incus remote add --protocol oci docker.io https://docker-mirror.example.com
```

Pointing the built-in registries at your own mirror is also how a proxied or
[air-gapped install](/air-gapped) works, without touching the compose file.

```yaml
# Both work, identical to Docker Compose
image: nginx:alpine # resolves to docker.io/library/nginx:alpine
image: ghcr.io/myorg/app:v1 # explicit registry
```

**Global cache:**

Like Docker, images are cached globally. An image pulled for one project is
available to all projects. This avoids duplicate downloads.

**Registry authentication:**

Docker reads `~/.docker/config.json`. incus-compose asks the remote's
credentials helper, which speaks the same protocol, so a
`docker-credential-pass` or `docker-credential-secretservice` that already holds
your logins is reused as-is:

```bash
incus remote add --protocol oci registry.example.com https://registry.example.com \
  --credentials-helper docker-credential-pass
```

The helper is asked once per registry per command, and the answer covers both
reading the image's config and the pull incusd performs.

A login can sit in the remote's address instead
(`https://user:token@registry.example.com`). That is simpler, but it keeps the
password in `~/.config/incus/config.yml` as plaintext. Either way the pull
itself carries the login to incusd inside the source URL, which incusd logs at
debug level - the Incus image API has no field to put it anywhere else.

A registry served by a built-in default has nowhere to hang a helper, so add it
as a remote first, even when the address does not change:

```bash
incus remote add --protocol oci ghcr.io https://ghcr.io --credentials-helper docker-credential-pass
```

_Since: v1.3.0_

**Platform selection:**

`platform:` takes the same values as docker compose - `linux/amd64`,
`linux/arm64`, `linux/arm/v7`. Unset, the image is pulled for the architecture
of the server incus-compose is connected to.

The cached copy is keyed by platform, so one cache holds every architecture of
an image side by side:

```
docker.io/library/alpine:3.20/amd64   x86_64
docker.io/library/alpine:3.20/arm64   aarch64
```

On a cluster, the architecture of the image is what decides which member runs
the instance, so `platform:` is also how a service is placed on one.

Two services cannot share one image reference at different platforms; give them
separate references or one platform.

## Port Publishing

**Docker Compose:**

```yaml
ports:
  - "8080:80" # iptables NAT rule
```

**incus-compose:**

```yaml
ports:
  - "8080:80" # Incus proxy device
```

```mermaid
flowchart LR
    HOST[host port 8080] --> D{"x-incus-compose.nat?"}
    D -->|"false, the default"| PX["Incus proxy device<br/>a userspace Go process<br/>per forwarded connection"]
    D -->|true| NAT["nftables DNAT rule<br/>kernel mode"]
    PX --> C[container port 80]
    NAT --> C

    LO["127.0.0.1 on the host"] --> PX
    LO -.->|"not reachable"| NAT
```

Both work the same from outside. By default incus-compose uses userspace proxy
devices (a Go process per forwarded connection). For high-throughput services
you can opt in to kernel-mode NAT via a service extension, which installs
nftables DNAT rules instead:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    ports:
      - published: "8081"
        target: "80"
        x-incus-compose:
          nat: true
    networks:
      - frontend
```

> **Warning:** with `nat: true`, published ports are not reachable via
> `localhost`/`127.0.0.1` on the host running incus-compose. The nftables DNAT
> rules only masquerade traffic for the hairpin case (an instance reaching
> itself via its own forwarded address); host-loopback traffic keeps its
> `127.0.0.1` source address, which is dropped or fails to route back. Use the
> host's real (LAN/bridge) address to reach the port, or stick with the default
> userspace proxy if you need `localhost` access to work.

_Since: v1.1.0_

## Network Naming

**Docker Compose:**

```
{project}_{network}  # e.g., myapp_frontend
```

**incus-compose:**

```
{project}-{network}  # e.g., myapp-frontend (if ≤13 chars)
ic-{hash}            # e.g., ic-a1b2c3d4e5 (if >13 chars)
```

Network names are limited to 13 chars for dhclient compatibility.

## Volume Permissions

**Docker Compose:**

- Volumes owned by root by default
- Manual chown often needed

**incus-compose:**

- Volumes automatically shifted to match container's UID/GID
- Reads `oci.uid` and `oci.gid` from image
- Files appear with correct ownership inside container

**Disabling shifting (`security.shifted: "false"`):**

Shifting maps host files to the container's UID/GID so they appear correctly
owned. Set `security.shifted: "false"` via `x-incus` to turn it off, e.g. for a
read-only bind mount you don't want re-owned. Without shifting, the host file
keeps its raw host UID/GID inside the container, which for an unprivileged
container outside the idmap range shows up as `nobody` (65534):

```yaml
services:
  web:
    volumes:
      - type: bind
        source: ./html
        target: /usr/share/nginx/html
        read_only: true
        x-incus:
          security.shifted: "false"
```

```console
$ ls -ln /usr/share/nginx/html/index.html
-rw-r--r-- 1 65534 65534 18 ... index.html
```

For a bind mount this must be set inline on the volume entry (see
[x-incus Volume Extensions](/extras#volumes)).

## External Volumes

**Docker Compose:** an external volume must already exist. Compose will never
create it, and never removes it (not even with `down --volumes` / equivalent),
since it doesn't own the volume's lifecycle.

**incus-compose:** every named volume, external or not, goes through the same
get-or-create path: reuse the Incus storage volume if it already exists, create
it if it doesn't. There's no tracking of "this one was pre-existing."
Concretely, that means:

- A typo'd or renamed volume that would fail fast under Docker (volume not
  found) instead silently creates a new, empty volume here.
- `incus-compose down --volumes` deletes every storage volume tracked for the
  project, including ones marked `external: true`, and there's no protection
  against removing a volume you intended to be pre-existing and shared with
  something else.

If you need to reference a real pre-existing Incus storage volume without
risking it being deleted, avoid `down --volumes` for that project, or manage the
volume directly with `incus storage volume` outside of compose.

## Instance Naming

Instances are named `{service}-{index}` where index starts at 1:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    deploy:
      replicas: 3
```

Creates instances: `web-1`, `web-2`, `web-3`

You can also override replicas via CLI:

```bash
incus-compose up --scale web=5
```

`--scale` applies only to that invocation. Like `docker compose up`, a plain
`up` reconciles each service back to `deploy.replicas` in both directions: it
recreates instances removed by an earlier `--scale` and tears down extras added
by one. Use `--scale` (or edit `deploy.replicas`) to change the persistent
count.

## DNS Resolution

After `up`, both the **service name** and the **instance name** resolve inside
containers:

```
database    → round-robins across all database instances (A/AAAA records)
database-1  → specific instance (registered by Incus dnsmasq)
```

This matches Docker Compose behavior. No configuration is required: records are
written automatically to the project bridge network's `raw.dnsmasq` (for bridge
networks) or answered dynamically by `ic-dns` (for OVN networks), and updated
whenever the scale changes. Pass `--no-dns` (or
`x-incus-compose.dns.disabled: true`) to opt out.

A service can also register extra DNS names for itself via `aliases`; see
[Network Aliases](/compose-compatibility/networks#network-aliases).

**Note:** Setting `raw.dnsmasq` on the bridge disables AppArmor for the dnsmasq
process (not for containers). dnsmasq still runs as an unprivileged user.

## Environment Variables

**Docker Compose:**

```bash
export MY_VAR=value
docker-compose up  # MY_VAR available
```

**incus-compose:**

```bash
export MY_VAR=value
incus-compose up  # MY_VAR NOT available (security)
```

Use `.env` files or `--os-env` flag for docker-compose compatibility.

## Config Output

`config --format=yaml` is byte-identical to `docker compose config`.
`config --format=json` deliberately is not.

Docker renders JSON straight from the compose model, and compose-go tags every
extension field `json:"-"` - so `docker compose config --format json` silently
drops every `x-` block. incus-compose renders JSON through the YAML
representation instead, which keeps them:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    x-incus:
      limits.cpu: "2"
```

```json
{
  "services": {
    "web": {
      "image": "docker.io/nginx:alpine",
      "x-incus": { "limits.cpu": "2" }
    }
  }
}
```

Since `x-incus` and `x-incus-compose` carry most of what makes a compose file
Incus-specific, dropping them would make the JSON output useless for scripting.

Two consequences of rendering through YAML:

- Fields Docker emits as explicit nulls - `command`, `entrypoint`, and a
  network's empty `ipam` - are omitted here rather than written as `null`/`{}`.
- Object keys are sorted alphabetically rather than following the compose-spec
  field order. JSON objects are unordered, so this only matters if you diff the
  raw text.

Parse the JSON rather than diffing it against `docker compose` output.

_Since: v1.2.0_
