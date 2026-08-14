#!/usr/bin/env bash
#
# Lint documentation conventions that contributors can run before CI.
#
# This intentionally delegates the changed-files policy to the CI guardrail so
# the local command and CI cannot drift. It also verifies that planning-index
# status annotations still agree with the status declared by each indexed plan.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
GUARDRAIL="$REPO_ROOT/scripts/ci/check_docs_guardrails.sh"
PLANNING_INDEX="$REPO_ROOT/docs/planning/README.md"

failures=0

fail() {
  printf 'docs-lint: %s\n' "$*" >&2
  failures=$((failures + 1))
}

normalize_status() {
  # Status labels may differ only in capitalization, punctuation, or the order
  # of parenthetical qualifiers (for example, "Historical snapshot (partially
  # completed)"). Compare their words rather than requiring presentation-only
  # details to match.
  printf '%s\n' "$1" \
    | tr '[:upper:]' '[:lower:]' \
    | tr -cs '[:alnum:]' '\n' \
    | awk 'NF' \
    | sort \
    | paste -sd ' ' -
}

run_guardrail() {
  if [[ ! -f "$GUARDRAIL" ]]; then
    fail "missing CI guardrail: $GUARDRAIL"
    return
  fi

  if ! bash "$GUARDRAIL"; then
    fail "changed-files documentation policy failed; see diagnostics above"
  fi
}

lint_planning_index() {
  if [[ ! -f "$PLANNING_INDEX" ]]; then
    fail "missing planning index: docs/planning/README.md"
    return
  fi

  local line_number=0 line document index_status plan_path plan_status
  local table_row_regex='^\| \[([^]]+)\]\(([^)]+)\) \| ([^|]+) \|'
  while IFS= read -r line || [[ -n "$line" ]]; do
    line_number=$((line_number + 1))
    [[ "$line" =~ $table_row_regex ]] || continue
    document="${BASH_REMATCH[2]}"
    index_status="${BASH_REMATCH[3]}"
    plan_path="$REPO_ROOT/docs/planning/$document"

    if [[ ! -f "$plan_path" ]]; then
      fail "docs/planning/README.md:$line_number indexes missing plan '$document'"
      continue
    fi

    plan_status="$(sed -nE 's/^> \*\*Status:\*\*[[:space:]]*(.*)[[:space:]]*$/\1/p' "$plan_path" | head -n 1)"
    if [[ -z "$plan_status" ]]; then
      fail "docs/planning/README.md:$line_number has status '$index_status', but $document has no '> **Status:** ...' annotation"
      continue
    fi

    if [[ "$(normalize_status "$index_status")" != "$(normalize_status "$plan_status")" ]]; then
      fail "docs/planning/README.md:$line_number status '$index_status' disagrees with $document status '$plan_status'"
    fi
  done < "$PLANNING_INDEX"
}

run_guardrail
lint_planning_index

if [[ "$failures" -gt 0 ]]; then
  printf 'docs-lint: failed with %d issue(s)\n' "$failures" >&2
  exit 1
fi

echo "docs-lint: passed"
