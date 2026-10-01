---
date: 2026-08-27T23:33:35Z
dateCreated: 2026-07-05T01:03:07.97Z
description: Which parts of the Compose Specification incus-compose supports, what it does differently, and the x-incus extensions for Incus-only options.
editor: markdown
published: true
tags: []
title: Compose Compatibility
leafwiki_id: 9dRX3lBvR
leafwiki_title: Compose Compatibility
leafwiki_created_at: "2026-07-05T03:53:59.388277193Z"
leafwiki_updated_at: "2026-08-27T23:33:35Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Compose Compatibility

incus-compose implements a subset of the Compose Specification. These pages list
what works and what doesn't, one page per top-level compose key.

| Page                                        | Covers                                                                               |
| ------------------------------------------- | ------------------------------------------------------------------------------------ |
| [Services](services/)                       | `image`, `command`, `entrypoint`, `user`, `labels`, `dns`, `sysctls`, `restart`, ... |
| [Networks](networks/)                       | external networks, DHCP ranges, static IPs, aliases                                  |
| [Volumes](volumes/)                         | named volumes, bind mounts, image volumes, prefetching                               |
| [Secrets and configs](secrets-and-configs/) | `secrets:`, `configs:`, overwriting image files                                      |
| [Behavioral Differences](differences/)      | where incus-compose does the same thing differently                                  |

Anything Incus offers beyond the Compose spec - `x-incus`, `x-incus-compose` and
the `compose.incus.yaml` override file - lives in [Extras](/extras).

## Projects

```yaml
x-incus:
  limits.cpu: "4"
  limits.memory: 2049MiB # +1 MiB
  limits.virtual-machines: 0

services:
  web:
    image: docker.io/nginx:alpine
    deploy:
      replicas: 4
    x-incus:
      limits.cpu: "1"
      limits.memory: 512MiB
```

Any
[Project option](https://linuxcontainers.org/incus/docs/main/reference/projects/)
is accepted.

The generic `restricted: "true"` flag, as distinct from scoping options like
`restricted.cluster.groups`, additionally forbids low-level config keys
outright - an instance-level `x-incus` block setting `raw.lxc`, `raw.idmap` or
similar fails with `Use of low-level config "raw.lxc" ... is forbidden` in a
project that has it set.

### Project-level keys

- `name` - Project name
- Project isolation (Incus projects)
- Profiles - Compose profiles

## Not Supported (Yet)

### External Secrets and Configs

`secrets[].external` and `configs[].external` are not supported.

In Docker Swarm, `external: true` means "this secret/config already exists:
don't create it, just reference it by name." You'd pre-create it once (e.g.
`docker secret create db_password ./password.txt`), and any number of
stacks/services could then point at that same object, so rotating it means
updating the one external secret rather than every compose file that uses it.

incus-compose has no equivalent standalone "secret" or "config" resource in
Incus to reference: it only knows how to read a `file`, inline `content`, or an
`environment` variable and push the result into a container as a file. There's
nothing in Incus for `external` to point _at_, so it's not a missing mapping to
fill in later, it's a concept without a target. Use `file`, `content` (configs
only), or `environment` instead.

### Extended Features

Not supported:

- `extends` - Service extension
- `deploy` - Most deployment options (except `replicas`)
- `links` - Legacy linking (use networks)
- `external_links` - Cross-project links

## Testing Compatibility

To test if your compose file works:

```bash
# Validate syntax
incus-compose config --quiet

# Show what will be created
incus-compose config

# Try starting
incus-compose up --no-start

# Check what was created
incus-compose list
```

## Reporting Compatibility Issues

If you find a compose feature that should work but doesn't, please report it
with:

1. Minimal `compose.yaml` that reproduces the issue
2. Expected behavior (what docker-compose does)
3. Actual behavior (what incus-compose does)
4. Incus version: `incus version`
