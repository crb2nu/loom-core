#!/usr/bin/env bash
# Shared filter for Go hook package enumeration. Sourced, not executed.
# Callers must cd to the repo root first: paths are checked relative to cwd.

# Reads "./pkg/dir" package paths on stdin and drops entries the main module
# cannot build:
#   - dirs that no longer exist in the working tree (deleted/renamed in the
#     diff range)
#   - dirs inside a nested Go module, i.e. any parent below the repo root
#     with its own go.mod (e.g. examples/sprocket)
filter_main_module_packages() {
  local pkg dir
  while IFS= read -r pkg; do
    [[ -n "$pkg" ]] || continue
    dir="${pkg#./}"
    [[ -n "$dir" ]] || dir="."
    [[ -d "$dir" ]] || continue
    while [[ "$dir" != "." ]]; do
      if [[ -f "$dir/go.mod" ]]; then
        continue 2
      fi
      dir="$(dirname "$dir")"
    done
    printf '%s\n' "$pkg"
  done
}
