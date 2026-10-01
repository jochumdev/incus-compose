---
date: 2026-08-28T01:33:50Z
dateCreated: 2026-08-14T11:46:35Z
leafwiki_id: I7ZeYt8vgz
leafwiki_title: log
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-08-28T01:33:50Z"
leafwiki_creator_id: system
leafwiki_last_author_id: system
---

# log

Prints every event that walks past it.

## Why Drops Travel The Chain

It is the reason a superseded event is handed on marked rather than taken out of
the chain: wherever this sits, it sees what was dropped and by whom.

Which makes it the one plugin worth listing **more than once** - before
[[developer/ievent/debounce|debounce]] it reports what Incus sent, and after
[[developer/ievent/dns|dns]] it reports what became of each one. A line per
position is how you see ordering and what a position cost, and it is noise
otherwise, which is why `--trace` adds positions rather than raising a level.

Listing it multiple times means separate constructions, not one value listed
repeatedly - that would have `Setup` called on it multiple times and a later
successor overwrite the earlier one.

`Handle` never guards on `Err` the way an acting plugin does: a dropped or
failed event is exactly what this exists to show, so gating on it would defeat
the point of being here multiple times. The `age` field it prints is time since
the source decoded the event, not time since this position saw it - so comparing
positions gives you a position's cost by subtracting one `age` from the other,
not by a stopwatch this plugin starts itself.

## It Names The Position, Not The Plugin

`Name()` returns `log/<position>` (such as `log/arrival`, `log/received`,
`log/enriched`, or `log/served`). The chain is a call stack and the same event
walks every position, so without it a burst reads as the same line repeated. An
unnamed one is just `log`, which is the right answer for a chain with only one.

## No Inbox, Alone Among The Plugins

An observer that buffered would report the chain in an order the chain did not
run in. What it costs to stay synchronous is one formatted line on the caller's
goroutine - the same goroutine that is about to do a great deal more than that.

## A Failure Is Never Quiet

`Level` defaults to `Debug`, so carrying this position in a chain costs one call
and nothing else until the handler is opened up - the same economy as having no
inbox. A failed event ignores that setting: it never prints below `Warn`,
because a failure is not routine just because nobody turned the volume up for
it.

## The CoreDNS Hook

`Hook`, in `coredns.go`, is separate machinery riding in the same package: it
routes CoreDNS's own logger into slog's default handler, called once before
anything logs. `plugin/pkg/log` writes everything through the standard library
logger and nothing else, every level a `golog.Print` behind a `"[LEVEL] "`
prefix, so the whole hook-up is a `writer` that reads that prefix back off -
`FATAL` folds into `Error` because slog has nothing above it and `clog.Fatal`
calls `os.Exit` itself, so nothing downstream needs to react further.
`golog.SetFlags(0)` turns off the standard logger's own timestamp and file
position, because adding those is slog's job now, not the source library's. An
unprefixed line still gets logged, at `Info`, since it is someone else's message
on the same std logger and still belongs. The `"plugin/<name>: "` prefix that
`clog.NewWithPlugin` adds is lifted into a `plugin` field instead of staying
three words of text, so it is something to filter on rather than parse. `Hook`
takes a level because `clog` drops its own debug output before the writer ever
sees it - asking for `slog.LevelDebug` also has to flip `clog.D` on, or debug
lines never arrive to be routed at all. `writer.Write` always reports back the
full length it was given, even though nothing is written to any file or socket,
because the standard logger checks that returned count against what it handed
over and treats a mismatch as an error.

## What It Does Not Print

It reports **what happened to an event and never what the event found.** The
reads that landed are named; what they read is the business of the plugin that
asked for them, and a log line carrying an instance's addresses is a log line
nobody reads twice.

## It Wants Nothing Of Its Own

It is in the chain, so it sees whatever walks, and causes no read. An action
nobody else wanted never reaches here, which is what keeps this from printing
the whole lifecycle stream.
