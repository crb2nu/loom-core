#!/usr/bin/env bash

set -euo pipefail

fleet_benchmark_packages=(
  ./internal/daemon
  ./pkg/transport/muxstdio
  ./pkg/mills/store
  ./cmd/custom-server
)
# Consumed by the gate and orchestration test after sourcing this file.
# shellcheck disable=SC2034
fleet_benchmark_rounds=11
fleet_benchmark_benchtime=1s
# Extra rounds a package may run to replace pairs skipped on a sample
# timeout. Bounded so a persistently slow package still fails the gate (the
# comparator needs fleet_benchmark_rounds pairs) instead of consuming the
# whole benchmark deadline.
# shellcheck disable=SC2034
fleet_benchmark_makeup_rounds=4
# Wall-clock cap for ONE benchmark binary run (all BenchmarkFleet* in a
# package, benchtime 1s each). 30s was the original value; on 2026-09-10 a
# runner node at load average 22–27 on 28 threads pushed the SQLite-backed
# store package past it on both sides and the whole phase aborted with
# "0/4 scenarios" while the benchmarks themselves were healthy (35µs/op).
# shellcheck disable=SC2034
fleet_benchmark_sample_timeout_seconds=90

fleet_benchmark_binary_path() {
  local binary_dir="$1"
  local side="$2"
  local package="$3"
  local package_slug="${package#./}"
  package_slug="${package_slug//\//-}"
  printf '%s/%s-%s.test\n' "$binary_dir" "$side" "$package_slug"
}

compile_fleet_benchmark_binaries() {
  local directory="$1"
  local binary_dir="$2"
  local side="$3"
  local package
  local binary

  mkdir -p "$binary_dir"
  for package in "${fleet_benchmark_packages[@]}"; do
    binary="$(fleet_benchmark_binary_path "$binary_dir" "$side" "$package")"
    (
      cd "$directory"
      GOWORK=off CGO_ENABLED=0 GOMAXPROCS=2 go test \
        -p=1 \
        -c \
        -o "$binary" \
        "$package"
    )
  done
}

start_fleet_benchmark_round() {
  local output="$1"
  local round="$2"

  printf '# paired-round=%d\n' "$round" >>"$output"
}

# run_paired_fleet_benchmark_rounds SAMPLER BASE_OUTPUT CANDIDATE_OUTPUT
#
# Drives the paired rounds. SAMPLER is invoked as `SAMPLER SIDE PACKAGE
# TMP_OUTPUT`, must write that side's benchmark lines to TMP_OUTPUT, and
# steers the phase with its exit status:
#   0    sample recorded
#   124  the sample timed out — this package's (base, candidate) pair for the
#        round is SKIPPED (nothing reaches either output, a `# skipped-pair`
#        comment records it on both) and the phase keeps going
#   125  the benchmark deadline is reached — the phase stops, returns 124
#   *    any other status stops the phase and is returned verbatim
#
# A pair reaches the real outputs only when BOTH sides succeeded, so the
# comparator's positional pairing (baseline[i] against candidate[i]) can
# never drift by one side. After the nominal rounds, packages still short of
# fleet_benchmark_rounds pairs get up to fleet_benchmark_makeup_rounds extra
# rounds; packages already at quota sit those out. Returns 0 when the rounds
# ran to completion — whether every package reached its quota is the
# comparator's verdict ("need at least N paired samples").
run_paired_fleet_benchmark_rounds() {
  local sampler="$1"
  local base_output="$2"
  local candidate_output="$3"
  local -a pairs_done=()
  local i package round side_a side_b tmp_a tmp_b status pending max_rounds
  local package_count="${#fleet_benchmark_packages[@]}"

  max_rounds=$((fleet_benchmark_rounds + fleet_benchmark_makeup_rounds))
  for ((i = 0; i < package_count; i++)); do
    pairs_done[i]=0
  done

  for ((round = 1; round <= max_rounds; round++)); do
    pending=0
    for ((i = 0; i < package_count; i++)); do
      if ((pairs_done[i] < fleet_benchmark_rounds)); then
        pending=1
      fi
    done
    if ((pending == 0)); then
      break
    fi
    if ((round > fleet_benchmark_rounds)); then
      echo "Benchmark make-up round ${round}: replacing pairs skipped on sample timeouts" >&2
    fi
    start_fleet_benchmark_round "$base_output" "$round"
    start_fleet_benchmark_round "$candidate_output" "$round"
    for ((i = 0; i < package_count; i++)); do
      package="${fleet_benchmark_packages[i]}"
      if ((pairs_done[i] >= fleet_benchmark_rounds)); then
        continue
      fi
      # Alternate which side goes first so drift within a round cancels
      # across rounds instead of always favouring one side.
      if ((round % 2 == 1)); then
        side_a=base
        side_b=candidate
      else
        side_a=candidate
        side_b=base
      fi
      tmp_a="$(mktemp "${TMPDIR:-/tmp}/loom-fleet-sample.XXXXXX")"
      tmp_b="$(mktemp "${TMPDIR:-/tmp}/loom-fleet-sample.XXXXXX")"
      set +e
      "$sampler" "$side_a" "$package" "$tmp_a"
      status=$?
      if ((status == 0)); then
        "$sampler" "$side_b" "$package" "$tmp_b"
        status=$?
      fi
      set -e
      case "$status" in
        0)
          if [[ "$side_a" == base ]]; then
            cat "$tmp_a" >>"$base_output"
            cat "$tmp_b" >>"$candidate_output"
          else
            cat "$tmp_a" >>"$candidate_output"
            cat "$tmp_b" >>"$base_output"
          fi
          pairs_done[i]=$((pairs_done[i] + 1))
          ;;
        124)
          echo "Benchmark sample for ${package} timed out in round ${round} (${fleet_benchmark_sample_timeout_seconds}s); pair skipped" >&2
          printf '# skipped-pair round=%d package=%s cause=sample_timeout\n' "$round" "$package" >>"$base_output"
          printf '# skipped-pair round=%d package=%s cause=sample_timeout\n' "$round" "$package" >>"$candidate_output"
          ;;
        125)
          rm -f "$tmp_a" "$tmp_b"
          printf 'LOOM_BENCHMARK_STOP status=124 cause=deadline sample_timeout_seconds=%d\n' "$fleet_benchmark_sample_timeout_seconds" >>"$base_output"
          printf 'LOOM_BENCHMARK_STOP status=124 cause=deadline sample_timeout_seconds=%d\n' "$fleet_benchmark_sample_timeout_seconds" >>"$candidate_output"
          return 124
          ;;
        *)
          rm -f "$tmp_a" "$tmp_b"
          printf 'LOOM_BENCHMARK_STOP status=%d cause=sample_failure sample_timeout_seconds=%d\n' "$status" "$fleet_benchmark_sample_timeout_seconds" >>"$base_output"
          printf 'LOOM_BENCHMARK_STOP status=%d cause=sample_failure sample_timeout_seconds=%d\n' "$status" "$fleet_benchmark_sample_timeout_seconds" >>"$candidate_output"
          return "$status"
          ;;
      esac
      rm -f "$tmp_a" "$tmp_b"
    done
  done
  return 0
}

run_fleet_benchmark_sample() {
  local directory="$1"
  local binary_dir="$2"
  local side="$3"
  local package="$4"
  local output="$5"
  local binary

  binary="$(fleet_benchmark_binary_path "$binary_dir" "$side" "$package")"
  (
    cd "$directory/${package#./}"
    GOWORK=off CGO_ENABLED=0 GOMAXPROCS=2 "$binary" \
      -test.run='^$' \
      -test.bench='^BenchmarkFleet' \
      -test.benchmem \
      -test.benchtime="$fleet_benchmark_benchtime" \
      -test.count=1
  ) | tee -a "$output"
}
