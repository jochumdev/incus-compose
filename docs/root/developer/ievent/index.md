---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
tags: []
leafwiki_id: EazkEp8DR
leafwiki_title: ievent
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# ievent

The base `ic-dns` and `ic-healthd` share: read the Incus event stream, fill each
event in with what the daemon knows, act on the result. Two consumers, which is
what makes extracting it something other than a guess.

This is the reasoning. The code is the shape, and where they disagree the code
is right.

```
incusd  ->  source  ->  debounce  ->  enricher  ->  dns / checker  ->  http  ->  log
```

## The Chain

**Plugins call plugins.** Each holds its successor and continues the walk, so a
plugin can work either side of `next` - which is what lets `trace` bracket what
it traces from two positions, and what lets `debounce` hand an event on late.

**`Handle` is the inbox door.** It runs on the _previous_ plugin's goroutine and
must not block: it enqueues and returns. The work happens on the plugin's own
goroutine, and `next` is called from there when that work is done. So a plugin's
state belongs to that one goroutine - the same single-owner rule everything here
follows, and the reason none of it is locked.

Most plugins have no inbox at all. `Handle` rather than an exposed channel is
what lets them stay a function call, and what puts the decision about a full
queue on the plugin that is full, under its own name.

**Ordering is the binary's**, at compile time. A plugin does not say where it
belongs.

## Wants, Declared Up Front

Declared once, from every plugin, _before_ anything is wired - `Wants()` is
apart from `Setup` for that reason. The union is one entry per action, and every
plugin is handed the same finished table.

It has to be whole first because the enricher serves the entire chain from one
action. A table assembled during the wiring would be whichever part of the union
happened to exist when its own `Setup` ran.

Two fields, folding opposite ways and both toward **doing more work**:

- `Enrich` - true wins. An action anyone wants is walked, read for everything
  anyone asked of it.
- `Debounce` - false wins. One plugin needing every event stops the collapsing
  for everybody, and the zero value vetoes.

A plugin that gets either wrong pays for reads it did not need, or sees events
it could have skipped. Neither loses one.

## Order Survives Concurrent Reads

Events reach the chain in the order they arrived, not the order their reads
finished. The cost is head-of-line blocking **on delivery alone**: a slow read
at the front holds up what is after it, but never holds up their reads, which
are already running. Throughput is the pool's; latency is the slowest read still
in flight.

Every plugin after the enricher inherits that order for free rather than each
rebuilding it. A `create` landing after the `delete` that superseded it is a
correctly-locked wrong answer, and nothing downstream can undo it.

## source

One read, and it is the stream. The rest is decoding and handing over: the
model, the enrichment and the sweep are the enricher's, at a position.

**One goroutine, one door.** Events off the socket and actions out of `Command`
enter from the same select, so what a plugin raises cannot overtake the events
that caused it. `Command` waits for that goroutine rather than dropping.

**The table decides what enters.** An action nobody declared in `Wants` is
decoded and goes no further, which is most of what a lifecycle stream carries.

**A session is a listener, not a connection.** A connection holds no server
state, so a lost stream is one listener closing and the next opening: backoff
1s..60s, reset only when a listener actually opened. Incus accepts the TLS
connection of a certificate it does not trust and refuses only the stream, so
resetting on the attempt would be a hot retry loop. `ActionConnected` and
`ActionDisconnected` bracket every session, and the command line is served in
the gap between two - a reconnect is exactly when a round is abandoned.

## debounce

Leading **and** trailing. The first event of a burst goes at once, so a lone
change is never delayed; the last goes once the key has been quiet, so whatever
it settled on is what stands. A burst of one is not reported twice.

Everything in between is superseded and handed on marked `dropped`. That is why
drops travel: `log` and the observers after it still see every event Incus sent,
and who dropped it.

## The enricher

A plugin at a position, not something ahead of the chain - which is what lets
`debounce` sit before and collapse a burst before it costs a read.

**Local patches, not re-reads.** An event says what changed, and what changed is
one instance, one network or one project. A delete deletes. A rename drops the
old key.

**An incomplete read is no read.** A NIC naming a network nothing has read yet
cannot be placed, so the read is refused whole rather than stored without it. A
subset stored once stays: the instance is then not on that wire, and the network
event that would repair it only ever reaches what is already known to be there.

**One read in flight per key.** A second event on a key joins the read already
running; coalescing saves the read, not the event, so both still walk carrying
what it found.

**Fan-out instead of a bulk read.** A profile change re-expands every instance
using it, and a network change moves every record on that wire - the event names
none of them. The model already knows who they are, so each becomes its own
read, and the project is never read to find out.

**A round, not a pass.** Nothing reads the fleet at once. A goroutine goes over
it by name for the life of the process, paced, handing each name back through
the path a live event takes. Duration therefore stops being a correctness
parameter: there is no bracket to sit inside, only an instant at the end.

## A plugin owns what it serves, and reconciles it

`dns` is not a plugin that hands records to a server somewhere else. It holds
the engine, starts the listener, and puts the forwarder after itself, so `main`
composes a chain and knows nothing about DNS - which is what lets a second
binary be a copy of `main` with a different list.

What it serves is not only what Incus reports. An instance labels itself with
extra names, and one of them may sit in a domain no project asked us to serve -
so a zone is invented for it, claiming the aliased names and falling through for
the rest. Being authoritative for a name and being authoritative for its domain
are different claims, and only the first was asked for.

Its listener outlives its fold loop. Answering from the last snapshot while the
chain drains beats refusing, and the two cannot hurt each other: one goroutine
writes an `atomic.Pointer` and every query goroutine reads it. That is the only
crossing, and it is why nothing in the query path is locked.

**The enricher reconciles the fleet.** The enricher lists names for what Incus
holds during its rounds, and emits synthetic `instance-deleted` events for what
disappeared. Downstream plugins like `dns` receive those deletes as ordinary
events off the chain rather than reading Incus themselves.

The round diff follows one rule: **prune against what you asked about**,
recorded when the listing went out, never against the store as it stands when
the answer lands. A name that arrived in between is not in the record and
survives.

A project whose marker has gone is a delete, not a project to stop reading -
otherwise its records would answer forever. That is the one question Incus
cannot answer, so the enricher is handed the predicate rather than deriving it.

## Readiness is an event

`http` serves `/metrics`, `/health` and `/ready`, and holds no reference to
anything. It learns by being in the chain.

`/metrics` needs no coupling at all: every plugin registers through `promauto`,
which is the default registry, which is what `promhttp` serves.

`/ready` is the interesting one. `dns` raises `dns/ready` through `Command` when
what it published becomes worth answering from, and what `http` latches on is
the `ChainState` riding on `enricher/sweep-end`. Not a pointer to `dns`, not a
registry, not an interface the chain is walked for: one path, in order against
the events that caused it, which is the same argument the end of a round rests
on. Edges rather than a level, so whoever folds them starts not-ready - the
truth before anything has been read.

That is also what opens the action namespace. Everything with a slash used to be
`source/*`; now the prefix names whoever raised it, and the rule doing the work
is unchanged - a slash, because an Incus lifecycle action cannot contain one.

The enricher is what turns the chain warm, stamping `ChainWarm` on the
`enricher/sweep-end` it raises. Warm therefore says a round has been all the way
round, and every plugin reads it off the events that follow.

`/health` and `/ready` answer different questions on purpose. The only sensible
response to `/health` failing is a restart, and a restart does not fix an Incus
that is down: it throws away everything held and answers nothing until the fleet
is re-read. So a lost stream is unready, never unhealthy.

## Unchanged goes no further

The enricher keeps the last event it emitted about each subject and compares
what a read found against it. A wire that moved is then one event rather than
one per instance on it, which is where a fleet-sized burst of "nothing changed"
came from.

The comparison is structural, never a hash: a collision would read a real change
as a repeat. It excludes when the event was decoded and what the chain was
doing, neither of which is about the subject, and includes the action, so a stop
is never absorbed by the update before it.

An event saying nothing new goes nowhere, whoever sent it - and most of what a
busy fleet emits is one, since `volatile.*` is stripped before the comparison
and a lease renewal writes nothing else.

A real event is trashed in the place it already holds rather than pulled out of
the line: a delete needs no read and settles at once, so taking the place away
would let it overtake the event before it.

## The key first, the round after it

A failed read fails its event immediately and reads that one key again a second
later, three times. Nothing retries inline: the events queued after that one
would wait out the whole delay. A read that landed on a running instance holding
no address is the same case - the lease arrives after `instance-started`, so the
read the event triggered is a moment too early.

A key those attempts did not fix waits for the round, which reaches every name
there is. Nothing pulls a round in early: it is the one repair mechanism that
must not be hurried, and a daemon having a bad minute would otherwise be met
with one round per failure.

`Command` injects at the head of the chain: the enricher raises the end of a
round that way so everything _before_ it sees it too. The fan-out goes forward
through `next` instead, already enriched - sending it round would have
`debounce` collapse a burst into its last instance and the enricher re-read
every one.

The release queue is unbounded on purpose, and `ReadTimeout` is what keeps it
finite: the head always settles inside one timeout, so the line only ever holds
what arrived during it. `debounce` before and per-key coalescing after make that
a small number. A timeout raised far enough would move that bound.

## Open Questions

Ours first, then what linking CoreDNS costs.

- **A state read that fails fails the event**, scheduling a fast retry without
  updating the model; a running instance holding no address (e.g. pending DHCP
  lease) records as address-less and is also retried.
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
- **Upstream metrics label off `metrics.WithServer`**, which reads a context key
  `core/dnsserver` sets - and nothing here runs it. So `coredns_cache_*` and
  friends carry `server=""` for good. Ours does not: `ecs_view` takes the name
  as a field at construction. There is no fix for theirs short of a Corefile.
- **Any upstream plugin links `core/dnsserver`**, because each carries a
  `setup.go` in its own package and Go compiles a package whole. It is a toll
  paid once rather than per plugin - `forward` is 408 packages and `cache` 415,
  nearly the same set - and it costs 8.7 MiB of binary and nothing at runtime. A
  `-light` build is `main` with `queryChain` returning nil, which is about 200
  packages and 13 MiB smaller.
