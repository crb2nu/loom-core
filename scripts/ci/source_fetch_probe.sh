#!/usr/bin/env bash
# Source-fetch transport probe.
#
# Diagnostic for the failure class seen in job #212512 (2026-08-02 16:23):
#
#   error: RPC failed; curl 92 HTTP/2 stream 7 was not closed cleanly: INTERNAL_ERROR
#   fetch-pack: unexpected disconnect while reading sideband packet
#   fatal: early EOF
#
# CI job pods reach GitLab over the LAN (`host_aliases` pins
# gitlab.flexinfer.ai -> 192.168.50.227 in the runner chart), so every job's
# own source fetch is exposed to router/flow-cache storms. This probe measures
# three things the retry design in `.clone_repo` depends on:
#
#   phase 1  baseline HTTP/2 failure rate, and — the load-bearing question —
#            whether a failed fetch followed by an IMMEDIATE in-pod retry
#            succeeds (retry-in-place works) or fails again (pod network is
#            wedged; only a job-level retry onto a fresh pod helps).
#   phase 2  HTTP/1.1 failure rate over the same path, to justify pinning
#            `http.version=HTTP/1.1` on later attempts.
#   phase 3  reachability of the in-cluster GitLab service, used as the last
#            attempt's fallback route (bypasses the LAN hop entirely).
#
# Run it from CI:  RUN_FETCH_PROBE=true on a pipeline (see diag:source-fetch-probe).
#
# Env:
#   PROBE_ITERATIONS        phase-1 iterations (default 50)
#   PROBE_HTTP11_ITERATIONS phase-2 iterations (default 10)
#   PROBE_DEADLINE_SECONDS  wall-clock budget for phase 1 (default 900)
#   CI_GIT_INTERNAL_URL     in-cluster GitLab base URL for phase 3

set -uo pipefail

ITERATIONS="${PROBE_ITERATIONS:-50}"
HTTP11_ITERATIONS="${PROBE_HTTP11_ITERATIONS:-10}"
DEADLINE="${PROBE_DEADLINE_SECONDS:-900}"
INTERNAL_URL="${CI_GIT_INTERNAL_URL:-http://gitlab-vm.gitlab.svc.cluster.local}"

if [ -z "${CI_JOB_TOKEN:-}" ]; then
  echo "ERROR: CI_JOB_TOKEN is not set; probe must run inside CI" >&2
  exit 1
fi

PRIMARY="https://gitlab-ci-token:${CI_JOB_TOKEN}@${CI_SERVER_HOST}/${CI_PROJECT_PATH}.git"
internal_scheme="${INTERNAL_URL%%://*}"
internal_host="${INTERNAL_URL#*://}"
internal_host="${internal_host%/}"
INTERNAL="${internal_scheme}://gitlab-ci-token:${CI_JOB_TOKEN}@${internal_host}/${CI_PROJECT_PATH}.git"

WORK="${CI_PROJECT_DIR:-$PWD}/.fetch-probe"
ERRLOG="${WORK}/err.log"

# Never let the token reach the log: git prints the remote URL in some errors.
# `|` as the sed delimiter — job tokens are JWT-ish and never contain one.
redact() { sed -e "s|${CI_JOB_TOKEN}|***|g"; }

# one_fetch <http-version> <remote> -> 0 ok / 1 fail. Leaves stderr in $ERRLOG.
one_fetch() {
  local version="$1" remote="$2"
  git -c "http.version=${version}" -c protocol.version=2 \
    fetch -q --no-tags "$remote" "${CI_COMMIT_SHA}" 2>"$ERRLOG"
}

fresh_repo() {
  rm -rf "${WORK}/repo"
  mkdir -p "${WORK}/repo"
  git -C "${WORK}/repo" init -q
}

echo "=== source-fetch probe ==="
echo "host          : ${CI_SERVER_HOST}"
echo "project       : ${CI_PROJECT_PATH}"
echo "sha           : ${CI_COMMIT_SHA}"
echo "node          : $(cat /etc/hostname 2>/dev/null || echo '?')"
echo "iterations    : ${ITERATIONS} (deadline ${DEADLINE}s)"
echo "internal url  : ${INTERNAL_URL}"
echo

mkdir -p "$WORK"
START=$(date +%s)

# ---------------------------------------------------------------- phase 1 ---
echo "--- phase 1: HTTP/2 baseline + immediate in-pod retry ---"
p1_ok=0
p1_fail=0
p1_retry_ok=0
p1_retry_fail=0
p1_runs=0

for i in $(seq 1 "$ITERATIONS"); do
  now=$(date +%s)
  if [ $((now - START)) -ge "$DEADLINE" ]; then
    echo "deadline reached after ${p1_runs} iterations"
    break
  fi

  fresh_repo
  p1_runs=$((p1_runs + 1))
  t0=$(date +%s)
  if (cd "${WORK}/repo" && one_fetch "HTTP/2" "$PRIMARY"); then
    t1=$(date +%s)
    p1_ok=$((p1_ok + 1))
    echo "  [${i}] ok ($((t1 - t0))s)"
  else
    p1_fail=$((p1_fail + 1))
    echo "  [${i}] FAIL:"
    redact <"$ERRLOG" | sed 's/^/        /'
    # The load-bearing measurement: same pod, same working dir, no sleep.
    if (cd "${WORK}/repo" && one_fetch "HTTP/2" "$PRIMARY"); then
      p1_retry_ok=$((p1_retry_ok + 1))
      echo "        -> immediate in-pod retry SUCCEEDED"
    else
      p1_retry_fail=$((p1_retry_fail + 1))
      echo "        -> immediate in-pod retry ALSO FAILED:"
      redact <"$ERRLOG" | sed 's/^/           /'
    fi
  fi
done
echo

# ---------------------------------------------------------------- phase 2 ---
echo "--- phase 2: HTTP/1.1 over the same path ---"
p2_ok=0
p2_fail=0
for i in $(seq 1 "$HTTP11_ITERATIONS"); do
  fresh_repo
  t0=$(date +%s)
  if (cd "${WORK}/repo" && one_fetch "HTTP/1.1" "$PRIMARY"); then
    t1=$(date +%s)
    p2_ok=$((p2_ok + 1))
    echo "  [${i}] ok ($((t1 - t0))s)"
  else
    p2_fail=$((p2_fail + 1))
    echo "  [${i}] FAIL:"
    redact <"$ERRLOG" | sed 's/^/        /'
  fi
done
echo

# ---------------------------------------------------------------- phase 3 ---
echo "--- phase 3: in-cluster fallback route (${INTERNAL_URL}) ---"
p3_ok=0
p3_fail=0
for i in 1 2 3; do
  fresh_repo
  t0=$(date +%s)
  if (cd "${WORK}/repo" && one_fetch "HTTP/1.1" "$INTERNAL"); then
    t1=$(date +%s)
    p3_ok=$((p3_ok + 1))
    echo "  [${i}] ok ($((t1 - t0))s)"
  else
    p3_fail=$((p3_fail + 1))
    echo "  [${i}] FAIL:"
    redact <"$ERRLOG" | sed 's/^/        /'
  fi
done
echo

rm -rf "$WORK"

# ---------------------------------------------------------------- summary ---
echo "=== summary ==="
printf 'phase 1  HTTP/2 primary      : %d/%d ok, %d fail\n' "$p1_ok" "$p1_runs" "$p1_fail"
printf 'phase 1  in-pod retry        : %d recovered, %d still failed\n' "$p1_retry_ok" "$p1_retry_fail"
printf 'phase 2  HTTP/1.1 primary    : %d/%d ok, %d fail\n' "$p2_ok" "$HTTP11_ITERATIONS" "$p2_fail"
printf 'phase 3  in-cluster fallback : %d/3 ok, %d fail\n' "$p3_ok" "$p3_fail"
echo
if [ "$p1_fail" -eq 0 ] && [ "$p2_fail" -eq 0 ]; then
  echo "VERDICT: quiescent network — no transport failures observed this run."
elif [ "$p1_retry_ok" -gt 0 ] && [ "$p1_retry_fail" -eq 0 ]; then
  echo "VERDICT: retry-in-place recovers every observed failure."
elif [ "$p1_retry_fail" -gt 0 ]; then
  echo "VERDICT: some failures survive an in-pod retry — the fallback route and"
  echo "         the CI-INFRA-FAILURE sentinel carry the weight, not the retries."
fi
if [ "$p3_ok" -eq 0 ]; then
  echo "NOTE: in-cluster fallback route is NOT usable from this pod."
fi

# The probe reports; it never gates. A red probe would tell operators nothing
# they cannot read from the summary above.
exit 0
