#!/usr/bin/env bash
#
# Workflow interpreter drain gate (S6-full CI deploy-safety gate, .loom/134).
#
# A journal written by one operator binary must replay on the next. The
# replay-compatibility surface is pinned in
# pkg/mills/workflow/testdata/interp_surface.golden (enforced by
# TestInterpreterSurfaceGolden) and by the go.starlark.net engine version in
# go.mod (enforced by TestInterpreterVersionMatchesGoMod). When an MR changes
# either, in-flight imperative workflow runs recorded by the OLD binary would
# hard abort-and-escalate on resume under the NEW one (version-pin refusal) —
# or worse, silently replay under drifted semantics if the pin was not bumped.
#
# This gate therefore fails any MR that touches the surface unless the author
# confirms the drain procedure with a [workflow-drain-confirmed] marker in the
# latest commit message:
#
#   1. Before MERGING (not before pushing), verify no in-flight imperative
#      runs: GET /api/mills/safety/quiescence on the operator must show
#      counts.active_workflow_runs == 0 for engine=imperative runs, or pause
#      them (paused_at) and let them abort-and-escalate deliberately.
#   2. Add "[workflow-drain-confirmed]" to the final commit message.
#
set -euo pipefail

GOLDEN_PATH="pkg/mills/workflow/testdata/interp_surface.golden"

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "workflow-drain-gate: no git repository available, skipping"
  exit 0
fi

latest_msg="$(git log -1 --pretty=%B 2>/dev/null || true)"
marker_present=0
if printf '%s' "$latest_msg" | grep -qi '\[workflow-drain-confirmed\]'; then
  marker_present=1
fi

remote="${WORKFLOW_DRAIN_REMOTE:-origin}"
base_ref="${WORKFLOW_DRAIN_BASE_REF:-}"

if [[ -z "$base_ref" ]]; then
  if [[ -n "${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-}" ]]; then
    base_ref="${remote}/${CI_MERGE_REQUEST_TARGET_BRANCH_NAME}"
  elif [[ -n "${GITHUB_BASE_REF:-}" ]]; then
    base_ref="${remote}/${GITHUB_BASE_REF}"
  elif [[ -n "${CI_DEFAULT_BRANCH:-}" ]]; then
    base_ref="${remote}/${CI_DEFAULT_BRANCH}"
  elif git rev-parse --verify "${remote}/main" >/dev/null 2>&1; then
    base_ref="${remote}/main"
  elif git rev-parse --verify HEAD~1 >/dev/null 2>&1; then
    base_ref="HEAD~1"
  fi
fi

if [[ -z "$base_ref" ]]; then
  echo "workflow-drain-gate: unable to determine base ref, skipping"
  exit 0
fi

if ! git rev-parse --verify "$base_ref" >/dev/null 2>&1; then
  for fallback in "${CI_MERGE_REQUEST_TARGET_BRANCH_NAME:-}" "${GITHUB_BASE_REF:-}" "${CI_DEFAULT_BRANCH:-}" "HEAD~1"; do
    if [[ -n "$fallback" ]] && git rev-parse --verify "$fallback" >/dev/null 2>&1; then
      base_ref="$fallback"
      break
    fi
  done
fi

if ! git rev-parse --verify "$base_ref" >/dev/null 2>&1; then
  echo "workflow-drain-gate: base ref '$base_ref' not available, skipping"
  exit 0
fi

if git merge-base "$base_ref" HEAD >/dev/null 2>&1; then
  range=("$base_ref...HEAD")
else
  range=("$base_ref" "HEAD")
fi

tripped_reasons=()

if git diff --name-only "${range[@]}" -- "$GOLDEN_PATH" | grep -q .; then
  tripped_reasons+=("replay-compatibility surface changed: $GOLDEN_PATH")
fi

# Engine bump: any diff line in go.mod (or vendored module metadata) touching
# go.starlark.net. -U0 keeps context lines out so an unrelated go.mod edit
# adjacent to the starlark requirement does not trip the gate.
if git diff -U0 "${range[@]}" -- go.mod | grep -E '^[+-][^+-].*go\.starlark\.net' | grep -q .; then
  tripped_reasons+=("interpreter engine version changed: go.starlark.net in go.mod")
fi

if [[ ${#tripped_reasons[@]} -eq 0 ]]; then
  echo "workflow-drain-gate: surface untouched, pass"
  exit 0
fi

if [[ "$marker_present" -eq 1 ]]; then
  echo "workflow-drain-gate: surface changed with [workflow-drain-confirmed]:"
  for r in "${tripped_reasons[@]}"; do echo "  - $r"; done
  echo "workflow-drain-gate: author has confirmed the drain procedure, pass"
  exit 0
fi

echo "workflow-drain-gate: FAIL — interpreter replay-compatibility surface changed without drain confirmation:"
for r in "${tripped_reasons[@]}"; do echo "  - $r"; done
cat <<'EOF'

Journals recorded by the currently deployed operator may not replay under this
change. Before this MR merges:

  1. Drain in-flight imperative workflow runs on the deployed operator:
       GET http://loom-mills-operator.loom-mills.svc.cluster.local:8090/api/mills/safety/quiescence
     must show no running/paused engine=imperative workflow runs (pipeline DAG
     runs do not replay through the interpreter and do not count) — or
     deliberately pause them and accept the version-pin abort-and-escalate.
  2. Re-run UPDATE_INTERP_SURFACE=1 go test ./pkg/mills/workflow -run TestInterpreterSurface
     if the golden is stale.
  3. Add "[workflow-drain-confirmed]" to the final commit message and push.

See .loom/134 §S6-full (CI deploy-safety gate) and pkg/mills/workflow/surface_test.go.
EOF
exit 1
