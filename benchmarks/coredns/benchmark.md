# Benchmarking

| level         | tool                                        | question                        |
| ------------- | ------------------------------------------- | ------------------------------- |
| in-process    | `just coredns-bench`                        | what does `ServeDNS` cost       |
| on the wire   | `dnsperf`, via `just coredns-verify-dns -b` | what does the server sustain    |
| on the daemon | incusd's own CPU                            | what does the plugin cost Incus |

The in-process number is the plugin. The wire number is the kernel's UDP path
with the plugin somewhere inside it, and it is dominated by the first.

## In-process

Two modules of their own: `benchmarks/coredns/ecs_view` is the engine with no
source in it, `benchmarks/coredns/kubernetes` CoreDNS's plugin as a yardstick.
The yardstick drags in client-go and cannot be tidied - it is pinned to the
`k8s.io/api` packages CoreDNS needs and `@latest` dropped.

```bash
just coredns-bench                                       # the engine
just coredns-bench benchmarks/coredns/kubernetes         # the yardstick
just coredns-bench benchmarks/coredns/ecs_view -count 5
just coredns-bench-check                                 # compile and lint both
```

First argument is the module, everything after it goes to `go test`.

**A published row is a `-count 5` median.** The 100-replica shape has swung 30%
between runs that changed no code, where every other row holds within 1%.

### The shape

`zones` x `replicas`, one query for a service name fanning out to every replica:
one project per zone with its own /24 pair and hosts sharing a `web` name,
against one namespace per zone and the endpoints of a headless service. Each
benchmark checks its answer before timing it, so a run that turned into an
NXDOMAIN measurement fails rather than reporting a fast number.

### Results

AMD Ryzen 9 5900X, go1.26, `-count 5` medians. `ns/op` is one query.

| zones | replicas | echo=off | echo=on | metrics=on |
| ----- | -------- | -------- | ------- | ---------- |
| 1     | 1        | 228.9    | 295.0   | 304.0      |
| 1     | 100      | 1764     | 1823    | 1857       |
| 50    | 1        | 245.3    | 308.8   | 319.2      |
| 50    | 20       | 557.1    | 615.6   | 625.9      |
| 500   | 1        | 252.9    | 312.8   | 327.3      |
| 500   | 20       | 540.3    | 598.1   | 618.5      |

168 B and 2 allocations with `echo_subnet` off, 184 B and 4 with it on, at every
shape. `metrics=on` runs with echo off, so it is the counter alone.

| option        | cost          | allocations |
| ------------- | ------------- | ----------- |
| `echo_subnet` | +58 to +66 ns | +2          |
| `--metrics`   | +69 to +78 ns | none        |

**Both cost a constant** - one option and one counter per reply however many
records are in it - so the percentage, 3% to 33%, is only the baseline moving.
Quote the absolute. The counter is the dearer of the two, `WithLabelValues`
hashing the label slice and looking it up.

### Against kubernetes

Not re-measured: it is CoreDNS's plugin, so it moves when CoreDNS does. The
columns are not from one run - orders of magnitude, not a percentage.

| zones | replicas | ecs_view | kubernetes | ecs_view allocs | kubernetes allocs |
| ----- | -------- | -------- | ---------- | --------------- | ----------------- |
| 1     | 1        | 228.9    | 1935       | 2               | 26                |
| 1     | 100      | 1764     | 53405      | 2               | 445               |
| 50    | 1        | 245.3    | 7380       | 2               | 173               |
| 50    | 20       | 557.1    | 18796      | 2               | 264               |
| 500   | 1        | 252.9    | 54175      | 2               | 1523              |
| 500   | 20       | 540.3    | 64900      | 2               | 1614              |

- **Zone count is free for ecs_view, linear for kubernetes.**
  `Snapshot.MatchZone` does a map lookup per label; `plugin.Zones.Matches` scans
  every zone calling `dns.IsSubDomain`, allocating twice each. 1 to 500 zones
  costs ecs_view 24 ns and kubernetes 52 us.
- **Allocations do not grow for ecs_view**, answers being precomputed per view
  at event time. The kubernetes plugin builds `msg.Service` values per query, so
  its count tracks the answer size.

A yardstick, not a verdict: ecs_view answers 2 records per replica to kubernetes'
1 and decodes a client subnet every query, while kubernetes carries pods, SRV,
autopath and federation through the same path.

## On the wire

```bash
export INCUS_REMOTE=ict-daily

just fleet flat up 160 20                     # or: just fleet nested
just coredns-compose up -d --build
just coredns-verify-dns -b -- -c 50 -q 150 -l 30
```

**Both ends run inside the daemon**, on one bridge - the wire the fleet's own
clients use. Driving the generator from the host measures the gap to the host.

Configuration is environment, and every script prints what it resolved: `coredns
run --help` and [`compose.yaml`](../../cmd/coredns/compose.yaml) for the server,
`just coredns-verify-dns --help` for the runner. Four decide whether a run means
anything:

|                          |                                                                                          |
| ------------------------ | ---------------------------------------------------------------------------------------- |
| `INCUS_REMOTE`           | points fleet and server at one daemon; wrong looks exactly like a plugin serving nothing |
| `COREDNS_FORWARD`        | never for a benchmark - it turns unmatched names into upstream traffic                   |
| `COREDNS_PROJECT_MARKER` | must match what `just fleet` stamps, `user.coredns.stress`                               |
| `COREDNS_SUBNET`         | drawn from the checkout path, so two checkouts get different /24s and run side by side   |

### Seven ways to measure the wrong thing

**Ask as somebody who can see the answer.** A querier on no known network is
refused, so from loopback every query is a fast NXDOMAIN. `-b` refuses to start
unless a real name answers under the subnet it asserts.

**100% NOERROR, or the number means nothing.** `BENCH_ALL=1` at 160 projects
returns 0.63% NOERROR against 99.37% NXDOMAIN - one view sees one project, so
99% of it times the fail-closed path. A denial carries an SOA at 144 B where an
answer here is 100 B.

**Check the query set is the one you think.** `verify-dns` caches
`/tmp/queries.txt` in the debug instance and reuses it, so a set curated once
with `NAMES=` keeps being the run afterwards. That cost an evening: a set pinned
to three host names dropped the service name, and two builds were compared
answering 1 record against 1.5. `REFRESH=1` rebuilds it; read `benchmarking 4
queries` and `response 100` before trusting anything.

**SERVFAIL is not NXDOMAIN.** An invisible name in a live zone is NXDOMAIN from
the snapshot; a name whose zone does not exist falls through the engine to
nothing. A fleet with empty projects measures that path instead.

**Watch what the generator is capped at.** Throughput cannot exceed
`outstanding / latency`. Five runs at `-q 100` all sat between 95.9 and 96.7 of
100 outstanding, so QPS was just `96 / latency` - and latency varied **7.6%**
with nothing changed.

**Only the sweep separates the cases.** A run near `-q/latency` may be
generator-bound or a server fast enough that 100 outstanding nearly saturates it.

**Wait for the fleet to converge.** `just coredns-cold-stats` first: converged
dual-stack reads `IPv4 == IPv6 == instances`. Short of that is instances without
a lease, and nothing announces one - it waits for the next pass,
`--sweep-interval`, 30 minutes. Restarting coredns forces one.

### Finding the ceiling

Sweep `-q` until throughput stops rising, take the highest point with zero loss.
2026-08-13, dnsperf 2.14.1, 160 projects / 480 instances, PTR served,
`--metrics`, `--pprof` and `--debug` off, `GOMAXPROCS=4` from the `cpus: "4.0"`
quota, four names of which one fans out to three replicas. Every row 100%
NOERROR:

| `-q` | QPS         | avg latency | lost  | of `-q/latency` |
| ---- | ----------- | ----------- | ----- | --------------- |
| 100  | **341,981** | 0.281 ms    | 0     | 96%             |
| 150  | 338,978     | 0.430 ms    | 0     | 97%             |
| 200  | 334,585     | 0.529 ms    | 109   | 88%             |
| 500  | 335,317     | 0.602 ms    | 1,853 | 40%             |

**~340k QPS, no loss.** Throughput does not rise from `-q` 100 to 150 - it falls
0.9%, inside the noise - and losses start at 200, so the plateau is the server's
own rate rather than the generator's. **A run with losses is not a
measurement**, exactly as a run under 100% NOERROR is not one.

**The client socket count is not it.** `-c 100` against `-c 50` at the same
`-q 150` returns 340,859 against 338,978 - 0.6%, nothing. Doubling the sockets
offering the load does not move the plateau, which is what you would expect of a
server reading one socket of its own.

An earlier sitting on the same build swept to 326,194 / 333,964 / 331,089 /
331,405 across the same four points: every row 1-5% lower, with the loss counts
reproducing to within 3% (108 against 109 at `-q 200`, 1,899 against 1,853 at
500). The plateau moves between sittings, the shape does not.

Read the configuration off the startup line rather than off the command that
started it - `plugin=dns` and `plugin=http` both print theirs:

```
plugin=dns  config="{... EchoSubnet:false Metrics:false ...}"
plugin=http config="{Listen::9153 Silence:0s Metrics:false Pprof:false}"
```

### Where the time goes

From `--pprof`, `/debug/pprof/profile?seconds=20` during a saturating run. Per
query, 6.94 us of CPU:

|                                       | share    | per query   |
| ------------------------------------- | -------- | ----------- |
| syscalls (`Syscall6`, flat)           | 34.1%    | 2.37 us     |
| UDP write path to `sendmsg`           | 36.6%    | 2.54 us     |
| UDP read path to `ReadUDP`            | 15.3%    | 1.06 us     |
| GC                                    | ~17%     | 1.20 us     |
| scheduler                             | ~8%      | 0.56 us     |
| miekg pack/unpack of names            | ~6%      | 0.42 us     |
| **the engine, socket write excluded** | **5.1%** | **0.36 us** |

The last row is `Answer` minus `write`; the in-process table says 0.33 us for
the same shape. **Two independent instruments agreeing to 10%** - so four fifths
of a query is the kernel's UDP path rather than anything here, and
`adapter.ServeDNS` above the engine is 1.15%, ~80 ns.

The wire therefore answers "what does the server sustain" and nothing finer: 74
ns against 430,000 ns is invisible, which is why toggling `--metrics` across
three runs landed inside the rig's own 1.9% spread.

### Absolute wire numbers do not survive the host

Take them from one machine in one sitting or not at all. A build from eight days
earlier, stood up beside the current one on the same host and fleet, swept to
**310,257** where its own recorded ceiling was **363,706** - same code, 15%
down, same quota, same Go.

### Does it scale, and with what

Ruled out: `LookupNet`, view count and `byView`, a build-time loop; GC, whose
live heap does not grow with the fleet; the engine, at 5.1% of CPU.

**The ceiling is not CPU** - a saturating run uses 243% of the 400% quota. The
candidate is serialization, and the visible place is the single UDP socket:
`miekg/dns` reads it in one loop per server and CoreDNS defaults to
`numSockets = 1` too. CoreDNS's `multisocket` opens several under
`SO_REUSEPORT`; this build has no equivalent. **Nobody should write "it scales"
until that has been run.**

340k QPS across 480 instances is 708 per second per instance, sustained.

### Memory

**Live heap is 6 MB** for 480 instances, 160 views, 957 addresses and 479 zones,
unmoved by 3.6M queries: records are shared by reference across views, so a
growing fleet does not grow the heap proportionally. **Do not measure it with
RSS** - against the same fleet and a 6 MB live heap, RSS has read 37 to 48 MiB.

cgroup2 charges anonymous memory plus page cache, including 28.9 MB of the
binary's own text, and there is no swap by default - so a too-tight limit shows
up as latency from text being re-faulted before it shows up as a kill. The peak
sits near 60-70 MiB: 100 MiB is comfortable, 50 MiB is not.
`container/coredns/entrypoint.sh` derives `GOMEMLIMIT` from the cgroup limit at
75%, `GOMEMLIMIT_PERCENT` to tune, and says so as its first line - `GOMEMLIMIT=
78643200B (75% of the 104857600 B cgroup limit)` under `memory: 100M`. Confirm
with `memory.peak`.

## On the daemon

The query path never reaches Incus: with CoreDNS saturated, incusd stays flat.
At rest the plugin costs one idle event stream plus one whole-fleet pass per
`--sweep-interval` - 30 minutes, not jittered. A pass is one `GetProjects`, one
`GetNetworks` per project owning networks, one `GetInstances` per served project
and one `GetInstanceState` per instance:

```
requests/sec ~= (2 + 2 x projects + instances) / 1800
```

160 projects x 3 instances is ~0.45 requests/second, as a burst every half hour.

```bash
cpu() { awk -v t="$(getconf CLK_TCK)" '{print ($14+$15)/t}' /proc/"$(pgrep -x incusd)"/stat; }
a=$(cpu); sleep 60; b=$(cpu); awk "BEGIN{print $b-$a}"
```

- **A read that keeps failing** marks the instance dirty and pulls the next pass
  in to `dirtyDelay`, 5 s - 360 times the resting rate, without backing off.
- **Reading the fleet from a script.** `incus network list` makes incusd walk
  every instance to compute `UsedBy`; `just coredns-verify-dns` caches its
  targets for exactly this reason.

## Fleets

| topology | per project                                       | views                |
| -------- | ------------------------------------------------- | -------------------- |
| `flat`   | 3 hosts on one network sharing a `web` name       | one per **project**  |
| `nested` | 1 gateway on every network, 2 workers on one each | one per **instance** |

`just fleet <flat|nested> up PROJECTS [CONCURRENCY]`. `nested` is the
pathological case for `byView` - no two instances share a network set, so
nothing collapses. `flat` is what the design assumes and the one with the
service fan-out the query set leans on. Both give each project
`features.networks` and an OVN network; `OVN=0` goes back to bridges.

`just coredns-cold-stats` prints the fleet as the plugin sees it, from what was
last published, so it answers while Incus is unreachable. Three lines are
warnings:

- **claimed twice** - addresses more than one instance holds, so those clients
  are refused rather than guessed at. A few at 160 projects is the birthday
  collision on Incus's random `10.x.y.0/24`, not a fault; the metric is
  `coredns_ecs_view_unidentified_clients_total`.
- **IPv4 well below instances** - not converged, and a run now measures a
  smaller answer set than it reports.
- **serials still at 1** - published once and never changed. All of them at 1
  after a restart means the serials did not survive and every secondary
  re-transfers.

## See Also

- [coredns.md](../../docs/root/coredns.md) - the binary, its flags and what it serves
- [ievent/architecture/index.md](../../docs/root/ievent/architecture/index.md) - the chain and the positions in it
- [ievent/architecture/dns.md](../../docs/root/ievent/architecture/dns.md) - the engine, and how a view is built and answered from
- [ievent/architecture/enricher.md](../../docs/root/ievent/architecture/enricher.md) - the pass, and what it asks Incus for
