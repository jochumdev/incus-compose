#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

export INCUS_REMOTE="local-https"
export INCUS_PROJECT="ic-runner"

incus storage create tmpfs dir source=/mnt/tmpfs || true

echo "Creating: ict-stable" >&2
./setup-nested-incus.sh -c "${SCRIPT_DIR}/../work/runner.crt" -n ict-stable -r stable -o -s tmpfs -f

echo "Creating: ict-custom" >&2
./setup-nested-incus.sh -c "${SCRIPT_DIR}/../work/runner.crt" -n ict-custom -r stable -p local -b vmbr0 -o -s tmpfs -f

echo "Creating: ict-lts" >&2
./setup-nested-incus.sh -c "${SCRIPT_DIR}/../work/runner.crt" -n ict-lts -r lts-7.0 -o -s tmpfs -f

echo "Creating: ict-daily" >&2
./setup-nested-incus.sh -c "${SCRIPT_DIR}/../work/runner.crt" -n ict-daily -r daily -o -s tmpfs -f

