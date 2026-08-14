#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: audit-advisory-sweep.sh --project GROUP/PROJECT --author USERNAME [options]

One-shot sweep of stale Mills audit-advisory digest issues. The default is a
dry run; no issue changes state unless --execute is explicitly supplied.

Options:
  --project GROUP/PROJECT  GitLab project path (or CI_PROJECT_PATH)
  --author USERNAME        Exact automation author (or MILLS_AUDIT_DIGEST_AUTHOR)
  --stale-after DAYS       Select issues older than DAYS (default: 30)
  --execute                Close every selected issue
  -h, --help               Show this help
EOF
}

project="${CI_PROJECT_PATH:-}"
author="${MILLS_AUDIT_DIGEST_AUTHOR:-}"
stale_after=""
execute=false
while (($#)); do
  case "$1" in
    --project|--repo) [[ $# -ge 2 ]] || { echo "error: $1 needs a value" >&2; exit 2; }; project="$2"; shift 2 ;;
    --author) [[ $# -ge 2 ]] || { echo "error: --author needs a value" >&2; exit 2; }; author="$2"; shift 2 ;;
    --stale-after) [[ $# -ge 2 ]] || { echo "error: --stale-after needs a value" >&2; exit 2; }; stale_after="$2"; shift 2 ;;
    --execute) execute=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$project" ]] || { echo "error: --project or CI_PROJECT_PATH is required" >&2; exit 2; }
[[ -n "$author" ]] || { echo "error: --author or MILLS_AUDIT_DIGEST_AUTHOR is required" >&2; exit 2; }
command -v glab >/dev/null || { echo "error: glab is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 2; }

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
contract="$repo_root/pkg/mills/audit/audit.go"
contract_value() {
  sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"([^\"]*)\".*/\1/p" "$contract"
}
contract_integer() {
  sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p" "$contract"
}
default_stale_after="$(contract_integer DefaultAuditAdvisoryStalenessDays)"
stale_after="${stale_after:-$default_stale_after}"
label="$(contract_value AuditAdvisoryDigestLabel)"
title_prefix="$(contract_value AuditAdvisoryDigestTitlePrefix)"
title_suffix="$(contract_value AuditAdvisoryDigestTitleSuffix)"
marker_prefix="$(contract_value AuditAdvisoryDigestMarkerPrefix)"
marker_suffix="$(contract_value AuditAdvisoryDigestMarkerSuffix)"
[[ -n "$default_stale_after$label$title_prefix$title_suffix$marker_prefix$marker_suffix" ]] || { echo "error: digest selector contract is unreadable" >&2; exit 2; }
[[ "$stale_after" =~ ^[1-9][0-9]*$ ]] || { echo "error: --stale-after must be a positive integer" >&2; exit 2; }

cutoff_epoch="$(date -u -d "$stale_after days ago" +%s)"
cutoff="$(date -u -d "@$cutoff_epoch" +%FT%TZ)"
encoded_project="$(jq -rn --arg value "$project" '$value | @uri')"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
all="$tmp_dir/all.ndjson"
: >"$all"

# Complete and validate discovery before any close, preventing a partial API
# response from authorizing mutation against an incomplete candidate set.
page=1
while :; do
  page_file="$tmp_dir/page-$page.json"
  glab api "projects/$encoded_project/issues?state=opened&labels=$label&per_page=100&page=$page" >"$page_file"
  jq -e 'type == "array"' "$page_file" >/dev/null || { echo "error: GitLab returned a non-array issue page" >&2; exit 1; }
  count="$(jq 'length' "$page_file")"
  jq -c '.[]' "$page_file" >>"$all"
  ((count < 100)) && break
  page=$((page + 1))
done

selected="$tmp_dir/selected.ndjson"
jq -c --arg author "$author" --argjson cutoff "$cutoff_epoch" --arg tp "$title_prefix" \
  --arg ts "$title_suffix" --arg mp "$marker_prefix" --arg ms "$marker_suffix" '
  select(.state == "opened" and .author.username == $author) |
  select(any(.labels[]?; . == $label)) |
  select(.title | startswith($tp) and endswith($ts)) |
  ($tp | length) as $pl | ($ts | length) as $sl |
  (.title[$pl:(.title | length)-$sl]) as $period |
  select($period | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) |
  select((.created_at | sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601) < $cutoff) |
  select((.description // "") | contains($mp + $period + $ms)) |
  {iid, web_url, title, created_at}
' --arg label "$label" "$all" >"$selected"

selected_count="$(wc -l <"$selected" | tr -d ' ')"
mode=DRY-RUN
$execute && mode=EXECUTE
printf '%s: %s stale audit digest issue(s) selected in %s before %s (author=%s)\n' "$mode" "$selected_count" "$project" "$cutoff" "$author"
while IFS= read -r issue; do
  [[ -n "$issue" ]] || continue
  iid="$(jq -r '.iid' <<<"$issue")"
  url="$(jq -r '.web_url' <<<"$issue")"
  title="$(jq -r '.title' <<<"$issue")"
  if $execute; then
    glab api -X PUT "projects/$encoded_project/issues/$iid" -f state_event=close >/dev/null
    printf 'CLOSED #%s %s — %s\n' "$iid" "$url" "$title"
  else
    printf 'WOULD_CLOSE #%s %s — %s\n' "$iid" "$url" "$title"
  fi
done <"$selected"

$execute || echo "No issues changed. Re-run with --execute after reviewing every WOULD_CLOSE line."
