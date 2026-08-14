#!/usr/bin/env bash
#
# Regression harness for scripts/ci/check_workflow_drain_gate.sh.
#
# Seeds a temp git repo with a base commit carrying a golden + go.mod, applies
# a candidate commit per case, and asserts the gate's exit code. Mirrors the
# check_docs_guardrails_test.sh pattern.
#
# Run: bash scripts/ci/check_workflow_drain_gate_test.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATE="$SCRIPT_DIR/check_workflow_drain_gate.sh"

if [[ ! -x "$GATE" ]]; then
  chmod +x "$GATE" || true
fi

pass_count=0
fail_count=0

GOLDEN="pkg/mills/workflow/testdata/interp_surface.golden"

# run_case <name> <expected_exit> <mutation-fn> <commit-msg>
run_case() {
  local name="$1"
  local expected="$2"
  local mutate="$3"
  local msg="$4"

  local tmp
  tmp="$(mktemp -d)"

  git init -q "$tmp"
  git -C "$tmp" config user.email "test@example.com"
  git -C "$tmp" config user.name "test"

  mkdir -p "$tmp/$(dirname "$GOLDEN")"
  printf 'interpreter_version=starlark-go@v1\nuniverse=agent gate\n' > "$tmp/$GOLDEN"
  printf 'module example.com/m\n\nrequire (\n\tgo.starlark.net v0.0.0-20260101000000-aaaaaaaaaaaa\n\tother.example/dep v1.2.3\n)\n' > "$tmp/go.mod"
  git -C "$tmp" add -A
  git -C "$tmp" commit -q --no-verify -m "base"

  ( cd "$tmp" && eval "$mutate" )
  git -C "$tmp" add -A
  git -C "$tmp" commit -q --no-verify --allow-empty -m "$msg"

  local actual=0
  ( cd "$tmp" && CI=1 WORKFLOW_DRAIN_BASE_REF="HEAD~1" bash "$GATE" >/tmp/wf-drain-gate-out.txt 2>&1 ) || actual=$?

  if [[ "$actual" -eq "$expected" ]]; then
    printf 'PASS  %-58s (exit=%d)\n' "$name" "$actual"
    pass_count=$((pass_count + 1))
  else
    printf 'FAIL  %-58s (want exit=%d, got=%d)\n' "$name" "$expected" "$actual"
    echo "----- gate output -----"
    cat /tmp/wf-drain-gate-out.txt
    echo "----- /gate output -----"
    fail_count=$((fail_count + 1))
  fi

  rm -rf "$tmp"
}

# Unrelated changes never trip the gate.
run_case "unrelated code change" 0 \
  'mkdir -p pkg/foo && echo x > pkg/foo/bar.go' \
  "unrelated change"

# Golden modification without the marker fails.
run_case "golden change without marker" 1 \
  "echo 'universe=agent gate merge' >> $GOLDEN" \
  "bump surface"

# Golden modification with the marker passes.
run_case "golden change with marker" 0 \
  "echo 'universe=agent gate merge' >> $GOLDEN" \
  "bump surface [workflow-drain-confirmed]"

# Engine bump in go.mod without the marker fails.
run_case "starlark bump without marker" 1 \
  'sed -i.bak "s/20260101000000-aaaaaaaaaaaa/20260201000000-bbbbbbbbbbbb/" go.mod && rm -f go.mod.bak' \
  "bump starlark"

# Engine bump with the marker passes.
run_case "starlark bump with marker" 0 \
  'sed -i.bak "s/20260101000000-aaaaaaaaaaaa/20260201000000-bbbbbbbbbbbb/" go.mod && rm -f go.mod.bak' \
  "bump starlark [workflow-drain-confirmed]"

# Touching OTHER go.mod requirements does not trip the gate.
run_case "non-starlark go.mod change" 0 \
  'sed -i.bak "s/v1.2.3/v1.2.4/" go.mod && rm -f go.mod.bak' \
  "bump other dep"

# Deleting the golden is also a surface change.
run_case "golden deletion without marker" 1 \
  "rm -f $GOLDEN" \
  "drop golden"

echo
echo "workflow-drain-gate tests: $pass_count passed, $fail_count failed"
[[ "$fail_count" -eq 0 ]]
