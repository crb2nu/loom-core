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
