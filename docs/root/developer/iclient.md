---
date: 2026-08-27T23:33:11Z
dateCreated: 2026-08-09T11:00:00Z
description: iclient, our fork of the Incus client - why one connection is safe to share, operations as channels, and what it deliberately does not do.
editor: markdown
title: iclient
leafwiki_id: vwIoKtUvRz
leafwiki_title: iclient
leafwiki_created_at: "2026-08-09T11:00:00Z"
leafwiki_updated_at: "2026-08-27T23:33:11Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# iclient

`iclient` is our fork of `github.com/lxc/incus/v7/client` (Apache-2.0).
Everything in incus-compose reaches Incus through it - `client/`, `project/`,
`cmd/incus-compose` and the `ic-healthd` sidecar. Nothing imports the upstream
client any more.

## Why it exists

The upstream client shares state between a connection, its event listeners and
the operations running on it, so **one `InstanceServer` cannot be driven from
several goroutines**:

- A second `GetEvents`/`GetEventsByType` on the same `*ProtocolIncus` does not
  open a socket. It joins the first caller's listener and inherits its type
  filter. So a long-lived `type=lifecycle` listener starves every later
  operation on that connection: `queryOperation` waits for a `type=operation`
  event that never arrives behind the lifecycle filter, and
  `RemoteOperation.Wait()` hangs forever.
- `skipEvents` has one guarded write against four unguarded reads. Any genuinely
  shared connection races on it.

incus-compose runs a [WorkerPool](/developer/client), so this is not a corner
case for us - it is the normal shape of a run. The old workaround was to hand
every resource its own `UseProject(...)` copy. An `iclient.Connection` holds
nothing mutable and every `ListenEvents` is a socket of its own, so a single
connection is safe to share and the workaround is gone - along with the project
it used to be scoped to, which is now
[a parameter](#the-project-is-a-parameter).

## Connecting

Three steps, each of which can be done once and reused:

```go
config, err := iclient.ReadConfig("")        // the Incus CLI configuration
info, err := config.RemoteInfos("my-remote") // everything needed to dial it
conn, err := iclient.NewConnection(info)     // the connection
```

`client.DialRemote(path, remote)` is those three lines, and is what the CLI and
the tests use. An empty remote means the configuration's default. An `oci`
remote has no daemon to dial; its `info` goes to
[`NewRepository`](#reading-an-images-config) instead.

`ReadConfig` is the only thing that touches disk. Nothing mutates a `*Config`
afterwards, so it is safe to share - a well-known registry is resolved against
it, never written into it, and the [credentials memo](#registry-credentials) is
filled in before it is shared.

| Method                         | Returns                                                         |
| ------------------------------ | --------------------------------------------------------------- |
| `WithMaxIdleConns(n, perHost)` | A copy with a pool of its own - resizing a live pool is a race. |

Nothing has to be handed back, and there is no `Close`. A `Connection` is not a
resource with a lifetime of its own: what it holds is a transport, and an
abandoned one closes its idle sockets after 90s and is then collected. Callers
drop connections; they do not close them.

### Transport

The tuning is not incidental, and each part has a reason:

- **No `http.Client.Timeout`.** It bounds the whole request including the body,
  which would cut off the event stream, an operation long-poll, and every
  console or SFTP transfer. The per-call bound is the context.
- **Keep-alives on, 128 idle connections / 32 per host.** Upstream sets
  `DisableKeepAlives`, paying a TCP and TLS handshake per request; Go's default
  of 2 idle per host makes a worker pool reconnect constantly.
- **`MaxConnsPerHost: 0`.** Bounding it here blocks, and the event listener
  holds a connection for the life of the process. The worker pool is where
  concurrency is meant to be bounded.
- **`ForceAttemptHTTP2: false`.** Events, exec and console need an HTTP/1.1
  upgrade, which h2 does not do.
- **`ResponseHeaderTimeout: 1h`.** An operation wait sends no header until it
  finishes.

## The project is a parameter

A `Connection` reaches one daemon, not one project. Every project-scoped call
takes its project as the parameter after `ctx`:

```go
one, _, err := conn.GetInstance(ctx, "blog", "web-1", nil)
err = conn.DeleteNetwork(ctx, "blog", "blog-frontend")
```

An empty project sends none, which incusd reads as the default project. A
server-level call takes none at all: `GetServer`, `HasExtension`,
`GetConnectionInfo`, `RawQuery`, the certificate calls, the project calls and
`GetStoragePoolNames`.

**An async call's project reaches its event listener**, and that is what the
parameter is threaded all the way down for rather than being resolved into a
query at the public boundary. incusd filters the event stream by project
(`internal/server/events/events.go:197`) and an operation's updates carry the
project it runs in (`internal/server/operations/linux.go:63`), so a listener
scoped anywhere else sees nothing and the caller blocks on updates that never
arrive - a hang, not an error.

A remote's configured `project:` is not read. It reached exactly one thing, the
project a `Connection` was born with, and there is no longer one to seed.

## Operations are channels

An asynchronous call hands back `<-chan api.Operation`: the operation as the
server accepted it, then every update, closing on a terminal state.

```go
updates, err := conn.UpdateInstanceState(ctx, name, put, "")
op, err := iclient.WaitOperation(ctx, updates)
```

The listener opens **before** the request goes out. That ordering is the whole
point: an operation that finishes immediately would otherwise complete in the
gap between the response and the subscription, and never be reported.

Waiting is ranging to the close, and the last value is the outcome - which is
what `WaitOperation` does. Consuming the updates yourself is how progress is
reported; see [Progress](/developer/progress).

> **Trap: token operations.** A trust token (`CreateCertificateToken`) is
> created and then waits to be _used_, so it never reaches a terminal state.
> Read the first value, which carries the token, and cancel the context. Ranging
> to the close waits for the token to expire, and `WaitOperation` never returns.
>
> An image secret is a token operation too, which is why `CopyImage` reads it
> from the response to the request rather than following it at all.

## Arguments, not method names

Upstream spells each axis of a call as its own method, up to
`GetInstancesFullAllProjectsWithFilter` - a set that doubles every time an axis
is added. Here the axes are a struct, and a `nil` one is the zero value:

```go
all, err := conn.GetInstances(ctx, "blog", &iclient.GetInstancesArgs{Full: true})
one, _, err := conn.GetInstance(ctx, "blog", "web-1", nil)
```

The same shape covers `GetImageArgs`, `GetImageAliasArgs`,
`GetStoragePoolVolumeArgs`, `GetInstanceArgs`, `ImageCopyArgs`,
`ImageCreateArgs`, `InstanceExecArgs`, `InstanceConsoleArgs` and
`DeleteProjectArgs`.

The exception is an axis that changes which endpoint the server serves.
All-projects is one: incusd answers 400 to a request carrying both it and a
project, and authorizes the two differently. So it is a function of its own -
`GetInstancesAllProjects`, `ListenEventsAllProjects` - which sends no project
and cannot be combined with one by mistake. There is no all-projects form of
`GetInstanceNames`, a bare name not being unique across projects.

## Errors

Sentinels, matched with `errors.Is`:

| Sentinel                     | Means                                                  |
| ---------------------------- | ------------------------------------------------------ |
| `ErrConfigRemoteNotFound`    | The configuration does not name that remote.           |
| `ErrConnectionNoAddress`     | The remote has nothing to dial.                        |
| `ErrConnectionUnsupported`   | The remote cannot serve that call.                     |
| `ErrInstanceBusy`            | Another operation holds the instance's operation lock. |
| `ErrVolumeInUse`             | The storage volume still has a user.                   |
| `ErrRegistryProtocol`        | `NewRepository` got a remote that is not a registry.   |
| `ErrRegistryAddrCredentials` | An address still carries a login; the fields take it.  |
| `ErrCredHelper`              | A remote's credentials helper failed.                  |

Everything else arrives as an `api.StatusError`, so
`api.StatusErrorCheck(err, 404)` works as it does upstream.

### The instance lock

Incus takes the instance's operation lock **in the driver, inside the
operation**, so a write issued while it is held is accepted and then fails from
the operation. `ErrInstanceBusy` therefore usually surfaces from
`WaitOperation`, not from the call that started it - a retry has to wrap the
wait, not just the request. A config PATCH is the exception and fails on the
call itself.

`WaitInstanceBusy(ctx, name)` blocks until no queryable operation holds the
lock, which turns a retry from a blind sleep into one that starts when the
instance is actually free. It cannot see everything: the lock is a map inside
incusd and this infers it from the operations list, so a holder with no API
operation behind it - autostart, shutdown, an exec - is invisible. That is why
callers keep a short delay as well.

## Images: the server fetches

A registry or a simplestreams remote is **somewhere to point the server at** for
the image itself. Resolving an OCI tag needs skopeo, which is the server's
business:

```go
conn.CreateImage(ctx, api.ImagesPost{
    Aliases: []api.ImageAlias{{Name: alias}},
    Source: &api.ImagesPostSource{
        ImageSource: api.ImageSource{Server: "https://docker.io", Protocol: "oci"},
        Type: "image", Mode: "pull", Fingerprint: "library/alpine:latest",
    },
}, nil)
```

`CopyImage(ctx, source, fingerprint, args)` is the same idea between two
connections, and it owns the secret a non-public image needs.

Passing `ImageCreateArgs` uploads the tarballs instead, which is how the compose
`build:` path imports a locally built image. The body is then the tarballs, so
the aliases, properties and public flag travel as `X-Incus-*` **headers** -
leaving them out imports the image and silently drops its alias.

### Reading an image's config

What the server pulls does not carry the OCI image config. Incus flattens
ENTRYPOINT and CMD into one `oci.entrypoint` and keeps no `Volumes` at all, and
`incus image export` hands back the same runtime spec rather than the image
config. `NewRepository` reads that config from the registry instead:

```go
info, err := config.RemoteInfos("docker.io")
repo, err := iclient.NewRepository(info, "library/redis:alpine")

desc, rc, err := repo.FetchReference(ctx, repo.Reference.Reference)
```

It is [oras-go](https://oras.land)'s `*remote.Repository`, so the OCI
Distribution API is the whole surface. Manifests and the config blob are what
this is for; layers stay the server's to fetch.

| From the remote         | Becomes                                                  |
| ----------------------- | -------------------------------------------------------- |
| `Addrs[0]` host         | The registry, so a mirror stands in for what it mirrors. |
| `Addrs[0]` scheme       | `PlainHTTP`, for `http://`.                              |
| `Username` / `Password` | The registry credential.                                 |
| `ServerCert`            | The registry certificate to pin.                         |
| `UserAgent`             | The `User-Agent` header.                                 |

`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` apply as they do to a `Connection`. A
remote whose protocol is not `oci` returns `ErrRegistryProtocol`.

### Registry credentials

`RemoteInfos` resolves an `oci` remote's login into `Username` and `Password`,
from its `credentials_helper` if it has one, else from a login its address
carries. The helper is the
[docker credentials helper](https://github.com/docker/docker-credential-helpers)
protocol, called exactly as the incus CLI calls it - the registry host on stdin,
`{"Username","Secret"}` back - so one helper serves both tools.

**The login never stays in `Addrs`.** It is lifted out into the two fields, and
an address that still carries one is refused by `NewRepository` with
`ErrRegistryAddrCredentials` rather than reached anonymously. Addresses end up
in logs and error strings; these two fields do not.

Each remote is resolved at most once per `Config`: `ReadConfig` gives every
remote a memo entry up front, so a run pulling a dozen images from one registry
asks the helper once, and two registries still resolve at the same time.

The one place a login becomes a URL again is the pull, where
`ImagesPost.Source.Server` is the only channel incusd offers - `ImageSource` has
no field for it. incusd logs that URL when it connects, which is inherent to the
API rather than something this can avoid.

_Since: v1.3.0_

## Streams

| Call                                              | Shape                                                      |
| ------------------------------------------------- | ---------------------------------------------------------- |
| `ListenEvents(ctx, project, types)`               | `<-chan api.Event`; the socket is this connection's own.   |
| `ListenEventsAllProjects(ctx, types)`             | The same, over every project the certificate may see.      |
| `ExecInstance(ctx, project, name, post, args)`    | Output to writers; the channel closes once it has drained. |
| `ConsoleInstance(ctx, project, name, post, args)` | Console to a writer; cancel the context to detach.         |
| `GetInstanceFileSFTP(ctx, project, name)`         | A `*sftp.Client`; the caller closes it.                    |
| `GetStoragePoolVolumeFileSFTP(ctx, project, ...)` | The same, for a custom volume.                             |

An event socket that says nothing for 30s counts as dead. The server pings every
10s, so silence is not something a healthy connection does - without the check a
half-open socket sits in `ReadMessage` until TCP keepalive gives up minutes
later, and nothing above learns the stream stopped.

`ListenEventsAllProjects` sends no project at all: the server takes a different
path, building a permission checker instead of authorizing one project, and
answers with every project the certificate may see. That is how one listener
serves projects that did not exist when it opened. ic-healthd is built on it;
see [Health Checking](/healthd).

A socket that names no project is a socket on the **default** project, not on
all of them (`cmd/incusd/events.go:60`), which is why the two are separate calls
rather than an empty string standing in for one of them.

## Not implemented

Deliberate, and each one returns `ErrConnectionUnsupported` or an error rather
than half-working:

- **Simplestreams connections.** Only the Incus REST API is spoken.
- **Pulling from a registry.** `NewRepository` reads an image's metadata; the
  image itself is still pulled by the server.
- **Interactive exec.** No PTY, no stdin, no resize control.
- **Console input.** `ConsoleInstance` attaches to watch a console, not to drive
  one.
- **Push and relay image copy.** Pull only.
- **Cluster targeting.** `PatchInstanceConfig` sends no target: instance config
  is cluster-wide state, so pinning the write to whichever member the caller
  reached would be arbitrary.

## Testing

Two tiers, following [Testing](/developer/testing):

- **Unit** tests use an `httptest` recording server and assert **what goes on
  the wire** - the path, the query, the headers. A real Incus answers happily
  without a `project` or `recursion` parameter, so those tests cannot catch a
  dropped one. `NewRepository` is tested the same way, against an `httptest`
  server speaking enough of the Distribution API to serve one manifest.
- **Integration and E2E** tests (`skipLocal` / `skipE2E`) drive a real Incus:
  the operation and event paths, exec, console, the busy lock, and an image pull
  from a registry.

## See Also

- [Architecture](/developer) - where this sits
- [Client Package](/developer/client) - the resource layer built on it
- [Health Checking](/healthd) - the sidecar and running the daemon directly
- [Progress](/developer/progress) - consuming an operation's updates
