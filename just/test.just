# Tests. The stages are one implementation plus an environment, see
# CONTRIBUTING.md: the skip helpers read the env vars below, and each wrapper
# sets its own and calls `test`. The wrappers re-invoke just, which is what lets
# a stage override v_test_timeout - the child parses this file with the stage's
# environment already set.

# How long one run may take. The stage that stands up containers wants more and
# sets it in its own environment before calling us.
v_test_timeout := env("TEST_TIMEOUT", "20m")

# Run tests against nested Incus, includes direct incus tests.
[env("INCUS_COMPOSE_IMAGE_CACHE", "incus-compose-tests-cache")]
test folder="./..." *args: lint
    mkdir -p test/logs
    export DATE=`date +%Y%m%d-%H%M%S`; \
      gotestsum --hide-summary=skipped --format testname --jsonfile=test/logs/${DATE}.json --packages={{ folder }} \
        --post-run-command "bash -c 'echo; echo Slowest tests; gotestsum tool slowest --num 10 --jsonfile test/logs/${DATE}.json'" \
        -- -race -parallel {{ v_test_procs }} -timeout {{ v_test_timeout }} -covermode atomic -coverprofile test/logs/${DATE}-cover.out -v "${@:2}"

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
test-all folder="./..." *args:
    @just test "$@"

# Run the tests with a race detector.
test-race folder="./..." *args:
    mkdir -p test/logs
    gotestsum --hide-summary=skipped --jsonfile=test/logs/`date +%Y%m%d-%H%M%S`.json --packages={{ folder }} -- -parallel {{ v_test_procs }} -timeout {{ v_test_timeout }} -race -v "${@:2}"

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

#   just test-log                       # everything the run printed
#   just test-log 'FAIL|Error:'         # only matching lines (extended regex)
#   just test-log '' TestE2EDownNoDeps  # only one test's output
#
# On a terminal it follows until Ctrl-C, so it can be started while a run is
# going; piped it reads once and exits. The newest log is picked at startup, so
# a run launched after this is not the one being followed.
[doc("Plain text of the newest test/logs/*.json. Follows on a terminal")]
test-log pattern="" test="":
    #!/usr/bin/env bash
    set -euo pipefail

    log=""
    for candidate in test/logs/*.json; do
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
    if [[ -n "{{ test }}" ]]; then
      select="${select} | select(.Test != null and (.Test | startswith(\"{{ test }}\")))"
    fi

    # Follow only on a terminal. Piped, `| grep FAIL` would never see EOF and
    # hang instead of answering, which is how this gets used from a script.
    if [[ -t 1 ]]; then
      reader=(tail -n +1 -F "${log}")
    else
      reader=(cat "${log}")
    fi

    set +e
    if [[ -z "{{ pattern }}" ]]; then
      "${reader[@]}" | jq --unbuffered -r "${select} | .Output | rtrimstr(\"\n\")"
    else
      "${reader[@]}" | jq --unbuffered -r "${select} | .Output | rtrimstr(\"\n\")" \
        | grep --line-buffered -E "{{ pattern }}"
    fi
    status=$?
    set -e

    # 130 is Ctrl-C, which is the only way out of a follow.
    if [[ "${status}" == 130 || "${status}" == 2 ]]; then
      exit 0
    fi

    # 141 is a closed pipe, which is what `just test-log | head` looks like.
    if [[ "${status}" == 141 ]]; then
      exit 0
    fi

    if [[ "${status}" == 1 && -n "{{ pattern }}" ]]; then
      echo "nothing matching '{{ pattern }}'" >&2
      exit 0
    fi

    exit "${status}"
