---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
tags: []
leafwiki_id: cnZ6LpUDR
leafwiki_title: source
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# source

Turns an Incus connection into a stream of events and hands each one to the
chain. It is `main`'s object, **not a plugin**: nothing in the chain holds it,
and it is the only thing here with a lifecycle of its own.

It only ever reads, and what is left of that is one read: the stream. The
enrichment, the model and the round belong to
[[developer/ievent/enricher|the enricher]], which is a plugin at a position
rather than something ahead of the chain. Nothing here changes anything in
Incus - the actions and writes belong to downstream plugins (such as `checker`
in `ic-healthd`), and that asymmetry is what the name is for.

The listener itself asks Incus for every project the certificate can see, in one
connection. Which of those are actually served is the enricher's call, on what
it reads, not this package's.

## One Goroutine, One Door

Events off the socket and actions out of `Command` enter from the same select,
so what a plugin raises cannot overtake the events that caused it.

`Command` **waits** for that goroutine rather than dropping. What it waits on is
a goroutine that only ever enqueues, so the line is short - eight - and
buffering more would not shorten the wait.

Every field on `Source` belongs to that one goroutine - the one running `Run`

- except the channels, which is how the plugins and `Run` meet at all.

## Decoding Judges Nothing

`decodeLifecycle` parses and nothing more: `Action` stays Incus's own string,
uncategorized, because deciding what it means is the enricher's job and the
chain's, not the source's.

The name comes through `iclient` rather than off the raw event directly, because
incusd fills `Name` on instance events alone and leaves every other kind
carrying it in `Source` instead. The project comes off the envelope
(`raw.Project`) rather than the payload for the same reason: incusd leaves it
empty on project and profile events. Everything downstream is keyed by project,
so an event naming none has nowhere to go - it is dropped as `errIgnored`, which
is not a failure, just most of what a lifecycle stream carries: plenty of it
belongs to no project at all.

A rename carries the pre-rename name in the event's context, under `old_name`,
and only on a rename. A value of the wrong type reads as absent, which the
consumer already handles.

The timestamp stamped on the event is `time.Now`, not `raw.Timestamp` - so what
a downstream measurement (like `log`'s) reports is time spent in the chain, not
the clock difference to whichever cluster member sent it.

## The Wants Table Decides What Enters

An action nobody declared in `Wants` is decoded and goes no further, which is
most of what a lifecycle stream carries.

The union is collected in a sweep of its own, before the first `Setup`, because
the enricher serves the whole chain from a single action. See
[[developer/ievent/index|the contract]].

## Wiring Happens Here, Not In main

Each plugin is given its successor by `Setup`, walking the slice backwards, so
`main` writes the chain forwards and in the order it runs. A constructor taking
`next` would make `main` build inside-out instead.

An error from any `Setup` stops the process. Configuration that cannot work is
rejected while somebody is still watching, rather than degrading once events are
flowing.

A plugin listed twice is refused before any of this runs, rather than wired
twice: `Setup` would be called on it a second time and the second successor
would silently overwrite the first.

Each plugin's `in` channel - the one `Command` questions arrive on - is
unbuffered on purpose. A slot would let the source ask a plugin that is not
actually listening and believe it had been heard.

Whether a plugin needs draining at shutdown is decided by what it is, not by
what `main` remembers to say: a plugin that does not implement
`Run(context.Context) error` has no goroutine to wait on, so it counts as
already finished the moment it is wired.

## A Session Is A Listener, Not A Connection

A connection holds no server state, so a lost stream is one listener closing and
the next opening. Backoff is 1s to 60s, **reset only when a listener actually
opened** - Incus accepts the TLS connection of a certificate it does not trust
and refuses only the stream, so resetting on the attempt would be a hot retry
loop against a daemon that will never let us in.

`ActionConnected` and `ActionDisconnected` bracket every session, and the
command line is served in the gap between two. A reconnect is exactly when a
round is abandoned and started again.

`ActionConnected` is raised the moment the listener opens - the moment Incus has
accepted the socket - and it is what the enricher reads a whole fleet off the
back of. `ActionDisconnected` pairs with every way a session can end, a canceled
context included, and resets `chain` to `iutil.ChainCold` along with it.

A daemon that is not there is not treated as a failure: `Run` just keeps opening
listeners on backoff. `Run` returning says only that the stream is closed - it
says nothing about the plugins, which is why `main` drains those only
afterwards.

The wait between sessions still drains `raised` and applies whatever comes
through: the command line has to stay served between sessions as well as inside
one, and a reconnect is exactly the moment a pass fails and raises something.

`chain` itself is blind cargo: a plugin's `Command` sets it, `apply` stamps it
onto every event handed to the head, and nothing here derives or checks the
transition between one value and the next.

## Shutdown Order

`plugged` pairs each plugin with a channel of its own, because a question has to
reach a plugin whose event inbox is full - and at shutdown that is every plugin.
Each also carries a `done` that closes when its `Run` returns, so the source
stops waiting for an answer that is not coming.

The plugins are asked to finish in the order events travel them, which is the
one shape that lets each drain what the one before it already handed over.

The answer `Drain` waits for from each is the same `CommandDrain` action echoed
back. Anything else a plugin raises on the way out is too late to carry, so it
is read off `raised` and dropped rather than left to block whoever sent it.

## Known Gaps

- **The plugins share one context**, so a shutdown cancels all of them at once
  and one can drain before the plugin before it has handed its last event over.
  A chain-shaped shutdown, each stopping after the one that feeds it, is what
  the source and the plugins already do between them.
- **`TestShutdownHandsOnWhatItHolds` used to fail about one run in ten** with
  "the enricher never answered the drain". It did not reproduce in 30
  consecutive `-race` runs on 2026-08-24, so if it is still there the rate is
  far below that. The shape fitted `raise` blocking on a full `CommandOut` once
  the harness had stopped reading it, which would keep `Run` from ever reaching
  the drain branch.
