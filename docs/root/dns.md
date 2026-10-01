---
date: 2026-09-09T11:37:56.000Z
dateCreated: 2026-08-14T11:46:35Z
tags: []
leafwiki_id: j-kkPt8Dgz
leafwiki_title: DNS (ic-dns)
leafwiki_created_at: "2026-08-14T11:46:35Z"
leafwiki_updated_at: "2026-09-09T11:37:56.000000000Z"
leafwiki_creator_id: system
leafwiki_last_author_id: public-editor
---

# DNS

DNS for an Incus fleet, sourced from the Incus API and kept current by its event
stream. One zone per project, no zone files, no reload.

**Answers depend on who is asking.** An instance resolves only the names it
shares a network with, and the answer holds only their addresses on those
networks. A name it cannot reach is NXDOMAIN rather than an address that times
out.

## Why The Answer Differs Per Querier

An Incus fleet is not one flat network. A project has its own bridges, an
instance sits on some of them, and two instances that share none cannot reach
each other at all. A single answer for a name is therefore wrong for somebody:
it either hands out an address the client cannot route to, or it names a host it
has no business seeing.

So a querier is placed on the set of networks it sits on, and a multi-homed host
is answered on the wire it shares with the client.

It fails closed. A querier that lands on no known network is refused, and a name
that exists but is invisible answers NXDOMAIN rather than NODATA, so response
codes leak nothing about what else is out there.

### How The Querier Is Identified

Two ways, in this order:

1. **[EDNS0 Client Subnet](https://datatracker.ietf.org/doc/html/rfc7871)**,
   when the query carries one. Incus's own dnsmasq fills it in with
   `add-subnet=32,128`.
2. **The query's source address**, when it does not - which is the case when an
   instance's `resolv.conf` names this server directly.

The order matters: whatever forwarded the query has already replaced the source
address with its own, so a client subnet is the only truth when one is present.

Worth knowing: **a client subnet is asserted by whoever sends it.** Anything
that can reach the server can claim to be any client. Reaching the server is the
real boundary, which is why this belongs on a network only its clients are on.

## Names An Instance Answers To

| Name                                   | When                                                              |
| -------------------------------------- | ----------------------------------------------------------------- |
| `<instance>.<zone>`                    | always                                                            |
| `<service>.<zone>`                     | `user.label.incus-compose.service`, else `user.label.dns.service` |
| `<alias>.<zone>`, or any absolute name | `user.label.dns.aliases`, as a CNAME onto `<instance>.<zone>`     |
| `in-addr.arpa` / `ip6.arpa`            | every address inside its own network's prefixes                   |

`<zone>` is `<project>.<suffix>`, `--suffix` being `incus` unless changed.

A service name resolves to every replica carrying it, which is what makes a
scaled service resolvable at all. A PTR names the instance and never the
service: a reverse lookup wants the one name that names this host alone.

Only subnets holding an instance are answered for. A reverse query into a bridge
nothing sits on falls through to `--forward` rather than answering NXDOMAIN for
a fleet this server knows nothing about.

## Labels That Configure DNS

Everything lives under `user.label.dns.*`. Nothing has to be labelled for this
to work.

| Key       | Set on   | Meaning                                                                                               |
| --------- | -------- | ----------------------------------------------------------------------------------------------------- |
| `scope`   | project  | opts the project in - see [Which Projects Are Served](#which-projects-are-served)                     |
| `zone`    | project  | the full zone name, replacing `<project>.<suffix>`                                                    |
| `ns`      | project  | comma-separated NS names for the zone - see [Naming Your Own NS Servers](#naming-your-own-ns-servers) |
| `service` | instance | an extra name every replica carrying it answers to                                                    |
| `aliases` | instance | comma-separated extra names, each a CNAME                                                             |

```bash
incus project set shop user.label.dns.scope=global
incus project set shop user.label.dns.zone=shop.example.org
incus config set web user.label.dns.service=api --project shop
incus config set web user.label.dns.aliases=alias1,me.example.com. --project shop
```

A project's keys come from `incus project set` and never from its default
profile, whose keys every instance already carries expanded - so a project-wide
setting would have nowhere of its own to live.

A project's labels are defaults its instances override, except `aliases`, which
does not inherit at all: one name claimed by every instance in a project is a
collision rather than a setting.

An empty value turns a label off again rather than reading as a blank name,
which is how an instance escapes what a profile handed it.

`user.label.incus-compose.service` is the one key outside our namespace that is
read, and it wins over `user.label.dns.service`: a compose fleet is named by the
compose file that owns it, so a stack resolves without being labelled twice.

### Naming Your Own NS Servers

Without `ns`, the zone's apex NS answers with a placeholder name nothing
resolves - harmless for a stub resolver, since it never reads an NS record, but
wrong the moment the zone is transferred or delegated: a secondary publishes
whatever NS set it was sent, and a resolver reaching this server by delegation
has to be able to find it.

```bash
incus project set shop user.label.dns.ns=ns1.example.org.,ns2.example.org.
```

Same rule as `aliases`: a trailing dot is absolute, anything else is relative to
the zone. Two projects resolving to one zone name union their `ns` sets, the
same way they union into one zone.

Naming this server itself only works where it is reachable from wherever the
zone is answered - true for a routed address (BGP-announced, for instance), not
for one behind a private bridge. There is no discovery: whoever transfers or
delegates a zone has to already know its name servers, same as configuring any
other authoritative DNS.

### Aliases Add Extra Names

A name ending in a dot is absolute; anything else is relative to the instance's
zone - the rule a zone file uses. `web` in project `shop` with the label above
answers to `alias1.shop.incus.` and to `me.example.com.`, both as a CNAME onto
`web.shop.incus.` with the address behind it in the same reply.

An alias is visible exactly where its instance is, so it is no way around the
per-querier filtering.

`example.com.` is not a zone any project serves, so one is invented to hold that
one name - and it claims **only** the names actually aliased. `www.example.com.`
still goes to `--forward`, so aliasing into a domain does not take the rest of
it away from the fleet.

An alias is refused, silently, in three cases: a name an instance or service
already answers to, a name two instances both claim, and a zone apex. Each would
need a CNAME sharing a name with other records, which is not a record set a
resolver may be handed.

## Which Projects Are Served

Three ways, in this order:

1. `--project` names them outright, and costs no marker lookup.
2. Otherwise a project opts in by carrying `--project-marker`, which is
   `user.label.dns.scope=global` unless changed. A bare key means `KEY=true`.
3. With neither, every project the certificate can see is served - the only
   answer that works on a plain Incus, which stamps nothing.

```bash
incus project set shop user.label.dns.scope=global   # read from now on
incus project unset shop user.label.dns.scope        # no longer read
```

Un-scoping stops the project being **read**. It does not retire what is already
held: a record goes when its instance is deleted or stops, and nothing prunes on
a project merely ceasing to appear. Stop or delete the instances to take their
names out of DNS.

## Flags And Environment Variables

Every flag has an environment variable. Defaults are what a deployment sees;
each plugin's own internals are not configurable.

### Connecting To Incus

| Flag                             | Env                                             | Default              |                                                                             |
| -------------------------------- | ----------------------------------------------- | -------------------- | --------------------------------------------------------------------------- |
| `--incus`                        | `INCUS_COMPOSE_DNS_INCUS`                       |                      | URL of the Incus API                                                        |
| `--token`                        | `INCUS_COMPOSE_DNS_TOKEN`                       |                      | one-time trust token; a token file under `--secrets-dir` is read when empty |
| `--data-dir`                     | `INCUS_COMPOSE_DNS_DATA_DIR`                    | `/var/lib/dns-incus` | the enrolled certificate and what was last served; empty keeps neither      |
| `--secrets-dir`                  | `INCUS_COMPOSE_DNS_SECRETS_DIR`                 | `/run/secrets`       | tmpfs directory holding the trust token                                     |
| `--client-cert` / `--client-key` | `INCUS_COMPOSE_DNS_CLIENT_CERT` / `_KEY`        |                      | present a certificate instead of enrolling                                  |
| `--restricted`                   | `INCUS_COMPOSE_DNS_RESTRICTED`                  | off                  | enroll confined to `--project`                                              |
| `--remote` / `--use-remote`      | `INCUS_REMOTE` / `INCUS_COMPOSE_DNS_USE_REMOTE` |                      | connect as a remote from the Incus CLI configuration                        |

### Choosing What To Serve

| Flag               | Env                                | Default                       |                                         |
| ------------------ | ---------------------------------- | ----------------------------- | --------------------------------------- |
| `--suffix`         | `INCUS_COMPOSE_DNS_SUFFIX`         | `incus`                       | TLD every project's zone is built under |
| `--project`        | `INCUS_COMPOSE_DNS_PROJECTS`       |                               | project(s) to serve                     |
| `--project-marker` | `INCUS_COMPOSE_DNS_PROJECT_MARKER` | `user.label.dns.scope=global` | what opts a project in                  |

### Where To Listen

| Flag        | Env                         | Default |                                                           |
| ----------- | --------------------------- | ------- | --------------------------------------------------------- |
| `--listen`  | `INCUS_COMPOSE_DNS_LISTEN`  | `:53`   | DNS, UDP and TCP both                                     |
| `--http`    | `INCUS_COMPOSE_DNS_HTTP`    | `:9153` | `/metrics`, `/health`, `/ready`; empty disables           |
| `--forward` | `INCUS_COMPOSE_DNS_FORWARD` |         | upstream(s) for names we do not serve; empty refuses them |

### Tuning The Chain

| Flag                    | Env                                     | Default |                                                                                                                                                                      |
| ----------------------- | --------------------------------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--ttl`                 | `INCUS_COMPOSE_DNS_TTL`                 | `5`     | seconds a record is served for, up to 3600                                                                                                                           |
| `--debounce-window`     | `INCUS_COMPOSE_DNS_DEBOUNCE_WINDOW`     | `250ms` | quiet period before the last of a burst is handed on                                                                                                                 |
| `--workers`             | `INCUS_COMPOSE_DNS_WORKERS`             | `16`    | Incus reads in flight at once                                                                                                                                        |
| `--read-timeout`        | `INCUS_COMPOSE_DNS_READ_TIMEOUT`        | `10s`   | budget for one read of the daemon                                                                                                                                    |
| `--sweep-project-delay` | `INCUS_COMPOSE_DNS_SWEEP_PROJECT_DELAY` | `30s`   | gap between one project of a round and the next                                                                                                                      |
| `--sweep-read-delay`    | `INCUS_COMPOSE_DNS_SWEEP_READ_DELAY`    | `5s`    | gap between the reads inside one project                                                                                                                             |
| `--echo-subnet`         | `INCUS_COMPOSE_DNS_ECHO_SUBNET`         | off     | echo the RFC 7871 client subnet back                                                                                                                                 |
| `--exclude`             | `INCUS_COMPOSE_DNS_EXCLUDE`             |         | chain position(s) to leave out                                                                                                                                       |
| `--log`                 | `INCUS_COMPOSE_DNS_LOG`                 |         | level the chain's log positions and the process itself print at: `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`; empty leaves the positions out and the process at `INFO` |

`--ttl` is short on purpose. A fleet moves, and a resolver that cached an
address for an hour is one handing out an address that has been reassigned.

`--exclude` takes `debounce`, `http` and the log positions. The enricher and
`dns` may not go: without the enricher nothing is ever read, and the process
would start, answer, and serve nothing.

## HTTP Endpoints

| Path       |                                                           |
| ---------- | --------------------------------------------------------- |
| `/metrics` | Prometheus, every plugin's                                |
| `/health`  | the stream is connected, and something walked recently    |
| `/ready`   | the fleet has been read whole, and the stream is still up |

The two answer different questions on purpose. The only sensible response to
`/health` failing is a restart, and a restart does not fix an Incus that is
down - it throws away everything held and answers nothing until the fleet has
been re-read. So **a lost stream is unready, never unhealthy.**

A round does not make it unready. One is always running and the server answers
from what it published last throughout, so `/ready` stays up and nothing pulls
the process out of rotation for it.

The fleet is read a name at a time rather than all at once, so those two delays
are what decides how long a change nothing announced goes unnoticed: roughly
`--sweep-read-delay` times how many instances there are. There is no safe
direction. Long leaves a wider window for a quirk; short pays for the round and
the event stream at the same time. The first round after a start or a reconnect
ignores both and runs flat out, because nothing is served until it lands.

## Restarts And Cold Start

With `--data-dir` set, what was last served is on disk with its zone serials, so
a restart answers before it has reached Incus - and a secondary polling the SOA
does not see the serial go backwards. Restored records are served with a short
TTL until every project has been re-read, and they are retired only when Incus
says they are gone, never on a timer.

## Zone Serials

A zone's serial moves when that zone's records move, and at no other time.
Republishing identical records leaves it alone, so a secondary re-transfers on a
real change and on nothing else.

## Management Commands

The `dns` command group manages the `ic-dns` sidecar directly:

| Subcommand        | Description                                           |
| ----------------- | ----------------------------------------------------- |
| `logs [--follow]` | Stream the ic-dns container log                       |
| `status`          | Print the status, IP addresses, and metrics of ic-dns |
| `up`              | Create or recreate the ic-dns sidecar                 |
| `down [--force]`  | Stop and remove the ic-dns sidecar                    |

### dns status

`dns status` queries the sidecar's `/ready` HTTP endpoint (and `/metrics` if
requested) via `incus port-forward` and reports its status and IP addresses.

```bash
incus-compose dns status [options]
```

| Option      | Description                                     | Default | Environment Variable               |
| ----------- | ----------------------------------------------- | ------- | ---------------------------------- |
| `--format`  | Output format: `text` or `json`                 | `text`  | `INCUS_COMPOSE_DNS_STATUS_FORMAT`  |
| `--port`    | HTTP port to query on the sidecar               | `9153`  | `INCUS_COMPOSE_DNS_HTTP_PORT`      |
| `--metrics` | Include Prometheus metrics in the status report | `false` | `INCUS_COMPOSE_DNS_STATUS_METRICS` |

Text output format (default):

```text
Status: ready
IPv4: 10.90.190.53
IPv6: fd42::53
```

When `--metrics` is passed:

```text
Status: ready
IPv4: 10.90.190.53
IPv6: fd42::53
Metrics:
# HELP ...
...
```

JSON output format (`--format json`):

```json
{
  "status": "ready",
  "ipv4": "10.90.190.53",
  "ipv6": "fd42::53"
}
```

When `--metrics` is passed with `--format json`:

```json
{
  "status": "ready",
  "ipv4": "10.90.190.53",
  "ipv6": "fd42::53",
  "metrics": "# HELP ...\n..."
}
```

## Further Reading

[[developer/ievent|ievent]] is the plugin chain this binary is composed from.
