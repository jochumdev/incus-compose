# Tests. The stages are one implementation plus an environment, see
# CONTRIBUTING.md: the skip helpers read the env vars below, and each wrapper
# sets its own and calls `test`. The wrappers re-invoke just, which is what lets
# a stage override v_test_timeout - the child parses this file with the stage's
# environment already set.

# How long one run may take. The stage that stands up containers wants more and
# sets it in its own environment before calling us.
v_test_timeout := env("TEST_TIMEOUT", "20m")

# Run tests against nested Incus, includes direct incus tests.
#
# covdata rather than -coverprofile, because the CLI is a subprocess and lands in
# no test binary's profile: testlib points its GOCOVERDIR at the same directory
# -test.gocoverdir sends `go test` to, and textfmt merges them. -args must go
# last. `|| status=$?` because the justfile shell is -e and the report has to run
# on a failing suite too.
[doc("Run all tests against nested Incus, then report coverage")]
[env("INCUS_COMPOSE_IMAGE_CACHE", "incus-compose-tests-cache")]
test folder="./..." *args:
    mkdir -p work/logs
    export DATE=`date +%Y%m%d-%H%M%S`; \
      export INCUS_COMPOSE_TEST_COVERDIR="${PWD}/work/covdata/${DATE}"; \
      mkdir -p "${INCUS_COMPOSE_TEST_COVERDIR}"; \
      status=0; \
      gotestsum --hide-summary=skipped --format testname --jsonfile=work/logs/${DATE}.json --packages={{ folder }} \
        --post-run-command "bash -c 'echo; echo Slowest tests; gotestsum tool slowest --num 10 --jsonfile work/logs/${DATE}.json'" \
        -- -race -parallel {{ v_test_procs }} -timeout {{ v_test_timeout }} -cover -covermode atomic -coverpkg ./... -v "${@:2}" \
        -args -test.gocoverdir="${INCUS_COMPOSE_TEST_COVERDIR}" || status=$?; \
      go tool covdata textfmt -i="${INCUS_COMPOSE_TEST_COVERDIR}" -o work/logs/${DATE}-cover.out; \
      just cover work/logs/${DATE}-cover.out; \
      exit ${status}

#   just cover                          # the newest run
#   just cover work/logs/X-cover.out    # a profile you name
#   just cover work/covdata/X           # a covdata directory, converted first
#
# TESTED drops the packages with no test files, which is the set a plain
# -coverprofile run measures. It is there to show TOTAL is comparable to one.
[doc("Per-package and total statement coverage of a run")]
cover profile="":
    #!/usr/bin/env bash
    set -euo pipefail

    profile="{{ profile }}"
    if [[ -z "${profile}" ]]; then
      for candidate in work/logs/*-cover.out; do
        [[ -f "${candidate}" ]] || continue
        [[ -z "${profile}" || "${candidate}" -nt "${profile}" ]] && profile="${candidate}"
      done
    fi

    if [[ -d "${profile}" ]]; then
      dir="${profile}"
      profile="${dir%/}.out"
      go tool covdata textfmt -i="${dir}" -o "${profile}"
    fi

    if [[ -z "${profile}" || ! -f "${profile}" ]]; then
      echo "no coverage profile: run just test first" >&2
      exit 1
    fi

    echo "Profile: ${profile}"
    echo

    # A package is "tested" when it ships test files, which is what the old
    # -coverprofile run measured. Through the environment, not -v: the list has
    # newlines in it, which POSIX awk rejects in a -v value.
    export TESTED
    TESTED=$(go list -f '{{ "{{" }} if or .TestGoFiles .XTestGoFiles {{ "}}" }}{{ "{{" }} .ImportPath {{ "}}" }}{{ "{{" }} end {{ "}}" }}' ./... | grep -v '^$')

    # Sorted by sort, not in awk: asorti is a gawk extension and the CI runner
    # has mawk.
    rows=$(awk '
      NR == 1 { next }
      {
        split($1, at, ":")
        pkg = at[1]
        sub(/\/[^\/]*$/, "", pkg)
        stmts[pkg] += $2
        if ($3 > 0) covered[pkg] += $2
      }
      END {
        for (pkg in stmts) printf "%s %d %d\n", pkg, stmts[pkg], covered[pkg]
      }
    ' "${profile}" | sort)

    printf '%-52s %7s %9s %9s\n' PACKAGE STMTS COVERED PERCENT

    echo "${rows}" | awk '
      BEGIN {
        split(ENVIRON["TESTED"], t, "\n")
        for (i in t) istested[t[i]] = 1
      }
      {
        printf "%-52s %7d %9d %8.1f%%\n", $1, $2, $3, $2 ? 100 * $3 / $2 : 0
        total += $2; totalCovered += $3
        if (istested[$1]) { tstmt += $2; tcov += $3 }
      }
      END {
        printf "\n%-52s %7d %9d %8.1f%%\n", "TESTED", tstmt, tcov, tstmt ? 100 * tcov / tstmt : 0
        printf "%-52s %7d %9d %8.1f%%\n", "TOTAL", total, totalCovered, total ? 100 * totalCovered / total : 0
      }
    '

# Run local unit-tests, incus-facing tests are skipped.
[env("INCUS_COMPOSE_TEST_LOCAL", "1")]
test-local folder="./..." *args:
    @just test "$@"

# Run e2e tests.
[env("INCUS_COMPOSE_TEST_E2E", "1")]
[env("TEST_TIMEOUT", "60m")]
test-e2e folder="./..." *args:
    @just test "$@"

# Run example tests.
[env("INCUS_COMPOSE_TEST_EXAMPLES", "1")]
test-examples *args:
    @just test ./examples/... "$@"

# Run all tests, includes direct incus, e2 and examples tests.
[env("INCUS_COMPOSE_TEST_E2E", "1")]
[env("INCUS_COMPOSE_TEST_EXAMPLES", "1")]
[env("TEST_TIMEOUT", "60m")]
test-all *args:
    #!/usr/bin/env bash
    set -euo pipefail
    for dir in $(just modules); do
        echo "==> ${dir#"${PWD}"/}"
        go -C "$dir" test ./... -race -cover "$@"
    done

# Run the tests with a race detector.
test-race folder="./..." *args:
    mkdir -p work/logs
    gotestsum --hide-summary=skipped --jsonfile=work/logs/`date +%Y%m%d-%H%M%S`.json --packages={{ folder }} -- -parallel {{ v_test_procs }} -timeout {{ v_test_timeout }} -race -v "${@:2}"

# Update snapshots for long running tests.
[env("INCUS_COMPOSE_TEST_E2E", "1")]
[env("TEST_TIMEOUT", "60m")]
[env("UPDATE_SNAPSHOTS", "1")]
update-e2e-snapshots folder="./..." *args:
    @just test "$@"

# Update snapshots for examples.
[env("INCUS_COMPOSE_TEST_EXAMPLES", "1")]
[env("UPDATE_SNAPSHOTS", "1")]
update-examples-snapshots folder="./..." *args:
    @just test "$@"

# Update snapshot test files that require a remote
[env("UPDATE_SNAPSHOTS", "1")]
update-snapshots folder="./..." *args:
    @just test "$@"

#   just test-log                          # everything the run printed
#   just test-log -p 'FAIL|Error:'         # only matching lines (extended regex)
#   just test-log -t TestE2EDownNoDeps     # only one test's output
#   just test-log -f | tee run.log         # follow even though piped
#
# On a terminal it follows until Ctrl-C, so it can be started while a run is
# going; piped it reads once and exits unless -f says to keep following. The
# newest log is picked at startup, so a run launched after this is not the one
# being followed.
[doc("Plain text of the newest work/logs/*.json. Follows on a terminal")]
test-log *args:
    #!/usr/bin/env bash
    set -euo pipefail

    usage() {
      cat <<'EOF'
    Usage: just test-log [options]

      -p, --pattern REGEX  only lines matching this extended regex
      -t, --test NAME      only one test's output, matched by prefix
      -f, --follow         follow even when not on a terminal, e.g. when piped
      -h, --help           this
    EOF
    }

    # Long options, and an unknown one an error rather than something taken for a
    # pattern. util-linux's, which is the only getopt that does either. It signals
    # that by exiting 4 on --test, so this has to run outside set -e.
    set +e
    getopt --test >/dev/null
    getopt_rc=$?
    set -e

    if ((getopt_rc != 4)); then
      echo "util-linux getopt is missing" >&2

      exit 1
    fi

    pattern=""
    test=""
    follow=""

    set +e
    parsed=$(getopt -n "just test-log" \
      -o p:t:fh \
      -l pattern:,test:,follow,help \
      -- "$@")
    getopt_rc=$?
    set -e

    if ((getopt_rc != 0)); then
      usage >&2

      exit 2
    fi

    eval set -- "${parsed}"

    while true; do
      case "$1" in
        -p | --pattern) pattern="$2"; shift ;;
        -t | --test) test="$2"; shift ;;
        -f | --follow) follow=1 ;;
        -h | --help) usage; exit 0 ;;
        --) shift; break ;;
      esac

      shift
    done

    if (($#)); then
      echo "extra arguments: $* - --help lists the options" >&2

      exit 2
    fi

    log=""
    for candidate in work/logs/*.json; do
      [[ -f "${candidate}" ]] || continue
      [[ -z "${log}" || "${candidate}" -nt "${log}" ]] && log="${candidate}"
    done

    if [[ -z "${log}" ]]; then
      echo "no test log yet: run just test first" >&2
      exit 1
    fi

    echo "Log: ${log}" >&2

    # gotestsum's own lines already end in a newline, so trim jq's.
    select='select(.Action == "output")'
    if [[ -n "${test}" ]]; then
      select="${select} | select(.Test != null and (.Test | startswith(\"${test}\")))"
    fi

    # Follow on a terminal, or anywhere -f says to. Piped without it,
    # `| grep FAIL` would never see EOF and hang instead of answering, which is
    # how this gets used from a script.
    if [[ -n "${follow}" ]]; then
      reader=(tail -n +1 -F "${log}")
    else
      reader=(cat "${log}")
    fi

    set +e
    if [[ -z "${pattern}" ]]; then
      "${reader[@]}" | jq --unbuffered -r "${select} | .Output | rtrimstr(\"\n\")"
    else
      "${reader[@]}" | jq --unbuffered -r "${select} | .Output | rtrimstr(\"\n\")" \
        | grep --line-buffered -E "${pattern}"
    fi
    status=$?
    set -e

    # 2 is Ctrl-C, which is the only way out of a follow.
    if [[ "${status}" == 2 ]]; then
      exit 0
    fi

    # 141 is a closed pipe, which is what `just test-log | head` looks like.
    if [[ "${status}" == 141 ]]; then
      exit 0
    fi

    if [[ "${status}" == 1 && -n "${pattern}" ]]; then
      echo "nothing matching '${pattern}'" >&2
      exit 0
    fi

    exit "${status}"
