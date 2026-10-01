---
date: 2026-08-28T00:09:08Z
dateCreated: 2026-08-27T23:33:35Z
leafwiki_id: Si801nwDg
leafwiki_title: Lifecycle
leafwiki_created_at: "2026-08-27T23:33:35Z"
leafwiki_updated_at: "2026-08-28T00:09:08Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# Lifecycle

`start`, `stop`, `kill`, `restart` and `pause`/`unpause` act on services that
already exist. See [up and down](../up-and-down/) for creating and removing
them.

## start

Start stopped services.

```
incus-compose start [SERVICE...]
```

| Option         | Description                                                       |
| -------------- | ----------------------------------------------------------------- |
| `--timeout`    | Start timeout (default: 1m)                                       |
| `--with-deps`  | Also start linked services (depends_on) - incus-compose extension |
| `--no-healthd` | Don't start healthd sidecar                                       |

## stop

Stop running services.

```
incus-compose stop [SERVICE...]
```

| Option         | Description                                                      |
| -------------- | ---------------------------------------------------------------- |
| `--timeout`    | Stop timeout (default: 10s)                                      |
| `--with-deps`  | Also stop linked services (depends_on) - incus-compose extension |
| `--no-healthd` | Don't stop healthd sidecar                                       |

Services are shut down gracefully and killed once `--timeout` is up, as docker
does. Incus itself does not escalate - it reports a failure and leaves the
instance running - so incus-compose issues the kill.

A timeout below a second is rounded up to one: Incus reads a zero timeout as an
immediate kill, which is the opposite of what a small number asks for.

_Changed in v1.3.0_: `stop` shuts down gracefully. It always killed outright
before, which made `--timeout` do nothing. Use [`kill`](#kill) for the old
behavior.

## kill

Force stop running services, skipping the graceful shutdown.

```
incus-compose kill [SERVICE...]
```

| Option           | Description                                                      |
| ---------------- | ---------------------------------------------------------------- |
| `-s`, `--signal` | Signal to send: `SIGKILL`, `KILL` or `9`, in any case            |
| `--with-deps`    | Also kill linked services (depends_on) - incus-compose extension |

`docker compose kill -s` can send any signal. The Incus state API has no field
for one, and Incus runs an OCI image's entrypoint under an init of its own, so
the entrypoint is not PID 1 and a signal aimed at it would reach the wrong
process.

So `-s` takes the three spellings of `SIGKILL` that docker takes, and anything
else is an error rather than a kill dressed up as the signal you asked for:

```bash
incus-compose kill -s SIGKILL   # and KILL, 9, sigkill
incus-compose kill -s SIGHUP    # error: unsupported signal
```

It has no environment variable, since the only value it accepts is the one it
already defaults to.

_Since: v1.3.0_

## restart

Restart running services.

```
incus-compose restart [SERVICE...]
```

| Option         | Description                                                         |
| -------------- | ------------------------------------------------------------------- |
| `--timeout`    | Stop/start timeout (default: 1m)                                    |
| `--with-deps`  | Also restart linked services (depends_on) - incus-compose extension |
| `--no-healthd` | Don't stop/start healthd sidecar                                    |

## pause / unpause

Freeze running services, and thaw them again.

```
incus-compose pause [SERVICE...]
incus-compose unpause [SERVICE...]
```

| Option        | Description                                                       |
| ------------- | ----------------------------------------------------------------- |
| `--with-deps` | Also pause linked services (depends_on) - incus-compose extension |

A paused service keeps its memory, its address and its open files; nothing
inside it runs. This is Incus's freeze/unfreeze, so it takes no timeout - the
instance is frozen at once - and `incus-compose ps` reports it as `Frozen`
without needing `--all`, as docker lists paused containers.

`pause` goes through the dependents first and `unpause` the other way round, so
a service is never running while something it depends on is frozen.

### Pausing and health checks

A frozen instance answers no healthcheck, and ic-healthd would read that as a
service that stopped and needs restarting. So `pause` also sets
`user.healthcheck.stopped`, the same marker `stop` uses, which tells the daemon
the stop was deliberate; `unpause` clears it again.

Two things follow. While paused, `user.healthcheck.status` reads `stopped`
rather than a status of its own. And a daemon older than v1.3.0 does not know
that a resume ends the pause, so after `unpause` it keeps treating the instance
as deliberately stopped until it next resyncs - run
`incus-compose healthd reload`, or update the daemon with
`incus-compose healthd up`.

_Since: v1.3.0_

## Environment variables

Flags given on the command line win. See
[Environment Variables](/environment-variables) for the resolution order and the
flags that deliberately have none.

| Command   | Variable                          | Flag          | Description                       |
| --------- | --------------------------------- | ------------- | --------------------------------- |
| `start`   | `INCUS_COMPOSE_START_TIMEOUT`     | `--timeout`   | Timeout for starting              |
| `start`   | `INCUS_COMPOSE_START_WITH_DEPS`   | `--with-deps` | Also start linked services        |
| `stop`    | `INCUS_COMPOSE_STOP_TIMEOUT`      | `--timeout`   | Timeout for stopping              |
| `stop`    | `INCUS_COMPOSE_STOP_WITH_DEPS`    | `--with-deps` | Also stop linked services         |
| `kill`    | `INCUS_COMPOSE_STOP_WITH_DEPS`    | `--with-deps` | Also kill linked services         |
| `pause`   | `INCUS_COMPOSE_PAUSE_WITH_DEPS`   | `--with-deps` | Also pause linked services        |
| `unpause` | `INCUS_COMPOSE_UNPAUSE_WITH_DEPS` | `--with-deps` | Also unpause linked services      |
| `restart` | `INCUS_COMPOSE_RESTART_TIMEOUT`   | `--timeout`   | Timeout for stopping and starting |
| `restart` | `INCUS_COMPOSE_RESTART_WITH_DEPS` | `--with-deps` | Also restart linked services      |

`kill` is the one command that reads another's variables: it is `stop` without
the graceful shutdown, so it takes `INCUS_COMPOSE_STOP_WITH_DEPS` rather than a
pair of its own.
