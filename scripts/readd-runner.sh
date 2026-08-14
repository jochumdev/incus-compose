#!/usr/bin/env bash
set -euo pipefail

declare -a RUNNERS
RUNNERS=( "ict-stable" "ict-custom" "ict-lts" "ict-daily" )

for r in "${RUNNERS[@]}"; do
	echo "Readding: ${r}" >&2
	INCUS_REMOTE=local incus remote rm "$r" || true
	INCUS_REMOTE=local incus remote add "$r" "$r" --accept-certificate || true
done

incus remote list

for r in "${RUNNERS[@]}"; do
	echo "Testing: ${r}" >&2
	incus list "${r}:" 1>/dev/null
done
