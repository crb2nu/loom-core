#!/usr/bin/env bash
#
# Regression harness for the load-bearing assumption behind
# scripts/ci/run_unit_tests.sh.
#
# The flake-quarantine design rests on three properties of gotestsum's
# --rerun-fails combined with Go binary coverage. All three were verified
# empirically before the design was written; this harness keeps them verified,
# because the only way they can regress is a deliberate GOTESTSUM_VERSION bump
# — exactly the moment nobody re-runs the original experiment.
#
#   1. A test that fails once and passes on rerun exits 0 (pipeline stays
#      green) and is named in the rerun report as "N runs, M failures" with
#      M < N.
#   2. A test that fails every attempt exits non-zero (hard red stays hard) and
#      is named with M == N.
#   3. Coverage SURVIVES the rerun. This is the one that bit us: with the old
#      `-coverprofile=FILE`, gotestsum's rerun re-invokes `go test` with the
#      same flags and overwrites FILE with its `-run`-filtered subset,
#      collapsing total coverage from 60.0% to 0.0% on the probe module and
#      tripping the coverage gate on every pipeline containing a flake.
#      Collecting into a `-test.gocoverdir` accumulates instead.
#
# Run: bash scripts/ci/run_unit_tests_test.sh

set -euo pipefail

GOTESTSUM_VERSION="${GOTESTSUM_VERSION:-v1.13.0}"

gobin="$(go env GOPATH)/bin"
if [ -x "$gobin/gotestsum" ] && "$gobin/gotestsum" --version 2>/dev/null | grep -qF "$GOTESTSUM_VERSION"; then
  :
else
  echo "installing gotestsum $GOTESTSUM_VERSION..."
  GOFLAGS='' go install "gotest.tools/gotestsum@${GOTESTSUM_VERSION}"
fi

fixture="$(mktemp -d "${TMPDIR:-/tmp}/loom-flake-contract.XXXXXX")"
trap 'yes | rm -rf "$fixture" >/dev/null 2>&1 || true' EXIT

mkdir -p "$fixture/alpha" "$fixture/beta"

cat >"$fixture/go.mod" <<'EOF'
module flakecontract

go 1.26
EOF

# alpha: 3 statements, 2 covered by the stable test -> 66.7%
cat >"$fixture/alpha/alpha.go" <<'EOF'
package alpha

func A1() int { return 1 }
func A2() int { return 2 }
func A3() int { return 3 }
EOF

cat >"$fixture/alpha/alpha_test.go" <<'EOF'
package alpha

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStable(t *testing.T) {
	if A1() != 1 || A2() != 2 {
		t.Fatal("unexpected")
	}
}

// TestFlaky fails on its first run and passes on every later one, using a
// marker file to carry state across test-binary invocations.
func TestFlaky(t *testing.T) {
	marker := filepath.Join(os.Getenv("FLAKE_CONTRACT_DIR"), "flaky.marker")
	if _, err := os.Stat(marker); err == nil {
		return
	}
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	t.Fatal("deliberate first-attempt failure")
}
EOF

# beta: 2 statements, 1 covered -> 50.0%. Total across both packages: 3/5 = 60.0%
#
# `totalUnused` is a DECOY, not a typo. `go tool cover -func` prints one line
# per function, so a bare `grep total` over that output matches any function
# whose name contains "total" (this repo really has internal/hud.
# totalPendingLocked and pkg/mills/runner.total). That collision made $TOTAL a
# multi-line string and silently disabled the coverage gate — see the comment
# in run_unit_tests.sh. Keeping a decoy here means the 60.0% assertions below
# fail loudly if anyone reverts to an unanchored match.
cat >"$fixture/beta/beta.go" <<'EOF'
package beta

func B1() int        { return 1 }
func totalUnused() int { return 2 }

// keep totalUnused reachable for the compiler without covering it
var _ = totalUnused
EOF

cat >"$fixture/beta/beta_test.go" <<'EOF'
package beta

import "testing"

func TestStable(t *testing.T) {
	if B1() != 1 {
		t.Fatal("unexpected")
	}
}
EOF

failures=0

fail() {
  echo "  FAIL: $*"
  failures=$((failures + 1))
}

pass() {
  echo "  ok: $*"
}

# run_fixture <case-name>; sets CASE_EXIT, CASE_REPORT, CASE_COVERAGE
run_fixture() {
  local name="$1"
  echo "--- case: $name"

  yes | rm -rf "$fixture/covdata" >/dev/null 2>&1 || true
  \rm -f "$fixture/flaky.marker" "$fixture/rerun.txt" "$fixture/coverage.out"
  mkdir -p "$fixture/covdata"

  set +e
  (
    cd "$fixture"
    FLAKE_CONTRACT_DIR="$fixture" GOWORK=off CGO_ENABLED=0 GOFLAGS='' \
      "$gobin/gotestsum" \
      --format testname \
      --rerun-fails=2 \
      --rerun-fails-max-failures=5 \
      --rerun-fails-report="$fixture/rerun.txt" \
      --packages="./alpha ./beta" \
      -- -cover -covermode=count -args -test.gocoverdir="$fixture/covdata"
  ) >"$fixture/output.log" 2>&1
  CASE_EXIT=$?
  set -e

  CASE_REPORT="$(cat "$fixture/rerun.txt" 2>/dev/null || true)"

  # The covdir -> text profile conversion the runner script performs.
  (cd "$fixture" && GOWORK=off GOFLAGS='' go tool covdata textfmt \
    -i="$fixture/covdata" -o="$fixture/coverage.out") >/dev/null 2>&1 || true
  # Same anchored extraction run_unit_tests.sh uses. Must stay anchored on the
  # `total:` first field, or the `totalUnused` decoy in the fixture poisons it.
  CASE_COVERAGE="$(cd "$fixture" && GOWORK=off GOFLAGS='' go tool cover \
    -func="$fixture/coverage.out" 2>/dev/null | awk '$1 == "total:" { pct = $3 } END { print pct }')"

  echo "  exit=$CASE_EXIT coverage=$CASE_COVERAGE report='${CASE_REPORT}'"
}

# --- Case 1: flake then pass -------------------------------------------------

run_fixture "flake-then-pass stays green, coverage intact"

if [ "$CASE_EXIT" -eq 0 ]; then
  pass "exit 0 — a rerun-pass does not red the pipeline"
else
  fail "exit $CASE_EXIT — a rerun-pass must keep the job green"
fi

if printf '%s' "$CASE_REPORT" | grep -q 'flakecontract/alpha.TestFlaky: 2 runs, 1 failures'; then
  pass "rerun report names the flake with failures < runs"
else
  fail "rerun report did not record the flake: '${CASE_REPORT}'"
fi

# The regression that motivated the whole covdir scheme.
if [ "$CASE_COVERAGE" = "60.0%" ]; then
  pass "coverage preserved at 60.0% across the rerun"
else
  fail "coverage = '${CASE_COVERAGE}', want 60.0% — reruns are clobbering the profile again"
fi

# --- Case 2: hard failure stays red -----------------------------------------

cat >"$fixture/alpha/hard_test.go" <<'EOF'
package alpha

import "testing"

func TestAlwaysBroken(t *testing.T) { t.Fatal("deterministic failure") }
EOF

run_fixture "hard failure stays red"

if [ "$CASE_EXIT" -ne 0 ]; then
  pass "exit $CASE_EXIT — a test that fails every attempt still reds the job"
else
  fail "exit 0 — reruns laundered a deterministic failure into a pass"
fi

if printf '%s' "$CASE_REPORT" | grep -q 'flakecontract/alpha.TestAlwaysBroken: 3 runs, 3 failures'; then
  pass "rerun report records failures == runs for the hard failure"
else
  fail "rerun report did not record the hard failure: '${CASE_REPORT}'"
fi

if [ "$CASE_COVERAGE" = "60.0%" ]; then
  pass "coverage still published on a red run"
else
  fail "coverage = '${CASE_COVERAGE}' on a red run, want 60.0%"
fi

\rm -f "$fixture/alpha/hard_test.go"

# --- Case 3: too many failures suppresses reruns entirely --------------------

cat >"$fixture/beta/many_test.go" <<'EOF'
package beta

import "testing"

func TestBroken1(t *testing.T) { t.Fatal("x") }
func TestBroken2(t *testing.T) { t.Fatal("x") }
func TestBroken3(t *testing.T) { t.Fatal("x") }
func TestBroken4(t *testing.T) { t.Fatal("x") }
func TestBroken5(t *testing.T) { t.Fatal("x") }
func TestBroken6(t *testing.T) { t.Fatal("x") }
EOF

run_fixture "failure count above --rerun-fails-max-failures aborts reruns"

if [ "$CASE_EXIT" -ne 0 ]; then
  pass "exit $CASE_EXIT — a broad break is red without wasting time on reruns"
else
  fail "exit 0 — a break above the rerun ceiling must stay red"
fi

if [ -z "$CASE_REPORT" ]; then
  pass "no rerun report — nothing was rerun, so nothing is misfiled as a flake"
else
  fail "expected an empty rerun report, got: '${CASE_REPORT}'"
fi

\rm -f "$fixture/beta/many_test.go"

# --- verdict -----------------------------------------------------------------

echo ""
if [ "$failures" -ne 0 ]; then
  echo "FAILED: $failures assertion(s) — the gotestsum flake-quarantine contract has changed."
  echo "Do NOT bump GOTESTSUM_VERSION past this point until run_unit_tests.sh is re-verified."
  exit 1
fi
echo "PASS: gotestsum ${GOTESTSUM_VERSION} flake-quarantine contract holds."
