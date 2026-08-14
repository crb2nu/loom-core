#!/usr/bin/env bash
# mills_status_snapshot.sh
#
# Read-only snapshot of loom-mills-operator state. Combines:
#   - cluster pod/PVC/ConfigMap status
#   - operator REST /api/mills/status
#   - last 50 council + pipeline runs
#   - latest eval scores per loop
#   - rolling 1d KPIs and stage telemetry from the operator REST API
#
# Output is markdown to stdout. Safe to run from a Mac with kubeconfig +
# optional read token configured. No mutations.
# This script is inherently read-only; --dry-run/--apply modes are unnecessary.
#
# Optional env:
#   LOOM_MILLS_OPERATOR_URL   defaults to https://mills.flexinfer.ai
#   LOOM_ADMIN_TOKEN          operator bearer token when required
#   LOOM_MILLS_TOKEN          legacy/read bearer token fallback
#   LOOM_MILLS_CF_ACCESS_ID / LOOM_MILLS_CF_ACCESS_SECRET
#                             Cloudflare Access service token for public ingress
#   CF_ACCESS_CLIENT_ID / CF_ACCESS_CLIENT_SECRET
#                             generic Cloudflare Access fallback
#   LOOM_MILLS_CURL_CONNECT_TIMEOUT / LOOM_MILLS_CURL_MAX_TIME
#                             curl deadlines in seconds (defaults: 5 / 20)
#   KUBECONFIG                kubeconfig used by kubectl when available

set -euo pipefail

OPERATOR_URL="${LOOM_MILLS_OPERATOR_URL:-https://mills.flexinfer.ai}"
TOKEN="${LOOM_ADMIN_TOKEN:-${LOOM_MILLS_TOKEN:-}}"
CURL_CONNECT_TIMEOUT="${LOOM_MILLS_CURL_CONNECT_TIMEOUT:-5}"
CURL_MAX_TIME="${LOOM_MILLS_CURL_MAX_TIME:-20}"

# Keep Cloudflare Access resolution aligned with the loom Mills client:
# Mills-specific env, then generic env, then HUD/hub values from config.yaml.
# Config parsing intentionally supports the simple scalar values emitted by loom
# without evaluating shell or YAML content.
read_loom_config_scalar() {
  local section="$1"
  local key="$2"
  local config_file="${HOME}/.config/loom/config.yaml"

  [[ -f "${config_file}" ]] || return 0
  awk -v wanted_section="${section}" -v wanted_key="${key}" '
    /^[^[:space:]#][^:]*:[[:space:]]*(#.*)?$/ {
      section = $0
      sub(/:.*/, "", section)
      in_section = (section == wanted_section)
      next
    }
    in_section {
      line = $0
      sub(/^[[:space:]]+/, "", line)
      prefix = wanted_key ":"
      if (index(line, prefix) != 1) {
        next
      }
      value = substr(line, length(prefix) + 1)
      sub(/^[[:space:]]+/, "", value)
      sub(/[[:space:]]+$/, "", value)
      if (length(value) >= 2) {
        first = substr(value, 1, 1)
        last = substr(value, length(value), 1)
        if ((first == "\"" && last == "\"") || (first == "\047" && last == "\047")) {
          value = substr(value, 2, length(value) - 2)
        }
      }
      print value
      exit
    }
  ' "${config_file}"
}

if [[ -n "${LOOM_MILLS_CF_ACCESS_ID:-}" ]]; then
  CF_ACCESS_ID="${LOOM_MILLS_CF_ACCESS_ID}"
  CF_ACCESS_SECRET="${LOOM_MILLS_CF_ACCESS_SECRET:-}"
elif [[ -n "${CF_ACCESS_CLIENT_ID:-}" ]]; then
  CF_ACCESS_ID="${CF_ACCESS_CLIENT_ID}"
  CF_ACCESS_SECRET="${CF_ACCESS_CLIENT_SECRET:-}"
else
  CONFIG_HUD_CF_ID="$(read_loom_config_scalar hud cf_access_client_id)"
  CONFIG_HUD_CF_SECRET="$(read_loom_config_scalar hud cf_access_client_secret)"
  CONFIG_HUB_CF_ID="$(read_loom_config_scalar hub cf_access_client_id)"
  CONFIG_HUB_CF_SECRET="$(read_loom_config_scalar hub cf_access_client_secret)"
  CF_ACCESS_ID="${CONFIG_HUD_CF_ID:-${CONFIG_HUB_CF_ID}}"
  CF_ACCESS_SECRET="${CONFIG_HUD_CF_SECRET:-${CONFIG_HUB_CF_SECRET}}"
fi

NS="${LOOM_MILLS_NS:-loom-mills}"

now() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

operator_is_loopback() {
  local authority="${OPERATOR_URL#*://}"
  local host

  authority="${authority%%/*}"
  authority="${authority##*@}"
  if [[ "${authority}" == \[* ]]; then
    host="${authority#\[}"
    host="${host%%\]*}"
  else
    host="${authority%%:*}"
  fi
  host="$(printf '%s' "${host}" | tr '[:upper:]' '[:lower:]')"
  case "${host}" in
    localhost|::1|127.*) return 0 ;;
    *) return 1 ;;
  esac
}

curl_op() {
  local curl_args=(-sf --connect-timeout "${CURL_CONNECT_TIMEOUT}" --max-time "${CURL_MAX_TIME}")
  if [[ -n "${TOKEN}" ]]; then
    curl_args+=(-H "Authorization: Bearer ${TOKEN}")
  fi
  if [[ -n "${CF_ACCESS_ID}" && -n "${CF_ACCESS_SECRET}" ]] && ! operator_is_loopback; then
    curl_args+=(-H "CF-Access-Client-Id: ${CF_ACCESS_ID}")
    curl_args+=(-H "CF-Access-Client-Secret: ${CF_ACCESS_SECRET}")
  fi
  curl "${curl_args[@]}" "${OPERATOR_URL}$1"
}

printf '# Loom Mills Status Snapshot\n\n'
printf '_Captured: %s_\n\n' "$(now)"

printf '## Cluster\n\n'
if command -v kubectl >/dev/null 2>&1; then
  printf '```\n'
  kubectl --request-timeout=10s get deploy,pod,pvc,cm,svc -n "${NS}" 2>&1 || true
  printf '```\n\n'
  printf '### Recent operator log (last 30 lines)\n\n'
  printf '```\n'
  kubectl --request-timeout=10s logs -n "${NS}" deploy/loom-mills-operator --tail=30 2>&1 || true
  printf '```\n\n'
else
  printf '_kubectl not found; skipping cluster section_\n\n'
fi

printf '## Operator status\n\n'
printf '```json\n'
curl_op /api/mills/status | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unreachable)"
printf '\n```\n\n'

printf '## Recent council runs (50)\n\n'
printf '```json\n'
curl_op '/api/mills/council/runs?limit=50' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unreachable)"
printf '\n```\n\n'

printf '## Active pipeline runs\n\n'
printf '```json\n'
curl_op '/api/mills/pipeline/runs' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unreachable)"
printf '\n```\n\n'

printf '## Recent terminal pipeline runs (50)\n\n'
printf '```json\n'
curl_op '/api/mills/pipeline/runs?state=terminal&limit=50' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unreachable)"
printf '\n```\n\n'

printf '## Eval scores (last 50)\n\n'
printf '```json\n'
curl_op '/api/mills/eval/scores?limit=50' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unreachable)"
printf '\n```\n\n'

printf '## Headline KPIs (1d)\n\n'
printf '```json\n'
curl_op '/api/mills/kpis?window=1d' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unavailable; KPI snapshot may not be populated yet)"
printf '\n```\n\n'

printf '## Stage telemetry (1d)\n\n'
printf '```json\n'
curl_op '/api/mills/telemetry/stages?window=1d' | (command -v jq >/dev/null && jq . || cat) || \
  printf '%s\n' "(unavailable)"
printf '\n```\n'
