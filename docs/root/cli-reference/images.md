---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: jkU01nwvg
leafwiki_title: Images
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Images

## build

Build or rebuild service images for services that define `build:`.

```
incus-compose build [SERVICE...]
```

| Option       | Description                                                                                                                                                                                     |
| ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--no-cache` | Disable the builder's layer cache, and skip the shared image cache the build would otherwise land in (see [Builds - Image Caching](/builds#image-caching))                                      |
| `--pull`     | Pull policy for the images this build depends on: `always`, `missing`/`policy`, `never`. Base-image freshness is the builder's own concern, set `build.pull: true` in the compose file for that |

When service names are provided, only matching build-configured services are
built. Services without `build:` are skipped. Built images are imported into the
Incus project and used by `up`.

See [Builds](/builds): for supported Compose build options and requirements.

## pull

Pull service images.

```
incus-compose pull [SERVICE...]
```

| Option                          | Description                                                                    |
| ------------------------------- | ------------------------------------------------------------------------------ |
| `--ignore-buildable`            | Ignore images that can be built                                                |
| `--ignore-build-failures`       | Pull what it can and ignores images with pull failures                         |
| `--policy`                      | Pull policy: `always` (default), `missing`, `never`                            |
| `--no-healthd`                  | Don't pull the healthd sidecar                                                 |
| `--healthd-image`               | Healthd OCI image to use; {version} is replaced with the incus-compose version |
| `--init`                        | Image the `run` helper comes from; {version} is replaced likewise              |
| `--include-deps`, `--with-deps` | Also pull linked services                                                      |

`pull` is the only command that needs a registry, and it fetches the ic-healthd
and `run` helper images alongside the service images. That is what an
[air-gapped or proxied install](/air-gapped) is built on.

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command | Variable                                  | Flag                     | Description                       |
| ------- | ----------------------------------------- | ------------------------ | --------------------------------- |
| `build` | `INCUS_COMPOSE_BUILD_NO_CACHE`            | `--no-cache`             | Do not use a cache when building  |
| `build` | `INCUS_COMPOSE_BUILD_PULL`                | `--pull`                 | Pull policy                       |
| `build` | `INCUS_COMPOSE_BUILD_BUILDER`             | `--builder`              | Preferred builder binary or path  |
| `pull`  | `INCUS_COMPOSE_PULL_IGNORE_BUILDABLE`     | `--ignore-buildable`     | Ignore images that can be built   |
| `pull`  | `INCUS_COMPOSE_PULL_IGNORE_PULL_FAILURES` | `--ignore-pull-failures` | Pull what it can, ignore failures |
| `pull`  | `INCUS_COMPOSE_PULL_INCLUDE_DEPS`         | `--include-deps`         | Also pull linked services         |
| `pull`  | `INCUS_COMPOSE_PULL_POLICY`               | `--policy`               | Pull policy                       |
| `pull`  | `INCUS_COMPOSE_NO_HEALTHD`                | `--no-healthd`           | Don't pull the healthd sidecar    |
| `pull`  | `INCUS_COMPOSE_HEALTHD_IMAGE`             | `--healthd-image`        | Healthd OCI image                 |
