#!/bin/sh
#
# Sets GOMEMLIMIT from the cgroup memory limit, then execs ic-dns with whatever
# arguments it was given.
#
# Without it nothing bounds the Go heap: the goal is roughly twice the live heap
# and the live heap tracks the fleet, so a limit that fits 480 instances is a
# limit that OOM-kills at 4800. GOMEMLIMIT turns that into more GC instead - the
# runtime collects and scavenges harder as it approaches, rather than growing
# past a ceiling it does not know about.
#
# The limit is what cgroup2 charges, which is anon plus page cache plus kernel
# memory plus socket buffers - and only anon is Go's. The binary's own text is
# tens of MiB of page cache, so a percentage of the limit is the reserve, not a
# rounding error. GOMEMLIMIT_PERCENT tunes it; an explicit GOMEMLIMIT wins over
# both and is left alone.
#
# See benchmarks/ic-dns/benchmark.md, "Running it under a memory limit", for
# the measurements these defaults come from.

set -eu

# Neither file exists on a host without cgroup2, and memory.high is what Incus
# writes for limits.memory.enforce=soft while leaving memory.max at "max". The
# lower of the two is the one that bites.
cgroup_limit() {
  lowest=""

  for f in /sys/fs/cgroup/memory.max \
           /sys/fs/cgroup/memory.high \
           /sys/fs/cgroup/memory/memory.limit_in_bytes; do
    [ -r "${f}" ] || continue

    value=$(cat "${f}" 2>/dev/null) || continue

    # "max" is cgroup2 for no limit; anything non-numeric is not ours to read.
    case "${value}" in
      '' | *[!0-9]*) continue ;;
    esac

    # cgroup1 spells no limit as a sentinel near the top of int64 rather than as
    # a word, and it is page-aligned rather than exactly 2^63-1.
    [ "${value}" -ge 9223372036854771712 ] 2>/dev/null && continue

    if [ -z "${lowest}" ] || [ "${value}" -lt "${lowest}" ]; then
      lowest="${value}"
    fi
  done

  printf '%s' "${lowest}"
}

if [ -z "${GOMEMLIMIT:-}" ]; then
  limit=$(cgroup_limit)

  if [ -n "${limit}" ]; then
    percent="${GOMEMLIMIT_PERCENT:-75}"

    budget=$((limit * percent / 100))

    if [ "${budget}" -gt 0 ]; then
      GOMEMLIMIT="${budget}B"
      export GOMEMLIMIT

      echo "GOMEMLIMIT=${GOMEMLIMIT} (${percent}% of the ${limit} B cgroup limit)"
    fi
  fi
fi

# GOMEMLIMIT is the ceiling; GOGC is what paces collection under it, and left at
# its default of 100 the two do not meet. The heap is collected at roughly twice
# the live heap - 12 MB against a 75 MB limit for a 480-instance fleet - so the
# limit never binds and the collector runs constantly: 7.7% of CPU under a
# saturating run, against 2.1% at 400, for 4.3% less CPU per query.
#
# Raising it is safe precisely because GOMEMLIMIT is there. This names a target
# of five times the live heap; the limit is what caps that, so a fleet big enough
# for five times to exceed the cgroup gets paced by the ceiling rather than by
# this. Peak memory did not move: 61 MiB of a 100 MiB limit either way.
#
# An explicit GOGC wins and is left alone, as GOMEMLIMIT is.
if [ -z "${GOGC:-}" ]; then
  GOGC=400
  export GOGC

  echo "GOGC=${GOGC} (the pacer; GOMEMLIMIT above is the ceiling)"
fi

# The usual entrypoint shape: no arguments, or a first one that looks like a
# flag, means ic-dns is what is being run and the binary goes in front. Anything
# else is executed as given, so `sh` still gets you a shell in here.
#
# By name rather than by path, so PATH decides which one - an ic-dns built
# somewhere else and mounted over it needs no change here. It lives in /usr/bin
# because nothing sets PATH for this container: the shell's built-in fallback is
# what resolves the name, and that covers /usr/bin but not /usr/local/bin.
# Anything that is not itself runnable is arguments for ic-dns. It used to be
# "empty, or starts with a dash", which was right when the binary took flags
# alone - the first argument is a subcommand now, and `run` matched neither.
#
# command -v rather than a list of subcommands, so this does not go stale every
# time one is added, and `entrypoint.sh sh` still gets a shell to debug in.
if ! command -v "${1:-}" >/dev/null 2>&1; then
  set -- ic-healthd "$@"
fi

exec "$@"
