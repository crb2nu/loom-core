#!/usr/bin/env bash
# scripts/mills/health-replay.sh — Factory Health Plane S0 detector replay.
#
# Reconstructs main-branch pipeline health over a window from the GitLab API
# using the Health Plane detector semantics: only terminal success/failed
# pipelines flip the health state; skipped/canceled/manual/running never do.
# Prints every red window, marks the ones that would page (>= threshold), and
# summarizes image-bump lag inputs. Read-only; requires GITLAB_TOKEN.
#
# Usage: GITLAB_TOKEN=... scripts/mills/health-replay.sh [days] [page_threshold_min]
# Defaults: 30 days, 90 minutes. See docs/MILLS_RUNBOOK.md "Factory Health
# Plane" for the 2026-08-29 kill-test run and interpretation guidance.
set -euo pipefail

DAYS="${1:-30}"
THRESH_MIN="${2:-90}"
HOST="${GITLAB_HOST:-gitlab.flexinfer.ai}"
PROJECT="${HEALTH_REPLAY_PROJECT:-services%2Floom-core}"
API="https://${HOST}/api/v4/projects/${PROJECT}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SINCE="$(jq -rn --argjson d "$DAYS" '(now - ($d*86400)) | todate')"

: > "$TMP/pipelines.jsonl"
page=1
while :; do
  chunk="$(curl -sS -H "PRIVATE-TOKEN: ${GITLAB_TOKEN}" \
    "${API}/pipelines?ref=main&updated_after=${SINCE}&per_page=100&page=${page}&order_by=id&sort=asc")"
  n="$(printf '%s' "$chunk" | jq 'length')"
  [ "$n" = "0" ] && break
  printf '%s' "$chunk" | jq -c '.[]' >> "$TMP/pipelines.jsonl"
  page=$((page + 1))
  [ "$page" -gt 25 ] && break
done

total="$(wc -l < "$TMP/pipelines.jsonl" | tr -d ' ')"
printf 'pipelines fetched: %s (ref=main since %s)\n\n' "$total" "$SINCE"

printf '== status histogram ==\n'
jq -r '.status' "$TMP/pipelines.jsonl" | sort | uniq -c | sort -rn

printf '\n== red windows (success/failed-only state flips; >=%sm pages) ==\n' "$THRESH_MIN"
jq -s -r --argjson thresh "$THRESH_MIN" '
  def ts: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601;
  sort_by(.created_at)
  | map(select(.status == "success" or .status == "failed"))
  | reduce .[] as $p ({state: "green", since: null, windows: []};
      if $p.status == "failed" and .state == "green" then
        {state: "red", since: $p.updated_at, windows: .windows}
      elif $p.status == "success" and .state == "red" then
        {state: "green", since: null,
         windows: (.windows + [{from: .since, to: $p.updated_at}])}
      else . end)
  | (if .state == "red"
     then .windows + [{from: .since, to: (now | todate)}]
     else .windows end)
  | .[]
  | . + {mins: ((((.to | ts) - (.from | ts)) / 60) | floor)}
  | "\(.from) -> \(.to)  (\(.mins) min)\(if .mins >= $thresh then "  << WOULD PAGE" else "" end)"
' "$TMP/pipelines.jsonl"

printf '\nNOTE: window edges use updated_at from the list API; the productionized\n'
printf 'poller must use per-pipeline finished_at (retried pipelines mutate\n'
printf 'updated_at and can produce small negative artifacts here).\n'
