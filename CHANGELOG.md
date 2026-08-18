# Changelog

All notable changes to incus-compose are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

Version numbering moved from `0.0.1` to `1.0.0` at beta11 (1.0.0 is the intended
final version), and the beta suffix gained a dot (`beta.16`) from beta.16 onward
for correct semver ordering. Headings below preserve each release's announced form.

## [Unreleased]

### Added

- `kill [SERVICE...]` force stops services without the graceful shutdown, which
  is what `stop` did until now. `-s` accepts `SIGKILL` only: the Incus state API
  carries no signal, and an OCI entrypoint is not PID 1 under Incus, so any
  other signal would reach the wrong process. (by @jochumdev)

- `cp SERVICE:SRC DEST` and `cp SRC SERVICE:DEST` copy files between a service's
  instance and your filesystem, as `docker compose cp` does. Which side is the
  instance is decided by the name before the colon naming a compose service, so
  a Windows drive or a path holding a colon stays local. A pushed file is owned
  by the instance's user rather than root, as configs and secrets already are;
  `--archive` keeps the source's ownership. (by @jochumdev)

- `top` and `events` scope `incus top` and `incus monitor` to the project.
  `top` reports per instance where `docker compose top` reports per process,
  and neither takes service arguments, since Incus filters by project rather
  than by instance. (by @jochumdev)

- `port SERVICE PRIVATE_PORT` prints the host address a published port is bound
  to, as `docker compose port` does. A stopped instance answers too, and an
  unpublished port names the ports the instance does have. (by @jochumdev)

- `port-forward SERVICE TARGET_PORT [LISTEN_PORT]` runs a local TCP listener and
  forwards every connection into the instance, reaching a port that was never
  published. Needs Incus 7.3 or 7.0.1 LTS. (by @jochumdev)

- `healthd status` prints the shared daemon's health status key. (by @jochumdev)

- `backup` copies a project's named volumes into a separate `<project>-backup`
  Incus project and keeps per-run restore points on them, where `down`,
  `down --volumes` and `down --project` cannot reach them. `backup create` takes
  a run, `list` shows them with their size, `verify` checks a run's restore
  points are still there and reports volumes the project has gained or lost
  since, `restore` puts a run back, and `delete --keep-last N` prunes. Configure
  the pool it uses with `x-incus-compose.backup.pool` - a separate disk is what
  makes a backup worth having. (by @ishaan-jindal and @jochumdev)

- Every path an image declares as a `VOLUME` now gets a storage volume of the
  service's own, filled from what the image ships there. Until now Incus mounted
  a tmpfs over those paths, so anything written to them was lost on restart -
  an `isso` container kept its database there. Declaring a volume, a bind or a
  tmpfs at the same target still wins, and `x-incus-compose.auto-volumes: false`
  turns it off for a project. (by @jochumdev)

- A named volume now starts from what the image ships at its target, as docker
  fills an empty volume from the image. A volume mounted over a directory the
  image populated - `conf:/etc/nginx/conf.d`, say - is no longer empty on the
  first run. `volume: {nocopy: true}` keeps it empty. (by @jochumdev)

- An external network can now name `<project>:<network>` to attach to a
  managed network owned by another compose project, instead of only a plain
  network name. (by @jochumdev)

- **library**: `Client.Resources()` returns the resources a client holds, so a
  caller can act on ones it did not declare itself. (by @jochumdev)

- **library**: `StorageVolumeConfig.Prefetch` fills a volume on first creation
  with what its image holds at that path, the way docker seeds an empty volume
  from the image. `StorageVolume.Created()` now reads from
  `StorageVolumeState`, so it follows a fetch instead of staying set.
  (by @jochumdev)

- **library**: `Image.SFTP()` reads an image's own filesystem, through a stopped
  instance it creates from the image. One such instance per image serves every
  prefetch and user lookup, and `Client.Done()` - which now releases what a
  client's resources hold before firing the done hooks - removes it.
  `Image.ResolveUser()` maps a `user[:group]` value to an `*Owner`, reading the
  image only when either side is a name. New alongside them:
  `InstanceConfig.User`, `StorageVolumeConfig.User`, `ImageState.OCIUser`,
  `OCIUserKey` and `ErrNoSuchUser`. (by @jochumdev)

### Changed

- `stop`, and with it `restart`, now shuts a service down gracefully and kills
  it once `--timeout` is up. Both always killed outright before, so `--timeout`
  did nothing at all. `kill` is the old behaviour under its own name. Incus does
  not escalate a shutdown that ran out of time - it reports a failure and leaves
  the instance running - so incus-compose issues that kill itself. (by @jochumdev)

- **library**: a stop timeout under a second no longer arrives at Incus as zero,
  which it reads as an immediate kill. It is rounded up to one second, so
  `OptionTimeout(500 * time.Millisecond)` shuts down rather than kills.
  (by @jochumdev)

- **library**: ownership is one type in one spelling. `Owner{UID, GID uint64}`
  replaces the three conventions that meant "unset" differently:
  `InstanceConfig.UID`/`GID` and `StorageVolumeConfig.UID`/`GID` (uint64, 0) and
  `InstanceFile.UID`/`GID` (int64, -1). Each is now an `Owner *Owner` where nil
  is unset, so the zero value needs no thought. Two behaviours follow: an
  instance config setting only one of the pair is no longer silently discarded
  and re-read from the instance, and a secret or config that sets `uid` without
  `gid` now leaves the other at 0 the way docker does, instead of taking the
  instance's. (by @jochumdev)

- **library**: `InstanceConfig.Full`/`project.ResourcesFull()` are gone -
  `Instance.fetch()` now always fetches runtime state, so `ps`, `list` and
  `exec` no longer need to opt in. `Instance.HasFull()` and
  `InstanceState.IncusInstanceFull` are renamed to `HasState()` and
  `IncusInstanceState` (now `*api.InstanceState`, not `*api.InstanceFull`).
  Waiting for a container's IP now reports a clearer timeout, naming the
  likely cause and a command to check it. (by @jochumdev)

### Fixed

- A service `user:` may now name its user and group, not only number them:
  `user: "netbox:root"` starts instead of failing the whole project. Names
  resolve against the image's own `/etc/passwd` and `/etc/group`, and one the
  image does not define is an error rather than a silent fall back to root. The
  same goes for the image's own `USER`, so an image built with `USER nginx` no
  longer runs as root. A name with no group takes that user's own group, as
  `login` would; a number with no group still keeps GID 0. (by @jochumdev)

- An OCI image whose `CMD` is empty is no longer re-read from its registry on
  every run. Incus stores no property for an empty value, so the key that marks
  the config as already read was never there for such an image - one round trip
  per run against the registry, and a warning for an image that is only local,
  such as a locally built ic-healthd. (by @jochumdev)

- A `healthd up` whose image cannot be pulled no longer leaves a storage volume
  behind that every later attempt fails on. The volume was created before the
  image was known, so it got `initial.uid=0` - which Incus applies at creation
  and cannot be changed afterwards - and the next run died with
  `UID mismatch, expected 65534 got 0` until the volume was deleted by hand.
  (by @jochumdev)

- `healthd up` now stops at the first resource that fails, the way `up`, `start`
  and `pull` already did, instead of carrying on to create the rest of a daemon
  that cannot run. (by @jochumdev)

- A config or secret whose target sits inside a volume is now written into that
  volume. It used to be written into the instance's filesystem, where the mount
  hid it, so the container started with the image's file or nothing at all. (by
  @jochumdev)

- `up` could hang until the start timeout on a service that had already been
  reported healthy, and then fail. Waiting for a container's IP refreshed the
  instance state as it polled, so a reading taken before ic-healthd reported
  could overwrite the verdict a dependent service was waiting for - and
  ic-healthd never repeats a status it has already written. Those polls now read
  into a state of their own. (by @jochumdev)

- `HealthdRunning`, used before waiting on health-check dependencies, checked
  only that the healthd instance was `Running`, not that its own health
  checks had passed - a service could start before healthd was actually ready
  to track it. It now waits for the health status key to become `healthy`.
  (by @jochumdev)

- A health check could fail with `websocket: bad handshake` instead of
  reporting its result. The exec control socket was dialed in map order, so a
  fast command could finish and retire the operation first. (by @jochumdev)

## [v1.2.0] - 2026-08-14

### Changed

- Incus 7.0.1 (LTS) or 7.2 is now the minimum, checked when a command connects
  rather than failing somewhere further in. Older daemons have no
  `oci_network_config`, which every compose network attachment relies on for
  static addresses, gateways and `oci.dns.*`. (by @jochumdev)

- The bridge the shared ic-healthd daemon attaches to is now called `icompose0`
  rather than `ic-healthd`. A daemon that is already running keeps its current
  bridge until it is recreated, and the old network is not removed for you.
  (by @jochumdev)

- `up --no-start` now returns once the containers are created, as `--detach`
  does. It used to go on to follow logs from instances it never started, and the
  interrupt that ended that stream tore them down again. (by @jochumdev)

- `up --build` now recreates the instances of the services whose image it
  rebuilt, so the new image is what they run. Previously the image was rebuilt
  but the existing instances kept the old one until `--recreate` was passed as
  well. A service that only consumes an image another service builds is
  recreated too; everything else is left alone. (by @jochumdev)

- **library**: the Incus API is reached through `iclient` instead of
  `github.com/lxc/incus/v7/client`, which cannot be used from several goroutines
  at once. `Client.Connection`, `Client.GlobalConnection` and
  `GlobalClient.Connection` now hand back an `*iclient.Connection`, which is safe
  to share; `GlobalClient.CliConfig` returns an `*iclient.Config`;
  `ClientProvideConnection` and `ProfileConfig.SourceServer` take the new type;
  `Instance.PushFiles` takes a context. `ClientProvideInstanceServer` is removed
  as a duplicate of `ClientProvideConnection`, and `AddWellKnownRegistriesHook`
  because a well-known registry is now resolved where the image source is,
  without writing to the shared CLI configuration. New:
  `client.DialRemote(path, remote)`. (by @jochumdev)
- `--pull always` on an image from an OCI registry re-downloads it rather than
  keeping a cached copy whose digest still matches. Deciding that needed a
  client-side registry lookup; resolving the reference is now left to the Incus
  server. Native `incus:` remotes are unaffected. (by @jochumdev)
- **library**: what a resource last read back from Incus moved off the resource
  into a state struct - `Instance.State()`, `Image.State()`, `Network.State()`,
  `Profile.State()`, `StorageVolume.State()`. `IncusInstance`,
  `IncusInstanceFull`, `IncusAlias`, `IncusNetwork`, `IncusProfile`,
  `IncusVolume`, `ETag` and the image's `UID`/`GID`/`Entrypoint`/`Cwd` are no
  longer fields you read directly. The state is swapped whole, so what `State()`
  hands back never changes under you. (by @jochumdev)

### Added

- `sysctls:` on a service is mapped to `linux.sysctl.*` on the instance, applied
  immediately and kept across a restart. (by @alien43)

- `x-incus-compose.gateway: false` on a service's network attachment allows a
  static `ipv4_address`/`ipv6_address` on a network that declares no CIDR. That
  combination is otherwise rejected, because the gateway is not known until the
  network exists. (by @jochumdev)

- **library**: `Image.AddService` records a compose service against an image.
  Several services usually share one image object, so appending to
  `ImageConfig.Services` by hand raced. (by @jochumdev)

### Fixed

- A NIC device carrying `nictype` (`bridged` with a `parent`, say) and no
  managed `network:` is accepted instead of rejected. Incus takes such a device
  on its own, and it is the only way to attach an instance to an unmanaged host
  bridge. (by @alien43)

- A failed image build says what failed. The lock the build takes first reported
  the daemon's error unwrapped, so anything wrong with it read as a bare
  `not found` against the image being built; it now names the lock volume and the
  project it lives in, and `--debug` reports which stage the build reached.
  (by @jochumdev)

- `name:` on a network now selects the Incus network it names, external or
  managed. It was documented but never read, so only `x-incus-compose.network`
  had any effect; an explicit `name:` now wins over that extension.
  (by @jochumdev)

- Working on several services at once no longer races. Every worker drove one
  shared Incus client, whose event-listener state cannot be used from more than
  one goroutine; each has its own connection now. The races that sat on top of
  it are gone with it: two workers setting up the image lock volume at the same
  time, simultaneous starts resolving which ic-healthd watches the project, and
  a wait for an instance's addresses that trusted a lifecycle event and stalled
  DNS registration until the timeout when one arrived late. (by @jochumdev)

- ic-healthd gives up on an Incus call that stops answering instead of leaking
  the goroutine waiting on it, so a health check or a restart that times out no
  longer costs the daemon anything. A probe abandoned mid-command also has its
  exec canceled, rather than leaving it running in the container. (by @jochumdev)
- ic-healthd waits for the instance's operation lock to clear before retrying a
  write it rejected, instead of retrying on a fixed delay that could expire six
  times while a slow stop was still running. (by @jochumdev)
- Loading a compose file reads the server's API extensions once at connect
  rather than once per service, network and published port. (by @jochumdev)
- Waiting for an image to appear in the cache gives up after the five minutes it
  was meant to, and stops when the command is canceled. The retry took its delay
  as a starting point for exponential backoff and ignored cancellation, so ten
  attempts could span hours that no interrupt would end. (by @jochumdev)
- A start or stop held up by another operation on the same instance no longer
  spends its `--timeout` on backing off. The wait for the instance lock is
  server-side and already correct; the retry around it doubled its delay on top,
  up to two minutes, which could turn contention it would have ridden out into a
  reported timeout. Waiting for ic-healthd to come up is likewise the three
  seconds it claims rather than fifteen. (by @jochumdev)
- A built image now carries its environment into the instance. The image's `ENV`
  was dropped on the way in, so a service built from a Dockerfile came up without
  the `PATH`, `HOME` and `TERM` the same image pulled from a registry gets.
  (by @jochumdev)

## [v1.2.0-rc.3] - 2026-08-07

### Changed

- The shared ic-healthd runs in an Incus project of its own, `incus-compose`, on
  a bridge of its own, `ic-healthd`, with its own root disk. It took all three
  from the `default` project's `default` profile before, so a server whose
  default profile uses an unmanaged bridge - or no NIC - could not bring the
  shared daemon up at all. No instance or volume of ours lands in the `default`
  project any more. (by @jochumdev)
- `--healthd-network` / `x-incus-compose.healthd.network` now applies to the
  shared daemon too, and a network the compose file declares is created before
  the daemon attaches to it. It was warned about and ignored outside project
  scope. Like `incus`, `workers` and `x-incus`, the first project to bring the
  shared daemon up supplies it. (by @jochumdev)
- ic-healthd's default pool sizes are `workers: 128` and `restart-workers: 32`,
  up from `32` and `12`. One daemon now watches every project, so the caps are
  fleet-wide and the old ones queued behind a handful of slow projects.
  (by @jochumdev)

> **Upgrading from `v1.2.0-rc.1` or `rc.2`** - those left a daemon in the
> `default` project, and nothing moves it for you. Run
> `incus-compose healthd down --force` **before** upgrading, or afterwards
> delete the `ic-healthd` instance and volume in the `default` project and its
> `ic-healthd-global` certificate. Two daemons watching the same projects
> otherwise both restart the same instances. Releases before `v1.2.0-rc.1` had
> no shared daemon and need nothing.

### Fixed

- `up` no longer fetches the ic-healthd image when the daemon already runs the
  one asked for. It pulled on every run, so a tag that had gone from the
  registry - or a registry that was simply unreachable - failed the whole
  project even though the daemon was healthy and nothing needed replacing.
  (by @jochumdev)
- Containers on a network that pins its own subnet can reach the outside again.
  Incus turns `ipv4.nat`/`ipv6.nat` on only for a subnet it picked itself, so a
  network given an explicit `ipv4.address` through `x-incus` came up without
  NAT and nothing on it could route out. Both now default to `true` for any
  non-`internal` network, matching docker; set them in `x-incus` to say
  otherwise. Networks that already exist keep the setting they were created
  with. (by @jochumdev)

## [v1.2.0-rc.2] - 2026-08-06

### Fixed

- `self-update` installs the build for the machine it runs on. It picked the
  first asset in the release instead - `darwin_amd64` for everybody - so on
  Linux and Windows it replaced the binary with one that cannot execute. The
  broken `self-update` is the one already installed, so 1.0.0 and 1.1.0 users
  have to reinstall once with `install.sh`; it works from here on.
  (by @jochumdev)

## [v1.2.0-rc.1] - 2026-08-06

### Added

- ic-healthd watches several projects from one event listener, and `--project` is
  now optional: without it it watches every project whose config matches
  `--project-marker`, by default `user.healthcheck.scope=global`. The flag takes
  a `KEY=VALUE` pair now; a bare key still means `KEY=true`.
  See [Health Checking](https://docs.incus-compose.org/healthd). (by @jochumdev)
- `x-incus-compose.healthd` gained `scope`, `workers`, `restart-workers` and
  `x-incus`. `workers`/`restart-workers` size the daemon's pools; `x-incus` is
  Incus instance config for the sidecar, e.g. `limits.cpu`. (by @jochumdev)
- `up` and `healthd up` upgrade the ic-healthd container: when the image you ask
  for is a newer release than the one it is running, the daemon is replaced by
  one built from it. The comparison is semver and forward-only, so a machine on
  an older incus-compose cannot downgrade a daemon shared with everyone else.
  Tags that are not release versions - moving tags like `latest`, and `git
describe` builds - are not comparable and replace on any difference.

  The replacement keeps the running daemon's configuration - its endpoint,
  worker counts, limits and anything else set on it - so an upgrade triggered by
  one project no longer resets settings another supplied. A flag or compose
  value given to the run doing the upgrade still wins, and a limit below the
  sidecar's own default is raised to it. (by @jochumdev)

- the `healthd` sub-commands run without a compose file, acting on the shared
  daemon. `incus-compose healthd up` on a bare server creates it before any
  project exists; `logs`, `restart`, `reload` and `down` fail with `no ic-healthd
is running` when there is none instead of complaining about a missing
  `compose.yaml`. (by @jochumdev)
- `--trace`, a level below `--debug` (which it implies), on both incus-compose
  and `ic-healthd run`. The daemon's per-event and per-check lines moved there,
  so `--debug` stays readable on a server watching many projects; `--trace` is
  what shows the Incus events arriving when a project is not being watched.
  incus-compose passes it to the sidecar as `INCUS_COMPOSE_HEALTHD_TRACE`; it
  has no level of its own in incus-compose yet. (by @jochumdev)
- `healthd down --force` stops the shared daemon without the confirmation prompt.
  Without it, taking down a daemon other projects rely on asks first, and
  refuses outright when there is no terminal to ask on. (by @jochumdev)
- **library**: `project.ErrNoComposeFile`, so a caller that can work without a
  compose file can tell that apart from a broken one. `Load` and `LoadModel`
  return it instead of an equivalent unwrapped error. (by @jochumdev)
- **library**: `Client.HealthdRunning`, `GlobalClient.ProjectConfig`,
  `GlobalClient.ProjectsWithConfig`, `GlobalClient.AddMissingProjectConfig` (was
  unexported) and `InstanceConfig.NoRootDevice`. (by @jochumdev)
- `entrypoint:` is supported and follows the compose spec. `command:` on its own
  still appends rather than replacing. See
  [Compose Compatibility](https://docs.incus-compose.org/compose-compatibility#entrypoint-and-command). (by @jochumdev)
- `--pull never` on `up`, `build` and `pull` never contacts a registry, for
  air-gapped use. `pull --policy` is honoured instead of ignored. (by @jochumdev)
- **library**: `StorageVolume.SFTP()` and `StorageVolume.Lock()`. (by @jochumdev)

### Changed

- **one ic-healthd for the whole server.** `up` no longer creates a sidecar per
  project. It creates a single shared daemon named `ic-healthd` in the Incus
  `default` project and marks the project `user.healthcheck.scope=global` so the
  daemon picks it up. A project that already had its own sidecar has it removed,
  before the mark is written, so the two never watch it at once.

  Keep a sidecar of your own with `up --healthd-scope project` or
  `x-incus-compose.healthd.scope: project`; that is also the way to keep the
  daemon's Incus certificate restricted to one project, since the shared one is
  unrestricted by necessity. Whichever a project uses is stored on the project
  and wins over the flag and the compose file from then on, so changing it later
  means changing that key.

  Projects last brought up by an earlier version carry no scope at all and are
  invisible to the shared daemon, so nothing changes for them until you run `up`.
  See [Health Checking](https://docs.incus-compose.org/healthd). (by @jochumdev)

- **health checking is opt-in.** ic-healthd watches an instance only when it
  carries `user.healthcheck.enabled: "true"`; a `healthcheck:` block or a restart
  policy alone is no longer enough. incus-compose writes it automatically.

  Instances created before this do not carry the key. If a project uses
  `healthcheck:` or a restart policy, run `up` once to have those enforced again;
  it adds the key in place, with no `--recreate` and no downtime. Projects that
  use neither need nothing, and containers keep running either way. See
  [Health Checking](https://docs.incus-compose.org/healthd#health-checking-is-opt-in). (by @jochumdev)

- ic-healthd caps the checks and restarts it runs at once, over every project it
  watches, with `--workers` (32) and `--restart-workers` (12). An action with no
  worker free is retried rather than queued, so it never counts as a check that
  timed out.

  The sidecar is created with 2 CPUs and 256MiB instead of 1 and 50MB, and an
  existing one is raised to that when it is replaced. Only a project-scoped
  sidecar counts against an aggregate `limits.cpu`/`limits.memory`; the shared
  daemon lives in the Incus `default` project and counts against nothing.
  (by @jochumdev)

- `user.healthcheck.status` is written by ic-healthd alone. incus-compose no
  longer stamps `starting`/`stopped` on it, so the value always says what a
  daemon actually saw: an instance carries no status until one reports, reports
  `stopped` while it is down, and `unknown` for good under `up --no-healthd`.
  `list` shows those as `Unknown` and `Stopped`. (by @jochumdev)
- the image cache moved from the Incus `default` project to `incus-compose-cache`.
  Whatever earlier versions cached in `default` stays there unread; delete it by
  hand. (by @jochumdev)
- a service with `build:` no longer rebuilds in every project - the cache is
  checked before the builder runs. Use `--build` to force a rebuild. (by @jochumdev)
- `up` without `--detach` now matches `docker compose`: create, start, stream
  logs, and `down` on interrupt. (by @jochumdev)
- `config --format=json` keeps the `x-incus` and `x-incus-compose` blocks, which
  `docker compose` drops. Parse the JSON rather than diffing it against docker's.
  (by @jochumdev)
- **library**: `InstanceConfig.AppendEntrypoint` became `InstanceConfig.Entrypoint`
  and `InstanceConfig.Command`, both `[]string`. `ImageConfig.CacheServer` became
  `ImageConfig.CacheClient` (a `*client.Client`). `Options.Pull` is a `PullMode`,
  not a `bool`. (by @jochumdev)
- **library**: `Instance.Ensure` and `GlobalClient.EnsureProject` add declared
  config keys that are missing, comparing keys only - an existing key keeps its
  value. A changed value still needs `--recreate`. (by @jochumdev)

### Fixed

- ic-healthd reliability: stalled API calls, checker cancellation races, invalid
  intervals, `unless-stopped` misread as a deliberate stop, instances re-checked
  forever after they stopped, and state lost on an event-listener reconnect.
  (by @jochumdev)
- concurrent `up` runs no longer fail creating the same volume, profile or
  network. (by @jochumdev)
- `build.dockerfile` is resolved relative to `build.context`, not the working
  directory. (by @jochumdev)
- `command:` arguments containing spaces, quotes or `$` are shell-quoted
  correctly instead of being re-split. (by @jochumdev)
- a `configs:` or `secrets:` entry whose target already exists in the image is
  written instead of silently skipped. (by @jochumdev)
- a static `ipv4_address`/`ipv6_address` on a network with no explicit address
  now fails with an explanation instead of producing a broken NIC. (by @jochumdev)
- `config --format=yaml` no longer nests the document under a `project:` key.
  (by @jochumdev)
- default storage pool detection checks the `default` profile's root device
  first. (by @jochumdev)
- a service's `x-incus.raw.dnsmasq` lines are no longer appended twice.
  (by @jochumdev)
- pushing directory content into a storage volume no longer closes each file
  twice. (by @jochumdev)

## [1.1.0] - 2026-07-31

### Changed

- `build:` now works on every platform instead of Linux/Unix-only: image
  building previously unpacked the built OCI image with `umoci` (gated to
  `unix && !darwin`, with a "not implemented" stub elsewhere), and now
  instead derives the minimal `config.json` needed by Incus's LXC driver
  (`Process.Args`/`Cwd`/`User`) straight from `<builder> inspect`, dropping
  the `opencontainers/image-spec` and `opencontainers/umoci` dependencies.
  Builder detection also no longer shells out to `<builder> version` to
  distinguish Podman from Docker; it now checks the resolved binary name,
  and `buildah` is tried alongside `podman`/`docker`. (by @jochumdev)
- Published ports (`ports:`) create a proxy device. By default this is a
  userspace proxy targeting the container loopback (`nat=false`, connect
  `127.0.0.1`); per-port `x-incus-compose.nat: true` opts into NAT mode
  instead, connecting via ARP/NDP-based instance IP detection (needs Incus
  7.2 or 7.0.1 LTS) or the NIC's static IP directly if one is configured
  (same version floor). Requesting `nat` on a server below that floor
  skips the port with a warning instead of silently falling back. `nat`
  was previously auto-enabled on Incus 7.0+; it's opt-in now because NAT
  mode doesn't work for host-side `localhost:<port>` access — it routes to
  the instance's real address, not the loopback interface. (by @ishaan-jindal)
- `ic-healthd` is now event-driven instead of poll/SIGHUP-based: it discovers
  instances once, then reacts to the Incus lifecycle event stream (start,
  stop, shutdown, delete) to keep its tracked set in sync, spawning or
  killing checkers for exactly the delta. A checker only probes and reports
  its own status; the runner alone decides whether to restart an instance.
  `incus-compose healthd restart` no longer needs to register a client-side
  reloader hook, since healthd resyncs itself from events. (by @jochumdev)
- `up`'s wait for a service's own healthcheck to become healthy, and its
  wait on `depends_on: { condition: service_healthy }` dependencies, no
  longer poll Incus every 500ms. The client now opens a project-scoped
  Incus lifecycle event listener and blocks on a per-instance condition
  variable that's broadcast whenever that instance's state is refreshed
  from a lifecycle event (start/stop/update), cutting idle `GetInstance`
  API calls during `up`. (by @jochumdev)
- Image caching: images built with `build:` now go through the same cache
  path as pulled images instead of being created directly in the project,
  fixing stale/duplicate builds when a cache is configured. Use
  `build.no_cache: true` to disable caching. `deleteCached` also no longer
  aborts before cleaning up the cache when the source image was already
  removed. (by @jochumdev)

### Removed

- `x-incus-compose.nat-proxy` extension and all associated post-start
  device attachment machinery. Ports are now handled entirely through
  the standard `ports:` field. (by @ishaan-jindal)

### Added

- `healthd up` now recreates the ic-healthd sidecar when the running
  instance's image no longer matches the configured one, instead of leaving
  it on the stale image until a manual `healthd down` first. `up` shares the
  same code path, so a plain `up` also picks up healthd image updates
  automatically. (by @jochumdev)
- `services.{name}.configs` / top-level `configs:`: mount config files into
  the container, sourced from a file, inline `content`, or an environment
  variable. `mode` defaults to `0444` (world-readable); the writable bit is
  always ignored per the compose-spec, even if an explicit `mode` is set.
  (by @ishaan-jindal)
- Well-known OCI registries (`docker.io`, `ghcr.io`, `mcr.microsoft.com`,
  `quay.io`, `registry.gitlab.com`) are now auto-added to the in-memory Incus CLI config
  when an image from that registry is used, removing the need for manual
  `incus remote add` steps. (by @ishaan-jindal)
- Do not ignore healthd in `up --no-deps <service>` it allows script to wait
  on the service to be ready. Use `up --no-deps --no-healthd <service>` if you
  want the old behaviour. (by @jochumdev)
- `x-incus-compose.healthd.external: true`: the compose-file equivalent of
  `up --external-healthd` / `down --external-healthd`, so a project can pin
  "bring your own healthd" permanently instead of passing the flag on every
  invocation. Combines with the flag by OR: either is enough to turn it on.
  (by @jochumdev)
- `networks.{name}.aliases` on a service's network attachment now registers
  extra DNS names for its instance: each alias becomes a
  `cname=<alias>,<instance>` record in the network's `raw.dnsmasq`, resolving
  immediately instead of waiting on a DHCP lease like the existing
  service-name records. Works across networks shared between projects
  (`external: true`) without clobbering the other project's records. Limited
  to single-instance services, since a CNAME alias can only point at one
  target. (by @jochumdev)
- `dns` / `dns_search` / `domainname` now map to Incus's `oci.dns.nameservers` /
  `oci.dns.search` / `oci.dns.domain` instance config keys, seeding the
  container's initial `/etc/resolv.conf`. `dns_opt` has no Incus equivalent
  and is not mapped. (by @jochumdev)

### Fixed

- `install.sh`: fixed the checksum filename to match goreleaser's current
  release-artifact naming (`checksums.txt`), it was still using the old
  `${PROJECT_NAME}_${VERSION}_checksums.txt` pattern. (by @jochumdev)
- `up --pull=always` and `pull`: the stale image was not always deleted from
  cache and project before re-copying, so a floating tag could keep serving
  the old image. Deleting the cache is now a distinct step that runs before
  create/refresh, and the well-known-registry hook fires on it too. (by @jochumdev)

### Internal

- `.golangci.yml`: enabled a much stricter linter set, and fixed the
  resulting findings across the codebase. (by @jochumdev)

## [1.0.0] - 2026-07-10

The first stable release! _hooray_

### Changed

- Refactored the whole image caching process, it's now doing the same as
  the incus client would do and allows disabling caching by setting it to empty.
- `self-update` got a `--drafts` flag and skips them by default.

## [1.0.0-rc.2] - 2026-07-08

Second release candidate cause of the breaking `user.` -> `user.label.` change below.

### Changed

- Labels now have a `user.label.` prefix instead `user.` only, to not conflict with
  other user settings.

## [1.0.0-rc.1] - 2026-07-07

First release candidate. File pushes move to the Incus SFTP API, `command` now
layers on top of the image entrypoint instead of replacing it, and `privileged`
services are supported.

This is the first release that should actually work on Windows and MacOS.

E2E suite green.

### Added

- `services.{name}.privileged: true`: run the container privileged
  (`security.privileged`).

### Changed

- File pushes (secrets and single-file bind seeds) use the Incus SFTP API instead
  of the old REST file endpoint.
- `command:` is appended to the image's `oci.entrypoint` as arguments instead of
  overwriting it, matching Docker's ENTRYPOINT/CMD semantics.
- `ic-healthd` logs more detail during operations.
- `down --volumes` now deletes volumes while keeping the project; it is no longer
  an alias for `--project`. Use `--project` to remove the whole project (and its
  volumes).
- `list` includes the ic-healthd sidecar by default; the `--healthd` flag is
  replaced by `--no-healthd` to omit it.

### Fixed

- `healthd up` / `healthd down` work with custom networks.
- Windows and macOS builds error cleanly instead of crashing on the umoci import.

### Removed

- `healthd up --recreate`; recreate the sidecar with `healthd down` followed by
  `healthd up`.

### Internal

- CI runs slow tests with a 20m timeout and without parallelism to avoid overload;
  tooling paths and changelog links updated; lint fixes.

<details>
<summary><strong>Pre-1.0 beta history</strong> (beta1 through beta.22, 2026-06-01 to 2026-07-06)</summary>

## [1.0.0-beta.22] - 2026-07-06

A real `pull` command, Docker-parity `user` handling and `exec`, plus per-service
raw devices and gateway selection.

E2E suite green still at ~60% coverage.

### Added

- `pull` command: pre-pull service images (and the healthd sidecar) without
  creating anything, with `--policy`, `--ignore-buildable`,
  `--ignore-build-failures`, `--no-healthd`, and `--with-deps`.
- `services.{name}.user`: run the container process as a numeric `UID` or
  `UID:GID` (mapped to `oci.uid` / `oci.gid`).
- `services.{name}.x-incus-compose.devices`: attach raw Incus devices (gpu,
  unix-char, ...) verbatim; the required `type` key selects the device type.
- `services.{name}.networks.<net>.x-incus-compose.gateway: true`: places that NIC
  last so Incus uses its gateway as the instance's default route.

### Changed

- `exec` runs as the instance's user/group by default (matching
  `docker compose exec`); override with `--user` / `--group`. The command and its
  arguments are passed to Incus verbatim, so leading-dash flags work unescaped.
- Service network attachments are ordered deterministically (they previously
  followed Go map iteration order).
- Documentation moved to https://docs.incus-compose.org.

## [1.0.0-beta.21] - 2026-07-04

Standalone and bugfixed healthd, more x-incus reach, a native exec, and an error-severity system so recoverable problems warn instead of aborting.

E2E suite green, ~60% coverage.

### Added

- `x-incus` extensions now pass through on service networks, service volumes, and
  devices, plus direct `tmpfs` on services (same verbatim key/value passthrough as
  instances and networks).
- Standalone `ic-healthd`: it now has its own tests and can run on its own. Env
  vars renamed to the `INCUS_COMPOSE_HEALTHD_*` prefix, and a `--token` flag was added.
- Error-severity system: `Clone()` and `IgnoreError()` let commands demote
  non-fatal problems to warnings instead of hard failures. `up`/`down`/`start`/
  `stop`/`restart` no longer abort on errors that don't matter.
- `StackFailFast()` and `Stack.SetOptions()`.
- Exported `SanitizeProjectName()`.

### Changed

- `exec` uses the native `incus exec` implementation instead of the in-house MVP
  terminal (~250 lines removed): better TTY handling and parity with the `incus` CLI.
- Overridden network names are honored for normal networks too, not just special cases.
- OCI config is extracted after a build; resource dedup now keys on both `Name()`
  and `IncusName()`.

### Fixed

- Instance volumes land on the correct storage pool.
- `security.shifted` is left alone when the user has set it.
- `progress.bypass()` for all stdout/stderr fixes garbled output (#37).
- DNS watcher is skipped when the service name equals the Incus name; no watcher
  for empty service names.

## [1.0.0-beta.20] - 2026-06-30

Internal project/stack refactor plus network-readiness and healthd reliability
fixes.

E2E suite green, ~50% coverage.

### Added

- Instances wait for the network before starting via `raw.lxc=lxc.start.delay=1`,
  fixing flaky startups where services came up before DNS/networking was usable.

### Changed

- Reworked the ordering logic for `up`/`down`/`start`/`stop`, with and without
  dependencies. Deliberate asymmetry: `up`/`down` follow `depends_on` by default
  (`--no-deps` limits to the named service); `start`/`stop`/`restart` act on the
  named service only (`--with-deps` makes them follow `depends_on`).
- Project no longer returns a `Stack`; the CLI now owns stack assembly, with a new
  helper that adds resources in priority order.
- Exported `SanitizeNetworkName`.

### Fixed

- DNS update retries once on an ETag mismatch (concurrent-update race).
- `user.healthchecking.stopped` updates go through a cleaner path; the hacky PATCH
  workaround is gone.

### Removed

- deb/rpm/apk packages. Releases now ship the tarball/binary and install script only.

## [1.0.0-beta.19] - 2026-06-29

Mostly CLI and healthd fixes, plus event-driven log following.

### Added

- Event-driven `logs --follow`: uses the Incus events API to attach and detach log
  streams as instances start and stop, no longer exiting when instances go away and
  picking up new instances automatically (#3).

### Changed

- `down --project` now deletes all resources (instances, networks, volumes, and the
  healthd sidecar) instead of relying on incus to do so.
- `--debug` no longer shows progress bars (they interfered with debug output).
- DNS watcher waits up to 5s after a dnsmasq restart before starting the next instance.

### Fixed

- healthd: restart counting during the start period, instance tracking after
  cancellation, and the `healthd up`/`healthd down` lifecycle (#5).

### Removed

- Automatic retry on client operations.
- The unnecessary `--with-deps` flag from `logs`.

### Docs

- Added a Terms section (#4); updated example healthchecks with `start_*` directives;
  immich example now waits for DNS readiness and drops tini.

## [1.0.0-beta.18] - 2026-06-28

### Changed

- **Breaking:** renamed the `--project-directory` shorthand from `-pd` to `-P`.
- **Breaking:** `core.https_address` is now required (the server must be reachable
  over the network for image caching); the CLI warns when connecting over a unix socket.
- Lowered the default `--workers` from 10 back to 4 to avoid storage IO contention on
  cold-cache / large-image starts.
- Use the non-`v` version for the healthd image while keeping the `v` prefix for
  incus-compose itself.

### Fixed

- Retry various client/CLI operations and tune the default timeouts; increased the
  delay between start/stop retries.
- `up --recreate` no longer recreates networks.
- `ic-healthd` no longer shows up as an orphan in `ps` output.
- healthd always restarts checkers on reload so new settings take effect.
- Silenced two noisy debug logs.

## [1.0.0-beta.17] - 2026-06-27

Hotfix on top of beta.16, focused on the health-check sidecar.

### Added

- Opt-out of volume shifting (`security.shifted`) for cases where matching IDs
  inside the container don't matter.

### Fixed

- `ic-healthd` now runs inside the project stack and attaches to the project's own
  network (the project default unless overridden via `x-incus-compose.healthd.network`),
  ensured just before regular instances. Network extra options are no longer lost.

## [1.0.0-beta.16] - 2026-06-26

Requires updating via the install script (version format changed to `beta.XX` for
a correct semver version).

### Added

- `list` now has a separate `HEALTH` column instead of appending health to `STATUS`
  (columns: KIND, NAME, INCUSNAME, IMAGE, STATUS, HEALTH, ADDRESSES).
- Every instance reports a health value; services without a healthcheck show
  "Unknown" rather than a blank.
- `up`/`start` wait for the healthcheck to report healthy after starting an instance
  that defines one (polled every 500ms, bounded by `--timeout`), making
  `depends_on: service_healthy` reliable.
- `ic-healthd` reports its own status (healthy on start, unhealthy on shutdown) and
  locates the daemon instance via a `user.healthcheck.daemon` marker.
- New `--healthd-incus` flag / `INCUS_COMPOSE_HEALTHD_INCUS` env var to set the API
  URL the sidecar connects to (empty = auto-detect the IP from the attached bridge).
- New top-level `x-incus-compose.healthd` extension (`incus` API URL and `network`
  as `<project>:<network>` or a plain bridge name; both default to the project's own
  network and the connection's port).

### Changed

- **Breaking:** compose now defaults to incus listening on all interfaces; set
  `INCUS_COMPOSE_HEALTHD_INCUS` to override.
- `up` reconciles the service count in both directions, matching `docker compose up`.
  Instances above the desired count (`deploy.replicas` or `--scale`) are torn down,
  highest index first. A manual `--scale N` applies only to that invocation (#12).
- Default `--timeout` raised from 10s to 1 minute.

### Removed

- **Breaking:** the defunct `x-incus-compose.network-profile` extension (replaced by
  `x-incus-compose.healthd`).

## [1.0.0-beta15] - 2026-06-23

### Added

- `self-update` command: checks GitHub releases and updates the binary in place
  (release builds only, when the binary directory is writable).
- Environment variables for all CLI flags; every global flag can now be set via
  `INCUS_COMPOSE_*` (e.g. `INCUS_COMPOSE_FILE`, `INCUS_COMPOSE_PROJECT_NAME`,
  `INCUS_COMPOSE_DEBUG`).
- Configurable worker count via `--workers` / `INCUS_COMPOSE_WORKERS` (default 10).

### Fixed

- `.incus.yaml` overlay loading: `docker-compose.incus.yaml` and `compose.incus.yaml`
  overlays were not loaded correctly (#6).
- Progress display and error rendering improvements (#7).
- healthd retry calculation: `retries = start_period / start_interval`.
- Small client-package cleanups.

### Docs

- Improved healthd documentation.

## [1.0.0-beta14] - 2026-06-22

### Added

- Network project/profile support: `x-incus-compose.network.project` and
  `x-incus-compose.network.profile` control which Incus project and profile healthd uses.

### Changed

- **Breaking:** bind-mounts are no longer seeded by default; they now default to
  non-seeded (simple disk device pass-through). Set `x-incus-compose.seed: true` on
  the volume to restore copying files into the instance.

### Fixed

- Client connection stability: fixed several data races (reused `ProtocolIncus`,
  `noColor` context var, random string generation).
- Network profile fallback: use `devices.eth0.parent` when `devices.eth0.network`
  isn't available in a profile.
- More robust healthd discovery (`FindHealthd`).
- Switched to `errors.As()` for proper error unwrapping.

### Internal

- Removed all remaining testify/suite usage; refactored `serviceToInstance()` into
  smaller helpers; split `project/project.go` into smaller files; removed dangling
  test fixtures; updated snapshots for the new bind-mount behavior.

## [1.0.0-beta13] - 2026-06-17

### Changed

- incus-compose is now part of the **lxc** organization on GitHub.
- The `main` branch entered a feature freeze ahead of 1.0.0.

### Fixed

- Do not assume the availability of `incusbr0`.

## [1.0.0-beta12] - 2026-06-13

### Added

- `--image-cache` global flag (`INCUS_COMPOSE_IMAGE_CACHE`) to point the image cache
  at a different Incus project (default: `default`).
- `--rmi` / `--images` on `down` to remove project images on teardown, matching
  docker compose behaviour.
- Extra storage volume config support via `x-incus-compose`.

### Fixed

- `--with-deps` scoping: `up`/`down` follow `depends_on` automatically; all other
  commands require an explicit `--with-deps`.
- Healthd is now skipped when no services require it.
- Build image name corrected to `localhost/<service>`.

## [1.0.0-beta11] - 2026-06-11

Moved to 1.0.0 from 0.0.1 (1.0.0 will be the final version). Mostly a
health/dependency/lifecycle hardening release.

### Added

- Healthy-dependency support: `up` wires `service_healthy` dependencies into start
  ordering/waiting; new `--dependency-timeout` flag.
- Healthd expanded and rewritten: now runs when services use restart policies or are
  depended on via `service_healthy`, not only for an explicit healthcheck. Supports
  `start_period`, `start_interval`, `interval`, `timeout`, and `retries`; health
  state standardized via `user.healthcheck.status`.
- `shm_size` maps to a `/dev/shm` tmpfs device.
- `container_name` support: used as the Incus instance name; scaled services become
  `container_name-1`, `container_name-2`, etc.
- Added `examples/many-dependencies` and `examples/wikijs`; moved
  `test/fixtures/immich` to `examples/immich`; timestamped test logs under
  `test/logs/`; added `just test-slow`.

### Changed

- **Breaking:** removed the direct Incus URL env vars from the documented/runtime
  connection flow (`INCUS_COMPOSE_URL`, `INCUS_COMPOSE_CERT`, `INCUS_COMPOSE_KEY`).
  Use Incus CLI remotes instead (`--remote`, `INCUS_REMOTE`, or the default remote).
- **Breaking:** timeout flags changed from integer seconds to Go duration values
  internally (`up`, `down`, `start`, `stop`, `restart`, healthd paths); use explicit
  durations like `--timeout 10s`.
- **Breaking:** healthd sidecar name changed to `{project}-ic-healthd`; scripts
  expecting plain `ic-healthd` need updating.
- Improved CLI progress: healthd commands participate, progress moved to stdout and
  is less likely to corrupt logs.
- DNS watcher rewritten for concurrent per-action updates; instance IP handling
  refactored to support multiple interfaces/IP sets.

### Fixed

- `exec` now targets the requested service instead of possibly choosing the wrong
  instance from the stack.
- `list`/`ps` output is sorted for deterministic output (#46).
- Progress no longer overwrites logs (#43), plus several writer/stdout/stderr fixes.
- Healthd stability: graceful deletion/reload, checker optimization, and the normal
  checker now starts correctly after the start-period checker succeeds.
- Instance lifecycle: fixes around already-running instances and recreate; Incus
  timeout `0` means "do not wait", so the internal default now maps to `-1` where needed.
- Storage volumes: better shifted-volume validation, delayed from `Ensure()` to
  `Start()` where runtime UID/GID data is available.
- Image properties are copied correctly instead of sharing/mutating state.
- Several fixes around deleting project, external, and managed/dangling networks.
- Uses `errors.Is(err, ErrNotFound)` in more places; better debug logging and wrapping.

## [0.0.1-beta10] - 2026-06-08

Making Compose workflows more complete: local image builds, better progress
feedback, and improved healthcheck restart behavior.

### Added

- Compose `build:` support. New commands and flags: `incus-compose build`,
  `incus-compose build <svc...>`, `up --build`, `up --no-build`. See `docs/build.md`.
- Progress output for all operations in the CLI.

### Changed

- **Breaking:** no more Windows client support (a consequence of adding build; file
  an issue if you need it).
- **Breaking:** storage volume names changed to `vol-{name}` (was `{project}-{name}`),
  hashed if longer than 59 chars. Existing volumes are not migrated automatically.
- `ic-healthd` restart backoff (5s -> 10s -> 20s -> 40s -> 60s max) avoids tight
  restart loops for unhealthy services.
- `restart: unless-stopped` handled more correctly: `stop` marks `user.stopped=true`,
  `start` clears it, and `ic-healthd` skips automatic restarts while a service was
  intentionally stopped.

### Fixed

- Bind-mounted directories now resolve through the actual Incus storage volume name.
- Copied files/directories use more normal permissions: `0644` / `0755`.
- Fixed a UID/GID copy bug.

## [0.0.1-beta9] - 2026-06-07

Major image-handling rework, plus volume/bind-mount and healthd changes.

### Changed

- OCI config is extracted at download time: UID, GID, entrypoint, and cwd are read
  from a temporary stopped container right after download and stored as image
  properties (`oci.uid`, `oci.gid`, etc.) via `UpdateImage`; later runs read them
  back from properties with no extra container.
- Two-stage image cache restored (source -> cache project -> instance project),
  along with `docs/architecture/client/image.md`.
- Deferred source/cache detection: `GetImageServer` and `EnsureProject` moved from
  `newImage()` into `Ensure()`, so no network connections happen during the
  configuration phase (fixing CI slowness).
- Resource deduplication: `ResourceStore.Get` now compares by `IncusName()`, so
  normalized references (`docker.io/nginx:alpine` vs `docker.io/library/nginx:alpine`)
  return the same object, preventing duplicate alias races.
- Bind-mount files are pushed post-start via `InstanceFile`; bind-mount directories
  become storage volumes with `HostPath` seeding on create. `PostDevices` and
  `ActionPostEnsure` are removed; volumes live in `Devices` and are ensured before
  `CreateInstanceFromImage`.
- Healthd resource removed: the `client.Healthd` wrapper is replaced by
  `healthdUp`/`healthdDown` helpers in `cmd/incus-compose/healthd.go`. `KindHealthd`,
  `HealthdConfig`, and `resource_healthd.go` are deleted.
- Healthd instance is prefixed with the project name to prevent cross-project collisions.

### Fixed

- Skip token registration when a cert already exists (prevents repeated
  re-registration on `ic-healthd` restart).
- `down` network-listing failure during `--project` delete is demoted to a warning;
  a nil-check prevents a panic when listing fails.

### Internal

- justfile version tags gained `--long --dirty` for healthd builds.

### Breaking

- Removed the project name from storage volume names (manual migration required;
  note beta10 renamed volumes again to `vol-{name}`).

## [0.0.1-beta8] - 2026-06-06

### Added

- `list --healthd`: opt-in flag to include the `ic-healthd` sidecar in list output.
- `up --detach|-d`: detach after starting services; logs are printed if not detached (#38).
- `incus-compose incus proxy`: pass-through command (`incus --project={name} <xyz>`) (#37).
- Image name in `ps`/`list`: the IMAGE column is populated for all instances,
  including the healthd sidecar, without requiring Image resources in read-only
  stacks (stored as `user.image_alias` at creation, resolved from
  `volatile.base_image` as a fallback).
- healthd resource limits: the sidecar is capped at 1 CPU and 50 MB RAM by default
  (required for project-wide limits).
- `ic-healthd` now compiles in and prints its version.
- `oci-registry-cache` promoted to a standalone helper project.

### Changed

- `healthd restart` works as intended: kill a service and healthd brings it back.
- **Breaking:** `--no-pull` replaced by `--pull` (string flag) for docker compose
  compatibility.

### Fixed

- `down` no longer deletes externally-managed networks.
- healthd: fixed API endpoint resolution when connecting to the Incus socket (#39).
- Various fixes for projects that attach to pre-existing external networks.

## [0.0.1-beta7] - 2026-06-05

### Added

- `incus-compose healthd` subcommand group for direct sidecar management:
  `logs` (stream), `reload` (reload health-check config), `restart`, `up`
  (recreate, `--recreate` supported), and `down`.
- External network name override via `x-incus-compose.network`: networks can declare
  their real Incus name independently of the compose key. Name resolution uses a
  4-candidate probe (raw and sanitized, for both the override and the compose name)
  and locks in the first match.

### Fixed

- serviceName truncation regression: hyphenated service names (e.g. `my-service`)
  were incorrectly stripped. Only trailing `-{n}` integer suffixes (the scaled
  instance convention) are now removed.
- `ic-healthd` now appears in `list`/`ps` output.
- Hardcoded default storage pool: `ic-healthd` resources now use the client's
  configured `DefaultStoragePool` instead of always `"default"`.
- `up --recreate` on a healthd container no longer loses `--incus`/`--project` OCI
  entrypoint flags; `ResourceStore.Remove()` is now called on every `Delete()`.

## [0.0.1-beta5] / [0.0.1-beta6] - 2026-06-04

### Added

- Automatic loading of a `compose.incus.yaml` override file when present next to the
  main Compose file, keeping upstream Docker Compose files unchanged while adding
  Incus-specific configuration in a separate file.

### Changed

- The `ic-healthd` image now uses `busybox:glibc` instead of `scratch`.

## [0.0.1-beta4] - 2026-06-04

### Added

- `x-incus` options: Compose services can pass raw Incus instance config directly
  through to Incus (memory/CPU limits, nesting, security flags, etc.).
- Automatic loading of the default incus profile.
- Project-wide `x-incus-compose.network-profile` support (disables a per-project
  default network/bridge).
- healthd reload on service changes.

### Fixed

- Network creation race that could cause dnsmasq failures in CI (avoids immediately
  updating a newly-created network before the old dnsmasq released its socket).
- `down` now deletes compose-managed networks when the project is brought down,
  fixing dangling networks.
- `up --no-pull` is now respected correctly (also ~2x faster test runs, 3min from 6min).

### Testing

- Test coverage +17% (35% -> 52%).

## [0.0.1-beta3] - 2026-06-03

### Added

- Kernel-mode NAT proxy for port proxies via `x-incus.nat-proxy` (#30).
- DHCP ranges and static IPv4 / IPv6 addresses on network creation.

### Fixed

- Healthd status in `ps`/`list`.
- Scaling now prunes dangling instances when `up --scale` lowers the count (#34).
- `logs` omits old logs then follows when `--follow` is set.
- Small fixes to keep CI green.

## [0.0.1-beta1] - 2026-06-01

Initial public beta. A ground-up Docker Compose workflow for Incus, inspired by
Brian Ketelsen's proof-of-concept.

### Added

- Familiar commands: `up`, `down`, `start`, `stop`, `restart`, `list` (and `ps`),
  `logs`, `exec`, `config`, plus `build`, `healthd`, `incus` (pass-through), and
  `self-update`.
- Compose project parsing via compose-go, with automatic `compose.incus.yaml`
  overrides and `x-incus` / `x-incus-compose` extensions for raw Incus options.
- Native OCI image pulling from docker.io, ghcr.io, and other registries.
- Two-stage image cache in a dedicated Incus project (survives `down`/`up`, avoids
  registry rate limits).
- Local image building via Podman/Docker.
- Bridge networks with automatic name sanitization.
- Static IPv4/IPv6 addresses with automatic DHCP ranges.
- Port forwarding via proxy devices or kernel NAT mode.
- Storage volumes with UID/GID shifting; bind mounts (pass-through by default,
  optional seeding).
- Health checks, restart policies, and `depends_on: service_healthy` ordering via
  the `ic-healthd` sidecar.
- Service scaling with `up --scale` and orphan pruning.
- Incus project isolation.
- Resource limits and other advanced compose features (`shm_size`,
  `container_name`, etc.).
- Configuration via `INCUS_COMPOSE_*` environment variables for every flag, with a
  configurable parallel worker count.

</details>
