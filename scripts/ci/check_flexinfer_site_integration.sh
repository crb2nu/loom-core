#!/usr/bin/env bash
set -euo pipefail

DOC_FILE="docs/FLEXINFER_SITE_INTEGRATION.md"

# Resolve the sibling flexinfer-site checkout relative to the CANONICAL
# loom-core checkout, not the current working tree. Inside a linked worktree
# (<repo>/.worktrees/<branch>, <repo>/.claude/worktrees/<name>) a plain
# ../flexinfer-site points at nothing, and until 2026-09-10 this guardrail
# silently reported "local flexinfer-site repo not present, skipped mapping
# check" from every worktree — the one place agents actually commit from.
# Mirrors sourceLayoutRoot() in flexinfer-site/scripts/sync-docs.mjs.
resolve_site_repo() {
  if [[ -n "${FLEXINFER_SITE_REPO:-}" ]]; then
    printf '%s\n' "${FLEXINFER_SITE_REPO}"
    return
  fi
  local common_dir
  if common_dir="$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)"; then
    printf '%s/flexinfer-site\n' "$(dirname "$(dirname "${common_dir}")")"
    return
  fi
  printf '../flexinfer-site\n'
}

SITE_REPO="$(resolve_site_repo)"
SYNC_SCRIPT="${SITE_REPO}/scripts/sync-docs.mjs"

if [[ ! -f "${DOC_FILE}" ]]; then
  echo "flexinfer-site-guardrail: missing ${DOC_FILE}"
  exit 1
fi

required_doc_patterns=(
  "services/flexinfer-site"
  "pnpm sync:loom-core-docs"
  "content/loom-core-docs"
  "/docs/loom-core"
)

for pattern in "${required_doc_patterns[@]}"; do
  if ! grep -q "${pattern}" "${DOC_FILE}"; then
    echo "flexinfer-site-guardrail: ${DOC_FILE} is missing required text: ${pattern}"
    exit 1
  fi
done

if [[ -f "${SYNC_SCRIPT}" ]]; then
  required_sync_patterns=(
    "name: 'loom-core'"
    "source: '../loom-core/docs'"
    "target: 'content/loom-core-docs'"
  )

  for pattern in "${required_sync_patterns[@]}"; do
    if ! grep -q "${pattern}" "${SYNC_SCRIPT}"; then
      echo "flexinfer-site-guardrail: ${SYNC_SCRIPT} is missing expected mapping: ${pattern}"
      exit 1
    fi
  done

  echo "flexinfer-site-guardrail: passed (verified docs + flexinfer-site sync mapping at ${SITE_REPO})"
  exit 0
fi

echo "flexinfer-site-guardrail: passed (verified docs; flexinfer-site checkout not found at ${SITE_REPO}, skipped mapping check)"
