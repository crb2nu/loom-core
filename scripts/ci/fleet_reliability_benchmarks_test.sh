#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scripts/ci/fleet_reliability_benchmarks.sh
source "$script_dir/fleet_reliability_benchmarks.sh"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-fleet-benchmark-test.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p "$tmp_dir/bin" "$tmp_dir/base" "$tmp_dir/candidate"
for package in "${fleet_benchmark_packages[@]}"; do
  mkdir -p "$tmp_dir/base/${package#./}" "$tmp_dir/candidate/${package#./}"
done

export FLEET_BENCHMARK_COMPILE_LOG="$tmp_dir/compile.log"
export FLEET_BENCHMARK_EXECUTE_LOG="$tmp_dir/execute.log"
: >"$FLEET_BENCHMARK_COMPILE_LOG"
: >"$FLEET_BENCHMARK_EXECUTE_LOG"

# The single-quoted fixture body is intentionally expanded by the generated
# fake go command, not by this test harness.
# shellcheck disable=SC2016
{
  printf '%s\n' '#!/usr/bin/env bash'
  printf '%s\n' 'set -euo pipefail'
  printf '%s\n' 'output=""' 'package=""'
  printf '%s\n' 'while (($#)); do'
  printf '%s\n' '  case "$1" in'
  printf '%s\n' '    -o) output="$2"; shift 2 ;;'
  printf '%s\n' '    ./*) package="$1"; shift ;;'
  printf '%s\n' '    *) shift ;;'
  printf '%s\n' '  esac' 'done'
  printf '%s\n' 'case "$package" in'
  printf '%s\n' '  ./internal/daemon) benchmark=BenchmarkFleetDaemonEventPublish ;;'
  printf '%s\n' '  ./pkg/transport/muxstdio) benchmark=BenchmarkFleetMuxRoundTrip ;;'
  printf '%s\n' '  ./pkg/mills/store) benchmark=BenchmarkFleetMillsEventAppend ;;'
  printf '%s\n' '  ./cmd/custom-server) benchmark=BenchmarkFleetCustomServerWrite ;;'
  printf '%s\n' '  *) printf "unexpected package: %s\n" "$package" >&2; exit 1 ;;'
  printf '%s\n' 'esac'
  printf '%s\n' 'case "$(basename "$output")" in'
  printf '%s\n' '  base-*) side=base ;;'
  printf '%s\n' '  candidate-*) side=candidate ;;'
  printf '%s\n' '  *) printf "unexpected benchmark binary: %s\n" "$output" >&2; exit 1 ;;'
  printf '%s\n' 'esac'
  printf '%s\n' 'printf "%s\n" "$package" >>"$FLEET_BENCHMARK_COMPILE_LOG"'
  printf '%s\n' 'printf "%s\n" "#!/usr/bin/env bash" >"$output"'
  printf '%s\n' 'printf '\''printf "%%s|%%s|%%s|%%s\\n" %q %q "$GOMAXPROCS" "$*" >>"$FLEET_BENCHMARK_EXECUTE_LOG"\n'\'' "$side" "$benchmark" >>"$output"'
  printf '%s\n' 'printf '\''printf "%%s-2\\t1\\t100 ns/op\\t16 B/op\\t1 allocs/op\\n" %q\n'\'' "$benchmark" >>"$output"'
  printf '%s\n' 'chmod +x "$output"'
} >"$tmp_dir/bin/go"
chmod +x "$tmp_dir/bin/go"

PATH="$tmp_dir/bin:$PATH" compile_fleet_benchmark_binaries "$tmp_dir/base" "$tmp_dir/binaries" base
PATH="$tmp_dir/bin:$PATH" compile_fleet_benchmark_binaries "$tmp_dir/candidate" "$tmp_dir/binaries" candidate

base_output="$tmp_dir/base.txt"
candidate_output="$tmp_dir/candidate.txt"
: >"$base_output"
: >"$candidate_output"

for ((round = 1; round <= fleet_benchmark_rounds; round++)); do
  start_fleet_benchmark_round "$base_output" "$round"
  start_fleet_benchmark_round "$candidate_output" "$round"
  for package in "${fleet_benchmark_packages[@]}"; do
    if ((round % 2 == 1)); then
      run_fleet_benchmark_sample "$tmp_dir/base" "$tmp_dir/binaries" base "$package" "$base_output" >/dev/null
      run_fleet_benchmark_sample "$tmp_dir/candidate" "$tmp_dir/binaries" candidate "$package" "$candidate_output" >/dev/null
    else
      run_fleet_benchmark_sample "$tmp_dir/candidate" "$tmp_dir/binaries" candidate "$package" "$candidate_output" >/dev/null
      run_fleet_benchmark_sample "$tmp_dir/base" "$tmp_dir/binaries" base "$package" "$base_output" >/dev/null
    fi
  done
done

if [[ "$(wc -l <"$FLEET_BENCHMARK_COMPILE_LOG" | tr -d ' ')" != "8" ]]; then
  echo "ERROR: expected exactly eight benchmark compilations" >&2
  exit 1
fi

expected_flags='-test.run=^$ -test.bench=^BenchmarkFleet -test.benchmem -test.benchtime=1s -test.count=1'
benchmark_order='BenchmarkFleetDaemonEventPublish BenchmarkFleetMuxRoundTrip BenchmarkFleetMillsEventAppend BenchmarkFleetCustomServerWrite'
if awk -F '|' -v expected_flags="$expected_flags" -v benchmark_order="$benchmark_order" '
  BEGIN { split(benchmark_order, benchmarks, " ") }
  {
    sample_index = NR - 1
    round = int(sample_index / 8) + 1
    within_round = sample_index % 8
    benchmark = benchmarks[int(within_round / 2) + 1]
    pair_position = within_round % 2
    first_side = (round % 2 == 1) ? "base" : "candidate"
    expected_side = (pair_position == 0) ? first_side : ((first_side == "base") ? "candidate" : "base")
    if ($1 != expected_side || $2 != benchmark || $3 != "2" || $4 != expected_flags) {
      exit 1
    }
  }
  END { if (NR != 88) exit 1 }
' "$FLEET_BENCHMARK_EXECUTE_LOG"; then
  :
else
  echo "ERROR: benchmark execution order, pairing, or flags changed" >&2
  exit 1
fi

benchmarks=(
  BenchmarkFleetDaemonEventPublish
  BenchmarkFleetMuxRoundTrip
  BenchmarkFleetMillsEventAppend
  BenchmarkFleetCustomServerWrite
)
for benchmark in "${benchmarks[@]}"; do
  for output in "$base_output" "$candidate_output"; do
    if [[ "$(grep -c "^${benchmark}-" "$output")" != "11" ]]; then
      echo "ERROR: expected eleven $benchmark samples in $output" >&2
      exit 1
    fi
  done
done

echo "fleet reliability benchmark orchestration passed: 8 builds, 88 runs, 11 samples per side"

# ---------------------------------------------------------------------------
# Paired-round driver: a timed-out sample skips its pair and is made up in an
# extra round; a deadline stops the phase with equal sample counts on both
# sides; any other sampler failure stops the phase and is returned verbatim.
# ---------------------------------------------------------------------------

driver_benchmark_for_package() {
  case "$1" in
    ./internal/daemon) printf 'BenchmarkFleetDaemonEventPublish' ;;
    ./pkg/transport/muxstdio) printf 'BenchmarkFleetMuxRoundTrip' ;;
    ./pkg/mills/store) printf 'BenchmarkFleetMillsEventAppend' ;;
    ./cmd/custom-server) printf 'BenchmarkFleetCustomServerWrite' ;;
    *) printf 'unexpected package: %s\n' "$1" >&2; return 1 ;;
  esac
}

driver_record_sample() {
  local side="$1" package="$2" output="$3"
  printf '%s-2\t1\t100 ns/op\t16 B/op\t1 allocs/op\n' "$(driver_benchmark_for_package "$package")" >>"$output"
  printf 'LOOM_BENCHMARK_SAMPLE wall_seconds=1\n' >>"$output"
  printf '%s|%s\n' "$side" "$package" >>"$driver_calls"
}

driver_assert_pairs() {
  local base="$1" candidate="$2" expected="$3" benchmark base_count candidate_count
  for benchmark in BenchmarkFleetDaemonEventPublish BenchmarkFleetMuxRoundTrip BenchmarkFleetMillsEventAppend BenchmarkFleetCustomServerWrite; do
    base_count="$(grep -c "^${benchmark}-" "$base" || true)"
    candidate_count="$(grep -c "^${benchmark}-" "$candidate" || true)"
    if [[ "$base_count" != "$candidate_count" ]]; then
      echo "ERROR: $benchmark sample counts drifted: base=$base_count candidate=$candidate_count" >&2
      exit 1
    fi
    if [[ -n "$expected" && "$base_count" != "$expected" ]]; then
      echo "ERROR: expected $expected $benchmark pairs, got $base_count" >&2
      exit 1
    fi
  done
}

driver_dir="$tmp_dir/driver"
mkdir -p "$driver_dir"

# Case 1: the base side of pkg/mills/store times out once (round 1). The pair
# is skipped, every other package proceeds, and a single make-up round (12)
# restores eleven pairs for the store while the others sit it out.
driver_calls="$driver_dir/calls-timeout.log"
: >"$driver_calls"
store_timeouts=0
sampler_timeout_once() {
  if [[ "$2" == ./pkg/mills/store && "$1" == base && "$store_timeouts" -eq 0 ]]; then
    store_timeouts=1
    return 124
  fi
  driver_record_sample "$1" "$2" "$3"
}
base_output="$driver_dir/timeout-base.txt"
candidate_output="$driver_dir/timeout-candidate.txt"
: >"$base_output"
: >"$candidate_output"
run_paired_fleet_benchmark_rounds sampler_timeout_once "$base_output" "$candidate_output"
driver_assert_pairs "$base_output" "$candidate_output" 11
for output in "$base_output" "$candidate_output"; do
  if [[ "$(grep -c '^# skipped-pair round=1 package=./pkg/mills/store cause=sample_timeout$' "$output")" != "1" ]]; then
    echo "ERROR: expected one skipped-pair marker for the store in $output" >&2
    exit 1
  fi
  if [[ "$(grep -c '^# paired-round=12$' "$output")" != "1" ]]; then
    echo "ERROR: expected exactly one make-up round in $output" >&2
    exit 1
  fi
  if grep -q '^# paired-round=13$' "$output"; then
    echo "ERROR: make-up rounds must stop once every package has its pairs" >&2
    exit 1
  fi
  if grep -q '^LOOM_BENCHMARK_STOP' "$output"; then
    echo "ERROR: a skipped pair must not stop the phase" >&2
    exit 1
  fi
done
# Recorded calls: the timed-out base sample records nothing and its partner
# never runs, so round 1 contributes no store calls; rounds 2–11 contribute
# ten pairs and the make-up round the eleventh. 3 × 22 + 22 = 88, and the
# store's 22 recorded calls are exactly eleven pairs.
if [[ "$(wc -l <"$driver_calls" | tr -d ' ')" != "88" ]]; then
  echo "ERROR: expected 88 recorded sampler calls with one skipped pair and one make-up pair, got $(wc -l <"$driver_calls" | tr -d ' ')" >&2
  exit 1
fi
if [[ "$(grep -c '|./pkg/mills/store$' "$driver_calls")" != "22" ]]; then
  echo "ERROR: expected the store to record 22 samples (eleven pairs after one skipped pair)" >&2
  exit 1
fi

# Case 2: the deadline arrives on the SECOND side of a pair. The half-sampled
# pair is discarded, both outputs carry the deadline STOP marker, counts stay
# equal, and the driver reports 124.
driver_calls="$driver_dir/calls-deadline.log"
: >"$driver_calls"
deadline_calls=0
sampler_deadline() {
  deadline_calls=$((deadline_calls + 1))
  if ((deadline_calls == 10)); then
    return 125
  fi
  driver_record_sample "$1" "$2" "$3"
}
base_output="$driver_dir/deadline-base.txt"
candidate_output="$driver_dir/deadline-candidate.txt"
: >"$base_output"
: >"$candidate_output"
if run_paired_fleet_benchmark_rounds sampler_deadline "$base_output" "$candidate_output"; then
  echo "ERROR: deadline must stop the benchmark phase" >&2
  exit 1
else
  driver_status="$?"
fi
if [[ "$driver_status" != "124" ]]; then
  echo "ERROR: deadline should surface as status 124, got $driver_status" >&2
  exit 1
fi
driver_assert_pairs "$base_output" "$candidate_output" ""
# Nine successful calls = four complete pairs plus one orphaned first side.
if [[ "$(grep -c '^BenchmarkFleet' "$base_output")" != "4" || "$(grep -c '^BenchmarkFleet' "$candidate_output")" != "4" ]]; then
  echo "ERROR: expected four pairs on each side before the deadline" >&2
  exit 1
fi
for output in "$base_output" "$candidate_output"; do
  if [[ "$(grep -c '^LOOM_BENCHMARK_STOP status=124 cause=deadline sample_timeout_seconds=' "$output")" != "1" ]]; then
    echo "ERROR: expected one deadline STOP marker in $output" >&2
    exit 1
  fi
done

# Case 3: a genuine sample failure stops the phase and is returned verbatim.
driver_calls="$driver_dir/calls-failure.log"
: >"$driver_calls"
sampler_failure() {
  if [[ "$2" == ./cmd/custom-server ]]; then
    return 3
  fi
  driver_record_sample "$1" "$2" "$3"
}
base_output="$driver_dir/failure-base.txt"
candidate_output="$driver_dir/failure-candidate.txt"
: >"$base_output"
: >"$candidate_output"
if run_paired_fleet_benchmark_rounds sampler_failure "$base_output" "$candidate_output"; then
  echo "ERROR: a sample failure must stop the benchmark phase" >&2
  exit 1
else
  driver_status="$?"
fi
if [[ "$driver_status" != "3" ]]; then
  echo "ERROR: sample failure status should be returned verbatim, got $driver_status" >&2
  exit 1
fi
driver_assert_pairs "$base_output" "$candidate_output" ""
for output in "$base_output" "$candidate_output"; do
  if [[ "$(grep -c '^LOOM_BENCHMARK_STOP status=3 cause=sample_failure' "$output")" != "1" ]]; then
    echo "ERROR: expected one sample_failure STOP marker in $output" >&2
    exit 1
  fi
done

echo "fleet reliability paired-round driver passed: skip-on-timeout + make-up round, deadline stop, failure passthrough"
