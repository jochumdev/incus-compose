#!/usr/bin/bash

POOL="${RUNNER_POOL:-tmpfs}"
POOL_SOURCE="${RUNNER_POOL_SOURCE:-/mnt/tmpfs}"
TMPFS_SIZE="${RUNNER_TMPFS_SIZE:-32g}"
TMPFS_INCUS_REMOTE="local-https"
TMPFS_INCUS_PROJECT="ic-runner"

ICTS=(
    "ict-daily-dev01-default"
    "ict-daily-dev01-wt01"
    "ict-daily-dev01-wt02"
)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step() { echo "==> $*" >&2; }
warn() { echo "!!  $*" >&2; }
die() { echo "!!  $*" >&2; exit 1; }

# Narrows ICTS down to the one matching $1, given as a full name or as the
# suffix after "ict-daily-dev01-".
select_ict() {
    want=$1
    [[ "$want" == ict-* ]] || want="ict-daily-dev01-${want}"

    for name in "${ICTS[@]}"; do
        if [[ "$name" == "$want" ]]; then
            ICTS=("$name")
            return
        fi
    done

    die "unknown ICT '$1', known: ${ICTS[*]}"
}

cd "${ROOT}" || exit 1

# .env points INCUS_REMOTE at a nested daemon, the exports below have to win.
# shellcheck source=/dev/null
source .env

export INCUS_REMOTE="${TMPFS_INCUS_REMOTE}"
export INCUS_PROJECT="${TMPFS_INCUS_PROJECT}"

create_ict() {
  name=$1

  apt_opts=()
  if [[ -n "${APT_CACHER_NG:-}" ]]; then
      apt_opts=(-a "${APT_CACHER_NG}")
  fi

  step "Creating $name"
  "${ROOT}/scripts/setup-nested-incus.sh" -c "$HOME/.config/incus/client.crt" -n "$name" -r daily -o -s "${POOL}" -f "${apt_opts[@]}"

  # ip
  ip="$(incus list --format=json | jq -r '.[] | select(.name == "'"${name}"'") | .state .network .eth0 .addresses .[] | select(.family == "inet") | .address')"

  # Local
  incus remote remove "$name" || true
  incus remote add "$name" "$ip" --accept-certificate

  # Token
  token="$(incus config trust add -q "${name}:dev01")"
  echo "$token"

  # On dev01
  incus --project=default exec dev01 -- sudo -u r3j0 incus remote remove "$name" 2>/dev/null || true
  incus --project=default exec dev01 -- sudo -u r3j0 incus remote add "$name" "$ip" --accept-certificate --token="${token}"
}

delete_ict() {
  name=$1

  step "Deleting $name"
  incus delete --force "$name" || warn "could not delete instance $name"

  # Local
  incus remote remove "$name" 2>/dev/null || true

  # On dev01
  incus --project=default exec dev01 -- sudo -u r3j0 incus remote remove "$name" 2>/dev/null || true
}

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
    incus storage create "${INCUS_REMOTE}:${POOL}" dir source="${POOL_SOURCE}" 2>/dev/null || true

    # --- The nested Incus daemons -------------------------------------------

    for name in "${ICTS[@]}"; do
        create_ict "$name"
    done
}

down() {
    # --- The nested Incus daemons -------------------------------------------

    for name in "${ICTS[@]}"; do
        delete_ict "$name"
    done

    # --- The storage pool ---------------------------------------------------

    if [[ -n "${ONLY:-}" ]]; then
        step "Keeping storage pool ${POOL}, the other ICTs still use it"
        return
    fi

    step "Deleting storage pool ${POOL}"
    incus storage delete "${INCUS_REMOTE}:${POOL}" ||
        warn "pool ${POOL} stayed behind, whatever is left on it still uses it"
}

usage() {
    cat <<EOF
Usage: $(basename "$0") <up|down> [-n <ict>]

up      Mount the ramdisk, create the pool and the nested Incus daemons,
        registering each one as a remote here and on dev01.
down    Delete the nested Incus daemons, their remotes and the pool.
        Best effort, the ramdisk itself stays mounted.

-n      Only act on that one ICT, named in full or by its suffix
        (${ICTS[*]/#ict-daily-dev01-/}). "down -n" keeps the pool.
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

[[ -n "${ONLY:-}" ]] && select_ict "$ONLY"

case "$cmd" in
    up) up ;;
    down) down ;;
    -h | --help | help) usage ;;
    *)
        usage >&2
        exit 1
        ;;
esac
