#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
WITH_CLEAN_GIT_ENV="${REPO_ROOT}/scripts/dev/with-clean-git-env.sh"

# shellcheck source=scripts/ci/go_build_package_filter.sh
source "${SCRIPT_DIR}/go_build_package_filter.sh"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p "$tmp_dir/runtime" "$tmp_dir/testonly"
printf 'module example.test/buildfilter\n\ngo 1.24\n' >"$tmp_dir/go.mod"
printf 'package runtime\n\nfunc Ready() bool { return true }\n' >"$tmp_dir/runtime/runtime.go"
printf 'package testonly\n\nimport "testing"\n\nfunc TestOnly(t *testing.T) {}\n' >"$tmp_dir/testonly/only_test.go"

actual="$({
  printf './testonly\n'
  printf './runtime\n'
} | (cd "$tmp_dir" && filter_buildable_go_packages))"

if [[ "$actual" != "./runtime" ]]; then
  printf 'unexpected buildable package selection: %q\n' "$actual" >&2
  exit 1
fi

printf 'go build package filter passed: test-only packages excluded\n'
