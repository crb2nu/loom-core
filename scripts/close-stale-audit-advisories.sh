#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: close-stale-audit-advisories.sh --project GROUP/PROJECT [options]

List stale Mills audit-advisory digest issues. The default is a dry run; issue
mutation requires --execute. In execute mode each issue receives the rollback
label before it is closed.

Options:
  --project GROUP/PROJECT  GitLab project path (or CI_PROJECT_PATH)
  --author USERNAME        Exact digest author (default: producer contract)
  --stale-after DAYS       Select issues older than DAYS (default: 30)
  --execute                Apply the rollback label, then close each issue
  -h, --help               Show this help
EOF
}

project="${CI_PROJECT_PATH:-}"
author=""
stale_after=""
execute=false
while (($#)); do
  case "$1" in
    --project) [[ $# -ge 2 ]] || { echo "error: --project needs a value" >&2; exit 2; }; project="$2"; shift 2 ;;
    --author) [[ $# -ge 2 ]] || { echo "error: --author needs a value" >&2; exit 2; }; author="$2"; shift 2 ;;
    --stale-after) [[ $# -ge 2 ]] || { echo "error: --stale-after needs a value" >&2; exit 2; }; stale_after="$2"; shift 2 ;;
    --execute) execute=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$project" ]] || { echo "error: --project or CI_PROJECT_PATH is required" >&2; exit 2; }
command -v glab >/dev/null || { echo "error: glab is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 2; }

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
identity_contract="$repo_root/pkg/mills/audit/audit.go"
cleanup_contract="$repo_root/pkg/mills/audit/advisory.go"
contract_string() {
  local file="$1" name="$2"
  sed -nE "s/^[[:space:]]*(const[[:space:]]+)?${name}[[:space:]]*=[[:space:]]*\"([^\"]*)\".*/\\2/p" "$file"
}
contract_days() {
  sed -nE 's/^[[:space:]]*const[[:space:]]+AuditAdvisoryDefaultStaleAfter[[:space:]]*=[[:space:]]*([0-9]+)[[:space:]]*\*.*/\1/p' "$cleanup_contract"
}

digest_label="$(contract_string "$identity_contract" AuditAdvisoryDigestLabel)"
title_prefix="$(contract_string "$identity_contract" AuditAdvisoryDigestTitlePrefix)"
title_suffix="$(contract_string "$identity_contract" AuditAdvisoryDigestTitleSuffix)"
marker_prefix="$(contract_string "$identity_contract" AuditAdvisoryDigestMarkerPrefix)"
marker_suffix="$(contract_string "$identity_contract" AuditAdvisoryDigestMarkerSuffix)"
default_author="$(contract_string "$cleanup_contract" AuditAdvisoryDigestAuthor)"
rollback_label="$(contract_string "$cleanup_contract" AuditAdvisoryBulkCloseRollbackLabel)"
default_stale_after="$(contract_days)"
for value in "$digest_label" "$title_prefix" "$title_suffix" "$marker_prefix" "$marker_suffix" "$default_author" "$rollback_label" "$default_stale_after"; do
  [[ -n "$value" ]] || { echo "error: audit-advisory producer contract is unreadable" >&2; exit 2; }
done
author="${author:-$default_author}"
stale_after="${stale_after:-$default_stale_after}"
[[ "$stale_after" =~ ^[1-9][0-9]*$ ]] || { echo "error: --stale-after must be a positive integer" >&2; exit 2; }

cutoff_epoch="$(date -u -d "$stale_after days ago" +%s)"
cutoff="$(date -u -d "@$cutoff_epoch" +%FT%TZ)"
encoded_project="$(jq -rn --arg value "$project" '$value | @uri')"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
all="$tmp_dir/all.ndjson"
: >"$all"

# Discover and validate every page before the first mutation. Any API, JSON,
# pagination, or selector failure therefore leaves every issue untouched.
page=1
while :; do
  page_file="$tmp_dir/page-$page.json"
  glab api "projects/$encoded_project/issues?state=opened&labels=$digest_label&per_page=100&page=$page" >"$page_file"
  jq -e 'type == "array"' "$page_file" >/dev/null || { echo "error: GitLab returned a non-array issue page" >&2; exit 1; }
  count="$(jq -e 'length' "$page_file")"
  [[ "$count" =~ ^[0-9]+$ ]] || { echo "error: invalid GitLab page length" >&2; exit 1; }
  jq -c '.[]' "$page_file" >>"$all"
  ((count < 100)) && break
  page=$((page + 1))
done

selected="$tmp_dir/selected.ndjson"
jq -c --arg author "$author" --arg label "$digest_label" --argjson cutoff "$cutoff_epoch" \
  --arg tp "$title_prefix" --arg ts "$title_suffix" --arg mp "$marker_prefix" --arg ms "$marker_suffix" '
  select(type == "object") |
  select((.iid | type) == "number" and .iid > 0) |
  select(.state == "opened" and .author.username == $author) |
  select(any(.labels[]?; . == $label)) |
  select((.title | type) == "string" and startswith($tp) and endswith($ts)) |
  ($tp | length) as $pl | ($ts | length) as $sl |
  (.title[$pl:(.title | length)-$sl]) as $period |
  select($period | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) |
  # Match strict Go calendar validation: jq may normalize dates
  # such as February 30, so require the parsed date to round-trip exactly.
  select((try ($period + "T00:00:00Z" | fromdateiso8601 | strftime("%Y-%m-%d")) catch "") == $period) |
  select((.created_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) < $cutoff) |
  select((.description // "") | contains($mp + $period + $ms)) |
  {iid, web_url, title, created_at}
' "$all" >"$selected"

selected_count="$(wc -l <"$selected" | tr -d ' ')"
mode="DRY-RUN"
$execute && mode="EXECUTE"
printf '%s: %s stale audit digest issue(s) selected in %s before %s (author=%s rollback-label=%s)\n' \
  "$mode" "$selected_count" "$project" "$cutoff" "$author" "$rollback_label"

while IFS= read -r issue; do
  [[ -n "$issue" ]] || continue
  iid="$(jq -er '.iid' <<<"$issue")"
  url="$(jq -er '.web_url // ""' <<<"$issue")"
  title="$(jq -er '.title' <<<"$issue")"
  if $execute; then
    # Ordering is the rollback invariant: a failed label call aborts before the
    # close call, so no issue can be closed without its recovery marker.
    glab api -X PUT "projects/$encoded_project/issues/$iid" -f "add_labels=$rollback_label" >/dev/null
    printf 'LABELED #%s %s — %s\n' "$iid" "$url" "$title"
    glab api -X PUT "projects/$encoded_project/issues/$iid" -f state_event=close >/dev/null
    printf 'CLOSED #%s %s — %s\n' "$iid" "$url" "$title"
  else
    printf 'WOULD_CLOSE #%s %s — %s\n' "$iid" "$url" "$title"
  fi
done <"$selected"

$execute || echo "No issues changed. Re-run with --execute only after reviewing every WOULD_CLOSE line."
