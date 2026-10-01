---
date: 2026-08-28T00:10:32Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: dW8AJnQDg
leafwiki_title: Volumes
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:10:32Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Volumes

- Named volumes (Incus custom storage volumes)
- Bind mounts - pass-through when incusd runs on your machine, or copied in with
  `x-incus-compose.seed` against any server (see
  [Extras](/extras#volume-seeding))
- Read-only volumes
- Automatic UID/GID shifting
- tmpfs mounts (with optional size limit)
- `x-incus` extension - pass any Incus volume config key directly (see
  [Extras](/extras#volumes))
- `x-incus-compose.pool` - select the storage pool for a named volume (see
  [Extras](/extras#volume-pool))
- `x-incus-compose.seed` - copy a bind mount's source into the instance (see
  [Extras](/extras#volume-seeding))
- Image volumes - a path the image declares as `VOLUME` gets one of its own (see
  below)
- Prefetching - a volume starts from what the image ships at its target (see
  below)

Not supported:

- Volume driver options

## Image Volumes

A path an image declares as `VOLUME` gets a storage volume of its own, named
after the service and mounted there:

```yaml
services:
  store:
    image: ghcr.io/isso-comments/isso:latest
```

isso declares `/config` and `/db`, so `store` comes up with a volume at each.
Without them Incus mounts a tmpfs over those paths, and isso's database is gone
on the next restart.

This isn't limited to declared `VOLUME` paths: an application container's rootfs
resets to the base image on every restart, so anything written directly into the
filesystem outside a mount is gone the next time it restarts too. A bind mount
target must already exist in the image, or live on storage that persists
independently, since it cannot be created this way.

Declaring anything at the same target takes it over, which is how you choose the
pool, the size, or that the path should not persist at all:

```yaml
services:
  store:
    image: ghcr.io/isso-comments/isso:latest
    volumes:
      - db:/db # a volume of your own, with your own x-incus keys
      - type: tmpfs
        target: /config # deliberately empty on every start
```

One volume per service, shared by its replicas. Turn the whole thing off for a
project with:

```yaml
x-incus-compose:
  auto-volumes: false
```

The volume is named after the service and the path, `vol-auto-store-db`, so it
cannot collide with a name you chose. An instance brings its volumes up and
takes them down again, so `down --volumes` removes them - after a plain `down`
there is no instance left to ask, and `down --project` is what clears them. The
next `up` recreates the instance and adopts the same volumes.

_Since: v1.3.0_

## Prefetching

A volume created empty starts from whatever the image holds at the path it is
mounted over, as docker fills an empty volume from the image. This matters for a
config directory the image ships:

```yaml
services:
  web:
    image: docker.io/nginx:alpine
    volumes:
      - conf:/etc/nginx/conf.d

volumes:
  conf:
```

`conf` arrives holding the image's `default.conf` instead of being empty. Only
volumes are filled, never bind mounts, and only on first creation - a volume
that already exists is left alone, whatever the image says.

`nocopy` keeps it empty:

```yaml
volumes:
  - type: volume
    source: conf
    target: /etc/nginx/conf.d
    volume:
      nocopy: true
```

Plain files and directories are copied, with their mode and owner. Symlinks,
devices, sockets and fifos are skipped and named in a warning; docker copies
them. A path the image does not have, or that holds nothing, leaves an empty
volume and is not an error.

_Since: v1.3.0_
