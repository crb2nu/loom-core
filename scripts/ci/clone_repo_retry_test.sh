#!/usr/bin/env bash
# Tests for the `.clone_repo` retry ladder in .gitlab-ci.yml.
#
# That block runs before EVERY Go job, so a control-flow bug in it reds the
# whole pipeline — and it is the one piece of CI that cannot be exercised by
# running CI. The block is extracted verbatim from .gitlab-ci.yml (not copied
# here, so the two cannot drift) and driven against a stub `git` whose failure
# plan is scripted per case.
#
# What is pinned:
#   - a work tree that already exists is left alone (idempotence)
#   - the ladder is HTTP/2, then HTTP/1.1, then HTTP/1.1 against the in-cluster
#     host — three attempts, not three identical ones
#   - exhaustion exits non-zero with `CI-INFRA-FAILURE: source-fetch` as the
#     LAST line, which is what triage greps for
#   - the stall-detection env is exported

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { echo "  ok   $1"; }
fail() {
  echo "  FAIL $1"
  failures=$((failures + 1))
}

# Extract the shell body of the `.clone_repo` anchor from the real CI config.
# awk, not a YAML library: this has to run in the plain golang CI image, which
# has no python3. The anchor is a single `- |` literal block, so dedenting its
# four-space body reproduces the script the runner executes.
awk '
  /^\.clone_repo: &clone_repo$/ { in_anchor = 1; next }
  in_anchor && /^  - \|$/       { in_body = 1; next }
  in_body && /^$/               { print ""; next }
  in_body && /^    /            { sub(/^    /, ""); print; next }
  in_body                       { exit }
' "$ROOT/.gitlab-ci.yml" >"$TMP/clone_repo.sh"

if [ ! -s "$TMP/clone_repo.sh" ] || ! grep -q 'CI-INFRA-FAILURE' "$TMP/clone_repo.sh"; then
  echo "could not extract .clone_repo from .gitlab-ci.yml" >&2
  exit 1
fi
if ! bash -n "$TMP/clone_repo.sh"; then
  echo "extracted .clone_repo block is not valid bash" >&2
  exit 1
fi

BIN="$TMP/bin"
mkdir -p "$BIN"

# Stub `sleep` so the 2s/8s backoff does not cost the test 10 seconds.
cat >"$BIN/sleep" <<'EOF'
#!/bin/sh
echo "sleep $1" >>"$STUB_LOG"
EOF

# Stub `git`. FETCH_PLAN is a space-separated list of exit codes consumed one
# per `fetch`; anything past the end of the plan succeeds.
cat >"$BIN/git" <<'EOF'
#!/usr/bin/env bash
args=("$@")
# Strip leading `-c key=value` pairs, recording them.
config=()
while [ "${1:-}" = "-c" ]; do
  config+=("$2")
  shift 2
done
verb="${1:-}"
case "$verb" in
  rev-parse)
    echo "rev-parse" >>"$STUB_LOG"
    [ "${INSIDE_WORK_TREE:-0}" = "1" ] && exit 0
    exit 128
    ;;
  init|remote|checkout)
    echo "$verb ${*:2}" >>"$STUB_LOG"
    exit 0
    ;;
  fetch)
    n=$(( $(cat "$STUB_COUNTER") + 1 ))
    echo "$n" >"$STUB_COUNTER"
    # Redact the token the same way a real log would need to.
    remote="$(printf '%s' "${*:2}" | sed -e 's|gitlab-ci-token:[^@]*@|gitlab-ci-token:***@|')"
    echo "fetch#${n} cfg=${config[*]-} ${remote}" >>"$STUB_LOG"
    read -r -a plan <<<"${FETCH_PLAN:-}"
    rc="${plan[$((n - 1))]:-0}"
    exit "$rc"
    ;;
  *)
    echo "unexpected git verb: $verb" >>"$STUB_LOG"
    exit 0
    ;;
esac
EOF
chmod +x "$BIN/git" "$BIN/sleep"

# run <fetch-plan> <inside-work-tree> -> sets OUT, RC, LOG
run() {
  STUB_LOG="$TMP/log"
  STUB_COUNTER="$TMP/counter"
  : >"$STUB_LOG"
  echo 0 >"$STUB_COUNTER"
  export STUB_LOG STUB_COUNTER
  OUT="$(
    cd "$TMP" && env \
      PATH="$BIN:$PATH" \
      FETCH_PLAN="$1" \
      INSIDE_WORK_TREE="$2" \
      CI_JOB_TOKEN="tok-en" \
      CI_SERVER_HOST="gitlab.example.test" \
      CI_PROJECT_PATH="services/loom-core" \
      CI_COMMIT_SHA="deadbeef" \
      bash -e "$TMP/clone_repo.sh" 2>&1
  )"
  RC=$?
  LOG="$(cat "$STUB_LOG")"
}

echo "clone_repo_retry_test:"

# --- 1. idempotence ----------------------------------------------------------
run "" 1
if [ "$RC" -eq 0 ] && ! printf '%s' "$LOG" | grep -q '^fetch'; then
  pass "skips entirely when already inside a work tree"
else
  fail "should not fetch inside an existing work tree; rc=${RC} log=${LOG}"
fi

# --- 2. happy path -----------------------------------------------------------
run "0" 0
attempts="$(printf '%s\n' "$LOG" | grep -c '^fetch#')"
if [ "$RC" -eq 0 ] && [ "$attempts" -eq 1 ] &&
  printf '%s' "$LOG" | grep -q 'fetch#1 cfg=http.version=HTTP/2'; then
  pass "one attempt over HTTP/2 when the first fetch succeeds"
else
  fail "expected a single HTTP/2 fetch; rc=${RC} attempts=${attempts} log=${LOG}"
fi

# --- 3. the ladder actually changes route ------------------------------------
run "1 1 0" 0
attempts="$(printf '%s\n' "$LOG" | grep -c '^fetch#')"
if [ "$RC" -eq 0 ] && [ "$attempts" -eq 3 ] &&
  printf '%s' "$LOG" | grep -q 'fetch#2 cfg=http.version=HTTP/1.1' &&
  printf '%s' "$LOG" | grep -q 'fetch#3 cfg=http.version=HTTP/1.1' &&
  printf '%s' "$LOG" | grep -q 'fetch#3 .*gitlab-vm.gitlab.svc.cluster.local' &&
  printf '%s' "$LOG" | grep -q '^sleep 2$' &&
  printf '%s' "$LOG" | grep -q '^sleep 8$'; then
  pass "escalates HTTP/2 -> HTTP/1.1 -> in-cluster host with 2s/8s backoff"
else
  fail "ladder did not escalate as expected; rc=${RC} attempts=${attempts} log=${LOG}"
fi

# --- 4. exhaustion emits the sentinel LAST -----------------------------------
run "1 1 1" 0
last_line="$(printf '%s\n' "$OUT" | grep -v '^[[:space:]]*$' | tail -1)"
attempts="$(printf '%s\n' "$LOG" | grep -c '^fetch#')"
if [ "$RC" -ne 0 ] && [ "$attempts" -eq 3 ] &&
  [ "$last_line" = "CI-INFRA-FAILURE: source-fetch" ]; then
  pass "exhaustion fails with the sentinel as the last line"
else
  fail "expected sentinel last on exhaustion; rc=${RC} attempts=${attempts} last='${last_line}'"
fi

# --- 5. the token never reaches the log --------------------------------------
run "1 1 1" 0
if ! printf '%s' "$OUT" | grep -q 'tok-en'; then
  pass "does not print the job token"
else
  fail "job token leaked into job output: ${OUT}"
fi

# --- 6. stall detection is exported ------------------------------------------
cat >"$TMP/echo_env.sh" <<'EOF'
echo "LOW_SPEED_LIMIT=${GIT_HTTP_LOW_SPEED_LIMIT:-unset}"
echo "LOW_SPEED_TIME=${GIT_HTTP_LOW_SPEED_TIME:-unset}"
EOF
OUT="$(
  cd "$TMP" && env \
    PATH="$BIN:$PATH" STUB_LOG="$TMP/log" STUB_COUNTER="$TMP/counter" \
    FETCH_PLAN="0" INSIDE_WORK_TREE=0 \
    CI_JOB_TOKEN="tok-en" CI_SERVER_HOST="gitlab.example.test" \
    CI_PROJECT_PATH="services/loom-core" CI_COMMIT_SHA="deadbeef" \
    bash -e -c 'echo 0 >"$STUB_COUNTER"; . "$1"; . "$2"' _ \
    "$TMP/clone_repo.sh" "$TMP/echo_env.sh" 2>&1
)"
if printf '%s' "$OUT" | grep -q 'LOW_SPEED_LIMIT=1000' &&
  printf '%s' "$OUT" | grep -q 'LOW_SPEED_TIME=30'; then
  pass "exports GIT_HTTP_LOW_SPEED_LIMIT/TIME for stall detection"
else
  fail "stall-detection env not exported: ${OUT}"
fi

if [ "$failures" -gt 0 ]; then
  echo "clone_repo_retry_test: ${failures} failure(s)"
  exit 1
fi
echo "clone_repo_retry_test: all checks passed"
