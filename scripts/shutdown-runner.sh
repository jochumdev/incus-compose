#!/usr/bin/env bash
set -uo pipefail
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# Stateful shutdown for the tmpfs setup: publish the GitHub runner into an image
# so it survives the reboot, then drop everything that lives on the ramdisk so
# the on-disk database still matches reality after the next boot.

RUNNER="${RUNNER:-runner}"
POOL="${RUNNER_POOL:-tmpfs}"
COMPRESSION="${RUNNER_COMPRESSION:-none}"
STOP_TIMEOUT="${RUNNER_STOP_TIMEOUT:-120}"

export INCUS_REMOTE="${INCUS_REMOTE:-local}"

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }

if ! incus info >/dev/null 2>&1; then
    warn "Incus is not reachable, nothing to do"
    exit 0
fi

# --- Save the runner --------------------------------------------------------

export INCUS_PROJECT="ic-runner"

step "Stopping ${RUNNER} (project ${INCUS_PROJECT})"
incus stop "${RUNNER}" --timeout "${STOP_TIMEOUT}" ||
    incus stop "${RUNNER}" --force || true

step "Publishing ${RUNNER} as 'runner-current'"
if incus publish "${RUNNER}" --alias runner-current \
    --compression "${COMPRESSION}" --reuse; then
    step "Rotating the aliases, dropping the previous 'runner-old'"
    incus image delete runner-old || true
    incus image alias rename runner runner-old || true
    incus image alias rename runner-current runner
else
    warn "publish failed, the 'runner' image is the one from the last run"
fi

# --- Drop everything that lives on the ramdisk ------------------------------

step "Removing the nested Incus containers"
for ict in $(incus list --format csv -c n ict-); do
    incus delete --force "${ict}" || warn "could not delete instance ${ict}"
done

if incus storage show "${POOL}" >/dev/null 2>&1; then
    step "Removing what is left on pool ${POOL}"
    volumes="$(incus storage volume list "${POOL}" --all-projects --format csv -c en)"
    while IFS=, read -r project name; do
        incus delete --force --project "${project}" "${name}" ||
            warn "could not delete instance ${project}/${name}"
    done <<<"${volumes}"

    step "Removing storage pool ${POOL}"
    if ! incus storage delete "${POOL}"; then
        warn "pool ${POOL} stayed behind, it is still used by:"
        incus storage show "${POOL}" >&2 || true
    fi
fi

exit 0
