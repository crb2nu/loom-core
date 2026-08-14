#!/usr/bin/env bash

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-go-package-select-test.XXXXXX")"
trap 'yes | rm -rf "$tmp_dir" >/dev/null 2>&1 || true' EXIT

# CGO-disabled jobs use fi-accel's built-in pure-Go fallback and must retain the
# entire test surface even when no native archive is present.
purego_file="$tmp_dir/purego.txt"
CGO_ENABLED=0 bash scripts/ci/select_go_test_packages.sh \
  --with-tests \
  --force-fi-accel-missing >"$purego_file"

for package in \
  "github.com/crb2nu/loom/internal/daemon" \
  "github.com/crb2nu/loom/pkg/agentcontext" \
  "github.com/crb2nu/loom/pkg/mills/store" \
  "github.com/crb2nu/loom/pkg/transport/muxstdio"; do
  if ! grep -Fqx "$package" "$purego_file"; then
    echo "ERROR: CGO-disabled selection removed pure-Go-capable package: $package" >&2
    exit 1
  fi
done

# CGO-enabled race jobs need the native archive from the module Go actually
# resolves. With GOWORK disabled, the pinned module cache intentionally lacks
# those non-module files, so only packages with a real fi-accel edge are removed.
cgo_file="$tmp_dir/cgo.txt"
CGO_ENABLED=1 GOWORK=off bash scripts/ci/select_go_test_packages.sh \
  --with-tests >"$cgo_file"

require_selected() {
  local package="$1"
  if ! grep -Fqx "$package" "$cgo_file"; then
    echo "ERROR: unaffected package was removed: $package" >&2
    exit 1
  fi
}

require_excluded() {
  local package="$1"
  if grep -Fqx "$package" "$cgo_file"; then
    echo "ERROR: fi-accel-linked package was retained: $package" >&2
    exit 1
  fi
}

require_selected "github.com/crb2nu/loom/pkg/mills/store"
require_selected "github.com/crb2nu/loom/pkg/transport/muxstdio"
require_selected "github.com/crb2nu/loom/cmd/custom-server"
require_selected "github.com/crb2nu/loom/internal/daemon/generation"
require_excluded "github.com/crb2nu/loom/internal/daemon"
require_excluded "github.com/crb2nu/loom/pkg/agentcontext"
require_excluded "github.com/crb2nu/loom/cmd/mcp-agent-context"

selected_count="$(wc -l <"$cgo_file" | tr -d ' ')"
if ((selected_count < 50)); then
  echo "ERROR: suspiciously small package selection: $selected_count" >&2
  exit 1
fi

echo "package selector passed: $selected_count unaffected test package(s) retained"
