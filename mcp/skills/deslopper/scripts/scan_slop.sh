#!/usr/bin/env bash
#
# scan_slop.sh — read-only scanner for AI-slop candidate signatures.
#
# Surfaces likely slop with file:line evidence so the agent can read each hit in
# context and confirm. It DETECTS ONLY — it never edits. The patterns are
# deliberately high-recall/low-precision: treat every hit as a *candidate*, not
# a verdict. See references/slop-taxonomy.md for how to judge each category.
#
# Usage:
#   scan_slop.sh [PATH ...]        # scan given paths (default: current dir)
#   scan_slop.sh --diff [BASE]     # scan only files changed vs BASE (default: main)
#   scan_slop.sh --help
#
# Exit status: 0 always (a scanner finding slop is not an error).
#
# Safety: this scanner has no write mode — it only reads and greps. It is
# effectively --dry-run always; the redirections below (2>/dev/null) are output
# suppression, not file writes. Safe for always_allow.

set -euo pipefail

usage() {
  sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
}

# --- choose search backend (ripgrep preferred, grep fallback) ----------------
if command -v rg >/dev/null 2>&1; then
  SEARCH=rg
else
  SEARCH=grep
fi

# Source-code extensions worth scanning; keeps noise out of the report.
EXT_RE='\.(go|py|ts|tsx|js|jsx|rs|java|rb|c|cc|cpp|h|hpp|sh|gd)$'

# Generated / vendored / minified paths are never authored slop — skip them.
PRUNE_RE='(^|/)(node_modules|vendor|dist|build|\.git|__pycache__|\.venv|target|pkg)/|\.min\.|\.gen\.|_pb2?\.|\.pb\.go$'

# --- resolve target file list ------------------------------------------------
DIFF_MODE=0
BASE="main"
PATHS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --help|-h) usage ;;
    --diff) DIFF_MODE=1; shift; if [ $# -gt 0 ] && [ "${1#-}" = "$1" ]; then BASE="$1"; shift; fi ;;
    *) PATHS+=("$1"); shift ;;
  esac
done
[ ${#PATHS[@]} -eq 0 ] && PATHS=(".")

collect_files() {
  local raw
  if [ "$DIFF_MODE" -eq 1 ]; then
    raw="$(git diff --name-only --diff-filter=d "$BASE"... 2>/dev/null || true)"
  elif command -v rg >/dev/null 2>&1; then
    raw="$(rg --files "${PATHS[@]}" 2>/dev/null || true)"
  else
    raw="$(find "${PATHS[@]}" -type f 2>/dev/null || true)"
  fi
  printf '%s\n' "$raw" | grep -E "$EXT_RE" 2>/dev/null | grep -Ev "$PRUNE_RE" 2>/dev/null || true
}

FILES="$(collect_files)"
if [ -z "$FILES" ]; then
  echo "No source files to scan." >&2
  exit 0
fi

# --- search helper: print "category | file:line: text" for matches -----------
TOTAL=0
report() {
  local label="$1" pattern="$2" hits
  # Feed the resolved file list via NUL-delimited xargs so paths with spaces survive.
  if [ "$SEARCH" = rg ]; then
    hits="$(printf '%s\n' "$FILES" | tr '\n' '\0' | xargs -0 rg --no-heading --line-number -e "$pattern" 2>/dev/null || true)"
  else
    hits="$(printf '%s\n' "$FILES" | tr '\n' '\0' | xargs -0 grep -nE -e "$pattern" 2>/dev/null || true)"
  fi
  if [ -n "$hits" ]; then
    local count
    count="$(printf '%s\n' "$hits" | grep -c . || true)"
    TOTAL=$((TOTAL + count))
    printf '\n## %s  (%s candidate hits)\n' "$label" "$count"
    printf '%s\n' "$hits" | sed 's/^/  /'
  fi
}

echo "# Slop scan — candidates only, confirm each in context (see slop-taxonomy.md)"
if [ "$DIFF_MODE" -eq 1 ]; then
  echo "scope: files changed vs ${BASE}"
else
  echo "scope: ${PATHS[*]}"
fi

# 1. Over-commenting — banner / section-divider comments
report "over-commenting: banner/section comments" '^\s*(//|#)\s*[-=*#]{6,}'

# 2. Dead scaffolding — generation TODOs and commented-out code
report "dead scaffolding: generation TODO/FIXME/XXX" '(//|#)\s*(TODO|FIXME|XXX)\b'
# Require code-like syntax after the keyword so prose comments ("// for browsing")
# don't trip the control-flow keywords.
report "dead scaffolding: commented-out code" '^\s*(//|#)\s*((if|for|while|switch)\s*\(|(return|def|func|class)\b.*[(:{]|(const|let|var)\s+\w+\s*=)'

# 6. Verbose boilerplate — bare boolean-literal return (often the if/else idiom)
report "verbose boilerplate: bare boolean-literal return" 'return (True|False);?\s*$'

# 7. Tonal slop — emoji + marketing adjectives in comments
report "tonal slop: emoji in source" '[🚀✨🔥💪🎉👍✅🎯]'
report "tonal slop: marketing adjectives in comments" '(//|#).*(blazingly|seamless|robust|comprehensive|cutting-edge|state-of-the-art|production-ready)'

printf '\n---\nTotal candidate hits: %s\n' "$TOTAL"
echo "Reminder: candidates ≠ confirmed slop. Read each in context before editing."
