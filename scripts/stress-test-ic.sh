#!/usr/bin/env bash
# Copyright (c) 2026 René Jochum <rene@jochum.dev>
# This script is released into the public domain or under CC0-1.0.
# Use it however you want, no restrictions.

#
# Usage: ./stress-test.sh [PAIRS_PER_BATCH] [BATCHES]
#
# Runs PAIRS_PER_BATCH concurrent (simplenginx, twoservices) pairs per batch,
# waits for the batch to finish, then repeats for BATCHES batches. Comparing
# batch 1's timings/probe to batch N's timings/probe isolates degradation
# that accumulates across runs from plain intra-batch concurrency contention.

simplenginx() {
  project_name=$1

  just run --debug -P test/fixtures/simple -p "${project_name}" up --detach
  rc=$?
  just run --debug -P test/fixtures/simple -p "${project_name}" down --project

  # incus-compose --debug -P test/fixtures/simple -p "${project_name}" up --detach
  # rc=$?
  # incus-compose --debug -P test/fixtures/simple -p "${project_name}" down --project

  return "${rc}"
}

# Two services includes ic-healthd
twoservices() {
  project_name=$1

  just run --debug -P test/fixtures/two-services -p "${project_name}" up --detach
  rc=$?
  just run --debug -P test/fixtures/two-services -p "${project_name}" down --project

  # incus-compose --debug -P test/fixtures/two-services -p "${project_name}" up --detach
  # rc=$?
  # incus-compose --debug -P test/fixtures/two-services -p "${project_name}" down --project

  return "${rc}"
}

set -u

if [[ $# -eq 0 ]]; then
  echo "Usage: $0 [PAIRS_PER_BATCH] [BATCHES]"
  echo "  PAIRS_PER_BATCH  concurrent (simplenginx, twoservices) pairs per batch (default: 5)"
  echo "  BATCHES          number of sequential batches (default: 4)"
  echo "No arguments given, running with defaults (5 pairs x 4 batches)."
  echo
fi

PAIRS="${1:-5}"    # concurrent (simplenginx, twoservices) pairs per batch
BATCHES="${2:-4}"  # number of sequential batches

RESULTS_DIR=$(mktemp -d)

run_timed() {
  local label=$1 func=$2 project=$3
  local start end duration status

  start=$(date +%s.%N)
  if "$func" "$project" >"${RESULTS_DIR}/${project}.log" 2>&1; then
    status="ok"
  else
    status="FAIL"
  fi
  end=$(date +%s.%N)
  duration=$(awk "BEGIN{printf \"%.2f\", ${end}-${start}}")

  echo "${duration}" >"${RESULTS_DIR}/${project}.time"
  echo "${status}" >"${RESULTS_DIR}/${project}.status"
  printf "  [%s] %-12s %-20s %6ss\n" "${status}" "${label}" "${project}" "${duration}"
}

summarize_batch() {
  local label=$1 batch=$2
  local -a times=()
  for f in "${RESULTS_DIR}/ic-b${batch}-${label}"*.time; do
    [[ -e "$f" ]] || continue
    times+=("$(cat "$f")")
  done

  local fail_count
  fail_count=$(grep -l "^FAIL$" "${RESULTS_DIR}/b${batch}-${label}"*.status 2>/dev/null | wc -l)

  printf "%s\n" "${times[@]}" | awk -v label="$label" -v fails="$fail_count" '
    { sum += $1; if (NR==1 || $1<min) min=$1; if (NR==1 || $1>max) max=$1; n++ }
    END {
      if (n == 0) { print "  "label": no data"; exit }
      printf "  %-12s runs=%-3d fails=%-3s avg=%6.2fs min=%6.2fs max=%6.2fs\n", label, n, fails, sum/n, min, max
    }
  '
}

declare -a batch_durations

echo "Logs in ${RESULTS_DIR}"
echo
for batch in $(seq 1 "${BATCHES}"); do
  echo "=== Batch ${batch}/${BATCHES} (${PAIRS} concurrent pairs) ==="

  pids=()
  batch_start=$(date +%s.%N)
  for ((i = 1; i <= PAIRS; i++)); do
    run_timed "simplenginx" simplenginx "ic-b${batch}-simplenginx${i}" &
    pids+=("$!")

    run_timed "twoservices" twoservices "ic-b${batch}-twoservices${i}" &
    pids+=("$!")
  done

  for pid in "${pids[@]}"; do
    wait "${pid}"
  done
  batch_end=$(date +%s.%N)
  batch_duration=$(awk "BEGIN{printf \"%.2f\", ${batch_end}-${batch_start}}")

  batch_durations+=("${batch_duration}")

  summarize_batch "simplenginx" "${batch}"
  summarize_batch "twoservices" "${batch}"
  echo "  batch wall-clock=${batch_duration}s"
  echo
done

echo "=== Batch-over-batch comparison ==="
printf "%-8s %-14s %-14s %-14s\n" "batch" "wall-clock" "probe-before" "probe-after"
for ((b = 1; b <= BATCHES; b++)); do
  printf "%-8s %-14s\n" "${b}" "${batch_durations[$((b - 1))]}s"
done

failed_status=$(grep -l "^FAIL$" "${RESULTS_DIR}"/*.status 2>/dev/null || true)
if [[ -n "${failed_status}" ]]; then
  echo
  echo "Failures detected, matching logs:"
  for f in ${failed_status}; do
    echo "${f%.status}.log"
  done
fi

echo
echo "Per-run logs and timings kept in: ${RESULTS_DIR} remove manually when done"
