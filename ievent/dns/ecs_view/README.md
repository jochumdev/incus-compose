# ecs_view

## Name

*ecs_view* - serves DNS records filtered by who is asking.

## Description

*ecs_view* holds an in-memory record set and answers each query with only what
that querier may see. It sources nothing itself: another plugin in the same
server block builds the records and publishes them here.

A querier is placed on a set of networks, and sees only the names reachable on
those networks, with only their addresses there. Two clients on different
networks asking the same name get different answers, and a multi-homed host is
always answered on the network it shares with the client - so every answer holds
an address the querier can actually reach.

The querier is identified two ways, in this order:

1. The [EDNS0 Client Subnet](https://datatracker.ietf.org/doc/html/rfc7871)
   option, which a forwarding resolver supplies. dnsmasq does it with
   `add-subnet=32,128`.
2. The query's source address, when no client subnet is present.

The order matters. A resolver that forwards on a client's behalf puts its own
address on the packet, so the client subnet is the only truth when one is there.
Point a client's `resolv.conf` straight at CoreDNS and nothing relays, so the
source address is the querier.

**It fails closed.** A querier that lands on no known network answers NXDOMAIN.
A name that exists but is invisible answers NXDOMAIN too, rather than NODATA, so
response codes leak no visibility.

## Syntax

~~~
ecs_view {
    echo_subnet
}
~~~

- `echo_subnet` echoes the EDNS0 Client Subnet option on the reply, with a scope.
  Off by default; see [Client Subnet Echo](#client-subnet-echo).

Everything it serves comes from the sources in the same server block, and each of
those carries its own configuration, so `ecs_view` on its own is the usual form.

Use `.` as the server block rather than a fixed zone. A source can name its zones
at runtime, so the zone list is known only then. Names outside every known zone
fall through to the next plugin.

## Sources

A source is any plugin in the same server block that implements:

~~~ go
// Source is a plugin that feeds views to ecs_view.
type Source interface {
    // SetSink is called once at startup, before the source produces anything.
    SetSink(Sink)
}
~~~

*ecs_view* finds them by walking the server block at startup, the same way
*ready* and *metadata* find theirs. A source then publishes whole snapshots:

~~~ go
// Sink is where a source publishes.
type Sink interface {
    // Replace swaps everything served for snap, which holds every zone the
    // source serves.
    Replace(snap *Snapshot)
    // SetHealthy says whether the source's data is currently fresh.
    SetHealthy(healthy bool)
}
~~~

Publishing is `Replace` semantics: a zone that stops appearing is deleted by
absence. The snapshot handed over is finished - serials stamped, records rendered -
and swapping it in is a pointer store. *ecs_view* derives nothing and accumulates
nothing; it is a read-only view onto what the source published.

[*incus*](../incus/README.md) is that source.

## Partial Zones

A zone may be marked `Fallthrough`, which makes it claim the names in it and
nothing else: a query for a name it does not hold goes to the next plugin
instead of answering NXDOMAIN.

It is for a zone a source invented rather than one anybody asked to serve - the
case being an alias like `me.example.com.` on an instance, which has to be
answered from *some* zone. Claiming `example.com.` outright would have that one
name blackhole every other name in the domain, for every querier.

Held is the test, not visible. A name the zone does hold is answered under the
usual rule, invisible included, so falling through cannot be used to tell a
hidden name from an absent one. The apex of such a zone is not held either, so
no SOA is synthesized for a domain this server does not serve.

## Aliases

A name may answer with a CNAME onto another, which `RenderCName` builds. The
chase is done when the snapshot is built rather than per query: the CNAME goes
into the A and AAAA sets as well as its own, canonical name first, so answering
through one is the same three map lookups as answering anything else.

One CNAME value serves every network the name is reachable on, since it is the
one record that does not vary by network. `Gather` relies on that identity to
join a multi-homed name without handing the querier two copies of it.

Nothing here knows what an alias is. A source decides which names get one and
how far they reach, by rendering them under the network keys it chooses - which
is what makes them obey the same visibility rule as everything else.

## Zone Serials

Each zone carries a serial that moves when that zone's records move, and at no
other time. Republishing identical records leaves it alone, so a secondary polling
the SOA re-transfers on a real change and on nothing else.

The source stamps it, because the source is the only thing that knows what it
published last. Deriving one from the clock, as Incus's own network zones do,
re-transfers on every publish whether or not anything moved.

A restart is the same question over a longer gap, and it is the source's to
answer too: this plugin holds nothing across one. *incus* carries its serials in
`data_dir`; a source that does not restarts every zone at 1.

## Client Subnet Echo

With `echo_subnet`, a query that carries a client subnet gets one back, as
[RFC 7871](https://datatracker.ietf.org/doc/html/rfc7871) requires: FAMILY,
SOURCE PREFIX-LENGTH and ADDRESS copied, and SCOPE PREFIX-LENGTH saying how
specific the answer is.

| answer | scope |
| --- | --- |
| A, AAAA, PTR, NODATA, the fail-closed NXDOMAIN | 32 / 128 |
| apex SOA and NS | 0 |

Scope is the only signal a cache has for how widely an answer may be shared, and
no option at all reads as "this answer was not tailored". A view-dependent answer
is scoped to the querier's whole address, so a conformant cache keys it per
client and shares it with nobody; the apex is identical for every querier and
says so.

A query with no client subnet gets no option back. The source address identified
the querier, and nothing asserted a subnet to be answered about.

**Turn it on when an ECS-aware cache sits in front of CoreDNS**, and leave it off
otherwise. Nothing else reads the field, while putting the option on the reply
and packing it costs the query path allocations on every query that carries a
subnet - which, where a resolver forwards, is all of them.

Without it the answers are the same answers: the option decides who is asking
either way, and only the reply is silent about how far it travels.

## TTL and Staleness

Records are rendered by the source with the TTL it was configured for, and the
snapshot carries it. While the source reports itself unhealthy, answers are
clamped to 5 seconds so stale records expire fast once a client can reach a
healthy server.

The clamp is deliberately coarse: it applies to every zone, not only the ones
affected. It errs toward shorter TTLs.

## Metrics

With the *prometheus* plugin enabled:

- `coredns_ecs_view_requests_total{server,result}` - requests by result, where
  result is `success`, `nodata`, `nxdomain` or `denied`.
- `coredns_ecs_view_unidentified_clients_total{server}` - requests refused
  because the querier could not be placed on any known network.
- `coredns_ecs_view_zones` - zones currently served.
- `coredns_ecs_view_addresses` - addresses indexed for client identification.

An address claimed by more than one host identifies no querier and is refused
here, but detecting the clash belongs to the source - see
`coredns_incus_ambiguous_addresses`.

## Ready

This plugin reports readiness to the *ready* plugin. It turns ready once a source
has published a snapshot and reports itself healthy.

An empty fleet publishes an empty snapshot, which is not the same as never having
published: only one of those is ready.

## Caveats

**`cache` must stay below `ecs_view`.** This plugin writes its own reply and
never calls the next plugin on an in-zone hit, so `cache` never sees an answer
that varies per querier. Putting `cache` first would cache one client's view and
serve it to another, and the scope on the reply would not save it: CoreDNS's
`cache` does not read the client subnet.

**A client subnet is asserted by whoever sends it.** Anything that can reach the
server can claim to be any client by attaching one. Reaching the server is the
real boundary, which is why *ecs_view* belongs on a network only its clients are
on.

## Examples

Serve Incus instances, per querier:

~~~ corefile
.:53 {
    ecs_view
    incus https://10.0.0.1:8443 {
        token_file /run/secrets/token
    }
    forward . 10.0.0.1
    errors
    log
}
~~~

## Also See

[RFC 7871](https://datatracker.ietf.org/doc/html/rfc7871) for EDNS0 Client
Subnet, and [docs/ecs_view.md](../../docs/ecs_view.md) for how the views are
built.
