#!/usr/bin/env bash

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

manifest_source="$repo_root/scripts/ci/fleet_reliability_suite_v1.json"
run_id="${CI_JOB_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
artifact_dir="${LOOM_RELIABILITY_ARTIFACT_DIR:-$repo_root/.loom/local/evidence/fleet-reliability/$run_id}"
manifest="$artifact_dir/suite-manifest.json"
gate_bin="${TMPDIR:-/tmp}/loom-fleet-reliability-gate-${run_id}"
benchmark_bin_dir=""
base_worktree=""
base_sha=""
current_stage="initialize"
started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# The CI wrapper allows 1080s for this script. Bound benchmark evidence to the
# first 930s, reserving 90s for the mandatory mixed-load test, 30s for an
# in-flight benchmark sample, and 30s for comparison and final evidence.
# Keep this value in sync with test:reliability in .gitlab-ci.yml.
reliability_shell_budget_seconds=1080
mixed_load_reserve_seconds=150
benchmark_deadline_seconds=$((reliability_shell_budget_seconds - mixed_load_reserve_seconds))
benchmark_sample_timeout_seconds=30

benchmark_files=(
  internal/daemon/fleet_reliability_benchmark_test.go
  pkg/transport/muxstdio/fleet_reliability_benchmark_test.go
  pkg/mills/store/fleet_reliability_benchmark_test.go
  cmd/custom-server/fleet_reliability_benchmark_test.go
)
config_files=(
  .gitlab-ci.yml
  Makefile
  cmd/fleet-reliability-gate/main.go
  internal/daemon/cache_revision.go
  internal/daemon/callpipeline.go
  internal/daemon/callpipeline_errors.go
  internal/daemon/callpipeline_routing.go
  internal/daemon/callpipeline_stages.go
  internal/daemon/callpipeline_timeout.go
  internal/daemon/daemon.go
  internal/daemon/daemon_dispatch_ops.go
  internal/daemon/daemon_dispatch_otel.go
  internal/daemon/daemon_dispatch_status.go
  internal/daemon/daemon_hub_keepalive.go
  internal/daemon/daemon_lifecycle.go
  internal/daemon/daemon_loops.go
  internal/daemon/daemon_new.go
  internal/daemon/daemon_resources.go
  internal/daemon/daemon_toolcache.go
  internal/daemon/daemon_tools_cache.go
  internal/daemon/daemon_tools_fetch.go
  internal/daemon/daemon_tools_handlers.go
  internal/daemon/generation/supervisor.go
  internal/daemon/health.go
  internal/daemon/hub_transport.go
  internal/daemon/local_checkout.go
  internal/daemon/manifest.go
  internal/daemon/muxcache.go
  internal/daemon/proc_stop.go
  internal/daemon/process_controller.go
  internal/daemon/reload_runtime.go
  internal/daemon/server_supervisor.go
  internal/daemon/tool_fetch.go
  internal/daemon/tool_refresh_debounce.go
  internal/fleetgate/manifest.go
  internal/fleetgate/testevents.go
  internal/fleetgate/benchmark.go
  internal/fleetgate/metadata.go
  internal/integration/fake_hub_reliability_test.go
  internal/integration/fake_hub_generation_soak_test.go
  internal/reliability/mixed_load_test.go
  # Mills transactional start, durable dispatch, and workflow semantics are
  # reliability-gate inputs. Bind both the exercised implementation and its
  # manifested scenarios into the evidence digest.
  pkg/mills/reconciler.go
  pkg/mills/reconciler_test.go
  pkg/mills/metrics.go
  pkg/mills/scope_overlap.go
  pkg/mills/council/backlog_mutator.go
  pkg/mills/pipeline/adapter.go
  pkg/mills/pipeline/integrator.go
  pkg/mills/pipeline/recursion.go
  pkg/mills/pipeline/runner.go
  pkg/mills/pipeline/runner_test.go
  pkg/mills/policy_manager.go
  pkg/mills/policy_test.go
  pkg/mills/store/dao_backlog.go
  pkg/mills/store/dao_events.go
  pkg/mills/store/dao_pipeline.go
  pkg/mills/store/dao_pipeline_start.go
  pkg/mills/store/dao_pipeline_start_test.go
  pkg/mills/store/dao_workflow.go
  pkg/mills/store/dao_workflow_test.go
  pkg/mills/store/migrations/011_pipeline_start_kernel.sql
  pkg/mills/store/scope_overlap.go
  pkg/mills/store/types.go
  pkg/mills/squads/outcome_recorder.go
  pkg/mills/workflow/journal_dao.go
  pkg/mills/workflow/journal_dao_test.go
  pkg/secrets/exec.go
  pkg/secrets/exec_cancel_other.go
  pkg/secrets/exec_cancel_unix.go
  pkg/secrets/file.go
  pkg/secrets/keychain.go
  pkg/secrets/onepassword.go
  pkg/secrets/store.go
  pkg/templatevars/expand.go
  "${benchmark_files[@]}"
  scripts/ci/fleet_reliability_gate.sh
  scripts/ci/fleet_reliability_base.sh
  scripts/ci/fleet_reliability_base_test.sh
  scripts/ci/fleet_reliability_benchmarks.sh
  scripts/ci/fleet_reliability_benchmarks_test.sh
  scripts/ci/fleet_reliability_manifest.sh
  scripts/ci/fleet_reliability_manifest_test.sh
  scripts/ci/go_build_hook.sh
  scripts/ci/go_build_package_filter.sh
  scripts/ci/go_build_package_filter_test.sh
  scripts/ci/select_go_test_packages.sh
  scripts/ci/select_go_test_packages_test.sh
)
schema_files=(docs/api/openapi.yaml internal/contracts/testdata/*.golden)

mkdir -p "$artifact_dir"

# shellcheck source=scripts/ci/fleet_reliability_manifest.sh
source "$repo_root/scripts/ci/fleet_reliability_manifest.sh"

hash_stream() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

digest_files() {
  local file
  for file in "$@"; do
    printf '%s\0' "$file"
    command cat "$file"
  done | hash_stream
}

cleanup() {
  if [[ -n "$base_worktree" ]] && git -C "$repo_root" worktree list --porcelain | grep -Fq "worktree $base_worktree"; then
    git -C "$repo_root" worktree remove --force "$base_worktree" >/dev/null 2>&1 || true
    git -C "$repo_root" worktree prune --expire now >/dev/null 2>&1 || true
  fi
  if [[ -n "$benchmark_bin_dir" ]]; then
    rm -rf "$benchmark_bin_dir" >/dev/null 2>&1 || true
  fi
  rm -f "$gate_bin" >/dev/null 2>&1 || true
}

json_escape() {
  local value="${1:-}"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

write_final_status() {
  local exit_status="$1"
  local passed=false
  local build_sha=""
  local manifest_digest=""
  local config_digest=""
  local schema_digest=""
  local metadata_present=false
  local status_tmp="$artifact_dir/final-status.json.tmp.$$"

  if [[ "$exit_status" -eq 0 ]]; then
    passed=true
  fi
  build_sha="$(git -C "$repo_root" rev-parse HEAD 2>/dev/null || true)"
  if [[ -f "$manifest" ]]; then
    manifest_digest="$(hash_stream <"$manifest" 2>/dev/null || true)"
  fi
  config_digest="$(digest_files "${config_files[@]}" 2>/dev/null || true)"
  schema_digest="$(digest_files "${schema_files[@]}" 2>/dev/null || true)"
  if [[ -f "$artifact_dir/metadata.json" ]]; then
    metadata_present=true
  fi

  if ! {
    printf '{\n'
    printf '  "schema_version": "loom.fleet-reliability.run-status/v1",\n'
    printf '  "run_id": "%s",\n' "$(json_escape "$run_id")"
    printf '  "passed": %s,\n' "$passed"
    printf '  "exit_status": %d,\n' "$exit_status"
    printf '  "stage": "%s",\n' "$(json_escape "$current_stage")"
    printf '  "started_at": "%s",\n' "$(json_escape "$started_at")"
    printf '  "finished_at": "%s",\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '  "build_sha": "%s",\n' "$(json_escape "$build_sha")"
    printf '  "base_sha": "%s",\n' "$(json_escape "$base_sha")"
    printf '  "manifest_sha256": "%s",\n' "$(json_escape "$manifest_digest")"
    printf '  "config_sha256": "%s",\n' "$(json_escape "$config_digest")"
    printf '  "schema_sha256": "%s",\n' "$(json_escape "$schema_digest")"
    printf '  "metadata_present": %s\n' "$metadata_present"
    printf '}\n'
  } >"$status_tmp"; then
    echo "ERROR: could not write final reliability status" >&2
    return
  fi
  if ! mv "$status_tmp" "$artifact_dir/final-status.json"; then
    echo "ERROR: could not publish final reliability status" >&2
  fi
}

finalize() {
  local exit_status="$1"
  trap - EXIT INT TERM
  set +e
  write_final_status "$exit_status"
  cleanup
  exit "$exit_status"
}

on_signal() {
  local exit_status="$1"
  local signal_name="$2"
  current_stage="${current_stage}-${signal_name}"
  exit "$exit_status"
}

trap 'finalize "$?"' EXIT
trap 'on_signal 130 interrupt' INT
trap 'on_signal 143 terminated' TERM

fleet_reliability_snapshot_manifest "$manifest_source" "$manifest"

run_test_group() {
  local group="$1"
  local output="$2"
  shift 2

  echo "Running required test group: $group"
  set +e
  "$@" | tee "$output"
  local test_status="${PIPESTATUS[0]}"
  set -e

  set +e
  "$gate_bin" verify-tests \
    --manifest "$manifest" \
    --group "$group" \
    --input "$output" \
    --output "$artifact_dir/tests-${group}-report.json"
  local verify_status="$?"
  set -e

  if [[ "$test_status" -ne 0 || "$verify_status" -ne 0 ]]; then
    echo "ERROR: required test group $group failed (go test=$test_status verifier=$verify_status)" >&2
    return 1
  fi
}

run_manifest_test_group() {
  local group="$1"
  local output="$2"
  local test_regex
  local package_lines
  local package
  local packages=()
  shift 2

  test_regex="$("$gate_bin" test-plan --manifest "$manifest" --group "$group" --field regex)"
  package_lines="$("$gate_bin" test-plan --manifest "$manifest" --group "$group" --field packages)"
  while IFS= read -r package; do
    if [[ -n "$package" ]]; then
      packages+=("$package")
    fi
  done <<<"$package_lines"
  if [[ -z "$test_regex" || "${#packages[@]}" -eq 0 ]]; then
    echo "ERROR: manifest produced an empty test plan for group $group" >&2
    return 1
  fi

  run_test_group "$group" "$output" "$@" -run "$test_regex" "${packages[@]}"
}

# shellcheck source=scripts/ci/fleet_reliability_base.sh
source "$repo_root/scripts/ci/fleet_reliability_base.sh"
# shellcheck source=scripts/ci/fleet_reliability_benchmarks.sh
source "$repo_root/scripts/ci/fleet_reliability_benchmarks.sh"

run_bounded_fleet_benchmark_sample() {
  local directory="$1"
  local binary_dir="$2"
  local side="$3"
  local package="$4"
  local output="$5"
  local binary

  if ((SECONDS + benchmark_sample_timeout_seconds > benchmark_deadline_seconds)); then
    echo "Benchmark deadline reached after ${SECONDS}s; reserving time for mixed-load gate" >&2
    return 124
  fi

  binary="$(fleet_benchmark_binary_path "$binary_dir" "$side" "$package")"
  (
    cd "$directory/${package#./}"
    GOWORK=off CGO_ENABLED=0 GOMAXPROCS=2 timeout --foreground "${benchmark_sample_timeout_seconds}s" "$binary" \
      -test.run='^$' \
      -test.bench='^BenchmarkFleet' \
      -test.benchmem \
      -test.benchtime="$fleet_benchmark_benchtime" \
      -test.count=1
  ) | tee -a "$output"
}

current_stage="package-selection"
echo "Validating dependency-derived Go package selection"
GOWORK=off bash scripts/ci/select_go_test_packages_test.sh
GOWORK=off bash scripts/ci/go_build_package_filter_test.sh
GOWORK=off bash scripts/ci/fleet_reliability_base_test.sh
GOWORK=off bash scripts/ci/fleet_reliability_benchmarks_test.sh
GOWORK=off bash scripts/ci/fleet_reliability_manifest_test.sh

current_stage="build-binaries"
echo "Building reliability verifier and black-box binaries"
GOWORK=off CGO_ENABLED=0 go build -o "$gate_bin" ./cmd/fleet-reliability-gate
GOWORK=off CGO_ENABLED=0 go build -o bin/loom ./cmd/loom
GOWORK=off CGO_ENABLED=0 go build -o bin/loomd ./cmd/loomd

current_stage="test-daemon"
run_manifest_test_group daemon "$artifact_dir/tests-daemon.jsonl" \
  env GOWORK=off CGO_ENABLED=0 go test -json -short -count=1

current_stage="test-race"
run_manifest_test_group race "$artifact_dir/tests-race.jsonl" \
  env GOWORK=off CGO_ENABLED=1 go test -json -race -short -count=1

current_stage="test-spawn"
run_manifest_test_group spawn "$artifact_dir/tests-spawn.jsonl" \
  env GOWORK=off CGO_ENABLED=0 go test -json -short -count=1

current_stage="test-contracts"
run_manifest_test_group contracts "$artifact_dir/tests-contracts.jsonl" \
  env GOWORK=off CGO_ENABLED=0 go test -json -count=1

current_stage="test-integration"
run_manifest_test_group integration "$artifact_dir/tests-integration.jsonl" \
  env LOOM_REPO_ROOT="$repo_root" LOOM_RUN_INTEGRATION=1 GOWORK=off CGO_ENABLED=0 \
  go test -json -timeout 95s -count=1

current_stage="fetch-baseline"
default_branch="${CI_DEFAULT_BRANCH:-main}"
if [[ "${LOOM_RELIABILITY_SKIP_FETCH:-0}" != "1" ]]; then
  git fetch -q origin "$default_branch"
fi
base_sha="$(fleet_reliability_select_base_sha "$repo_root" "$default_branch" "origin/$default_branch")"
echo "Selected reliability benchmark baseline: $base_sha"
base_worktree="$repo_root/.worktrees/ci-fleet-base-${CI_JOB_ID:-$$}"
mkdir -p "$(dirname "$base_worktree")"
git worktree add --detach "$base_worktree" "$base_sha" >/dev/null

current_stage="prepare-benchmarks"
for file in "${benchmark_files[@]}"; do
  mkdir -p "$base_worktree/$(dirname "$file")"
  install -m 0644 "$repo_root/$file" "$base_worktree/$file"
done

benchmark_bin_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-fleet-reliability-bench-${run_id}.XXXXXX")"
current_stage="compile-benchmarks-base"
compile_fleet_benchmark_binaries "$base_worktree" "$benchmark_bin_dir" base
current_stage="compile-benchmarks-candidate"
compile_fleet_benchmark_binaries "$repo_root" "$benchmark_bin_dir" candidate

benchmark_base_output="$artifact_dir/benchmark-base.txt"
benchmark_candidate_output="$artifact_dir/benchmark-candidate.txt"
: >"$benchmark_base_output"
: >"$benchmark_candidate_output"

echo "Running ${fleet_benchmark_rounds} package-adjacent, paired same-runner benchmark rounds (deadline=${benchmark_deadline_seconds}s, sample-timeout=${benchmark_sample_timeout_seconds}s)"
benchmark_status=0
for ((round = 1; round <= fleet_benchmark_rounds; round++)); do
  start_fleet_benchmark_round "$benchmark_base_output" "$round"
  start_fleet_benchmark_round "$benchmark_candidate_output" "$round"
  for package in "${fleet_benchmark_packages[@]}"; do
    if ((round % 2 == 1)); then
      current_stage="benchmark-round-${round}-base-${package#./}"
      if run_bounded_fleet_benchmark_sample "$base_worktree" "$benchmark_bin_dir" base "$package" "$benchmark_base_output"; then
        :
      else
        benchmark_status="$?"
        break 2
      fi
      current_stage="benchmark-round-${round}-candidate-${package#./}"
      if run_bounded_fleet_benchmark_sample "$repo_root" "$benchmark_bin_dir" candidate "$package" "$benchmark_candidate_output"; then
        :
      else
        benchmark_status="$?"
        break 2
      fi
    else
      current_stage="benchmark-round-${round}-candidate-${package#./}"
      if run_bounded_fleet_benchmark_sample "$repo_root" "$benchmark_bin_dir" candidate "$package" "$benchmark_candidate_output"; then
        :
      else
        benchmark_status="$?"
        break 2
      fi
      current_stage="benchmark-round-${round}-base-${package#./}"
      if run_bounded_fleet_benchmark_sample "$base_worktree" "$benchmark_bin_dir" base "$package" "$benchmark_base_output"; then
        :
      else
        benchmark_status="$?"
        break 2
      fi
    fi
  done
done

current_stage="test-load"
run_manifest_test_group load "$artifact_dir/tests-load.jsonl" \
  env LOOM_RUN_RELIABILITY=1 LOOM_RELIABILITY_LOAD_DURATION="${LOOM_RELIABILITY_LOAD_DURATION:-60s}" \
  GOWORK=off CGO_ENABLED=0 go test -json -timeout 90s -count=1

if [[ "$benchmark_status" -ne 0 ]]; then
  echo "ERROR: benchmark phase stopped with status $benchmark_status after ${SECONDS}s; mixed-load gate completed" >&2
fi

current_stage="compare-benchmarks"
"$gate_bin" compare-benchmarks \
  --manifest "$manifest" \
  --baseline "$benchmark_base_output" \
  --candidate "$benchmark_candidate_output" \
  --output "$artifact_dir/benchmark-report.json"

current_stage="write-metadata"
config_digest="$(digest_files "${config_files[@]}")"
schema_digest="$(digest_files "${schema_files[@]}")"

dirty=false
if [[ -n "$(git status --porcelain)" ]]; then
  dirty=true
fi

"$gate_bin" write-metadata \
  --manifest "$manifest" \
  --output "$artifact_dir/metadata.json" \
  --build-sha "$(git rev-parse HEAD)" \
  --base-sha "$base_sha" \
  --dirty="$dirty" \
  --config-digest "$config_digest" \
  --schema-digest "$schema_digest" \
  --command "GOWORK=off make ci-reliability" \
  --go-version "$(go version)"

current_stage="complete"
echo "Fleet reliability gate passed; evidence: $artifact_dir"
