#!/usr/bin/env bash
# Shared by the staged/range Go build hook. Sourced, not executed.

# Reads package paths on stdin and keeps only packages with at least one
# non-test Go or CGo file for the current build environment. `go build` rejects
# intentionally test-only packages (for example internal/integration) with
# "no non-test Go files", even though their tests compile and run normally.
filter_buildable_go_packages() {
  local pkg buildable
  while IFS= read -r pkg; do
    [[ -n "$pkg" ]] || continue
    buildable="$("${WITH_CLEAN_GIT_ENV}" go list -e -f '{{if or .GoFiles .CgoFiles}}yes{{end}}' "$pkg")"
    [[ "$buildable" == "yes" ]] || continue
    printf '%s\n' "$pkg"
  done
}
