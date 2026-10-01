---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
leafwiki_id: 6GWeLtUDg
leafwiki_title: http
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# http

Serves what an operator and an orchestrator ask of the process: `/metrics`,
`/health` and `/ready`.

## A Position In The Chain, Not A Server Of Its Own

The reason is the chain itself. A plugin in the chain sees every event, so
liveness here is "the stream is connected and something walked recently" rather
than "the process has not exited", and readiness arrives as an event like
everything else.

**Nothing here holds a reference to another plugin.** Not a pointer to `dns`,
not a registry, not an interface the chain is walked for. It learns by being in
the chain.

`/metrics` needs no coupling at all: every plugin registers through `promauto`,
which is the default registry, which is what `promhttp` serves.

`/ready` is the interesting one. [[developer/dns|dns]] raises `dns/ready`
through `Command` for whoever is watching, and what http latches on is the
`ChainState` the enricher stamps on `enricher/sweep-end` - one path, in order
against the events that caused it, which is the same argument the end of a round
rests on. Edges rather than a level, so whoever folds them starts not-ready: the
truth before anything has been read.

`ready` latches true on `ChainWarm` and clears on `ChainCold`, so it can flap -
a fleet that drops back to cold is not-ready again until the chain warms back
up. `connected` is its own atomic, set straight off `ActionConnected` /
`ActionDisconnected`: readiness does not imply the stream is up, and the reverse
doesn't hold either, which is why `/ready` checks both and fails on whichever
one is missing.

## /health And /ready Answer Different Questions

The only sensible response to `/health` failing is a restart, and a restart does
not fix an Incus that is down - it throws away everything held and answers
nothing until the fleet has been re-read.

So **a lost stream is unready, never unhealthy.**

|           | fails when                                                           |
| --------- | -------------------------------------------------------------------- |
| `/health` | the chain has been silent for `Silence` (`Silence > 0`)              |
| `/ready`  | the stream isn't connected, or nothing has been published yet (cold) |

`Silence` is zero by default, which never fails on silence. A quiet fleet is the
normal state, and a liveness probe that restarts a process for being idle is
worse than one that never fires. Set it where something is known to be talking -
the round bounds it, because a round is events.

## Two Goroutines Touch It

The chain's, through `Handle`, and `net/http`'s, through the handlers. That is
the case the contract names as the legitimate one for a second reader, and
atomics are enough for it: nothing here is read and written as a pair.

`lastEvent` is a `UnixNano` `atomic.Int64` rather than a `time.Time` behind a
lock, for the same reason.

`Handle` folds an event into those atomics and calls `next` immediately, on the
caller's goroutine - no inbox, like [[developer/ievent/log|log]]. Staying
synchronous costs three atomic stores, which is cheaper than owning a goroutine
and a channel to protect work this small.

## Defaults

Read timeout 5s, write 10s, shutdown 5s. Everything it answers is a field read,
so a client that cannot manage this is not one worth holding open.

An empty `Listen` serves nothing, which is what a build that only wants DNS asks
for. `Run` still blocks and waits rather than returning, so an empty `Listen`
doesn't read as a failure.

## /debug/pprof

Off by default, and never on in a deployment: the handlers expose the command
line and the heap, and a profile costs what it measures. It exists because the
query path's own cost is a fraction of what a wire run reports - the rest can
only be found with a profile.

The routes carry no method prefix: `go tool pprof` resolves symbols with a POST,
and the trailing slash on `/debug/pprof/` is what routes `/heap`, `/goroutine`
and `/allocs` to `Index`. The write timeout is switched off whenever `Pprof` is
on - `?seconds=30` only answers once the profile is finished, and the usual
timeout would cut everything longer than itself and hand back a truncated
profile.

## Finishing Is A Command Too

`in` and `out` are this plugin's own channel pair, separate from the chain of
events: the source asks http to finish through a `Command`, and http answers on
`out` - its own channel so the question arrives whatever else is going on.

With a `Listen`, nothing is held to drain - http keeps no event state - so the
endpoints keep answering normally until the server is told to shut down.
Shutdown runs against `context.WithoutCancel(ctx)` with its own
`shutdownTimeout` budget: `ctx` may already be canceled, and a shutdown that
inherited it would cut live connections instead of letting them finish.

With no `Listen`, `Run` still waits on that same `Command` instead of returning
outright - the source can't otherwise tell a plugin waiting quietly from one
that's wedged.
