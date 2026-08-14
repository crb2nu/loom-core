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
