---
date: 2026-08-28T00:09:59Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: ui80J7QDgz
leafwiki_title: Networks
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:59Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Networks

- Bridge and OVN networks (OVN is auto-selected when supported by Incus)
- Network isolation between services
- DNS resolution by service name and by instance name
- Extra DNS names per service via `aliases` (see below)
- External networks (pre-existing Incus networks)
- `x-incus` extension - pass any Incus network config key directly (see
  [Extras](/extras#networks))
- Automatic DHCP range configuration on creation (see below)
- Static IP assignment per service via `ipv4_address` / `ipv6_address` (see
  below)

Not supported:

- Custom network drivers

## Network Drivers (OVN and Bridge)

incus-compose supports both **OVN** and **bridge** network drivers.

When creating a new project, incus-compose automatically selects OVN if the
Incus server has OVN networking configured and includes the networking and ACL
bugfixes from Incus 7.5+ or 7.0.2 LTS. Otherwise, it falls back to bridge
networks. Existing projects preserve the driver they were created with.

You can configure the network driver and uplink network using the top-level
`x-incus-compose.network` extension:

```yaml
x-incus-compose:
  network:
    driver: ovn
    uplink: incusbr0
```

- `driver`: `auto` (default), `ovn`, or `bridge`. Can also be overridden via the
  `--network-driver` flag or `INCUS_COMPOSE_NETWORK_DRIVER` environment variable
  on `up`.
- `uplink`: Uplink parent network for OVN networks (e.g. `incusbr0`). Can also
  be overridden via `--network-uplink` or `INCUS_COMPOSE_NETWORK_UPLINK` on
  `up`.

Individual networks can also override the uplink using `x-incus-compose.uplink`
(or `x-incus-compose.parent`):

```yaml
networks:
  backend:
    x-incus-compose:
      uplink: incusbr0
```

## External Networks

Mark a network as `external: true` to attach services to a pre-existing Incus
network. incus-compose will never create or delete an external network.

```yaml
networks:
  shared:
    external: true
```

Set `name:` when the Incus network is not called what the compose file calls it.
A bare value is an Incus network name, taken literally — use it for a bridge you
manage yourself:

```yaml
networks:
  shared:
    external: true
    name: alpha:dns # the "dns" network of the "alpha" compose project
```

The reference goes through the same
[naming rules](/compose-compatibility/differences#network-naming) the owning
project used, so it keeps resolving after a rename to a hash — `alpha:dns`
becomes `alpha-dns`, and a pair long enough to exceed the interface limit
becomes the same `ic-` hash on both sides. Only the project that declares the
network creates it; everyone else is `external: true`.

**Name resolution** — incus-compose probes the following candidates in order and
uses the first one that exists in Incus:

1. `name:` value — literal, only when it names no project
2. `name:` value — resolved (`{project}-{network}`, or its hash)
3. Compose network name — raw
4. Compose network name — sanitized

If none of the candidates match an existing network, `up` fails with a not-found
error.

_Since: v1.2.0_

## Automatic DHCP Ranges

When a managed bridge network is created, incus-compose automatically configures
DHCP ranges if they are not already set:

**IPv4** - The first quarter of the address block is reserved for static
assignment. The DHCP range starts at that boundary:

| Subnet | Static range   | DHCP range       |
| ------ | -------------- | ---------------- |
| /24    | `.1-.63`       | `.64-.254`       |
| /16    | `.0.0-.63.255` | `.64.0-.255.254` |
| /28    | `.1-.3`        | `.4-.14`         |

**IPv6** - The first 256 addresses (`::0-::ff`) are reserved for static; DHCP
runs from `::100` to `::ffff`. Stateful DHCPv6 (`ipv6.dhcp.stateful`) is enabled
automatically.

Setting `ipv4.dhcp.ranges` or `ipv6.dhcp.ranges` in `x-incus` disables
auto-calculation for that protocol. Existing networks (already present in Incus
when `up` runs) are never modified.

## Static IP Assignment

A service can be assigned a fixed IP on a specific network using the standard
Compose `ipv4_address` / `ipv6_address` fields on the per-service network
attachment:

:::warning An address without a netmask (e.g. `10.100.0.2` instead of
`10.100.0.2/24`) is invalid and fails silently. :::

```yaml
services:
  db:
    image: docker.io/postgres:16-alpine
    networks:
      backend:

  web:
    image: docker.io/nginx:alpine
    depends_on:
      db: service_healthy
    networks:
      backend:
      frontend:
        ipv4_address: 10.100.0.2/24
        ipv6_address: fd42:abc::2/64

networks:
  frontend:
    x-incus:
      ipv4.address: "10.0.0.1/24"
      ipv6.address: "fd42:abc::1/64"

  backend:
    internal: true
```

The address is set as `ipv4.address` / `ipv6.address` on the Incus NIC device.
The bridge's built-in DHCP server reserves it so the instance always receives
that address on the network.

The address must fall within the static zone (first quarter of the block) to
avoid conflicts with DHCP-assigned addresses.

Setting `internal: true` on a network disables its gateway by setting
`ipv4.gateway` and `ipv6.gateway` to `none`. Override this per-service with
`x-incus-compose.internal: false`.

_`internal: true` since: v1.1.0_

## Network Aliases

The standard Compose `aliases` field on a service's network attachment registers
extra DNS names for that instance:

```yaml
services:
  db:
    image: docker.io/postgres:16-alpine
    container_name: my-db
    networks:
      default:
        aliases:
          - db.mydomain.lan
```

Each alias becomes a `cname=<alias>,<instance>` record in the network's
`raw.dnsmasq` on bridge networks, or a `user.label.dns.aliases` label on the
instance for OVN networks using `ic-dns`, resolving straight to the instance,
with no DHCP lease to wait for, unlike the IP-based service-name records
described in
[DNS Resolution](/compose-compatibility/differences#dns-resolution). Aliases on
networks shared by multiple projects (`external: true` / `name:`) coexist
without clobbering each other's records, the same way service-name records do.

:::warning Because a CNAME alias can only point at one target, `aliases` is for
single-instance services. When a service is scaled, `incus-compose` assigns the
alias only to the first replica (`<service>-1`), avoiding contested name
collisions in `ic-dns` and `dnsmasq`. Use the service name, which does
round-robin, for scaled services instead. :::

_Since: v1.1.0_
