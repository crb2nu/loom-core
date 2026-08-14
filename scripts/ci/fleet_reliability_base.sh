#!/usr/bin/env bash

# Resolve the commit used as the fleet reliability benchmark baseline.
#
# Usage:
#   fleet_reliability_select_base_sha REPO_ROOT DEFAULT_BRANCH [DEFAULT_REF]
#
# The caller remains responsible for fetching DEFAULT_REF before invoking this
# function. Diagnostics are written to stderr so stdout contains only the
# selected commit SHA.

fleet_reliability_resolve_commit() {
  local repo_root="$1"
  local ref="$2"

  git -C "$repo_root" rev-parse --verify "$ref^{commit}" 2>/dev/null
}

fleet_reliability_is_zero_sha() {
  local sha="$1"

  [[ -n "$sha" && "$sha" =~ ^0+$ ]]
}

fleet_reliability_select_base_sha() {
  if (( $# < 2 || $# > 3 )); then
    echo "ERROR: fleet reliability base selection requires REPO_ROOT DEFAULT_BRANCH [DEFAULT_REF]" >&2
    return 2
  fi

  local repo_root="$1"
  local default_branch="$2"
  local default_ref="${3:-origin/$default_branch}"
  local override_ref="${LOOM_RELIABILITY_BASE_REF:-}"
  local current_branch="${CI_COMMIT_BRANCH:-}"
  local before_sha="${CI_COMMIT_BEFORE_SHA:-}"
  local head_sha
  local default_sha=""
  local base_sha=""
  local is_default_branch=false

  if ! head_sha="$(fleet_reliability_resolve_commit "$repo_root" HEAD)"; then
    echo "ERROR: fleet reliability candidate HEAD is not a commit" >&2
    return 1
  fi

  if [[ -n "$override_ref" ]]; then
    if ! base_sha="$(git -C "$repo_root" merge-base "$head_sha" "$override_ref" 2>/dev/null)"; then
      echo "ERROR: fleet reliability override '$override_ref' does not resolve to a common commit with HEAD" >&2
      return 1
    fi
  else
    if [[ -z "$current_branch" ]]; then
      current_branch="$(git -C "$repo_root" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
    fi

    if [[ "$current_branch" == "$default_branch" ]]; then
      is_default_branch=true
    elif [[ -z "$current_branch" ]]; then
      # Local detached-HEAD runs do not have CI_COMMIT_BRANCH. Recognize the
      # default branch only when its fetched tip is exactly the candidate.
      default_sha="$(fleet_reliability_resolve_commit "$repo_root" "$default_ref" || true)"
      if [[ -n "$default_sha" && "$head_sha" == "$default_sha" ]]; then
        is_default_branch=true
      fi
    fi

    if [[ "$is_default_branch" == true ]]; then
      if [[ -n "$before_sha" ]] && ! fleet_reliability_is_zero_sha "$before_sha"; then
        if ! base_sha="$(fleet_reliability_resolve_commit "$repo_root" "$before_sha")"; then
          echo "ERROR: CI_COMMIT_BEFORE_SHA '$before_sha' is unavailable; refusing an ambiguous reliability baseline" >&2
          return 1
        fi
      elif ! base_sha="$(fleet_reliability_resolve_commit "$repo_root" 'HEAD^1')"; then
        echo "ERROR: default-branch reliability run has no prior commit to use as a benchmark baseline" >&2
        return 1
      fi
    elif ! base_sha="$(git -C "$repo_root" merge-base "$head_sha" "$default_ref" 2>/dev/null)"; then
      echo "ERROR: fleet reliability default ref '$default_ref' does not resolve to a common commit with HEAD" >&2
      return 1
    fi
  fi

  if [[ "$base_sha" == "$head_sha" ]]; then
    echo "ERROR: fleet reliability baseline resolves to HEAD; set LOOM_RELIABILITY_BASE_REF to a distinct pre-candidate ref" >&2
    return 1
  fi
  if ! git -C "$repo_root" merge-base --is-ancestor "$base_sha" "$head_sha" 2>/dev/null; then
    echo "ERROR: fleet reliability baseline '$base_sha' is not an ancestor of candidate HEAD '$head_sha'" >&2
    return 1
  fi

  printf '%s\n' "$base_sha"
}
