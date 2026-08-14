#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/ci/fleet_reliability_manifest.sh
source "$script_dir/fleet_reliability_manifest.sh"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-fleet-manifest-test.XXXXXX")"
trap 'rm -rf "$tmp_dir"' EXIT

source_manifest="$tmp_dir/source.json"
snapshot_manifest="$tmp_dir/evidence/suite-manifest.json"
expected_manifest="$tmp_dir/expected.json"

printf '{"suite_version":1,"marker":"original"}\n' >"$source_manifest"
cp "$source_manifest" "$expected_manifest"

fleet_reliability_snapshot_manifest "$source_manifest" "$snapshot_manifest"
cmp "$expected_manifest" "$snapshot_manifest"

# A source mutation during the run must not affect or replace the snapshot.
printf '{"suite_version":2,"marker":"mutated"}\n' >"$source_manifest"
if fleet_reliability_snapshot_manifest "$source_manifest" "$snapshot_manifest" >/dev/null 2>&1; then
  echo "FAIL: existing reliability suite snapshot was replaced" >&2
  exit 1
fi
cmp "$expected_manifest" "$snapshot_manifest"

if snapshot_mode="$(stat -f '%Lp' "$snapshot_manifest" 2>/dev/null)"; then
  :
else
  snapshot_mode="$(stat -c '%a' "$snapshot_manifest")"
fi
if [[ "$snapshot_mode" != "444" ]]; then
  echo "FAIL: reliability suite snapshot mode is $snapshot_mode, want 444" >&2
  exit 1
fi

echo "fleet reliability manifest snapshot passed: source mutation cannot change run selection"
