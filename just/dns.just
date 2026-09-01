# The ic-dns compose stack, and what it is worth asking.

# Drive the ic-dns compose stack. Args go to incus-compose, so `up -d`, `down`,
# `logs` and `incus ...` all work.
dns-compose *args:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Using remote '${INCUS_REMOTE-"local"}':" >&2

    # Values, not directives. There is no Corefile any more: every one of these
    # is a flag of `ic-dns run`, so what goes in is what comes out.
    export DNS_PORT="${DNS_PORT:-53}"
    export PROMETHEUS_PORT="${PROMETHEUS_PORT:-9153}"
    export DNS_TTL="${DNS_TTL:-5}"
    export DNS_FORWARD="${DNS_FORWARD:-}"
    export DNS_PROJECT_MARKER="${DNS_PROJECT_MARKER-user.dns.stress}"

    # Values now rather than directive names. Empty is off, which is what the
    # binary's own defaults are.
    export DNS_LOG="${DNS_LOG:-}"
    export DNS_ECHO_SUBNET="${DNS_ECHO_SUBNET:+true}"

    # Every interface rather than loopback: the scraper is outside. Empty turns
    # /metrics, /health and /ready off with it.
    export DNS_HTTP="${DNS_HTTP-:${PROMETHEUS_PORT}}"

    export DNS_IPV4="${IC_SUBNET}.10"
    export IC_IPV4_GATEWAY="${IC_SUBNET}.1"

    # The identity `incus` itself uses - compose.yaml reads client.crt and
    # client.key out of it, which is what makes this a no-token setup.
    export INCUS_CONF="${INCUS_CONF:-${XDG_CONFIG_HOME:-${HOME}/.config}/incus}"

    [[ -r "${INCUS_CONF}/client.crt" ]] || {
      echo "no client certificate at ${INCUS_CONF}/client.crt" >&2
      exit 1
    }

    remote=$(incus remote get-default)
    export INCUS_ENDPOINT="${INCUS_ENDPOINT:-$(incus remote list -f csv |
      sed 's/ (current)//' |
      awk -F, -v r="${remote}" '$1 == r { print $2; exit }')}"

    # An address, always, and resolved here rather than in the instance. The
    # plugin dials from inside a container, where the daemon's own hostname is
    # the container itself: ict-daily-dev01 lands on 127.0.1.1 there and the
    # dial is refused. A unix:// remote has no address to dial at all.
    listen=$(incus config get core.https_address)

    port="${INCUS_ENDPOINT##*:}"
    [[ "${port}" =~ ^[0-9]+$ ]] || port="${listen##*:}"
    [[ "${port}" =~ ^[0-9]+$ ]] || port=8443

    host="${INCUS_ENDPOINT#*://}"
    host="${host%:*}"
    [[ "${INCUS_ENDPOINT}" == unix://* ]] && host=""

    if [[ -z "${host}" || "${listen}" == "[::]"* || "${listen}" == :* ]]; then
      # [::]:8443 and :8443 both mean every address, which from an instance is
      # the bridge gateway rather than loopback - incusd is on this bridge
      # precisely because it listens on all of them.
      [[ -n "${listen}" ]] || { echo "incusd has no core.https_address, set INCUS_ENDPOINT" >&2; exit 1; }

      host="${IC_IPV4_GATEWAY}"
    elif [[ ! "${host}" =~ ^[0-9]+(\.[0-9]+){3}$ ]]; then
      # A named remote on some other host. Resolve it here, where the name still
      # means what it does to us.
      resolved=$(getent ahostsv4 "${host}" | awk '{ print $1; exit }')
      [[ -n "${resolved}" ]] || { echo "cannot resolve ${host} to an IPv4 address" >&2; exit 1; }

      host="${resolved}"
    fi

    export INCUS_ENDPOINT="https://${host}:${port}"

    # --os-env because incus-compose reads .env files only, and everything above
    # is an OS variable. -P names the project directory, since compose.yaml lives
    # in cmd/ic-dns. exec, so Ctrl-C reaches incus-compose and not this script.
    just run -P container/ic-dns --os-env "$@"

# Run the ServeDNS benchmarks. No -race: that would measure the race detector.
# Two modules since the restructure - ecs_view is ours, kubernetes the yardstick.
dns-bench module="benchmarks/ic-dns/ecs_view" *args:
    go -C {{ module }} test -run '^$' -bench . -benchmem ./... "${@:2}"

# The numbers a benchmark row is written in, taken from the cold store rather
# than from Incus - so it is the fleet as this binary saw it, and readable while
# Incus is unreachable. Both files come out of the data volume:
# enricher-cold-store.json is what the enricher holds, cold.json the serials dns
# published.
#
# Three lines are warnings, not facts:
#
#   claimed twice          an address more than one instance holds, so those
#                          clients are refused rather than guessed at. Two
#                          projects drew overlapping subnets; above 0 wants a
#                          renumber.
#
#   IPv4 below instances   the fleet has not converged. Nothing announces a DHCP
#                          lease, so an address waits for the next pass.
#
#   serials still at 1     a zone published once and never changed since. After
#                          a restart that carried its store across, this should
#                          be low - every one of them at 1 means the serials did
#                          not survive, and every secondary re-transfers.
[doc("Fleet shape from the enricher store, and the serials dns published")]
dns-cold-stats:
    #!/usr/bin/env bash
    set -euo pipefail

    command -v jq >/dev/null || { echo "jq is missing" >&2; exit 1; }

    # Two files now: the enricher writes what it holds, dns writes the serials
    # it published. Neither can be computed from the other.
    tmp=$(mktemp -d)
    trap 'rm -rf "${tmp}"' EXIT

    pull() {
      just ic-dns-compose incus storage volume file pull default "vol-data/$1" "${tmp}/$1" \
        >/dev/null 2>"${tmp}/err" && return 0

      sed 's/^/  /' "${tmp}/err" >&2
      echo "could not pull $1 out of the vol-data volume." >&2

      return 1
    }

    pull enricher-cold-store.json || {
      echo "It is written as the enricher reads the fleet, so a stack that has" >&2
      echo "not reached Incus yet has none." >&2
      exit 1
    }

    # Missing is not fatal: dns writes it on the first publish, which is later.
    pull cold.json || echo '{}' > "${tmp}/cold.json"

    for f in enricher-cold-store.json cold.json; do
      jq -e . "${tmp}/${f}" >/dev/null 2>&1 || {
        echo "${f} is not readable as JSON - this binary would start cold" >&2
        exit 1
      }
    done

    echo "enricher-cold-store.json  written $(date -d "@$(stat -c %Y "${tmp}/enricher-cold-store.json")" '+%Y-%m-%d %H:%M')"

    jq -r --slurpfile cold "${tmp}/cold.json" '
      # Pad the label column. jq gives null for a repeat of zero or less, and
      # null is the identity for +, so an over-wide label still prints.
      def pad($w): . + (" " * ($w - length));
      def row($label; $value): "  " + ($label | pad(24)) + ($value | tostring);

      # Nested by project: a project holds its instances and its networks, and
      # an instance holds the interfaces it sits on and the networks those name.
      (.projects // {})                                              as $projects
      | [$projects[] | (.instances // {}) | to_entries[]]            as $inst
      | ($cold[0].serials // {})                                     as $serials

      # A wire is named by the project that owns it, not the one looking at it,
      # so two projects on one bridge are one wire rather than two.
      | [$projects | to_entries[]
          | .key as $p | (.value.networks // {}) | keys[] | $p + "/" + .]
                                                                     as $nets
      | [$inst[] | (.value.interfaces // [])[]]                      as $on
      | [$inst[]
          | [(.value.interfaces // [])[] | .project + "/" + .network] | unique]
                                                                     as $sets
      | [$on[] | (.ipv4 // [])[]]                                    as $v4
      | [$on[] | (.ipv6 // [])[]]                                    as $v6
      # Deduplicated per instance first, so a multi-homed host holding one
      # address on two of its own networks is not read as a clash with itself.
      | [$inst[]
          | [(.value.interfaces // [])[] | ((.ipv4 // []) + (.ipv6 // []))[]] | unique]
                                                                     as $owned
      | [$inst[] | .value.config["user.label.dns.service"]
                // .value.config["user.label.incus-compose.service"]
                | select(. != null and . != "")]                     as $services
      | [$serials | keys[] | select(endswith(".arpa."))]             as $reverse
      | [$serials[] | select(.Serial == 1)]                          as $fresh

      | [
          "",
          "fleet",
          row("projects";            [$projects[] | select(.config != null)] | length),
          row("instances";           $inst | length),
          row("multi-homed";         [$sets[] | select(length > 1)] | length),
          row("named by a service";  "\($services | length)  (\($services | unique | length) distinct)"),
          "",
          "views",
          row("distinct network sets"; $sets | unique | length),
          row("networks held";       $nets | length),
          row("networks sat on";     [$on[] | .project + "/" + .network] | unique | length),
          row("subnets";             [$projects[] | (.networks // {})[] | (.ipv4, .ipv6)
                                       | select(. != null and . != "")] | unique | length),
          "",
          "addresses",
          row("IPv4";                $v4 | length),
          row("IPv6";                $v6 | length),
          row("claimed twice";       [$owned[][]] | group_by(.) | map(select(length > 1)) | length),
          "",
          "zones  (from cold.json)",
          row("forward";             ($serials | length) - ($reverse | length)),
          row("reverse";             $reverse | length),
          row("serials still at 1";  "\($fresh | length)  (highest \([$serials[] | .Serial] | max // 0))")
        ]
      | .[]
    ' "${tmp}/enricher-cold-store.json"

# Asks ic-dns for every instance in the stress fleet and counts what answers.
# Addresses are ignored - an answer is an answer, which is the whole question.
#
#   just dns-verify                          # count what answers
#   just dns-verify -b -- -c 50 -l 30        # hand the same names to dnsperf
#   just dns-verify -u                       # through the unbound in front
#   just dns-verify -r                       # after a fleet went up or down
#   just dns-verify --help                   # every option, and its variable
#
# -u asks unbound instead of ic-dns, with the same queries: the client subnet
# asserted per query is what ic-dns keys a view on and what unbound keys its
# subnet cache on, so the run says whether the cache in front kept them apart. It
# wants DNS_ECHO_SUBNET set, or unbound has nothing to key on.
#
# The queries run from the debug instance - see just/ic-dns-verify-dns.sh. This
# half reads the fleet out of the Incus API, writes the target list, pushes it in
# with the runner, and execs. Both files are rewritten every run: a stale one lies.
#
# Targets are cached in ./dns-targets and re-read only when it is missing or -r
# is given. Reading them is the expensive part by a wide margin - incusd walks
# every instance to compute UsedBy per network - so paying it per run makes the
# daemon look like the queries are loading it.
[doc("Ask ic-dns for every instance in the fleet, from the debug instance. -b runs dnsperf, -u asks unbound")]
dns-verify *args:
    #!/usr/bin/env bash
    set -u

    usage() {
      cat <<'EOF'
    Usage: just ic-dns-verify-dns [options] [-- dnsperf arguments]

      -b, --benchmark      hand the names to dnsperf instead of counting answers
      -u, --unbound        ask the unbound in front rather than ic-dns
      -p, --port PORT      port to ask on                        [PORT, 53]
      -s, --server ADDR    address to ask, instead of the instance's own  [SERVER]
      -n, --names "a b"    instance names to ask for, instead of probing  [NAMES]
      -r, --refresh        re-read the fleet from Incus first     [REFRESH]
      -h, --help           this

          --prefix NAME    fleet whose <prefix>-*-net1 bridges are the queriers
                                                                  [PREFIX, flat]
          --suffix NAME    zone suffix                            [SUFFIX, incus]
          --list FILE      where the targets are cached           [LIST, dns-targets]
          --queries FILE   dnsperf query file                     [QUERYFILE, queries.txt]
          --all            benchmark the whole fleet, not one project  [BENCH_ALL]
          --anon           assert the bridge rather than an instance    [BENCH_ANON]
                           query mix, hit:absent:foreign:shadow         [MIX]
                           how many queries the mix generates           [MIX_COUNT]
                           domain the fleet aliases into                [ALIAS]

    Everything after -- goes to dnsperf, which only runs under -b. In brackets is
    the environment variable each option reads when it is not given, and its
    default; the option wins.
    EOF
    }

    # Long options, and an unknown one an error rather than something taken for a
    # port number. util-linux's, which is the only getopt that does either.
    getopt --test >/dev/null
    if (($? != 4)); then
      echo "util-linux getopt is missing" >&2

      exit 1
    fi

    # Defaults first, so a variable set in the environment is what an unwritten
    # option falls back to.
    benchmark=0
    instance=ic-dns
    port="${PORT:-53}"
    prefix="${PREFIX:-flat}"
    server="${SERVER:-}"
    suffix="${SUFFIX:-incus}"
    names="${NAMES:-}"
    list="${LIST:-dns-targets}"
    queries="${QUERYFILE:-queries.txt}"
    refresh="${REFRESH:-}"
    all="${BENCH_ALL:-}"
    anon="${BENCH_ANON:-}"

    # Forwarded rather than defaulted here: the script owns the default, so an
    # unset MIX must arrive unset and not as an empty string, which means
    # something else entirely - hits only.
    mix_args=()
    [[ -n "${MIX+set}" ]] && mix_args+=(--env MIX="${MIX}")
    [[ -n "${MIX_COUNT+set}" ]] && mix_args+=(--env MIX_COUNT="${MIX_COUNT}")
    [[ -n "${ALIAS+set}" ]] && mix_args+=(--env ALIAS="${ALIAS}")

    parsed=$(getopt -n "just ic-dns-verify-dns" \
      -o bup:s:n:rh \
      -l benchmark,unbound,port:,server:,names:,refresh,help \
      -l prefix:,suffix:,list:,queries:,all,anon \
      -- "$@")

    if (($? != 0)); then
      usage >&2

      exit 2
    fi

    # getopt has already moved dnsperf's arguments behind the -- it emits, so
    # the loop below ends on one whether the caller wrote it or not.
    eval set -- "${parsed}"

    while true; do
      case "$1" in
        -b | --benchmark) benchmark=1 ;;
        -u | --unbound) instance=unbound ;;
        -p | --port) port="$2"; shift ;;
        -s | --server) server="$2"; shift ;;
        -n | --names) names="$2"; shift ;;
        -r | --refresh) refresh=1 ;;
        -h | --help) usage; exit 0 ;;
        --prefix) prefix="$2"; shift ;;
        --suffix) suffix="$2"; shift ;;
        --list) list="$2"; shift ;;
        --queries) queries="$2"; shift ;;
        --all) all=1 ;;
        --anon) anon=1 ;;
        --) shift; break ;;
      esac

      shift
    done

    # What is left is dnsperf's, and nothing runs dnsperf without -b. Saying so
    # beats the silence a positional port was read out of.
    if (($#)) && ((benchmark == 0)); then
      echo "extra arguments: $* - those go to dnsperf, which needs -b. --help lists the options" >&2

      exit 2
    fi

    if [[ -n "${refresh}" || ! -s "${list}" ]]; then
      # Every project's first bridge: one line of "subnet project". The bridge's
      # own address is a querier on that network, which is how ecs_view decides
      # what may be seen.
      #
      # --all-projects, because a project with features.networks owns its
      # networks and they are in no other listing. json rather than csv: the
      # address comes from the network's own config, where csv has it in a column
      # whose position moves with the flags. A managed network can also carry
      # "none" or "auto" there, so a value without a subnet in it is no querier.
      if raw=$(incus network list --all-projects -f json 2>&1); then
        fresh=$(jq -r --arg p "${prefix}-" '
          .[]
          | select(.name | startswith($p) and endswith("-net1"))
          | (.config["ipv4.address"] // "") as $subnet
          | select($subnet | contains("/"))
          | "\($subnet | sub("/.*"; "")) \(.name | sub("-net1$"; ""))"
        ' <<<"${raw}")

        reason="no ${prefix}-*-net1 network in this remote - wrong --prefix or INCUS_REMOTE?"
      else
        fresh=""
        reason="${raw}"
      fi

      if [[ -n "${fresh}" ]]; then
        printf '%s\n' "${fresh}" >"${list}"

        echo "targets: $(wc -l <"${list}") projects, read from Incus"
      elif [[ -s "${list}" ]]; then
        echo "targets: $(wc -l <"${list}") projects, cached - ${reason}" >&2
      else
        echo "no targets, and ${list} is empty - ${reason}" >&2

        exit 1
      fi
    else
      # The age is printed because a stale cache is the one way this lies: a
      # fleet rebuilt since then has new subnets, and every querier reads as
      # unknown.
      echo "targets: $(wc -l <"${list}") projects, cached $(( ($(date +%s) - $(stat -c %Y "${list}")) / 60 ))m ago - -r to re-read"
    fi

    # Whatever address the instance holds inside the daemon - ic-dns, or the
    # unbound in front of it under -u. The IPv4 column carries a " (eth0)"
    # suffix, which is the interface and not part of the address.
    if [[ -z "${server}" ]]; then
      server=$(just ic-dns-compose incus list "${instance}" --format=compact -c n4 2>/dev/null |
        awk -v n="${instance}" '$1 == n { print $2; exit }')

      [[ -n "${server}" ]] || {
        echo "no IPv4 address for the ${instance} instance - is the stack up?" >&2

        exit 1
      }
    fi

    echo "asking ${instance} at ${server}"

    # Cleared before anything is written. A push over an existing file fails with
    # "permission denied" - the second run in a row hits it - and a push that
    # fails silently leaves the previous run's fleet in place to be measured
    # again.
    just ic-dns-compose incus exec debug -- rm -f /tmp/dns-targets /tmp/verify.sh /tmp/queries.txt

    push() {
      just ic-dns-compose incus file push "$@" >/dev/null && return 0

      echo "could not push $1 into the debug instance - is the stack up?" >&2

      exit 1
    }

    push "${list}" debug/tmp/dns-targets
    push just/dns-verify-dns.sh debug/tmp/verify.sh --mode 0755

    # A pushed queries.txt is used as it stands, which is what makes it worth
    # curating by hand. -r rebuilds it from the fleet instead.
    if ((benchmark)) && [[ -s "${queries}" && -z "${refresh}" ]]; then
      echo "queries: $(wc -l <"${queries}") from ${queries}"

      push "${queries}" debug/tmp/queries.txt
    fi

    just ic-dns-compose incus exec debug \
      --env SERVER="${server}" \
      --env TARGET="${instance}" \
      --env PORT="${port}" \
      --env SUFFIX="${suffix}" \
      --env BENCHMARK="${benchmark}" \
      --env NAMES="${names}" \
      --env BENCH_ALL="${all}" \
      --env BENCH_ANON="${anon}" \
      "${mix_args[@]}" \
      --env DNSPERF_ARGS="$*" \
      -- /tmp/verify.sh
