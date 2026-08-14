#!/usr/bin/env bash
#
# test:unit runner — unit tests with flake quarantine and HONEST reruns.
#
# Why this exists
# ---------------
# On 2026-08-02 a single timing-dependent test
# (TestStopSpawnLateStartCleanupFailureRetainsRetryablePod) intermittently
# redded main and blocked every image build for ~10 hours, because red main
# gates the deploy stage. The specific race is fixed; the CLASS is not. A
# naive `retry: 1` on the job would have hidden it — unacceptable in a repo
# whose product includes race-sensitive spawn supervision.
#
# The contract here: a test that fails once and passes on rerun keeps the
# pipeline GREEN, but is recorded as a first-class event (rerun report
# artifact + `FLAKY-TEST-DETECTED` log line + a deduplicated `flaky-test`
# GitLab issue, filed by the after_script reporter). A test that fails every
# attempt still reds the job.
#
# Coverage (the load-bearing detail)
# ----------------------------------
# gotestsum's reruns re-invoke `go test` with the SAME flags. With the old
# `-coverprofile=coverage.out`, the rerun OVERWRITES the profile with its
# `-run`-filtered subset, collapsing total coverage to ~0% and tripping the
# coverage gate on every pipeline that contained a flake. Measured on a probe
# module: 60.0% -> 0.0%.
#
# So coverage is collected as Go 1.20+ binary coverage into a directory
# (`-args -test.gocoverdir=<abs dir>`) instead. Each test binary writes its own
# uniquely named counter file, reruns ACCUMULATE rather than clobber, and
# `go tool covdata textfmt` merges the lot back into the ordinary text profile
# the rest of the pipeline (gocover-cobertura, `go tool cover -func`) expects.
# Same probe with this scheme: 60.0% preserved across a flake+rerun.
#
# The covdir path MUST be absolute: `go test` runs each package's test binary
# with its own package directory as the working directory.
#
# Environment:
#   COVERAGE_THRESHOLD        minimum total coverage percent (unset = no gate)
#   GOTESTSUM_VERSION         pinned gotestsum (default v1.13.0)
#   FLAKE_RERUN_ATTEMPTS      --rerun-fails value (default 2)
#   FLAKE_RERUN_MAX_FAILURES  --rerun-fails-max-failures value (default 5)
#   FLAKE_RERUN_REPORT        rerun report path (default rerun-report.txt)
#
# Exit code is gotestsum's verdict: 0 when everything passed (including
# flake-then-pass), non-zero when any test failed every attempt or the failure
# count exceeded --rerun-fails-max-failures.

set -euo pipefail

GOTESTSUM_VERSION="${GOTESTSUM_VERSION:-v1.13.0}"
RERUN_ATTEMPTS="${FLAKE_RERUN_ATTEMPTS:-2}"
RERUN_MAX_FAILURES="${FLAKE_RERUN_MAX_FAILURES:-5}"
RERUN_REPORT="${FLAKE_RERUN_REPORT:-rerun-report.txt}"

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

COVER_DIR="$repo_root/.coverdata"
COVER_PROFILE="$repo_root/coverage.out"
COVER_XML="$repo_root/coverage.xml"
JUNIT_FILE="$repo_root/junit-unit.xml"

# Start from a clean slate: stale counter files from a cached workspace would
# silently inflate coverage.
yes | rm -rf "$COVER_DIR" >/dev/null 2>&1 || true
mkdir -p "$COVER_DIR"
: >"$RERUN_REPORT"

gobin="$(go env GOPATH)/bin"

# --- tooling -----------------------------------------------------------------

if [ -x "$gobin/gotestsum" ] && "$gobin/gotestsum" --version 2>/dev/null | grep -qF "$GOTESTSUM_VERSION"; then
  echo "Using cached gotestsum $GOTESTSUM_VERSION"
else
  echo "Installing gotestsum $GOTESTSUM_VERSION..."
  go install "gotest.tools/gotestsum@${GOTESTSUM_VERSION}"
fi

# Build the flake reporter NOW, while the module/build cache is warm and before
# any test can red the job. The job's after_script runs it in a fresh shell
# that never sourced before_script's GOPATH/GOCACHE exports, so handing it a
# prebuilt binary keeps `go run` (and a possible cold-cache module fetch) off
# the after_script path entirely.
echo "Building flake reporter..."
go build -o "$gobin/flakereport" ./scripts/flakereport

# --- package selection -------------------------------------------------------

echo "Selecting packages with tests..."
mapfile -t PKGS < <(bash scripts/ci/select_go_test_packages.sh --with-tests)
echo "Packages with tests: ${#PKGS[@]}"
if [ "${#PKGS[@]}" -eq 0 ]; then
  echo "ERROR: package selector returned zero unit-test packages"
  exit 1
fi

# --- test run ----------------------------------------------------------------

echo ""
echo "Running unit tests (reruns: ${RERUN_ATTEMPTS}, abort above ${RERUN_MAX_FAILURES} failures)..."

# `--packages` is REQUIRED, not decorative: gotestsum refuses to combine
# --rerun-fails with explicit `go test` args unless the package list arrives
# via this flag ("when go test args are used with --rerun-fails the list of
# packages to test must be specified by the --packages flag"). Passing the list
# here (and NOT as positional args) also keeps reruns scoped to the failing
# package instead of re-walking the whole tree.
set +e
"$gobin/gotestsum" \
  --format testname \
  --rerun-fails="$RERUN_ATTEMPTS" \
  --rerun-fails-max-failures="$RERUN_MAX_FAILURES" \
  --rerun-fails-report="$RERUN_REPORT" \
  --junitfile="$JUNIT_FILE" \
  --packages="${PKGS[*]}" \
  -- -cover -covermode=count -args -test.gocoverdir="$COVER_DIR"
test_exit=$?
set -e

echo ""
echo "gotestsum exit code: ${test_exit}"

# --- coverage ----------------------------------------------------------------

# Always publish coverage, including on a red run: the artifact is diagnostic,
# and an empty/short profile must not mask the real test failure below.
echo "Merging binary coverage into a text profile..."
if ! go tool covdata textfmt -i="$COVER_DIR" -o="$COVER_PROFILE" 2>&1; then
  echo "WARNING: covdata textfmt failed; coverage will not be published"
  printf 'mode: count\n' >"$COVER_PROFILE"
fi

profile_lines=$(wc -l <"$COVER_PROFILE" | tr -d ' ')
if [ "$profile_lines" -le 1 ]; then
  echo "WARNING: coverage profile is empty (${profile_lines} line(s)); skipping coverage report"
  if [ "$test_exit" -ne 0 ]; then
    exit "$test_exit"
  fi
  echo "ERROR: tests passed but produced no coverage data"
  exit 1
fi

if [ -x "$gobin/gocover-cobertura" ]; then
  echo "Using cached gocover-cobertura"
else
  go install github.com/boumenot/gocover-cobertura@v1.4.0
fi
"$gobin/gocover-cobertura" <"$COVER_PROFILE" >"$COVER_XML"

echo ""
echo "Coverage Summary:"
go tool cover -func="$COVER_PROFILE" | tail -20

# Anchor on the literal `total:` FIRST FIELD, not a bare `grep total`.
#
# `go tool cover -func` prints one line per function as
# "<path>:<line>:\t<FuncName>\t<pct>%", so a plain `grep total` also matches
# any function or path containing "total" — this repo has at least
# internal/hud.totalPendingLocked and pkg/mills/runner.total. That made $TOTAL
# a MULTI-LINE string ("100.0\n100.0\n60.6"), which in turn made the `-lt`
# test below fail with "integer expression expected". A `[` syntax error
# inside an `if` is not fatal under `set -e` — it just takes the else branch —
# so the coverage gate silently PASSED regardless of real coverage.
# Observed on kill-test pipeline 21963: reported 100.0, actual 60.6.
TOTAL=$(go tool cover -func="$COVER_PROFILE" | awk '$1 == "total:" { pct = $3 } END { print pct }' | tr -d '%')
echo ""
echo "Total Coverage: ${TOTAL}%"

# --- verdict -----------------------------------------------------------------

# Test failures win over everything below. A red suite's coverage number is not
# meaningful (packages aborted early), and reporting "below threshold" — or
# "could not parse coverage" — for a run that actually failed on a broken test
# misdirects triage to the wrong problem.
if [ "$test_exit" -ne 0 ]; then
  echo ""
  echo "Unit tests FAILED (exit ${test_exit}). See the failures above."
  echo "Reruns cannot rescue a test that fails every attempt — this is a hard red."
  exit "$test_exit"
fi

if ! printf '%s' "$TOTAL" | grep -Eq '^[0-9]+(\.[0-9]+)?$'; then
  echo "ERROR: could not parse a total coverage percentage from ${COVER_PROFILE} (got '${TOTAL}')"
  exit 1
fi

if [ -n "${COVERAGE_THRESHOLD:-}" ]; then
  # Float comparison in awk rather than `cut -d.`-truncated integers: exact,
  # and it cannot silently succeed on a malformed value the way `[ -lt ]` did.
  if awk -v total="$TOTAL" -v threshold="$COVERAGE_THRESHOLD" 'BEGIN { exit !(total < threshold) }'; then
    echo "ERROR: Coverage ${TOTAL}% is below threshold ${COVERAGE_THRESHOLD}%"
    exit 1
  fi
  echo "Coverage threshold met (${TOTAL}% >= ${COVERAGE_THRESHOLD}%)"
fi

# A non-empty rerun report on an otherwise green run means a flake was
# absorbed. The after_script reporter prints the loud per-test lines and files
# the issues; this is just a pointer so the main log section says so too.
if [ -s "$RERUN_REPORT" ]; then
  echo ""
  echo "NOTE: this run absorbed test reruns — see the flake report in after_script."
fi
