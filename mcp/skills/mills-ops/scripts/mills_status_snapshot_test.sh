#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_TMP="$(mktemp -d)"
trap 'rm -rf -- "${TEST_TMP}"' EXIT

mkdir -p "${TEST_TMP}/bin" "${TEST_TMP}/home/.config/loom"
cat >"${TEST_TMP}/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${MILLS_SNAPSHOT_CURL_LOG:?}"
url="${!#}"
case "${url}" in
  */api/mills/status)
    printf '%s\n' '{"policy_enabled":true}'
    ;;
  */api/mills/council/runs\?limit=50)
    printf '%s\n' '[]'
    ;;
  */api/mills/pipeline/runs)
    printf '%s\n' '[]'
    ;;
  */api/mills/pipeline/runs\?state=terminal\&limit=50)
    printf '%s\n' '[]'
    ;;
  */api/mills/eval/scores\?limit=50)
    printf '%s\n' '[]'
    ;;
  */api/mills/kpis\?window=1d)
    if [[ "${MILLS_SNAPSHOT_FAIL_KPIS:-}" == "1" ]]; then
      exit 22
    fi
    printf '%s\n' '{"ID":7,"SnapshotAt":"2026-07-22T12:00:00Z","WindowSeconds":86400,"Metrics":{"pipeline_cost_usd":1.25,"pipeline_merged_real":2}}'
    ;;
  */api/mills/telemetry/stages\?window=1d)
    printf '%s\n' '{"window_seconds":86400,"generated_at":"2026-07-22T12:00:00Z","runs":{"total":3}}'
    ;;
  *)
    printf 'unexpected URL: %s\n' "${url}" >&2
    exit 22
    ;;
esac
STUB
chmod +x "${TEST_TMP}/bin/curl"

cat >"${TEST_TMP}/home/.config/loom/config.yaml" <<'CONFIG'
hub:
  cf_access_client_id: "config-hub-id"
  cf_access_client_secret: "config-hub-secret"
hud:
  cf_access_client_id: "config-hud-id"
  cf_access_client_secret: "config-hud-secret"
CONFIG

assert_call_count() {
  local log_file="$1"
  if [[ "$(wc -l <"${log_file}")" -ne 7 ]]; then
    printf '%s\n' 'snapshot did not issue the expected seven REST reads' >&2
    exit 1
  fi
}

assert_every_call_has() {
  local log_file="$1"
  local expected="$2"
  if grep -Fv -- "${expected}" "${log_file}" >/dev/null; then
    printf 'snapshot REST read missing expected curl argument: %s\n' "${expected}" >&2
    exit 1
  fi
}

# Public ingress uses bounded calls, the operator bearer token, and the
# Mills-specific Cloudflare Access pair ahead of the generic/config fallbacks.
PUBLIC_LOG="${TEST_TMP}/public-curl.log"
MILLS_SNAPSHOT_CURL_LOG="${PUBLIC_LOG}" \
PATH="${TEST_TMP}/bin:/usr/bin:/bin" \
HOME="${TEST_TMP}/home" \
LOOM_MILLS_OPERATOR_URL="https://mills.example.test" \
LOOM_ADMIN_TOKEN="test-token" \
LOOM_MILLS_CF_ACCESS_ID="mills-id" \
LOOM_MILLS_CF_ACCESS_SECRET="mills-secret" \
CF_ACCESS_CLIENT_ID="generic-id" \
CF_ACCESS_CLIENT_SECRET="generic-secret" \
LOOM_MILLS_CURL_CONNECT_TIMEOUT="3" \
LOOM_MILLS_CURL_MAX_TIME="9" \
  bash "${SCRIPT_DIR}/mills_status_snapshot.sh" >"${TEST_TMP}/public-snapshot.md"

grep -Fq '## Headline KPIs (1d)' "${TEST_TMP}/public-snapshot.md"
grep -Eq '"pipeline_cost_usd"[[:space:]]*:[[:space:]]*1\.25' "${TEST_TMP}/public-snapshot.md"
grep -Fq '## Stage telemetry (1d)' "${TEST_TMP}/public-snapshot.md"
grep -Eq '"total"[[:space:]]*:[[:space:]]*3' "${TEST_TMP}/public-snapshot.md"
assert_call_count "${PUBLIC_LOG}"
assert_every_call_has "${PUBLIC_LOG}" '--connect-timeout 3 --max-time 9'
assert_every_call_has "${PUBLIC_LOG}" '-H Authorization: Bearer test-token'
assert_every_call_has "${PUBLIC_LOG}" '-H CF-Access-Client-Id: mills-id'
assert_every_call_has "${PUBLIC_LOG}" '-H CF-Access-Client-Secret: mills-secret'
if grep -Eq 'generic-|config-' "${PUBLIC_LOG}"; then
  printf '%s\n' 'snapshot did not honor Cloudflare Access credential precedence' >&2
  exit 1
fi

# With no env credentials, match the CLI config fallback (HUD values before
# hub values, because loadHUDConfig returns that shared pair).
CONFIG_LOG="${TEST_TMP}/config-curl.log"
MILLS_SNAPSHOT_CURL_LOG="${CONFIG_LOG}" \
PATH="${TEST_TMP}/bin:/usr/bin:/bin" \
HOME="${TEST_TMP}/home" \
LOOM_MILLS_OPERATOR_URL="https://mills.example.test" \
LOOM_ADMIN_TOKEN="" \
LOOM_MILLS_TOKEN="" \
LOOM_MILLS_CF_ACCESS_ID="" \
LOOM_MILLS_CF_ACCESS_SECRET="" \
CF_ACCESS_CLIENT_ID="" \
CF_ACCESS_CLIENT_SECRET="" \
  bash "${SCRIPT_DIR}/mills_status_snapshot.sh" >"${TEST_TMP}/config-snapshot.md"

assert_call_count "${CONFIG_LOG}"
assert_every_call_has "${CONFIG_LOG}" '-H CF-Access-Client-Id: config-hud-id'
assert_every_call_has "${CONFIG_LOG}" '-H CF-Access-Client-Secret: config-hud-secret'
if grep -Fq 'config-hub-' "${CONFIG_LOG}"; then
  printf '%s\n' 'snapshot preferred hub credentials over HUD credentials' >&2
  exit 1
fi

# Never send Cloudflare credentials to a loopback port-forward. A failed read
# must also terminate within the curl bounds and render the documented fallback.
LOOPBACK_LOG="${TEST_TMP}/loopback-curl.log"
MILLS_SNAPSHOT_CURL_LOG="${LOOPBACK_LOG}" \
MILLS_SNAPSHOT_FAIL_KPIS="1" \
PATH="${TEST_TMP}/bin:/usr/bin:/bin" \
HOME="${TEST_TMP}/home" \
LOOM_MILLS_OPERATOR_URL="http://127.0.0.1:18090" \
LOOM_ADMIN_TOKEN="loopback-token" \
LOOM_MILLS_CF_ACCESS_ID="must-not-leak-id" \
LOOM_MILLS_CF_ACCESS_SECRET="must-not-leak-secret" \
  bash "${SCRIPT_DIR}/mills_status_snapshot.sh" >"${TEST_TMP}/loopback-snapshot.md"

assert_call_count "${LOOPBACK_LOG}"
assert_every_call_has "${LOOPBACK_LOG}" '--connect-timeout 5 --max-time 20'
assert_every_call_has "${LOOPBACK_LOG}" '-H Authorization: Bearer loopback-token'
if grep -Fq 'CF-Access-' "${LOOPBACK_LOG}"; then
  printf '%s\n' 'snapshot leaked Cloudflare Access credentials to loopback' >&2
  exit 1
fi
grep -Fq '(unavailable; KPI snapshot may not be populated yet)' "${TEST_TMP}/loopback-snapshot.md"

if grep -Fq '/metrics' "${PUBLIC_LOG}" "${CONFIG_LOG}" "${LOOPBACK_LOG}"; then
  printf '%s\n' 'snapshot unexpectedly queried the separate metrics listener' >&2
  exit 1
fi

printf '%s\n' 'mills status snapshot smoke test: ok'
