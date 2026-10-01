---
date: 2026-08-27T23:48:20Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: gW8017wvg
leafwiki_title: Secrets and configs
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-27T23:48:20Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Secrets and configs

## Secrets

- `secrets` - File-based secrets pushed into container at `/run/secrets/{name}`
- `secrets[].file` - Read secret from file
- `secrets[].environment` - Read secret from environment variable
- Service `secrets[].target` - Custom target path
- Service `secrets[].uid` / `secrets[].gid` - File ownership
- Service `secrets[].mode` - File permissions (default: 0400)

Setting one of `uid`/`gid` and not the other leaves the other at 0, as docker
does. Setting neither is where we differ: the file is written owned by the
instance user, where docker uses root. A 0400 secret owned by root is unreadable
to the non-root user most OCI images run as, which makes docker's default
useless here. The same applies to `configs[]`.

## Configs

- `configs` - Config files pushed into the container at `/{name}` by default
- `configs[].file` - Read config from a file
- `configs[].content` - Inline content in the compose file
- `configs[].environment` - Read config from an environment variable
- Service `configs[].target` - Custom target path
- Service `configs[].uid` / `configs[].gid` - File ownership
- Service `configs[].mode` - File permissions (default: `0444`); the writable
  bit is always ignored, per the compose-spec, even if an explicit mode with a
  write bit is set

```yaml
configs:
  app_config:
    file: ./app_config.txt

services:
  app:
    configs:
      - app_config
      - source: app_config
        target: /etc/app/config.txt
        uid: "1000"
        gid: "1000"
        mode: 0o440
```

### Overwriting Image Files

Configs and secrets are written into the instance before it first starts, and
they replace a file the image already ships at that target. This is how you
override an application's own default config:

```yaml
services:
  web:
    image: docker.io/library/caddy:2-alpine
    configs:
      - source: caddyfile
        target: /etc/caddy/Caddyfile

configs:
  caddyfile:
    file: ./Caddyfile
```

Docker achieves the same by mounting over the path, so the image file is only
hidden for the container's lifetime. incus-compose writes into the instance's
root filesystem instead, so the replacement is permanent for that instance - the
original is gone until the instance is recreated.

A target inside a volume is written into that volume instead, since a mount
would otherwise hide it. So a config lands on top of what
[prefetching](/compose-compatibility/volumes#prefetching) put there, which is
the order docker mounts them in:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    volumes:
      - conf:/etc/nginx/conf.d
    configs:
      - source: site
        target: /etc/nginx/conf.d/site.conf
```

The volume gets the image's `default.conf` and your `site.conf` beside it, and
`site.conf` is rewritten on every start. A target under a tmpfs or a
pass-through bind has nowhere to be written before the instance starts, so it is
warned about and skipped.

_Changed in 1.3.0_: such a file used to be written into the instance's
filesystem, where the mount hid it.

_Changed in 1.2.0_: a target that already existed in the image was previously
left untouched, which silently ignored the config or secret.

`/run` is a special case: Incus mounts a tmpfs over it for OCI containers, which
would hide a secret or config written there before the instance starts. It skips
that tmpfs when the image populates `/run`, which a pushed file does, so the
default `/run/secrets/{name}` target survives.
