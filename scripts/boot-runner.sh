#!/usr/bin/env bash
set -euo pipefail
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# Boot counterpart of shutdown-runner.sh: bring back the ramdisk, the pool, the
# nested Incus daemons and the GitHub runner that was published as the "runner"
# image on the last clean shutdown.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RUNNER="${RUNNER:-runner}"
POOL="${RUNNER_POOL:-tmpfs}"
POOL_SOURCE="${RUNNER_POOL_SOURCE:-/mnt/tmpfs}"
TMPFS_SIZE="${RUNNER_TMPFS_SIZE:-32g}"
CERT="${RUNNER_CERT:-work/runner.crt}"

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }

cd "${SCRIPT_DIR}/.."

# shellcheck source=/dev/null
source .env

export INCUS_REMOTE="local-https"
export INCUS_PROJECT="ic-runner"

# --- The ramdisk ------------------------------------------------------------

if mountpoint -q "${POOL_SOURCE}"; then
    step "${POOL_SOURCE} is already mounted"
else
    step "Mounting ${TMPFS_SIZE} of tmpfs on ${POOL_SOURCE}"
    [[ -d "${POOL_SOURCE}" ]] || sudo -n mkdir -p "${POOL_SOURCE}"
    sudo -n mount -t tmpfs -o "size=${TMPFS_SIZE}" none "${POOL_SOURCE}"
fi

# --- The storage pool -------------------------------------------------------

step "Creating storage pool ${POOL}"
incus storage create "${POOL}" dir source="${POOL_SOURCE}" || true

# --- The nested Incus daemons -----------------------------------------------

# setup-nested-incus.sh pushes the runner's own client certificate into each
# nested daemon, and the runner is not up yet to pull a fresh one from.
if [[ ! -f "${CERT}" ]]; then
    warn "missing ${CERT}, restore the runner first and pull it with:"
    warn "  incus --project=ic-runner file pull ${RUNNER}/home/runner/.config/incus/client.crt ${CERT}"
    exit 1
fi

apt_opts=()
if [[ -n "${APT_CACHER_NG:-}" ]]; then
    apt_opts=(-a "${APT_CACHER_NG}")
fi

setup_nested=("${SCRIPT_DIR}/setup-nested-incus.sh"
    -c "$(realpath "${CERT}")" -o -s "${POOL}" -f "${apt_opts[@]}")

step "Creating ict-stable"
"${setup_nested[@]}" -n ict-stable -r stable

step "Creating ict-custom"
"${setup_nested[@]}" -n ict-custom -r stable -p local -b vmbr0

step "Creating ict-lts"
"${setup_nested[@]}" -n ict-lts -r lts-7.0

step "Creating ict-daily"
"${setup_nested[@]}" -n ict-daily -r daily

# --- The runner -------------------------------------------------------------

if incus info "${RUNNER}" >/dev/null 2>&1; then
    step "Starting the existing ${RUNNER}"
    incus start "${RUNNER}" || true
else
    step "Restoring ${RUNNER} from the 'runner' image"
    incus launch runner "${RUNNER}" --storage "${POOL}" \
        -c security.nesting=true \
        -c security.privileged=true
fi
