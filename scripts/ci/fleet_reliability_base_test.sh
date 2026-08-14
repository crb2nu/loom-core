#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scripts/ci/fleet_reliability_base.sh
source "$script_dir/fleet_reliability_base.sh"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-fleet-base-test.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT

pass_count=0

assert_equal() {
  local name="$1"
  local expected="$2"
  local actual="$3"

  if [[ "$actual" != "$expected" ]]; then
    printf 'FAIL  %s\n  expected: %s\n  actual:   %s\n' "$name" "$expected" "$actual" >&2
    exit 1
  fi
  printf 'PASS  %s\n' "$name"
  pass_count=$((pass_count + 1))
}

assert_fails_with() {
  local name="$1"
  local expected_text="$2"
  shift 2

  local output
  local status
  set +e
  output="$("$@" 2>&1)"
  status=$?
  set -e

  if [[ "$status" -eq 0 || "$output" != *"$expected_text"* ]]; then
    printf 'FAIL  %s\n  expected nonzero status containing: %s\n  status: %d\n  output: %s\n' \
      "$name" "$expected_text" "$status" "$output" >&2
    exit 1
  fi
  printf 'PASS  %s\n' "$name"
  pass_count=$((pass_count + 1))
}

init_repo() {
  local repo="$1"

  git init -q -b main "$repo"
  git -C "$repo" config user.email "fleet-base-test@example.com"
  git -C "$repo" config user.name "Fleet Base Test"
}

commit_empty() {
  local repo="$1"
  local message="$2"

  git -C "$repo" commit -q --allow-empty -m "$message"
  git -C "$repo" rev-parse HEAD
}

run_resolver() {
  local repo="$1"
  local branch="$2"
  local before_sha="$3"
  local override_ref="$4"

  (
    export CI_DEFAULT_BRANCH=main
    if [[ "$branch" == __unset__ ]]; then
      unset CI_COMMIT_BRANCH
    else
      export CI_COMMIT_BRANCH="$branch"
    fi
    if [[ "$before_sha" == __unset__ ]]; then
      unset CI_COMMIT_BEFORE_SHA
    else
      export CI_COMMIT_BEFORE_SHA="$before_sha"
    fi
    if [[ "$override_ref" == __unset__ ]]; then
      unset LOOM_RELIABILITY_BASE_REF
    else
      export LOOM_RELIABILITY_BASE_REF="$override_ref"
    fi
    fleet_reliability_select_base_sha "$repo" main origin/main
  )
}

feature_repo="$tmp_dir/feature"
init_repo "$feature_repo"
feature_base="$(commit_empty "$feature_repo" base)"
feature_main="$(commit_empty "$feature_repo" main-advance)"
git -C "$feature_repo" switch -q -c feature "$feature_base"
feature_head="$(commit_empty "$feature_repo" feature-change)"
git -C "$feature_repo" update-ref refs/remotes/origin/main "$feature_main"

actual="$(run_resolver "$feature_repo" feature "$feature_main" __unset__)"
assert_equal "feature branch uses merge-base with default branch" "$feature_base" "$actual"

actual="$(run_resolver "$feature_repo" main "$feature_main" "$feature_base")"
assert_equal "explicit override has highest priority" "$feature_base" "$actual"

default_repo="$tmp_dir/default"
init_repo "$default_repo"
default_base="$(commit_empty "$default_repo" base)"
default_middle="$(commit_empty "$default_repo" middle)"
default_head="$(commit_empty "$default_repo" candidate)"
git -C "$default_repo" update-ref refs/remotes/origin/main "$default_head"

actual="$(run_resolver "$default_repo" main "$default_base" __unset__)"
assert_equal "default branch uses pre-push SHA instead of fetched HEAD" "$default_base" "$actual"

zero_sha="0000000000000000000000000000000000000000"
actual="$(run_resolver "$default_repo" main "$zero_sha" __unset__)"
assert_equal "zero pre-push SHA falls back to first parent" "$default_middle" "$actual"

default_tip="$(commit_empty "$default_repo" later-main-tip)"
git -C "$default_repo" update-ref refs/remotes/origin/main "$default_tip"
git -C "$default_repo" switch -q --detach "$default_head"
actual="$(run_resolver "$default_repo" main "$default_base" __unset__)"
assert_equal "retry remains pinned when fetched default branch advances" "$default_base" "$actual"

missing_sha="ffffffffffffffffffffffffffffffffffffffff"
assert_fails_with \
  "unavailable nonzero pre-push SHA fails closed" \
  "is unavailable" \
  run_resolver "$default_repo" main "$missing_sha" __unset__

divergent_sha="$(git -C "$default_repo" commit-tree "$default_base^{tree}" -p "$default_base" -m unrelated)"
assert_fails_with \
  "divergent pre-push SHA fails closed" \
  "is not an ancestor" \
  run_resolver "$default_repo" main "$divergent_sha" __unset__

root_repo="$tmp_dir/root"
init_repo "$root_repo"
root_head="$(commit_empty "$root_repo" root)"
git -C "$root_repo" update-ref refs/remotes/origin/main "$root_head"
assert_fails_with \
  "root commit without pre-push SHA has no baseline" \
  "has no prior commit" \
  run_resolver "$root_repo" main "$zero_sha" __unset__

git -C "$feature_repo" update-ref refs/remotes/origin/main "$feature_head"
assert_fails_with \
  "feature branch already contained by default rejects self-baseline" \
  "baseline resolves to HEAD" \
  run_resolver "$feature_repo" feature __unset__ __unset__

assert_fails_with \
  "override resolving to candidate rejects self-baseline" \
  "baseline resolves to HEAD" \
  run_resolver "$feature_repo" feature __unset__ HEAD

printf 'fleet reliability base selection passed: %d cases\n' "$pass_count"
