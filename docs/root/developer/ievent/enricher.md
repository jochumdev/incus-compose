---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
leafwiki_id: lMZ6Yt8vg
leafwiki_title: enricher
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# enricher

Reads what an event's subject looks like now, and fills the rest of the event in
from what it already holds.

It asks for no read of its own. What it reads is what everybody else asked for:
`SetupArgs.Wanted` is the union of every plugin's `Wants`, so one action is one
lookup and one fetch plan, whoever after here needed it.

It does declare the actions its own dispatch acts on, with no enrichment against
them. An action nobody names is dropped by the source, so a branch here that
decides what an action costs would never run - the profile actions are those,
and without them a profile change reaches an instance only when the round next
reads it.

`profile-updated` is collapsible and `profile-deleted` is not. A profile carries
no history an update could lose, and each one costs a read per instance in the
project, so the last of a burst is the one worth paying for. A delete lost in a
burst is a delete lost.

It is a plugin at a position rather than something ahead of the chain, which is
what lets [[developer/ievent/debounce|debounce]] sit before and collapse a burst
before it costs a read.

## Internal Flow

```mermaid
flowchart TD
    subgraph Inputs["Event Sources"]
        Inbox["inbox (Event Stream)"]
        Sweeps["sweeper (Fleet Round)"]
        RetryTimer["retries.timer (Fast / Slow Retries)"]
    end

    subgraph RunLoop["Run Goroutine (State & Queue Owner)"]
        Accept{"accept(event)"}

        subgraph ReadsDeferred["deferred (In-Flight Read Manager)"]
            CheckFlight{"In-flight read for key?"}
            CancelOld["d.cancel: Abort context & transfer waiting items"]
            Coalesce["out.join: Coalesce items into in-flight read"]
            HoldCold["Hold cold / pendingProject"]
            SubmitPool["d.submit to worker pool"]
        end

        subgraph Queue["q (Arrival-Order Ring Queue)"]
            QPush["q.push: placeholder item"]
            QSettle["q.settle: attach enriched event"]
            QTrash["q.trash: mark skipped"]
            QRelease["q.release: drain contiguous settled head"]
        end

        subgraph Settle["settleRead"]
            CheckCurrent{"res.call == d.calls[key]?"}
            DevNull["/dev/null (Discard obsolete result)"]
            DoneCall["d.done: cleanup calls & cancels"]
            PatchState["patchState: update model"]
            ArchiveCheck{"archive.changed?"}
        end

        subgraph StateStore["state (In-Memory Fleet Cache)"]
            StateModel[("Instances, Networks, Projects")]
        end
    end

    subgraph Workers["ants.Pool (Async Pool Workers)"]
        WorkerExec["incusReader: GetInstance & GetInstanceState"]
        PollLoop{"waitForRunning or wantInterfaces?"}
        PollWorker["Poll until Running & IP assigned"]
        SendResult["Send to d.results channel"]
    end

    Downstream["args.Next(event)"]

    %% Event Ingestion
    Inbox --> Accept
    Sweeps --> Accept
    RetryTimer --> Accept

    %% Accept routing
    Accept -->|"Non-instance or delete"| PatchState
    Accept -->|"Needs instance read"| QPush
    QPush --> CheckFlight

    %% Deferred read decision
    CheckFlight -->|"Newer start supersedes"| CancelOld
    CancelOld --> SubmitPool
    CheckFlight -->|"Compatible read"| Coalesce
    CheckFlight -->|"Cold or project pending"| HoldCold
    CheckFlight -->|"No read in flight"| SubmitPool

    %% Worker Execution
    SubmitPool --> WorkerExec
    WorkerExec --> PollLoop
    PollLoop -->|"Yes"| PollWorker
    PollLoop -->|"No"| SendResult
    PollWorker --> SendResult

    %% Result Settlement
    SendResult --> CheckCurrent
    CheckCurrent -->|"Cancelled / Superseded"| DevNull
    CheckCurrent -->|"Active read"| DoneCall
    DoneCall --> PatchState
    PatchState --> StateModel
    PatchState --> ArchiveCheck

    %% Deduplication and Queue Settlement
    ArchiveCheck -->|"No change"| QTrash
    ArchiveCheck -->|"Changed"| QSettle

    %% Output
    QSettle --> QRelease
    QTrash --> QRelease
    QRelease --> Downstream
```

## Four Things Happen Here

Only the first is a read of the event's own subject.

**An instance action** reads that instance. **One read in flight per key** - a
second event on the key joins the read already running, or cancels and
supersedes it if the new event requires waiting for running state (such as
`instance-started`). Coalescing saves the read, not the event: both still walk,
carrying what it found.

**A network action** patches the wire and re-reads everything sitting on it,
because a subnet moving changes every record on that wire.

**A profile action** re-reads every instance in the project, because a profile
re-expands their configuration and devices and the event names none of them. The
model already knows who they are, so the project is never read to find out.
Except a delete: Incus refuses to delete a profile still expanded into anything,
so the event names a profile no instance uses any more, and fanning out would
read a whole project's instances to learn nothing changed.

**A round** goes over the whole fleet by name, for ever, at its own pace. Each
name it hands back takes the first path above; what it holds and the listing
does not is dropped.

The middle two are fan-out. They issue reads through the pool; once settled,
each updated instance is handed forward through `next`, skipping `Command` so
`debounce` does not collapse them into one.

A network and an instance called the same thing in one project would otherwise
share a read: the key each flight is filed under is prefixed with which kind it
is, so the two can never collide.

## Every Event Carries Its Project's Labels Too

Before any of the four paths above, an event whose action somebody wants
`EnrichedProject` for gets the owning project's own configuration attached - not
a read of its own, but what the round already holds from its project listing,
read once per whole-fleet pass rather than again each time one of its instances
moves.

A project the pass has not reached yet is left unenriched rather than enriched
with nothing, so a consumer can tell "this project sets none" from "this project
has not been read" - both would otherwise arrive as the same empty map. An event
naming a project the model does not hold issues a targeted project read through
the pool rather than waiting on the next round. Once the read lands, the
project's config is stored, and the enricher fans out re-reads over that
project's instances so their events walk with labels attached.

## An Incomplete Read Is No Read

A NIC naming a network nothing has read cannot be placed, and what is left is a
subset of where the instance actually sits. So nothing is stored, nothing is
filed, and the events waiting on it fail the same way a read that errored fails
them - the key is simply owed another read.

Storing the subset is what made it permanent, and it cost a whole e2e run to
find. An instance read a second before its network was created recorded only the
wires that already existed. It was then not on the new wire, so the
`network-updated` that would have fixed it fanned out over `instancesOn` -
everyone already known to be there, which is everyone except it. Nothing looks
again: a wire arriving is not an instance event, and a lease writes only
`volatile.*`, which is stripped before the comparison. The record answered on
one wire and was missing on the other for the life of the process.

The same rule covers a rename in flight, where the old key is gone and the new
one has not landed. **A wire that is unknown is always one that is about to be
known**: an instance is created on a network that already exists, and a managed
network anything is attached to cannot be deleted.

## Local Patches, Not Re-Reads

An event says what changed, and what changed is one instance, one network or one
project. A delete deletes; a rename drops the old key. Reading the fleet to
learn what a delete already told us is the cost the model exists to avoid.

The model belongs to the goroutine `Run` owns. Nothing in it is locked, and
nothing in it may be touched from a pool worker: a worker reads Incus and hands
the answer back, and the patch happens where the model lives.

## Unchanged Goes No Further

The last event emitted about each subject is kept, keyed by the kind its action
names and the subject's project and name. What a read found is compared against
it, so a wire that moved is one event rather than one per instance sitting on
it - the fan-out is where a fleet-sized burst of "nothing changed" comes from.

`Event.Equal` is structural rather than a hash: a collision would read a real
change as a repeat, and this is two events rather than a fleet. It leaves out
what is not about the subject - when the event was decoded, what the chain was
doing, and a plugin's own values. Two things it deliberately keeps: the action,
so a stop is never absorbed by the update before it, and whether a read landed,
because "no networks" and "nobody asked for networks" are different answers.

**An event that says nothing new goes nowhere**, whoever sent it. Most of what a
busy fleet emits is one: `volatile.*` is stripped before the comparison, so a
DHCP lease renewal writes keys nobody here reads and produces an
`instance-updated` identical to the last one.

What differs is how each stops. A real event already holds a place in the line,
taken when it arrived and before its read landed, so it is trashed in place and
skipped on the way out. The place stays until it reaches the front: an
`instance-deleted` needs no read and settles at once, so pulling the place out
from under it would let the delete overtake the update before it.

A fan-out event has no place to keep. They are created in one burst, so their
order among themselves says nothing, and a live event on the same key joins the
same read rather than racing it. The read is issued without one, and the event
goes in only if what came back was worth it. On completion the real events
settle first, so the invented one is compared against what they just filed -
which is how a live event and a fan-out on one key stay one event.

A failed read files nothing. What the next read finds is compared against the
last answer there was, never against a failure.

**One instance read is absorbed in one place**, whether an event asked for it or
a round brought the name back. The patch and what the key is owed are decided
together, so the two paths cannot come to different conclusions about the same
answer - a state read that failed fails the event and retries soon, while a
running instance holding no address is a lease that has not landed yet and is
also retried.

## Order Survives Concurrent Reads

Reads run in the pool and land in whatever order Incus answers. What leaves the
enricher is still the order it arrived: an event goes when its own read has
landed **and** everything ahead of it has already gone.

The cost is head-of-line blocking **on delivery alone**. A slow read at the
front holds up what is after it, but never holds up their reads - those are
already running. Throughput is the pool's; latency is the slowest read still in
flight.

Every plugin after this one inherits that order for free rather than each
rebuilding it. A `create` landing after the `delete` that superseded it is a
correctly locked wrong answer, and nothing downstream can undo it.

The line is a ring rather than a slice reslicing forward, so the array is reused
instead of the front being abandoned and reallocated every time it fills. It
doubles and halves, so a fleet-wide burst does not leave it permanently the size
of the worst moment it ever had. One goroutine pushes, settles and releases, so
it holds no lock.

It is unbounded on purpose, and `ReadTimeout` is what keeps it finite: the head
always settles inside one timeout, so the line only ever holds what arrived
during it. `debounce` before and per-key coalescing after make that a small
number. A timeout raised far enough would move that bound.

## The Key First, The Round After It

A read that failed, and a read that found a running instance with no address,
fail their event immediately and read that one key again a second later, three
times. **Nothing retries inline** - the events queued after that one would wait
out the whole delay.

A second, because when an address appears differs widely between Incus LTS and
daily, and a round is far too blunt an instrument for a lease that is one moment
late. A re-read goes through the fan-out, so what it finds reaches the chain as
an event of its own; a key that settles has its attempts cleared and starts
again from three.

A key those attempts did not fix waits for the round, which reaches every name
there is. Nothing pulls a round in early: a daemon having a bad minute would
otherwise be met with one round per failure, and the round is the thing that
must not be hurried.

Spending the three fast attempts does not hand the key to the round outright -
repair never stops, it only slows, to one read every five seconds, for as long
as the key keeps failing. Stopping would mean waiting on a round that is hours
away by design, and the common case here is an instance read that landed between
its start and its lease - a gap nothing else ever announces, so nothing but
another read closes it.

A wire is not re-read at all. The fan-out creates `instance-updated`, so handing
it a network would have the daemon asked for an instance named after the wire -
which is the bug the action prefix exists to prevent.

## The Round

A goroutine `Run` starts, going over the fleet for the life of the process. It
sends on a channel and touches nothing else - not the model, not the queue -
which is what keeps this package free of a mutex.

Names, never listings. `GetProjectNames`, `GetNetworkNames` and
`GetInstanceNames`, then a read per name. Two phases, because the wire map has
to be whole before an instance distills against it:

```
projects:  names -> a read each -> prune what is no longer served
networks:  per owner: names -> a read each -> prune that owner's
           -> warm, which thaws whatever was held back
instances: per project: names -> prune -> then one name at a time
round end: enricher/sweep-end
```

Each name becomes a bare `instance-updated` taking the same path a live event
does, so an unchanged instance costs a read and no event.

**Cold until the first pass warms it.** An instance read arriving before `warm`
is held rather than issued - joined into a flight the same way a concurrent read
on the same key would be, just never submitted - and released in arrival order
the moment the round's network phase lands. Only instance reads wait: a network
read patches the one wire it names directly, and a profile or network fan-out
only ever names instances the model already holds, which is nothing while cold
either way. Once warm, it never goes cold again - a reconnect leaves the wires
exactly as stale as everything else this plugin holds, which is what the pass
that follows one is for.

**Two rates.** The first round runs at no delay, because nothing is served until
it lands. So does one that follows a round abandoned for a reconnect or given up
on because the daemon would not answer: both leave what is held as old as
whatever went wrong. Every other round is paced by one `ReadDelay`, between
projects and networks and the instance reads alike - and `SweepInterval` sits
between rounds, or a fleet read in no time is read again immediately, for ever.

**Absence is decided against what was asked about.** The model's keys in a scope
are recorded when that scope's listing goes out, and what is pruned is that
record minus the listing. A name created while the request was in flight is not
in the record, so it survives - and it had to, because nothing would put it
back: the next round reads it unchanged and emits nothing.

**A prune emits an `instance-deleted` event.** What a project listing does not
have has gone. The enricher creates synthetic `instance-deleted` events for
missing instances and passes them through `accept`, updating the model, dropping
their archive entries, and releasing them downstream so plugins like
[[developer/dns|dns]] and `checker` learn about the deletes.

**A listing that failed prunes nothing.** An empty answer and a daemon that
would not answer arrive the same way, and one of them means every name in the
project.

`enricher/sweep-end` is raised through `Command`, which enters at the head of
the chain, because everything _before_ here has to see it too. It says a round
has been all the way round; it never says what exists.

## The Numbers

|                 |                                                                               |
| --------------- | ----------------------------------------------------------------------------- |
| `Workers`       | 16, capping reads in flight against one endpoint                              |
| `ReadTimeout`   | 10s, one instance read or one listing                                         |
| `InboxSize`     | 1024, matched to the Incus client's own event channel                         |
| `ReadDelay`     | 5s between the reads of a round - projects, networks and instances alike      |
| `SweepInterval` | 6h between the end of one round and the start of the next                     |
| `StoreInterval` | 5s between writes of what is held, so a start finds at most that stale        |
| `StoreFile`     | file path for the cold store cache (`enricher-cold-store.json`), empty to off |
| `TTL`           | 0 (disabled), duration to keep held entries valid before re-reading           |
| `Metrics`       | false, enables Prometheus counters and gauges                                 |
| repair delay    | 1s, three attempts per key, then one every 5s for as long as it keeps failing |

How stale the fleet gets is `ReadDelay` times how much of it there is, and there
is no safe direction: long is a longer window for a quirk nothing announced,
short is paying for the round and the events at once.

One Incus endpoint can front a hundred machines, so every bound here is
fleet-wide rather than per project.

## Reading Incus Correctly

Lifecycle actions are prefixed with the entity they happened to, and that prefix
is load-bearing: without it a name is a name, a `network-updated` carries one
too, and asking the daemon for an instance called `net0` is the bug that shape
invites.

Networks are read per owning project. A project with `features.networks` owns
its own, so one listing answers for that project alone; otherwise a bridge lives
in `default` and other projects reference it. Owners are found with
`incusutil.IsTrue`, because `"1"` and `"yes"` own their networks too.

## Known Gaps

- **A state read that fails fails the event**, scheduling a fast retry without
  updating the model; a running instance holding no address (e.g. pending DHCP
  lease) records as address-less and is also retried.
