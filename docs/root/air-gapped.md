---
date: 2026-08-28T05:01:42Z
dateCreated: 2026-08-27T23:47:20Z
leafwiki_id: UtxvxnQDg
leafwiki_title: Air-gapped and Proxied Installs
leafwiki_created_at: "2026-08-27T23:47:20Z"
leafwiki_updated_at: "2026-08-28T05:01:42Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Air-gapped and Proxied Installs

`incus-compose pull` is the only command that needs a registry. Everything after
it - `up`, `down`, `start`, `stop`, `restart`, `exec`, `run`, `backup` - works
against the local image store, so a project can be pulled on a connected machine
and run on a disconnected one.

## What needs fetching

Three images, not one. Two of them are easy to forget, because nothing in the
compose file names them:

| Image              | Comes from                             | Point it elsewhere with                                       |
| ------------------ | -------------------------------------- | ------------------------------------------------------------- |
| Service images     | `image:` in the compose file           | an Incus remote, see [Mirrors](#mirrors-the-middle-ground)    |
| ic-healthd sidecar | `ghcr.io/lxc/incus-compose/ic-healthd` | `--healthd-image`, `INCUS_COMPOSE_HEALTHD_IMAGE`              |
| The `run` helper   | `ghcr.io/lxc/incus-compose/ic-sleep`   | `--init`, `x-incus-compose.init`, `INCUS_COMPOSE_SLEEP_IMAGE` |

`pull` fetches all three, but only _warns_ on the last two - see
[How the exit code is obtained](/cli-reference/exec-run-and-cp#how-the-exit-code-is-obtained).
That warning is the only notice you get that `incus-compose run` will fail
later, disconnected, so do not let it scroll past.

## Pull while connected

```bash
incus-compose pull
```

That is the whole connected step. `--policy` defaults to `always`, so it
refreshes what it already has.

Pulling is also what captures each image's
[entrypoint/command split](/compose-compatibility/services#entrypoint-and-command),
the one thing incus-compose reads from a registry directly. Project copies carry
it, so `command:` still replaces `CMD` rather than the whole argv once you are
offline.

The images land in the shared cache project (`incus-compose-cache` by default),
which survives `down` and `up`. Do not set `--image-cache ""` on a machine that
has to work disconnected: that skips the cache project, and the images then only
exist as per-project copies that `down` removes.

## Run disconnected

```bash
incus-compose up --pull never
```

`never` means a store hit wins and a store miss is a hard failure - the registry
is never contacted. Without it the default is `missing`, which is silent about
the difference until the moment something is absent and the pull hangs on a
registry that is not there.

The spelling differs by command, which is worth pinning in scripts:

| Command | Flag       | Values                                 | Default   |
| ------- | ---------- | -------------------------------------- | --------- |
| `up`    | `--pull`   | `always`, `missing`, `never`, `policy` | `policy`  |
| `pull`  | `--policy` | `always`, `missing`, `never`           | `always`  |
| `run`   | `--pull`   | `always`, `missing`, `never`           | `missing` |
| `build` | `--pull`   | `always`, `missing`, `never`, `policy` | `policy`  |

Set it once for the whole machine instead of per invocation:

```bash
export INCUS_COMPOSE_UP_PULL=never
export INCUS_COMPOSE_RUN_PULL=never
```

## Mirrors: the middle ground

Most "air-gapped" networks are really proxied: no route to Docker Hub, but a
registry of your own. Adding an Incus remote for one of the six built-in
registries with `incus remote add` overrides its built-in address, so
`image: nginx:alpine` still resolves to `docker.io/library/nginx:alpine` and the
compose file does not change. See
[Images](/compose-compatibility/differences#images) for the commands, resolution
and registry authentication, and
[OCI Registry Cache](/examples/oci-registry-cache) for running the pull-through
cache itself on Incus.

The two incus-compose images need the same treatment, since `ghcr.io` is a
different upstream:

```bash
export INCUS_COMPOSE_HEALTHD_IMAGE=registry.example.com/incus-compose/ic-healthd:{version}
export INCUS_COMPOSE_INIT_IMAGE=registry.example.com/incus-compose/ic-sleep:{version}
```

`{version}` is replaced with the incus-compose version, so one value survives an
upgrade.

## What does not work disconnected

- **`build:` services** still need whatever their Dockerfile pulls. The builder
  is Podman or Docker and its base-image freshness is its own concern - see
  [Builds](/builds).
- **A cluster mixing CPU architectures** cannot run `incus-compose run` - see
  [How the exit code is obtained](/cli-reference/exec-run-and-cp#how-the-exit-code-is-obtained).
- **`self-update`** reaches GitHub by definition.

Dockerfile `HEALTHCHECK` is read when an image is pulled, not on every `up`, so
a cached image needs no registry. Where the registry cannot be reached the read
is skipped with a warning and the image carries no check; declare
`healthcheck.test` in the compose file to be sure of one - see
[Dockerfile HEALTHCHECK](/healthd#dockerfile-healthcheck).
