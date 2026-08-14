#!/usr/bin/env bash
set -euo pipefail
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# On-demand counterpart of shutdown-runner.sh: stop the runner, refresh the
# "runner" image, start it again, so a panic costs less than everything since
# the last clean shutdown. The runner is down for the whole publish.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RUNNER="${RUNNER:-runner}"
COMPRESSION="${RUNNER_COMPRESSION:-none}"
STOP_TIMEOUT="${RUNNER_STOP_TIMEOUT:-120}"

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }

cd "${SCRIPT_DIR}/.."

# shellcheck source=/dev/null
source .env

export INCUS_REMOTE="local-https"
export INCUS_PROJECT="ic-runner"

# Every exit path has to bring the runner back, including a failed publish.
restart() {
    step "Starting ${RUNNER} again"
    incus start "${RUNNER}" || true
}
trap restart EXIT

step "Stopping ${RUNNER}"
incus stop "${RUNNER}" --timeout "${STOP_TIMEOUT}" ||
    incus stop "${RUNNER}" --force || true

step "Publishing ${RUNNER} as 'runner-current'"
incus publish "${RUNNER}" --alias runner-current \
    --compression "${COMPRESSION}" --reuse

step "Rotating the aliases, dropping the previous 'runner-old'"
incus image delete runner-old || true
incus image alias rename runner runner-old || true
incus image alias rename runner-current runner
