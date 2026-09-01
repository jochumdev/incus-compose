#!/usr/bin/env bash
#
# The half of `just ic-dns-verify-dns` that speaks DNS, which is why it runs in
# the debug instance: ic-dns answers on a bridge inside the nested daemon, and
# driving a load generator across that gap would measure the gap.
#
# Pushed to /tmp/verify.sh and exec'd every run. Inputs arrive as environment
# variables, so nothing has to be quoted into a generated script:
#
#   SERVER TARGET PORT SUFFIX BENCHMARK NAMES BENCH_ALL BENCH_ANON DNSPERF_ARGS
#   MIX MIX_COUNT ALIAS
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
target="${TARGET:-ic-dns}"
port="${PORT:-53}"
suffix="${SUFFIX:-incus}"
benchmark="${BENCHMARK:-0}"

# MIX is hit:absent:foreign:shadow, as weights rather than percentages, and it is
# what makes a run resemble traffic instead of a curated list.
#
#   hit      a name the fleet serves, answered from the snapshot
#   absent   a name in a zone it serves that nothing answers to - NXDOMAIN, and
#            the zone still has to be found for the SOA
#   foreign  a name outside every zone - falls through to whatever is next, and
#            every label boundary has to fail before that is known
#   shadow   a name inside a zone an alias invented, which the fleet holds a
#            handful of names in - matched, not held, so it falls through with
#            the walk stopping early instead of running out of labels
#
# The default is what a resolver named in a container's resolv.conf actually
# sees: a fifth of its traffic is fleet names and the rest is the internet. A
# hits-only run describes the rig rather than the deployment, which is why it is
# no longer the default - MIX= restores it, and BENCH_ALL still wins outright.
#
# The three miss kinds cost differently, so the weights are not decoration: hit
# and absent are answered from the snapshot, foreign and shadow fall through, and
# only shadow has a zone to match on the way.
mix="${MIX-20:5:70:5}"
mix_count="${MIX_COUNT:-2000}"
alias_domain="${ALIAS:-shadow.example}"

# Real ones, at the length real ones have: a short name is a short walk, and
# padding them out would flatter anything that rejects on a prefix.
foreign_names="github.com deb.debian.org registry.npmjs.org mirror.archlinux.org
cloudflare.net storage.googleapis.com quay.io cdn.kernel.org"

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
  # Reached with MIX= or BENCH_ALL set. One project, because a client subnet is
  # one view and in this fleet a view is one project, so asking for the whole
  # fleet spends the run measuring NXDOMAIN - which BENCH_ALL=1 does on purpose.
  # A pushed queries.txt is used as it stands.
  # BENCH_ALL is a different question - every name in the fleet from one view -
  # so it takes the file rather than sharing it with a mix.
  if [[ ! -s "${queries}" && -n "${mix}" && -z "${BENCH_ALL:-}" ]]; then
    IFS=: read -r w_hit w_absent w_foreign w_shadow <<<"${mix}"

    w_hit="${w_hit:-0}"
    w_absent="${w_absent:-0}"
    w_foreign="${w_foreign:-0}"
    w_shadow="${w_shadow:-0}"

    if ((w_hit == 0)); then
      echo "MIX needs a non-zero hit weight: the run asserts a real answer, and" >&2
      echo "the project it asks as is taken from the first query" >&2

      exit 1
    fi

    # Drawn rather than cycled: i % span walks the span in order, which lays the
    # file out in blocks - all the hits, then all the misses - so any window of a
    # run sees one path rather than the mix. srand is seeded, so two runs of the
    # same MIX get the same file. The first line is forced to a hit, which is
    # what the project and subnet below are read from.
    awk -v s="${suffix}" -v n="${names[*]}" -v ad="${alias_domain}" -v fn="${foreign_names}" \
      -v total="${mix_count}" -v h="${w_hit}" -v a="${w_absent}" -v f="${w_foreign}" -v sh="${w_shadow}" '
      { subnet[NR] = $1; project[NR] = $2; projects = NR }
      END {
        split(n, name, " ")
        split(fn, foreign, " ")

        span = h + a + f + sh
        srand(1)

        # One project, not all of them. A client subnet is one view and a view
        # is one project, so a fleet name belonging to any other project is
        # answered NXDOMAIN however real it is - cycling projects here turns
        # the hit share into BENCH_ALL, and the run then measures the
        # fail-closed path while reporting itself as mixed.
        p = project[1]

        for (i = 0; i < total; i++) {
          at = (i == 0) ? 0 : rand() * span

          if (at < h)
            print name[(i % length(name)) + 1] "." p "." s " A"
          else if (at < h + a)
            print "absent-" i "." p "." s " A"
          else if (at < h + a + f)
            print foreign[(i % length(foreign)) + 1] " A"
          else
            print "shadow-" i "." p "." ad " A"
        }
      }' "${list}" >"${queries}"

    echo "mix ${mix} (hit:absent:foreign:shadow) over ${mix_count} queries, MIX= for hits only"
  fi

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
