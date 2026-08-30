#!/usr/bin/env bash
set -euo pipefail
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

# The whole lifecycle of the tmpfs runner host. Everything the runner needs
# lives on a ramdisk, so the GitHub runner is published into the "runner" image
# on the way down and restored from it on the way up.

# shellcheck source=/dev/null
source .env

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RUNNER="${RUNNER:-runner}"
POOL="${RUNNER_POOL:-tmpfs}"
POOL_SOURCE="${RUNNER_POOL_SOURCE:-/mnt/tmpfs}"

# Where the nested daemons live, the runner's own pool unless it is set. Only
# POOL is the ramdisk, so only POOL is created here and dropped again on the way
# down; an ICT pool of its own is expected to exist already and is left alone.
ICT_POOL="${RUNNER_ICT_POOL:-${POOL}}"
TMPFS_SIZE="${RUNNER_TMPFS_SIZE:-32g}"
CERT="${RUNNER_CERT:-work/runner.crt}"
COMPRESSION="${RUNNER_COMPRESSION:-none}"
STOP_TIMEOUT="${RUNNER_STOP_TIMEOUT:-120}"
RUNNER_INCUS_REMOTE="${RUNNER_INCUS_REMOTE:-local}"
RUNNER_INCUS_PROJECT="${RUNNER_INCUS_PROJECT:-ic-runner}"

# Each entry is the instance name followed by its setup-nested-incus.sh flags.
ICTS=(
    "ict-stable -r stable"
    "ict-custom -r stable -p local -b vmbr0"
    "ict-lts -r lts-7.0"
    "ict-daily -r daily"
)

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }
die() { echo "!!  $*" >&2; exit 1; }

# Narrows ICTS down to the one matching $1, given as a full name or as the
# suffix after "ict-".
select_ict() {
    want=$1
    [[ "$want" == ict-* ]] || want="ict-${want}"

    for ict in "${ICTS[@]}"; do
        if [[ "${ict%% *}" == "$want" ]]; then
            ICTS=("$ict")
            return
        fi
    done

    die "unknown ICT '$1', known: ${ICTS[*]%% *}"
}

cd "${SCRIPT_DIR}/.."

export INCUS_REMOTE="${RUNNER_INCUS_REMOTE}"
export INCUS_PROJECT="${RUNNER_INCUS_PROJECT}"

# --- Shared steps -----------------------------------------------------------

# runner_exec runs a shell command inside the runner, as the runner user.
#
# The cd is not optional: exec lands in /root, a login shell does not leave it,
# and the runner user cannot stat it.
runner_exec() { incus exec "${RUNNER}" -- sudo -u runner -H bash -lc "cd \"\${HOME}\" && $*"; }

# Points the runner at the nested daemons. Each one is new and carries a
# certificate of its own, so the remote it replaces has to go first.
readd_remotes() {
    local -a names=("${ICTS[@]%% *}")

    step "Re-adding ${names[*]} on ${RUNNER}"

    for name in "${names[@]}"; do
        runner_exec "INCUS_REMOTE=local incus remote rm ${name} || true"
        runner_exec "INCUS_REMOTE=local incus remote add ${name} ${name} --accept-certificate"
    done

    runner_exec "incus remote list"

    # Adding a remote says nothing about reaching it, so each one is read once.
    for name in "${names[@]}"; do
        step "Testing ${name}"
        runner_exec "incus list ${name}: >/dev/null"
    done
}

stop_runner() {
    step "Stopping ${RUNNER} (project ${INCUS_PROJECT})"
    incus stop "${RUNNER}" --timeout "${STOP_TIMEOUT}" ||
        incus stop "${RUNNER}" --force || true
}

# Non-zero only when the publish itself failed, a botched rotation still leaves
# the new image behind under "runner-current".
publish_runner() {
    step "Publishing ${RUNNER} as 'runner-current'"
    incus publish "${RUNNER}" --alias runner-current \
        --compression "${COMPRESSION}" --reuse || return 1

    step "Rotating the aliases, dropping the previous 'runner-old'"
    incus image delete runner-old || true
    incus image alias rename runner runner-old || true
    incus image alias rename runner-current runner ||
        warn "'runner' still points at the previous image, the new one is 'runner-current'"
}

# --- up ---------------------------------------------------------------------

up() {
    # --- The ramdisk --------------------------------------------------------

    if mountpoint -q "${POOL_SOURCE}"; then
        step "${POOL_SOURCE} is already mounted"
    else
        step "Mounting ${TMPFS_SIZE} of tmpfs on ${POOL_SOURCE}"
        [[ -d "${POOL_SOURCE}" ]] || sudo -n mkdir -p "${POOL_SOURCE}"
        sudo -n mount -t tmpfs -o "size=${TMPFS_SIZE}" none "${POOL_SOURCE}"
    fi

    # --- The storage pool ---------------------------------------------------

    step "Creating storage pool ${POOL}"
    incus storage create "${POOL}" dir source="${POOL_SOURCE}" || true

    # A pool of its own is somebody else's, so say so here rather than let
    # setup-nested-incus.sh fail on it four times.
    if [[ "${ICT_POOL}" != "${POOL}" ]]; then
        incus storage show "${ICT_POOL}" >/dev/null 2>&1 ||
            die "pool ${ICT_POOL} does not exist, RUNNER_ICT_POOL is not created here"

        step "Putting the nested daemons on ${ICT_POOL}"
    fi

    # --- The nested Incus daemons -------------------------------------------

    # setup-nested-incus.sh pushes the runner's own client certificate into each
    # nested daemon, and the runner is not up yet to pull a fresh one from.
    if [[ ! -f "${CERT}" ]]; then
        warn "missing ${CERT}, restore the runner first and pull it with:"
        warn "  incus --project=${INCUS_PROJECT} file pull ${RUNNER}/home/runner/.config/incus/client.crt ${CERT}"
        exit 1
    fi

    apt_opts=()
    if [[ -n "${APT_CACHER_NG:-}" ]]; then
        apt_opts=(-a "${APT_CACHER_NG}")
    fi

    setup_nested=("${SCRIPT_DIR}/setup-nested-incus.sh"
        -c "$(realpath "${CERT}")" -o -s "${ICT_POOL}" -f "${apt_opts[@]}")

    for ict in "${ICTS[@]}"; do
        read -ra spec <<<"${ict}"
        step "Creating ${spec[0]}"
        "${setup_nested[@]}" -n "${spec[@]}"
    done

    # --- The runner ---------------------------------------------------------

    if [[ -n "${ONLY:-}" ]]; then
        step "Leaving ${RUNNER} alone, only ${ICTS[*]%% *} was asked for"
        return
    fi

    if incus info "${RUNNER}" >/dev/null 2>&1; then
        step "Starting the existing ${RUNNER}"
        incus start "${RUNNER}" || true
    else
        step "Restoring ${RUNNER} from the 'runner' image"
        incus launch runner "${RUNNER}" --storage "${POOL}" \
            -c security.nesting=true \
            -c security.privileged=true
    fi

    readd_remotes
}

# --- down -------------------------------------------------------------------

down() {
    # Best effort from here on, a half-torn-down ramdisk still has to end up
    # gone or the on-disk database no longer matches reality after the reboot.
    set +e

    if ! incus info >/dev/null 2>&1; then
        warn "Incus is not reachable, nothing to do"
        return 0
    fi

    # --- One ICT only -------------------------------------------------------

    if [[ -n "${ONLY:-}" ]]; then
        ict="${ICTS[0]%% *}"
        step "Removing ${ict}"
        incus delete --force "${ict}" || warn "could not delete instance ${ict}"
        return 0
    fi

    # --- Save the runner ----------------------------------------------------

    stop_runner
    publish_runner ||
        warn "publish failed, the 'runner' image is the one from the last run"

    # --- Drop everything that lives on the ramdisk --------------------------

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

    return 0
}

# --- backup -----------------------------------------------------------------

backup() {
    # Every exit path has to bring the runner back, including a failed publish.
    restart() {
        step "Starting ${RUNNER} again"
        incus start "${RUNNER}" || true
    }
    trap restart EXIT

    stop_runner
    publish_runner
}

usage() {
    names=("${ICTS[@]%% *}")

    cat <<EOF
Usage: $(basename "$0") <up|down|backup> [-n <ict>]

up      Mount the ramdisk, create the pool and the nested Incus daemons, then
        start ${RUNNER} or restore it from the 'runner' image.
down    Publish ${RUNNER} into the 'runner' image, then drop everything that
        lives on the ramdisk. Best effort, the ramdisk itself stays mounted.
backup  Refresh the 'runner' image without tearing anything down. ${RUNNER} is
        stopped for the whole publish and started again on every exit path.

-n      Only act on that one ICT, named in full or by its suffix
        (${names[*]/#ict-/}), and leave ${RUNNER} and the pool alone.
        Ignored by backup.
EOF
}

cmd="${1:-}"
shift 2>/dev/null || true

while getopts ":n:h" opt; do
    case "$opt" in
        n) ONLY="$OPTARG" ;;
        h) usage; exit 0 ;;
        :) die "-$OPTARG needs an ICT name" ;;
        *) usage >&2; exit 1 ;;
    esac
done

if [[ -n "${ONLY:-}" ]]; then
    select_ict "$ONLY"
fi

case "$cmd" in
    up) up ;;
    down) down ;;
    backup) backup ;;
    -h | --help | help) usage ;;
    *)
        usage >&2
        exit 1
        ;;
esac
