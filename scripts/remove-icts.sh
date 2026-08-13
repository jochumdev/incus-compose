#!/usr/bin/env bash
set -euo pipefail

export INCUS_PROJECT="ic-runner"

declare -a RUNNERS
RUNNERS=( "ict-stable" "ict-custom" "ict-lts" "ict-daily" )

for r in "${RUNNERS[@]}"; do
	echo "Removing: ${r}" >&2
	INCUS_REMOTE=local incus rm -f $r || true
done

incus storage rm tmpfs
