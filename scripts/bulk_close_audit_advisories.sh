#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: bulk_close_audit_advisories.sh --repo GROUP/PROJECT --author USERNAME [options]

List stale, bot-authored audit-advisory digest issues. This is a dry-run unless
--execute is supplied.

Options:
  --repo GROUP/PROJECT  GitLab project path (or set CI_PROJECT_PATH)
  --author USERNAME     Exact digest automation author's GitLab username
                        (or set MILLS_AUDIT_DIGEST_AUTHOR)
  --before YYYY-MM-DD   Close digests older than this UTC date (default: today)
  --execute             Close selected issues
  -h, --help            Show this help
EOF
}

execute=false
project="${CI_PROJECT_PATH:-}"
author="${MILLS_AUDIT_DIGEST_AUTHOR:-}"
before="$(date -u +%F)"
while (($#)); do
  case "$1" in
    --repo) [[ $# -ge 2 ]] || { echo "error: --repo needs a value" >&2; exit 2; }; project="$2"; shift 2 ;;
    --author) [[ $# -ge 2 ]] || { echo "error: --author needs a value" >&2; exit 2; }; author="$2"; shift 2 ;;
    --before) [[ $# -ge 2 ]] || { echo "error: --before needs a value" >&2; exit 2; }; before="$2"; shift 2 ;;
    --execute) execute=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$project" ]] || { echo "error: --repo or CI_PROJECT_PATH is required" >&2; exit 2; }
[[ -n "$author" ]] || { echo "error: --author or MILLS_AUDIT_DIGEST_AUTHOR is required" >&2; exit 2; }
[[ "$before" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]] || { echo "error: --before must be YYYY-MM-DD" >&2; exit 2; }
command -v glab >/dev/null || { echo "error: glab is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 2; }
before_epoch="$(jq -ern --arg before "$before" '$before + "T00:00:00Z" | fromdateiso8601')" || {
  echo "error: --before is not a valid calendar date" >&2
  exit 2
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
contract="$repo_root/pkg/mills/audit/audit.go"
contract_value() {
  local name="$1"
  sed -nE "s/^[[:space:]]*${name}[[:space:]]*=[[:space:]]*\"([^\"]*)\".*/\1/p" "$contract"
}
label="$(contract_value AuditAdvisoryDigestLabel)"
title_prefix="$(contract_value AuditAdvisoryDigestTitlePrefix)"
title_suffix="$(contract_value AuditAdvisoryDigestTitleSuffix)"
marker_prefix="$(contract_value AuditAdvisoryDigestMarkerPrefix)"
marker_suffix="$(contract_value AuditAdvisoryDigestMarkerSuffix)"
for value in "$label" "$title_prefix" "$title_suffix" "$marker_prefix" "$marker_suffix"; do
  [[ -n "$value" ]] || { echo "error: audit digest selector contract is unreadable" >&2; exit 2; }
done

encoded_project="$(jq -rn --arg value "$project" '$value|@uri')"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
all="$tmp_dir/all.ndjson"
: >"$all"

# Fetch the complete candidate set before mutating anything. An upstream error
# therefore aborts without making a partially-informed selection.
page=1
while :; do
  page_file="$tmp_dir/page-$page.json"
  glab api "projects/$encoded_project/issues?state=opened&labels=$label&per_page=100&page=$page" >"$page_file"
  jq -e 'type == "array"' "$page_file" >/dev/null || { echo "error: GitLab returned a non-array issue page" >&2; exit 1; }
  count="$(jq 'length' "$page_file")"
  jq -c '.[]' "$page_file" >>"$all"
  (( count < 100 )) && break
  page=$((page + 1))
done

selected="$tmp_dir/selected.ndjson"
jq -c \
  --arg author "$author" --argjson before_epoch "$before_epoch" --arg tp "$title_prefix" \
  --arg ts "$title_suffix" --arg mp "$marker_prefix" --arg ms "$marker_suffix" '
  select(.author.username == $author) |
  select(.title | startswith($tp) and endswith($ts)) |
  ($tp | length) as $pl | ($ts | length) as $sl |
  (.title[$pl:(.title|length)-$sl]) as $period |
  select($period | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) |
  select(($period + "T00:00:00Z" | fromdateiso8601?) < $before_epoch) |
  select((.description // "") | contains($mp + $period + $ms)) |
  {iid, web_url, title, period}
' "$all" >"$selected"

count="$(wc -l <"$selected" | tr -d ' ')"
mode="DRY-RUN"
$execute && mode="EXECUTE"
printf '%s: %s stale audit digest issue(s) selected in %s before %s (author=%s)\n' "$mode" "$count" "$project" "$before" "$author"
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

if ! $execute; then
  echo "No issues changed. Re-run with --execute only after reviewing every WOULD_CLOSE line."
fi
