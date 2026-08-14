#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: select_go_test_packages.sh [--all|--with-tests] [--force-fi-accel-missing]

Print first-party Go import paths, one per line. When the fi-accel native
library is unavailable, only packages whose production or test dependency
graph reaches fi-accel are removed.
EOF
}

mode="with-tests"
force_missing=false

while (($# > 0)); do
  case "$1" in
    --all)
      mode="all"
      ;;
    --with-tests)
      mode="with-tests"
      ;;
    --force-fi-accel-missing)
      force_missing=true
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
  shift
done

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/loom-go-package-select.XXXXXX")"
trap 'yes | rm -rf "$tmp_dir" >/dev/null 2>&1 || true' EXIT

all_file="$tmp_dir/all.txt"
excluded_file="$tmp_dir/excluded.txt"
selected_file="$tmp_dir/selected.txt"

case "$mode" in
  all)
    go list ./... | sed '/^$/d' | sort -u >"$all_file"
    ;;
  with-tests)
    go list -f '{{if or (gt (len .TestGoFiles) 0) (gt (len .XTestGoFiles) 0)}}{{.ImportPath}}{{end}}' ./... \
      | sed '/^$/d' \
      | sort -u >"$all_file"
    ;;
esac

if [[ ! -s "$all_file" ]]; then
  echo "ERROR: Go package selection produced zero packages" >&2
  exit 1
fi

goos="$(go env GOOS)"
goarch="$(go env GOARCH)"
if [[ -n "${LOOM_FI_ACCEL_DIR:-}" ]]; then
  fi_accel_module_dir="$LOOM_FI_ACCEL_DIR/go/fiaccel"
else
  fi_accel_module_dir="$(go list -m -f '{{.Dir}}' gitlab.flexinfer.ai/libs/fi-accel/go/fiaccel)"
fi
fi_accel_go_dir="$(dirname "$fi_accel_module_dir")"
fi_accel_include="$fi_accel_go_dir/include/fi_accel.h"
fi_accel_lib_dir="$fi_accel_go_dir/lib/${goos}_${goarch}"

fi_accel_available=false
if [[ "$force_missing" != true ]] && \
   [[ -f "$fi_accel_include" ]] && \
   { [[ -f "$fi_accel_lib_dir/libfi_accel_ffi.so" ]] || \
     [[ -f "$fi_accel_lib_dir/libfi_accel_ffi.a" ]] || \
     [[ -f "$fi_accel_lib_dir/libfi_accel_ffi.dylib" ]]; }; then
  fi_accel_available=true
fi

if [[ "${CGO_ENABLED:-$(go env CGO_ENABLED)}" == "0" ]]; then
  # fi-accel ships pure-Go fallback files under !cgo. A missing native archive
  # is therefore irrelevant to hermetic CGO-disabled unit/build jobs.
  install -m 0644 "$all_file" "$selected_file"
elif [[ "$fi_accel_available" == true ]]; then
  install -m 0644 "$all_file" "$selected_file"
else
  echo "fi-accel native library unavailable; deriving exclusions from the Go import graph" >&2

  # `go list -test` emits synthetic package names for test binaries. Normalize
  # those names back to their first-party import path so test-only fi-accel
  # edges are excluded alongside production edges.
  # shellcheck disable=SC2016 # Go template syntax is intentionally literal.
  go list -test -f '{{$ip := .ImportPath}}{{range .Deps}}{{printf "%s|%s\n" $ip .}}{{end}}' ./... \
    | awk -F'|' 'index($2, "gitlab.flexinfer.ai/libs/fi-accel/") == 1 {print $1}' \
    | sed -E 's/ \[[^]]*\]$//; s/\.test$//; s/_test$//' \
    | sort -u >"$excluded_file"

  awk 'NR == FNR { excluded[$0] = 1; next } !($0 in excluded)' \
    "$excluded_file" "$all_file" >"$selected_file"

  echo "excluded $(wc -l <"$excluded_file" | tr -d ' ') fi-accel-linked package(s)" >&2
fi

if [[ ! -s "$selected_file" ]]; then
  echo "ERROR: Go package filtering removed every selected package" >&2
  exit 1
fi

cat "$selected_file"
