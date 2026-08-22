# incus-compose

[![CI](https://github.com/lxc/incus-compose/actions/workflows/test-e2e.yml/badge.svg?branch=main)](https://github.com/lxc/incus-compose/actions?query=event%3Apush+branch%3Amain)
[![Go Reference](https://pkg.go.dev/badge/github.com/lxc/incus-compose.svg)](https://pkg.go.dev/github.com/lxc/incus-compose)
[![Coverage 75%](https://img.shields.io/badge/coverage-75%25-yellow)](https://github.com/lxc/incus-compose/actions/workflows/test-e2e.yml)

A drop-in replacement for `docker compose` that runs your `compose.yaml` on
[Incus](https://linuxcontainers.org/incus/) - with the full Incus API available
as an escape hatch when you need more than the Compose spec covers.

```yaml
services:
  db:
    image: docker.io/postgres:18-alpine
    healthcheck:
      test: ["CMD", "pg_isready", "-U", "postgres"]
    deploy:
      resources:
        limits:
          cpus: "2"
          memory: 2G

  web:
    image: docker.io/nginx:alpine
    depends_on:
      db: { condition: service_healthy }
    ports:
      - "8080:80"
    deploy:
      resources:
        limits:
          cpus: "1"
          memory: 512M
```

```bash
incus-compose up
```

A plain compose file, running unchanged.

## Demos

- [30-service dependency graph, 30 parallel workers](https://asciinema.org/a/1260145)
- [Immich - a full photo-management stack](https://asciinema.org/a/1259458)

## Why incus-compose?

Point it at the `compose.yaml` you already have and run it against Incus instead
of Docker.

- Use existing `docker-compose.yml` files unchanged - no rewrite, no new format
  to learn
- Windows, macOS, and Linux clients drive a remote Incus host over HTTPS - no
  Docker Desktop, no WSL, no local VM
- Pull Docker/OCI images directly from docker.io, ghcr.io, and other registries
  via Incus's native OCI registry support

New to Incus? See [Why Incus?](https://docs.incus-compose.org/why-incus) for
what the platform brings over a classic OCI engine setup.

## Features

**Drop-in.** All the commands you know - `up`, `down`, `start`, `stop`,
`restart`, `pause`, `logs`, `exec`, `cp`, `top`, `ps`, `config`, `build` -
parsing via compose-go with `.env` interpolation, profiles, `depends_on`,
secrets, and configs. See the
[CLI reference](https://docs.incus-compose.org/cli-reference) and the
[compatibility matrix](https://docs.incus-compose.org/compose-compatibility).

**Operable.** Health checks, restart policies, and `depends_on: service_healthy`
ordering via the `ic-healthd` sidecar; scaling with `up --scale`; project
isolation; live progress for pulls and lifecycle. See
[Health Checking](https://docs.incus-compose.org/healthd).

**Fast images.** OCI pulls from any registry, a two-stage cache that survives
`down`/`up` and dodges rate limits, and local builds via Podman/Docker. See
[Builds](https://docs.incus-compose.org/builds).

**Real networking and storage.** Bridge networks with static IPs, port
publishing via proxy devices or kernel NAT, volumes with UID/GID shifting,
seeded bind mounts, and per-volume pool placement.

**Incus-native when you want it.** Every instance, network, and volume option
passes straight through via `x-incus`; `x-incus-compose` adds devices (GPU, USB,
raw disk), project-wide resource limits, and healthd tuning. See
[Compose Compatibility](https://docs.incus-compose.org/compose-compatibility).

**Extensions.** `incus-compose backup` snapshots a project's data volumes into a
backup project - create, list, verify, restore, and prune - so a stack's state
survives the project itself, and `incus-compose port-forward` forwards a local
TCP port into an instance, published or not. See
[backup](https://docs.incus-compose.org/cli-reference#backup) and
[port-forward](https://docs.incus-compose.org/cli-reference#port-forward).

## Quick Start

Requires Incus 7.0.1 (LTS) or 7.2+, `podman` or `docker` for image building and
an Incus https remote (needed for healthchecking) with OCI registries added. See
[Getting Started](https://docs.incus-compose.org/getting-started) for the full
setup walkthrough.

Install the latest release:

```bash
curl -sSfL https://raw.githubusercontent.com/lxc/incus-compose/main/install.sh | sh -s -- -b ~/.local/bin
```

Or grab a prebuilt archive from the
[Releases Page](https://github.com/lxc/incus-compose/releases). On Arch Linux,
install
[incus-compose-bin](https://aur.archlinux.org/packages/incus-compose-bin) (or
[incus-compose-git](https://aur.archlinux.org/packages/incus-compose-git) for
builds from `main`) from the AUR - maintained by @neitsab and @jochumdev.

Then point it at your existing `compose.yaml`:

```bash
# Start services
incus-compose up -d

# View logs
incus-compose logs -f

# List running services
incus-compose list

# Stop and remove
incus-compose down
```

## Quick Links

All docs: [docs.incus-compose.org](https://docs.incus-compose.org)

- **[Getting Started](https://docs.incus-compose.org/getting-started)** -
  Install and run your first compose project
- **[CLI Reference](https://docs.incus-compose.org/cli-reference)** - Commands
  and options
- **[Compose Compatibility](https://docs.incus-compose.org/compose-compatibility)** -
  What works and what doesn't
- **[Architecture](https://docs.incus-compose.org/architecture)** - the
  resource-first design behind incus-compose
- **[Why Incus?](https://docs.incus-compose.org/why-incus)** - What Incus brings
  over a classic OCI engine setup
- **[Changelog](CHANGELOG.md)** - what changed since 0.0.1-beta1

### Examples

Descriptions are in our [docs](https://docs.incus-compose.org/examples) while
the files are in [examples](examples/).

## Support and community

The following channels are available for questions and discussion around
incus-compose.

### Bug reports

You can file bug reports and feature requests at:
[`https://github.com/lxc/incus-compose/issues/new`](https://github.com/lxc/incus-compose/issues/new)

### Community support

Community support is handled at:
[`https://discuss.linuxcontainers.org`](https://discuss.linuxcontainers.org)

## Contributing

Fixes and new features are greatly appreciated. Make sure to read our
[contributing guidelines](CONTRIBUTING.md) first!

## Credits

incus-compose wouldn't be what it is without the people who tested it, filed
reports, and pushed on ideas along the way: @Sagi, @neitsab, @pyrodogg, @kgoetz,
@edorgeville, @bburky, @blurry, @stgraber, @ishaan-jindal, @code-by-tanveer, and
@Tofil.

It also stands on a few libraries that make maintaining it far easier:

- [compose-spec/compose-go](https://github.com/compose-spec/compose-go) - parses
  and resolves the compose file
- [lxc/incus](https://github.com/lxc/incus) - the container/VM engine and Go
  client this all talks to
- [creativeprojects/go-selfupdate](https://github.com/creativeprojects/go-selfupdate) -
  powers `self-update`
- [dominikbraun/graph](https://github.com/dominikbraun/graph) - the `depends_on`
  dependency graph
- [bradleyjkemp/cupaloy](https://github.com/bradleyjkemp/cupaloy) - snapshot
  testing across the test suite

This project is inspired by
[@bketelsen](https://github.com/bketelsen/incus-compose). Some components are
adapted from [docker compose](https://github.com/docker/compose). The
`install.sh` script is adapted from
[golangci-lint](https://github.com/golangci/golangci-lint).

This project uses AI tools as development aids (drafting, iteration, reviews,
tests, and documentation). Architecture, constraints, and final code decisions
are owned by the human committers.

Earlier development was on [Gitlab](https://gitlab.com/r3j0/incus-compose/).

## License

[Apache 2.0](LICENSE)
