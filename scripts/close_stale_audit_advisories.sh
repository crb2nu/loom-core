#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: close_stale_audit_advisories.sh --project GROUP/PROJECT --cutoff CUTOFF [--execute]

Select open issues labelled audit_digest that were created strictly before the
cutoff. CUTOFF may be YYYY-MM-DD (midnight UTC) or an RFC3339 UTC timestamp.

The default is a dry run. No issues are changed unless --execute is supplied.
EOF
}

project="${CI_PROJECT_PATH:-}"
cutoff=""
execute=false
while (($#)); do
  case "$1" in
    --project|--repo) [[ $# -ge 2 ]] || { echo "error: $1 needs a value" >&2; exit 2; }; project="$2"; shift 2 ;;
    --cutoff) [[ $# -ge 2 ]] || { echo "error: --cutoff needs a value" >&2; exit 2; }; cutoff="$2"; shift 2 ;;
    --execute) execute=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[[ -n "$project" ]] || { echo "error: --project or CI_PROJECT_PATH is required" >&2; exit 2; }
[[ -n "$cutoff" ]] || { echo "error: --cutoff is required" >&2; exit 2; }
command -v glab >/dev/null || { echo "error: glab is required" >&2; exit 2; }
command -v jq >/dev/null || { echo "error: jq is required" >&2; exit 2; }

# Parse once up front. fromdateiso8601 accepts the normalized UTC spelling and
# provides both calendar validation and a numeric comparison for jq.
if [[ "$cutoff" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}$ ]]; then
  cutoff="${cutoff}T00:00:00Z"
fi
cutoff_epoch="$(jq -ern --arg cutoff "$cutoff" '$cutoff | fromdateiso8601')" || {
  echo "error: --cutoff must be YYYY-MM-DD or an RFC3339 UTC timestamp" >&2
  exit 2
}

encoded_project="$(jq -rn --arg value "$project" '$value | @uri')"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
all_issues="$tmp_dir/all.ndjson"
: >"$all_issues"

# Fetch and validate every page before closing anything. If authentication,
# transport, pagination, or JSON parsing fails, no mutation has occurred.
page=1
while :; do
  page_file="$tmp_dir/page-$page.json"
  glab api "projects/$encoded_project/issues?state=opened&labels=audit_digest&per_page=100&page=$page" >"$page_file"
  jq -e 'type == "array"' "$page_file" >/dev/null || {
    echo "error: GitLab returned a non-array issue page" >&2
    exit 1
  }
  count="$(jq 'length' "$page_file")"
  jq -c '.[]' "$page_file" >>"$all_issues"
  ((count < 100)) && break
  page=$((page + 1))
done

selected="$tmp_dir/selected.ndjson"
jq -c --argjson cutoff "$cutoff_epoch" '
  select(.state == "opened") |
  select(any(.labels[]?; . == "audit_digest")) |
  select((.created_at | fromdateiso8601) < $cutoff) |
  {iid, web_url, title, created_at}
' "$all_issues" >"$selected"

selected_count="$(wc -l <"$selected" | tr -d ' ')"
mode="DRY-RUN"
$execute && mode="EXECUTE"
printf '%s: %s stale audit_digest advisory issue(s) selected in %s before %s\n' \
  "$mode" "$selected_count" "$project" "$cutoff"

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
  echo "No issues changed. Re-run with --execute after reviewing every WOULD_CLOSE line."
fi
