---
date: 2026-08-27T23:33:35Z
dateCreated: 2026-07-05T01:03:37.823Z
description: How incus-compose pulls and caches OCI images - from a registry remote, into the shared incus-compose-cache project, then copied into your own.
editor: markdown
published: true
tags: []
title: Image Resource
leafwiki_id: yKmu3_fvg
leafwiki_title: Image Resource
leafwiki_created_at: "2026-07-05T03:54:01Z"
leafwiki_updated_at: "2026-08-27T23:33:35Z"
leafwiki_creator_id: vOmfrlBDg
leafwiki_last_author_id: vOmfrlBDg
---

# Image Resource

The Image resource handles OCI image pulling and caching in Incus.

## 3-Stage Image Flow

Images go through three stages:

1. **Remote** - OCI registry (docker.io, ghcr.io)
2. **Cache** - Local image store (the `incus-compose-cache` project unless
   overridden via `--image-cache`)
3. **Project** - Per project copy of the image

Projects are created with `features.images=true`, so each project keeps its own
image store. Creating an instance copies the image from the cache into the
active project. These per-project copies are removed on `down` (see
[Delete](#delete)); the cache itself lives in a separate project and persists.

This design provides:

- **Faster subsequent runs** - no re-pulling from registry
- **No registry rate limits** - cached locally after first pull
- **Persistent cache** - survives `down`/`up` cycles and project deletion

## The cache is keyed by platform

A stored alias resolves to one fingerprint, so the cached alias carries the
architecture and the bare reference is never written there:

```
docker.io/library/alpine:3.20/amd64   x86_64
docker.io/library/alpine:3.20/arm64   aarch64
```

The suffix is the registry spelling without the OS (`amd64`, `arm/v7`), taken
from `ImageConfig.Platform` or, unset, from the connected server's own
architecture. The per-project copy in hop B keeps the bare reference: one
project, one run, one architecture.

Without this the cache is shared cluster-wide but arch-blind, so the first
member to materialize a reference fixes its architecture for every other member
that reuses it - and since `images.architecture` is what Incus filters cluster
members on, the wrong image also places the instance on the wrong member.

**An OCI pull is pinned to a manifest digest.** `ociResolveSource` reads the
index over the registry API, picks the manifest for the wanted platform, and the
pull asks for `reference@sha256:...` rather than the tag. incusd resolves an
unpinned OCI reference with `skopeo inspect`, which reports the architecture of
whichever cluster member ran it. Pinning is what lets any member fetch any
architecture.

Everything else - simplestreams, a native `incus:` remote - resolves server-side
and cannot be pinned, so the stored image's architecture is checked afterwards
and a mismatch fails instead of caching the wrong thing under the key.

Variants matter here: an index publishes `arm/v6` and `arm/v7` as one
architecture distinguished only by `Platform.Variant`, so both are compared in
the registry spelling rather than through Incus' architecture table, which folds
them. `arm64/v8` and `amd64/v3` fold to the architecture, which is what that
table models.

## Image Status

Images report their status via `Status()`:

| Status  | Description        |
| ------- | ------------------ |
| Unknown | Not downloaded yet |
| Cached  | In cache project   |

## ImageConfig

Configuration for image sources:

```go
type ImageConfig struct {
    // Source is the image server to copy the image from.
    Source incusClient.ImageServer

    // CacheClient is the project-scoped client to use as the image cache.
    // Takes precedence over CacheProject.
    CacheClient *Client

    // CacheProject is the project name to use as cache (for CLI users).
    // The project will be created if it doesn't exist.
    // Ignored if CacheClient is set.
    CacheProject string

    // LockVolume names the storage volume in the cache project that holds
    // the per-alias advisory locks. Empty means DefaultLockVolume.
    LockVolume string

    // Platform is the OCI platform the image is wanted for, for example
    // linux/arm64. Empty means the connected server's architecture.
    Platform string

    // Remote is the domain part of the image reference.
    Remote string

    // Image is the image reference without the remote prefix.
    Image string
}

const (
    DefaultCacheProject = "incus-compose-cache"
    DefaultLockVolume   = "ic-image-lock"
)
```

### Cache Configuration

- **CacheClient**: a caller-built cache client, used as-is
- **CacheProject**: For CLI users, specifies project name (auto-created); the
  CLI sets this from `--image-cache` / `INCUS_COMPOSE_IMAGE_CACHE`
- **Default**: Uses the `incus-compose-cache` project
  (`client.DefaultCacheProject`)

The cache is a `*Client`, not a bare `incusClient.InstanceServer`, because the
[lock volume](#locking) is a `StorageVolume` resource that has to be ensured in
the cache project - which needs the resource machinery a `*Client` carries.

```go
// Provide a cache client directly
cache, _ := gc.EnsureProject("my-image-cache", EnsureProjectWithCreate())

img, _ := project.Resource(client.KindImage, "docker.io/nginx:alpine", &client.ImageConfig{
    CacheClient: cache,
})

// CLI usage - specify cache project name
img, _ := project.Resource(client.KindImage, "docker.io/nginx:alpine", &client.ImageConfig{
    CacheProject: "my-image-cache",
})

// Override the lock volume name
img, _ := project.Resource(client.KindImage, "docker.io/nginx:alpine", &client.ImageConfig{
    CacheClient: cache,
    LockVolume:  "my-locks",
})
```

`ClientLockVolume` sets the name once for every image on a client, mirroring
`ClientCacheProject`; `ImageConfig.LockVolume` overrides it per image.

## Image Reference Parsing

The reference is split into the remote, which says where to fetch from, and the
image, which is what to ask that remote for. A name whose prefix is a remote in
the Incus configuration is taken as a native `remote:image` reference;
everything else is parsed as a Docker-style one with
`github.com/distribution/reference`:

| Reference                       | Remote      | Image                   | Alias                             |
| ------------------------------- | ----------- | ----------------------- | --------------------------------- |
| `nginx:alpine`                  | `docker.io` | `library/nginx:alpine`  | `docker.io/library/nginx:alpine`  |
| `docker.io/library/alpine:3.18` | `docker.io` | `library/alpine:3.18`   | `docker.io/library/alpine:3.18`   |
| `ghcr.io/myorg/myapp:v1.0`      | `ghcr.io`   | `myorg/myapp:v1.0`      | `ghcr.io/myorg/myapp:v1.0`        |
| `alpine`                        | `docker.io` | `library/alpine:latest` | `docker.io/library/alpine:latest` |
| `localhost/myapp:dev`           | `local`     | `myapp:dev`             | `local/myapp:dev`                 |
| `images:alpine/edge`            | `images`    | `alpine/edge`           | `images:alpine/edge`              |

The alias is `Image.IncusName()`, and it is what the resource store deduplicates
on - which is why `nginx:alpine` and `docker.io/library/nginx:alpine` are one
resource rather than two. A native reference keeps its `remote:image` form,
since that is already unique.

None of the three can be set by hand: they come from the reference, so the alias
cannot disagree with what was asked for.

## Ensure Flow

### The store

There is one concept the whole flow turns on:

```
store = cache ?? project
```

With caching on, the store is the shared cache project. With caching off
(`--image-cache ""`), the store _is_ the compose project. Everything else is
expressed against `store`, which is why caching off needs no special-casing - it
collapses one hop rather than taking a different path.

Ensure is then **two hops, each skipped when the image is already there**:

| Hop | From   | To      | Skipped when                                                      |
| --- | ------ | ------- | ----------------------------------------------------------------- |
| A   | source | store   | the alias is already in the store                                 |
| B   | store  | project | `store == project`, or the project already holds that fingerprint |

```mermaid
flowchart LR
    subgraph on["caching on (default)"]
        direction LR
        S1[source] -->|hop A| C1[(cache project<br/>= store)]
        C1 -->|hop B| P1[compose project]
    end

    subgraph off["caching off (--image-cache '')"]
        direction LR
        S2[source] -->|hop A| P2[compose project<br/>= store]
        P2 -.->|hop B is a no-op| P2
    end
```

### Sources

A source is either a **registry remote** or the **local builder** (a service
with `build:`). They differ only inside hop A; everything around it is shared.

The source may be `nil` - but only when the alias is already in the store.
Nothing in the store and nothing to make it from is a hard failure.

### The store hit is authoritative

If the alias is in the store, Ensure contacts **nothing**: no registry, no
builder. This is what makes "build once, use many" work - the second project to
want an image copies it out of the store instead of rebuilding or re-pulling.

The cost is that an edited Dockerfile behind an unchanged image name is not
noticed. `--build` is the escape hatch, matching `docker compose`.

### Flow

```mermaid
flowchart TD
    S([Ensure]) --> K{build configured?}
    K -->|yes| KB[source = builder]
    K -->|no| KR[source = registry remote<br/>or nil]
    KB --> ST
    KR --> ST

    ST[store = cache ?? project] --> LK[[lock alias in store]]

    LK --> POL{--build?}
    POL -->|yes| DEL
    POL -->|no| A1
    DEL[delete from store and project] --> A1

    A1{alias present in store?}
    A1 -->|yes| UL
    A1 -->|no| GATE{source usable?<br/>create allowed, policy != never}
    GATE -->|no| ERRU[[unlock]]
    ERRU --> ERR([hard failure])
    GATE -->|yes| MAKE[materialize into store:<br/>build, or copy from registry]
    MAKE --> OCI[extract OCI config,<br/>persist as image properties]
    OCI --> UL

    UL[[unlock]] --> B1{store == project?}
    B1 -->|yes| DONE([ensured])
    B1 -->|no| B2{same fingerprint in project?}
    B2 -->|yes| DONE
    B2 -->|no| CP[copy store to project<br/>properties carry the OCI config]
    CP --> DONE
```

Ensure contacts the source on a store miss and nowhere else. It deletes nothing
either: refreshing is the caller's decision, made before this runs. See
[Refresh](#refresh-pull-always).

### Pull policy

| Policy              | Behavior                                                                                       |
| ------------------- | ---------------------------------------------------------------------------------------------- |
| `missing` (default) | Store hit wins. Source is contacted only on a store miss.                                      |
| `always`            | The same, on whatever the caller left in the store after [Refresh](#refresh-pull-always).      |
| `never`             | Store hit wins; a store miss is a hard failure. Never contacts the source, for air-gapped use. |

`--build` is the equivalent force for build sources, and is independent of
`--pull`.

### OCI config extraction

`ociStoreConfig` runs **once**, on the way out of hop A, and writes its result
into the image's properties. Hop B copies the image with those properties
attached, so a project copy never re-derives what is already known.

It reads the registry only when nothing already holds the answer: a build passes
the config the builder reported, and a [refresh](#refresh-pull-always) leaves it
in `ImageState` on its way to deciding to re-pull.

A `USER` naming a user rather than numbering one is the exception: `oci.uid`
takes nothing but a number, and only the image's own `/etc/passwd` resolves the
name. Hop A stores the value verbatim in `user.incus-compose.oci.user` and
leaves the resolution to the project, where `ResolveUser` does it alongside the
compose `user:` override.

## Reading the image

An image has no file API, so anything needing its bytes goes through
`Image.SFTP`: a stopped instance created from the image, read over SFTP. A
stopped instance mounts no disk devices, so what it shows is the image's own
rootfs.

The instance is created on first use and then kept - `SFTP` hands every caller
the same connection, and `Client.Done` removes it when the command ends. It is
named `ic-seed-*`, carries `user.incus-compose.temp=true` so a hard kill leaves
something reapable, and gets an explicit root disk because a profile-less create
fails without one.

Two callers: `StorageVolume` filling a volume from a path in the image, and
`ResolveUser`.

### ResolveUser

`ResolveUser` maps a `user[:group]` value - the compose override or the image's
own `USER` - to the numbers `oci.uid` / `oci.gid` take.

It reads the image only when it must: both sides numeric returns straight away,
which is what keeps the common case free of an instance. A name is looked up in
`/etc/passwd` or `/etc/group`, and one the image does not define is
`ErrNoSuchUser` rather than a silent 0.

A named user with no group takes that user's own group, as `login` would. A
numeric uid with no group keeps GID 0, since reading the image for it would cost
an instance per service that sets `user:`.

## Locking

Hop A is guarded by a per-alias advisory lock, so two workers - or two separate
`incus-compose` invocations - cannot pull or build the same alias into the store
at once, and a force delete cannot race a reader.

Image locking uses the unified [Advisory Locking](/developer/client/locking)
system: it calls
`r.client.Global().Lock(ctx, "image/"+r.cacheAlias(), imageLockStale)` on the
global `LocksVolume` (`locks` in `globalProject`).

Before acquiring the image lock, `Global().Lock` waits on `LockGlobalProject` to
ensure no infrastructure upgrade is in flight. Once the global project is clear,
it acquires the lock on the image path with `stale = 2m`. The holder heartbeats
while a slow pull or a long build runs, and a crashed holder is reaped rather
than wedging the cache for everyone.

```go
func (r *Image) lockStore(ctx context.Context) (func(), error) {
    if r.cache == nil {
        return func() {}, nil
    }

    return r.client.Global().Lock(ctx, "image/"+r.cacheAlias(), imageLockStale)
}
```

One volume holds every lock; the per-alias granularity is one file per alias
inside it under `image/<alias>`.

```mermaid
sequenceDiagram
    participant A as project A
    participant L as locks volume (global)
    participant S as store (cache)
    participant B as project B

    A->>L: lock(image/alias)
    B->>L: lock(image/alias)
    Note over B: blocks
    A->>S: miss - build/pull into store
    A->>S: extract OCI config to properties
    A->>L: unlock
    L-->>B: acquired
    B->>S: hit - nothing to do
    B->>L: unlock
    Note over A,B: hop B runs unlocked in both,<br/>each into its own project
```

Hop B is deliberately outside the lock: it targets the compose project, so two
projects copying the same store image are not in conflict.

**With caching off there is no lock**, because there is no cache project to host
the volume. That is the correct outcome rather than a gap: the store is then the
compose project, which is not shared with anyone, so the race the lock exists to
prevent cannot occur.

The `"Alias already exists"` fallback - re-read and adopt the winner - stays as
a backstop for writers outside our control, such as an older `incus-compose` or
a hand-run `incus image copy`.

## Source Configuration

The source is resolved from the image reference itself, against the Incus CLI
configuration the client loaded at startup:

```go
img, _ := project.Resource(client.KindImage, "docker.io/nginx:alpine", &client.ImageConfig{})
```

A registry is somewhere the **server** is pointed at, never something
incus-compose connects to: the copy request carries the remote's address and
protocol, and incusd does the pull. Only a native `incus:` remote is dialed, to
resolve an alias to a fingerprint before the request is built.

The registries in `client.WellKnownRegistries` - `docker.io`, `ghcr.io`,
`quay.io`, `mcr.microsoft.com`, `registry.gitlab.com`, `codeberg.org` - resolve
without any setup. Anything else must be an Incus remote, and a configured
remote overrides a well-known one:

```bash
incus remote add --protocol oci registry.example.com https://registry.example.com
```

A reference whose domain is neither returns an error from Ensure:

```go
img, _ := project.Resource(client.KindImage, "registry.example.com/app:latest", &client.ImageConfig{})
err := client.RunAction(ctx, img, client.ActionEnsure, client.OptionCreate())
// err: "image source not configured"
```

## Delete

Delete removes the **per-project copy** of the image from the active project
(the copy hop B left behind). It is idempotent: if no copy exists, it is a
no-op. The cache lives in a separate project and is not touched by a plain
Delete, so cached images persist across `down`/`up` cycles. Cache cleanup is a
separate concern (e.g. a future `prune` command). With caching off the store
_is_ the project, so Delete removes the only copy and the next `up`
re-materializes it.

```go
err := client.RunAction(ctx, img, client.ActionDelete) // active-project copy, keeps cache
```

`OptionCache()` takes the cached copy with it, under the same per-alias lock hop
A holds. That is what [Refresh](#refresh-pull-always) uses, and the only thing
that deletes out of a cache shared with every other project on the server.

```go
err := client.RunAction(ctx, img, client.ActionDelete, client.OptionCache())
```

The cache is resolved by Ensure, so an image this process never ensured has none
to delete and keeps its cached copy.

## Refresh (`--pull always`)

`up --pull always` re-fetches the images whose source has moved off what the
store holds. Without `--pull`, a stored image is reused as-is.

**Ensure does none of it.** It is three steps at the caller, and the middle one
is the only place that decides to destroy anything:

1. Ensure with `OptionResolveSource()`, which reads what the source holds now
   into `ImageState.SourceFingerprint` and changes nothing.
2. Delete the images whose `SourceFingerprint` differs from their stored alias,
   with `OptionCache()` so the cached copy goes too - the store hit is
   authoritative, so leaving it behind would simply copy the stale image back.
3. Ensure again. The deleted ones now miss the store and take the plain create
   path.

`cmd/incus-compose`'s `refreshImages` is those three steps; `pull`, `up`, `run`
and `healthd up` all go through it.

### Comparing like for like

The stored alias target is **not** the manifest digest. incusd runs
`skopeo inspect` and hashes the concatenated layer digest strings, so step 1
computes the same thing from the registry manifest - see `ociSourceFingerprint`.
Comparing anything else makes every image read as changed on every run.

An image index carries one fingerprint per architecture, so the manifest is
picked for the architecture the stored image was built for, not the client's. A
native `incus:` remote is simply asked, since it answers with the fingerprint
directly.

The config hangs off that same manifest, so `ociResolveSource` reads both in one
walk and step 1 flattens the config into the same `ImageState` fields an image's
properties are read into, beside the fingerprint. The
[OCI config extraction](#oci-config-extraction) after the re-pull then writes
properties from those instead of asking the registry again.

That is also why `clearState` only forgets the fetch - the alias, the ETag and
the size. The OCI config describes the image's content, which is exactly what a
refresh deletes the image in order to pull back.

This is also why `RefreshImage` is not used: a registry update that only changes
manifest metadata leaves the layers alone, so Incus considers the image already
up to date even though the tag now points somewhere else.

A source the client cannot reach leaves `SourceFingerprint` empty, and an empty
side never counts as a difference - so a client that cannot see the registry its
server pulls from keeps the image it has rather than failing the run.

## Built Images

A build source differs from a registry source only inside hop A: instead of
pointing the server at a remote, the builder runs and its rootfs/metadata
tarballs are uploaded into the store. Everything around it - the store-hit
check, the lock, the OCI extraction, hop B - is the same code.

That is what makes "build once, use many" work with `build:` left in the compose
file. The first `up` anywhere misses the store and builds; every project after
that finds the alias and copies. It is also why a client with no local builder
can consume an image someone else built: hop A never runs for it. See
[Builds - Reusing a built image](/builds#reusing-a-built-image-across-projects).

Because the store entry is keyed by the built image's Incus alias
(`r.incusName`, derived from the local image name), two builds that resolve to
the same image name are the same entry. `no_cache: true` opts a service out of
the shared store - it then builds into the project every time, at the cost of no
longer seeding the cache for anyone else. The same applies with `r.cache == nil`
(`--image-cache ""`), where the store is the project to begin with. See
[Builds - Image Caching](/builds#image-caching) for the user-facing version.

_Since: v1.1.0_

## Podman Compatibility

Images with "localhost" remote (common in podman) are converted to "local":

```go
// Input: "localhost/myimage:latest"
// Remote becomes: "local"
```

## Priority and Parallel Downloads

Images have priority 1024, placing them after profiles but before networks.

When Stack.Run processes multiple images, they download in parallel via
WorkerPool.

## Auto-Update

Images are configured with `AutoUpdate: true`. Incus periodically checks the
source registry and refreshes the cached image. Running containers are not
affected; new containers use the updated image.
