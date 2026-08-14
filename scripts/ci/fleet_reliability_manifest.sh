#!/usr/bin/env bash

# Copy the suite manifest into one run-scoped, read-only snapshot. Refusing to
# replace an existing destination keeps every phase of a gate run pinned to the
# same selection even if the source worktree changes concurrently.
fleet_reliability_snapshot_manifest() {
  local source_manifest="$1"
  local snapshot_manifest="$2"

  if [[ ! -f "$source_manifest" ]]; then
    echo "ERROR: reliability suite manifest not found: $source_manifest" >&2
    return 1
  fi
  if [[ -e "$snapshot_manifest" ]]; then
    echo "ERROR: reliability suite snapshot already exists: $snapshot_manifest" >&2
    return 1
  fi

  mkdir -p "$(dirname "$snapshot_manifest")"
  install -m 0444 "$source_manifest" "$snapshot_manifest"
}
