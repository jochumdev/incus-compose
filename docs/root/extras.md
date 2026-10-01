---
date: 2026-08-27T23:59:45Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: JZUAJnQDRN
leafwiki_title: Extras
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-27T23:59:45Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Extras

The Compose spec covers what is portable across engines. Everything Incus can do
beyond it is reachable from the same compose file, through three escape hatches:

- **`compose.incus.yaml`** - an override file loaded automatically, so the
  upstream `compose.yaml` stays untouched.
- **`x-incus`** - passes any Incus config key straight through to the instance,
  network or volume it sits on. incus-compose does not interpret these.
- **`x-incus-compose`** - features incus-compose implements itself: devices,
  volume placement and seeding, backups, healthd, network, and DNS.

For what the Compose spec itself supports, see
[Compose Compatibility](/compose-compatibility).

## The Incus override file

If a `compose.incus.yaml` file exists next to the selected `compose.yaml`,
incus-compose loads it automatically as an additional Compose file. Use it for
Incus-specific overrides while keeping the upstream Docker Compose file
unchanged.

```text
compose.yaml
compose.incus.yaml
```

Example `compose.incus.yaml`:

```yaml
services:
  web:
    ports: !reset []
    x-incus:
      limits.memory: 512MiB

networks:
  default:
    x-incus:
      ipv4.address: 10.100.0.2/24
      ipv4.gateway: 10.100.0.1
```

Running with the base file also applies the Incus override when present:

```bash
incus-compose -f compose.yaml up
```

The override file follows normal Compose merge rules. For example, `!reset []`
clears a list from the base file.

## x-incus

`x-incus` keys are passed verbatim to Incus and never interpreted by
incus-compose, so any option Incus accepts works the day Incus adds it.

### Instances

Any Incus instance config key can be set via the `x-incus` extension block on a
service definition. Keys are passed verbatim to the Incus instance config on
creation.

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    x-incus:
      limits.memory: 512MiB
      limits.cpu: "2"
      security.privileged: "true"
```

Any
[Incus instance option](https://linuxcontainers.org/incus/docs/main/reference/instance_options/)
is accepted.

### Networks

Any Incus network config key can be set via the `x-incus` extension block on a
network definition. Keys are passed verbatim to the Incus network config on
creation.

```yaml
networks:
  backend:
    x-incus:
      ipv4.address: 10.100.0.1/24
      ipv6.address: fd42:abc::1/64
      ipv4.dhcp.ranges: 10.100.0.100-10.100.0.200
```

Any
[Incus bridge network option](https://linuxcontainers.org/incus/docs/main/reference/network_bridge/)
is accepted.

### Volumes

Any Incus storage volume config key can be set via the `x-incus` extension block
on a volume definition. Keys are passed verbatim to the Incus volume config on
creation.

```yaml
volumes:
  data:
    x-incus:
      size: 10GiB
      block.filesystem: ext4
```

Any
[Incus storage volume option](https://linuxcontainers.org/incus/docs/main/reference/storage_volumes/)
is accepted.

The `x-incus` block also works inline on a volume entry, which is the only way
to set options on a **bind mount** (a bind's source is a path, not a named
volume):

```yaml
services:
  web:
    volumes:
      - type: bind
        source: ./html
        target: /usr/share/nginx/html
        x-incus:
          security.shifted: "false"
```

An inline `x-incus` block takes precedence over the matching named volume
definition. See
[Volume Permissions](/compose-compatibility/differences#volume-permissions) for
`security.shifted`.

## x-incus-compose

These are implemented by incus-compose itself rather than passed through.

### Devices

Attach raw Incus devices to a service's instances with the
`x-incus-compose.devices` block. Each named entry is passed to Incus verbatim;
the `type` key selects the device type and is required.

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    x-incus-compose:
      devices:
        gpu0:
          type: gpu
          gputype: physical
          pci: "0000:01:00.0"
        extra-disk:
          type: disk
          source: /dev/sdb
          path: /mnt/data
```

This is an escape hatch for device types incus-compose does not model natively
(`gpu`, `unix-char`, `usb`, ...). Compose-managed devices (`ports`, `volumes`,
`networks`) should use their native keys. Any
[Incus device](https://linuxcontainers.org/incus/docs/main/reference/devices/)
is accepted; keys collide by device name, so a raw device sharing a name with a
compose-managed one overrides it.

A `gpu` device's `id:` is the DRM ID (`incus info --resources`, under `GPU` ->
`DRM` -> `ID`, typically a small integer), not the `/dev/dri/cardN` or
`renderDNNN` filename - the device name fails with "Failed to detect requested
GPU device" otherwise.

A `unix-hotplug` device matches on `idVendor`/`idProduct` present on a device's
own udev attributes. It cannot reach a character device created several sysfs
levels below the matched USB node by a kernel driver that fans out into a
different subsystem - the DVB character devices a `dvb-usb` driver creates are
one such case. That case is accepted with no error, and the device simply never
appears in the instance.

_Since 1.0.0-beta.22_

### Volume pool

Set `x-incus-compose.pool` on a named volume to place it in a specific Incus
storage pool. Without this the client's default storage pool is used.

```yaml
volumes:
  data:
    x-incus-compose:
      pool: fast-ssd

services:
  app:
    image: docker.io/myapp:latest
    volumes:
      - data:/var/lib/app
```

To move an existing volume to a different pool, stop the project, then use
`incus storage volume move` via the `incus-compose incus` passthrough:

```bash
incus-compose stop
incus-compose incus storage volume move default/vol-library ext/vol-library
incus-compose start
```

Then update `x-incus-compose.pool` in your compose file and run
`incus-compose up --recreate` to reattach.

Volumes are stored with a `vol-` prefix. Long names are hashed, so
`my-very-long-volume-name` may become `vol-a1b2c3d4...`. Use
`incus storage volume list` to find the actual name before moving:

```bash
incus-compose incus storage volume list default
```

### Volume seeding

A bind mount is normally passed through: Incus attaches a disk device and
**incusd** resolves the source path on its own filesystem, so the files have to
be on the server. Set `x-incus-compose.seed: true` inline on the volume entry to
**copy** the source instead, which is how a bind mount works against a server
that is not your machine:

```yaml
services:
  web:
    image: docker.io/library/busybox:glibc
    volumes:
      - type: bind
        source: ./html
        target: /www
        read_only: true
        x-incus-compose:
          seed: true
```

The source is read by incus-compose, on the machine you run it from, and must
exist there. What happens next depends on what it is:

- **A directory** becomes a custom storage volume, filled from the directory
  when the volume is **created**. Later changes on your machine do not
  propagate: the volume goes its own way from there, and `up` will not re-seed
  it. Delete the volume to start over.
- **A single file** is pushed into the instance on **every start**, while it is
  still stopped, overwriting what is there. Handy for a config or a key you want
  refreshed each time.

Seeding is a copy, in one direction. Nothing written inside the container comes
back out, so it does not replace a named volume for data you mean to keep.

Seeding is off by default: bind mounts are plain pass-through unless you ask.

_Since: v1.0.0_

### Backup

Configure `incus-compose backup` with the top-level `x-incus-compose.backup`
extension:

```yaml
x-incus-compose:
  backup:
    pool: hdd

services:
  app:
    image: docker.io/nginx:alpine
    volumes:
      - data:/var/lib/app

volumes:
  data:
```

| Key           | Description                                                                                                               |
| ------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `pool`        | Storage pool the backup copies live in. Defaults to the client's default pool; a separate disk is what makes them useful. |
| `meta_volume` | Volume holding the manifests and the locks. Defaults to `ic-backup-manifest`.                                             |

The key only has to be present, so an empty `backup:` is enough to opt in. See
[CLI Reference - backup](/cli-reference/extensions/backup) for the commands.

### Healthd

Configure the ic-healthd sidecar with the top-level `x-incus-compose.healthd`
extension:

```yaml
x-incus-compose:
  healthd:
    scope: global
    incus: https://:8443
    network: :default
    workers: 128
    restart-workers: 32
    x-incus:
      limits.cpu: 2
      limits.memory: 256MiB

services:
  web:
    image: docker.io/nginx:alpine
```

| Key               | Description                                                                                                                                                                        |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `scope`           | `global` (one shared daemon in the Incus `incus-compose` project, the default) or `project` (a sidecar of this project's own). Loses to a scope the Incus project already carries. |
| `incus`           | The Incus API URL healthd connects to. Defaults to the bridge gateway and the connection's port.                                                                                   |
| `network`         | `<project>:<network>` for a managed network, or a plain bridge name. Defaults to the bridge of the project the daemon runs in.                                                     |
| `workers`         | Health checks the daemon runs at once, over every project it watches. Default 128.                                                                                                 |
| `restart-workers` | Restarts it runs at once, over every project it watches. Default 32.                                                                                                               |
| `x-incus`         | Raw Incus instance config for the sidecar, e.g. `limits.*`.                                                                                                                        |
| `external`        | Use a healthd you run yourself; incus-compose neither creates nor looks one up.                                                                                                    |

`scope`, `incus` and `network` are also `--healthd-scope`, `--healthd-incus` and
`--healthd-network` on the CLI, which override the compose file. See
[Health Checking - Scope](/healthd#scope-one-daemon-or-one-per-project) and
[Network Configuration](/healthd#network-configuration).

With `scope: global` the daemon is shared, so the first project to bring it up
supplies `incus`, `workers`, `restart-workers` and `x-incus`; a later project
asking for something different is warned and ignored.

When this option is set, incus-compose does not create compose-managed Incus
network resources for service network attachments. Instances use the network
devices provided by the copied profile instead. Service-level static IP
assignments (`ipv4_address` / `ipv6_address`) are not supported in this mode
because incus-compose does not create explicit NIC devices.

### Network

Configure default network driver and uplink settings for the project with the
top-level `x-incus-compose.network` extension:

```yaml
x-incus-compose:
  network:
    driver: ovn
    uplink: incusbr0

services:
  web:
    image: docker.io/nginx:alpine
```

| Key      | Description                                                                                                                                                                                                                      |
| -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `driver` | Network driver to use for project networks: `auto` (default, uses OVN if available on the Incus server, otherwise bridge), `ovn`, or `bridge`. Can also be set via `--network-driver` or `INCUS_COMPOSE_NETWORK_DRIVER` on `up`. |
| `uplink` | Uplink parent network to use for OVN networks (e.g. `incusbr0`). Applied to OVN networks that do not define their own uplink. Can also be set via `--network-uplink` or `INCUS_COMPOSE_NETWORK_UPLINK` on `up`.                  |

Individual networks can override the uplink using `x-incus-compose.uplink` (or
`x-incus-compose.parent`):

```yaml
networks:
  custom:
    x-incus-compose:
      uplink: my-uplink
```

### DNS

Configure the ic-dns sidecar with the top-level `x-incus-compose.dns` extension:

```yaml
x-incus-compose:
  dns:
    disabled: false
    scope: global
    zone: shop.example.org
    network: custom-net
    ipv4_address: 10.0.0.2
    ipv6_address: fd42::2
    no_metrics: false
    allow_transfer:
      - 192.168.1.0/24

services:
  web:
    image: docker.io/nginx:alpine
```

| Key              | Description                                                                                                                                                                           |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `disabled`       | Set to `true` to disable DNS sidecar creation and DNS configuration for the project. Equivalent to `--no-dns` on `up`.                                                                |
| `scope`          | `global` (one shared daemon in the Incus `incus-compose` project, the default) or `project` (a sidecar dedicated to this project).                                                    |
| `zone`           | Custom DNS zone domain for the project, replacing the default `<project>.incus`.                                                                                                      |
| `network`        | Network attachment for the DNS sidecar. Defaults to the project bridge/network.                                                                                                       |
| `ipv4_address`   | Static IPv4 address assigned to the DNS sidecar instance.                                                                                                                             |
| `ipv6_address`   | Static IPv6 address assigned to the DNS sidecar instance.                                                                                                                             |
| `no_metrics`     | Disable Prometheus metrics endpoint on the DNS sidecar.                                                                                                                               |
| `allow_transfer` | CIDR prefix(es) permitted to perform zone transfers (AXFR/IXFR). Configures `--allow-transfer` on the sidecar and automatically stamps `user.label.dns.transfer=true` on the project. |
| `transfer`       | Explicitly opt the project zone into transfers (`user.label.dns.transfer=true`) without configuring sidecar CIDRs (useful when sharing a global sidecar).                             |

See [DNS (ic-dns)](/dns) for details on split-horizon resolution and DNS
configuration.
