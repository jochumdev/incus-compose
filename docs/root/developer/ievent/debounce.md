---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
leafwiki_id: hMW6Lt8DR
leafwiki_title: debounce
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# debounce

Collapses a burst of events on one key into two.

## Why It Sits Before The Enricher

It sits first, ahead of [[developer/ievent/enricher|the enricher]], and that is
the whole point. An `instance-updated` storm - somebody editing devices, a
script walking a project - costs two reads rather than one per event. Collapsing
_after_ the enricher would pay for every event in the burst and throw all but
one away.

## Leading And Trailing

The first event of a burst goes at once, so a lone change is never delayed and
the common case costs nothing. The last goes when the key has been quiet for the
window, so whatever the burst settled on is what stands. Everything between them
is superseded.

A burst of one is not reported twice.

A burst that never goes quiet never closes its window, so its trailing event
waits. The leading one already went, which is what makes that survivable: an
instance flapping faster than the window has still been reported once, and the
enricher's round is what reconciles one that stays that way.

## What Gets Held

The key is `project/name`, not the bare name - two projects can reuse a name
without colliding.

Collapsing needs all three: `Err` still nil (nothing has already dropped or
failed this event), the chain `ChainWarm` (the fleet has been read whole at
least once), and `Want.Debounce`. A cold start never collapses - everything
before the first full sweep goes straight through, in order, so what that first
sweep reads is everything Incus sent, not whatever survived a window. An event
already dropped or failed is not held either: it already reports something, so
delaying it buys nothing.

An event that lands for a key with an open window but does not itself qualify to
collapse closes that window first and hands on whatever it held, then goes on
itself - the older event arrived first, so it leaves first.

An event with no name - the source's own, not one Incus sent - carries nothing
to key on and is never held.

## A Full Inbox Also Drops

`Handle` does not block on a full inbox; it drops the event on the spot, marked
and handed straight on, same as a superseded one - so the observers after it
still see it, and see debounce as the reason it is gone.

## What A Command To Finish Does That A Cancelled Context Does Not

A `Command` asking this plugin to finish drains the inbox and closes every open
window, whatever its deadline, before answering: everything still held gets
handed on first, the source only hears back once it has. A cancelled context is
not that - it is an abort, and whatever is still held goes nowhere.

## What May Be Collapsed Is Not Its Decision

It reads `Want.Debounce` off the source's finished table, where **false wins**.
An action that fires once anyway is not worth the window, and an action somebody
needs every one of is not collapsed at all.

This plugin is before the enricher and still gets the whole table, which is the
point of it being the same one for everybody: what may be collapsed is decided
by plugins that all sit after this one. See
[[developer/ievent/index|the contract]] for why the zero value vetoes.

## Why Drops Travel The Chain

It is the plugin that drops on purpose, and the reason a drop is handed on
rather than taken out of the chain: a superseded event walks on marked, so
[[developer/ievent/log|log]] and the observers after it still see every event
Incus sent, and who dropped it.

## Defaults

|             |                                                                                                                         |
| ----------- | ----------------------------------------------------------------------------------------------------------------------- |
| `Window`    | 250ms - short enough that a lone change is not noticeably late, long enough that a scripted burst lands inside one      |
| `InboxSize` | 1024, matched to the Incus client's own event channel: a burst that overruns that was never going to be delivered whole |

It asks the daemon for nothing, so it configures only `Window` and `InboxSize`.
