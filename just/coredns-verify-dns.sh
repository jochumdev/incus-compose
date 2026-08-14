#!/usr/bin/env bash
#
# The half of `just coredns-verify-dns` that speaks DNS, which is why it runs in
# the debug instance: CoreDNS answers on a bridge inside the nested daemon, and
# driving a load generator across that gap would measure the gap.
#
# Pushed to /tmp/verify.sh and exec'd every run. Inputs arrive as environment
# variables, so nothing has to be quoted into a generated script:
#
#   SERVER TARGET PORT SUFFIX BENCHMARK NAMES BENCH_ALL BENCH_ANON DNSPERF_ARGS
#
# TARGET only names what SERVER is, for the message when nothing answers.
#
# Reads /tmp/dns-targets, one "subnet project" per line, and writes
# /tmp/queries.txt unless one was pushed in.

# -u only: the loops below lean on `test && action`, which is a non-zero
# statement whenever the test is false, and kdig failing is an answer here.
set -u

list=/tmp/dns-targets
queries=/tmp/queries.txt

# shellcheck disable=SC2153  # SERVER is an input, not a typo for server
server="${SERVER}"
target="${TARGET:-CoreDNS}"
port="${PORT:-53}"
suffix="${SUFFIX:-incus}"
benchmark="${BENCHMARK:-0}"

candidates="gateway worker1 worker2 host1 host2 host3 web"

# One query first, so a server that is not there says so once instead of as a
# MISS per name. Any response will do, even a refusal.
#
# version.bind in CHAOS rather than the root: under -u the server is a recursive
# resolver, and a cold root query outrunning the two seconds below would read as
# nothing listening. Both ends answer or refuse this one locally.
if ! kdig @"${server}" -p "${port}" +time=2 +retry=0 version.bind CH TXT >/dev/null 2>&1; then
  echo "no answer from ${server}:${port}, nothing to verify - is ${target} running?" >&2

  exit 1
fi

# Which instance names to ask for. flat makes hosts, nested makes a gateway and
# workers, and a fleet somebody built by hand makes neither - so they are found
# rather than assumed. Asking for names that are not there is indistinguishable
# from a fleet that is not served, and reads as every line missing.
read -r -a names <<<"${NAMES:-}"

if ((${#names[@]} == 0)); then
  read -r probe_subnet probe_project <"${list}"

  for candidate in ${candidates}; do
    [[ -n "$(kdig @"${server}" -p "${port}" +subnet="${probe_subnet}/32" +short \
      "${candidate}.${probe_project}.${suffix}" A 2>/dev/null)" ]] && names+=("${candidate}")
  done

  if ((${#names[@]} == 0)); then
    echo "no known instance name answers in ${probe_project} - set NAMES to what the fleet calls them" >&2

    exit 1
  fi

  echo "names: ${names[*]} (found in ${probe_project})"
fi

if ((benchmark)); then
  # One project by default. A client subnet is one view, and in this fleet a view
  # is one project, so asking for the whole fleet spends the run measuring
  # NXDOMAIN - the fail-closed path, which answers a different question than the
  # one worth asking. BENCH_ALL=1 asks for everything on purpose, which is a fair
  # way to measure exactly that path. A pushed queries.txt is used as it stands.
  if [[ ! -s "${queries}" ]]; then
    if [[ -n "${BENCH_ALL:-}" ]]; then
      awk -v s="${suffix}" -v n="${names[*]}" '{ split(n, a, " "); for (i in a) print a[i] "." $2 "." s " A" }' \
        "${list}" >"${queries}"
    else
      awk -v s="${suffix}" -v n="${names[*]}" 'NR == 1 { split(n, a, " "); for (i in a) print a[i] "." $2 "." s " A" }' \
        "${list}" >"${queries}"
    fi
  fi

  # The querier follows the file rather than the other way round: whichever
  # project the first query names is the view this run measures, so a hand-edited
  # queries.txt cannot end up asking as somebody who cannot see it.
  project=$(awk -F. 'NR == 1 { print $2 }' "${queries}")
  subnet=$(awk -v p="${project}" '$2 == p { print $1; exit }' "${list}")

  if [[ -z "${subnet}" ]]; then
    echo "no subnet for ${project} in ${list} - REFRESH=1 to re-read the fleet" >&2

    exit 1
  fi

  # The guard the Go benchmarks have, applied to the wire: a run that has turned
  # into an NXDOMAIN measurement must fail rather than report a fast number. Its
  # answer is also the address the run asserts.
  #
  # An instance's own address is one map lookup, where a bridge address belongs
  # to no instance and is matched against every known prefix instead.
  # BENCH_ANON=1 asserts the bridge, to measure that scan on purpose.
  querier=$(kdig @"${server}" -p "${port}" +subnet="${subnet}/32" +short \
    "${names[0]}.${project}.${suffix}" A 2>/dev/null | head -1)

  if [[ -z "${querier}" ]]; then
    echo "${names[0]}.${project}.${suffix} does not answer from ${server}:${port} as ${subnet}," >&2
    echo "so this would time NXDOMAIN rather than anything worth knowing" >&2

    exit 1
  fi

  [[ -n "${BENCH_ANON:-}" ]] && querier="${subnet}"

  # RFC 7871 option 8: family 1 (IPv4), source prefix 32, scope 0, then the
  # address.
  IFS=. read -r a b c d <<<"${querier}"
  ecs=$(printf '00012000%02x%02x%02x%02x' "${a}" "${b}" "${c}" "${d}")

  echo "benchmarking $(wc -l <"${queries}") queries as ${querier} (${project}) against ${server}:${port}"

  # Unquoted on purpose: DNSPERF_ARGS is a flag list, and quoting it would hand
  # dnsperf one argument with spaces in it.
  # shellcheck disable=SC2086
  exec dnsperf -s "${server}" -p "${port}" -d "${queries}" -e -E "8:${ecs}" ${DNSPERF_ARGS:-}
fi

ok=0
bad=0

while read -r subnet project; do
  for name in "${names[@]}"; do
    if [[ -n "$(kdig @"${server}" -p "${port}" +subnet="${subnet}/32" +short \
      "${name}.${project}.${suffix}" A 2>/dev/null)" ]]; then
      ok=$((ok + 1))
    else
      bad=$((bad + 1))
      echo "MISS ${name}.${project}.${suffix}"
    fi
  done
done <"${list}"

echo "answered ${ok}, missing ${bad}"

[[ ${bad} -eq 0 ]]
