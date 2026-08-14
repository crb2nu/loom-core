#!/usr/bin/env bash
# Tests for scripts/ci/check_ci_external_endpoints.sh.
#
# A lint that cannot fail is decoration. These cases pin the four behaviours the
# guard is bought for: it flags a direct endpoint, it does NOT flag one that is
# only named in a comment, the allowlist actually suppresses, and an allowlist
# entry without a reason is itself an error.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECK="${ROOT}/scripts/ci/check_ci_external_endpoints.sh"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
pass() { echo "  ok   $1"; }
fail() {
  echo "  FAIL $1"
  failures=$((failures + 1))
}

# run <allowlist-file> <target...> -> sets OUT and RC
run() {
  local allow="$1"
  shift
  OUT="$(CI_ENDPOINT_ALLOWLIST="$allow" bash "$CHECK" "$@" 2>&1)"
  RC=$?
}

: >"${TMP}/empty-allowlist.txt"

echo "check_ci_external_endpoints_test:"

# --- 1. a direct endpoint is caught ------------------------------------------
cat >"${TMP}/dirty.yml" <<'EOF'
job:
  image: docker.io/library/node:20
  script:
    - pip install --index-url https://pypi.org/simple foo
EOF
run "${TMP}/empty-allowlist.txt" "${TMP}/dirty.yml"
if [ "$RC" -ne 0 ] &&
  printf '%s' "$OUT" | grep -q "docker.io" &&
  printf '%s' "$OUT" | grep -q "pypi.org"; then
  pass "flags direct docker.io and pypi.org"
else
  fail "expected non-zero exit naming docker.io and pypi.org; got rc=${RC}: ${OUT}"
fi

# --- 2. endpoints named only in comments are not flagged ---------------------
cat >"${TMP}/comments.yml" <<'EOF'
# We deliberately do not use docker.io here.
job:
  image: registry.harbor.lan/dockerhub-cache/library/node:20  # not pypi.org either
EOF
run "${TMP}/empty-allowlist.txt" "${TMP}/comments.yml"
if [ "$RC" -eq 0 ]; then
  pass "ignores endpoints that appear only in comments"
else
  fail "comment-only mentions should not fail; got rc=${RC}: ${OUT}"
fi

# --- 3. the allowlist suppresses, and only for the listed file+endpoint ------
# The checker keys the allowlist on the path as printed, which for an explicit
# argument outside ROOT is the absolute path.
cat >"${TMP}/allow.txt" <<EOF
${TMP}/dirty.yml docker.io # test fixture
EOF
run "${TMP}/allow.txt" "${TMP}/dirty.yml"
if [ "$RC" -ne 0 ] &&
  ! printf '%s' "$OUT" | grep -q "endpoint 'docker.io'" &&
  printf '%s' "$OUT" | grep -q "endpoint 'pypi.org'"; then
  pass "allowlist suppresses only the listed endpoint"
else
  fail "allowlist should suppress docker.io but keep pypi.org; got rc=${RC}: ${OUT}"
fi

# --- 4. an allowlist entry without a reason is an error ----------------------
printf '%s/dirty.yml docker.io\n' "$TMP" >"${TMP}/noreason.txt"
run "${TMP}/noreason.txt" "${TMP}/dirty.yml"
if [ "$RC" -ne 0 ] && printf '%s' "$OUT" | grep -q "must carry a '# reason'"; then
  pass "rejects an allowlist entry with no reason"
else
  fail "expected a missing-reason error; got rc=${RC}: ${OUT}"
fi

# --- 5. the repo's own CI YAML is clean -------------------------------------
OUT="$(bash "$CHECK" 2>&1)"
RC=$?
if [ "$RC" -eq 0 ]; then
  pass "repo CI YAML is clean"
else
  fail "repo CI YAML has unallowlisted endpoints: ${OUT}"
fi

if [ "$failures" -gt 0 ]; then
  echo "check_ci_external_endpoints_test: ${failures} failure(s)"
  exit 1
fi
echo "check_ci_external_endpoints_test: all checks passed"
