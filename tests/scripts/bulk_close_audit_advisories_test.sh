#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/bulk_close_audit_advisories.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin"

cat >"$tmp_dir/bin/glab" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
method=GET
path=""
while (($#)); do
  case "$1" in
    api) shift ;;
    -X) method="$2"; shift 2 ;;
    -f) shift 2 ;;
    *) path="$1"; shift ;;
  esac
done
if [[ "$method" == PUT ]]; then
  iid="${path##*/}"
  printf '%s\n' "$iid" >>"$MOCK_STATE"
  printf '{}\n'
  exit 0
fi
page="$(sed -nE 's/.*[?&]page=([0-9]+).*/\1/p' <<<"$path")"
if [[ "${MOCK_FAIL_PAGE:-}" == "$page" ]]; then
  echo 'mock upstream failure' >&2
  exit 1
fi
jq --argjson page "$page" --slurpfile closed <(jq -Rsc 'split("\n")[:-1]' "$MOCK_STATE") '
  map(select((.page == $page) and ((.iid|tostring) as $iid | ($closed[0] | index($iid) | not))))
  | map(del(.page))
' "$MOCK_FIXTURES"
MOCK
chmod +x "$tmp_dir/bin/glab" "$script"

fixtures="$tmp_dir/issues.json"
state="$tmp_dir/closed"
: >"$state"
jq -n '
  def issue($iid; $page; $date; $author; $title; $marker): {
    iid: $iid, page: $page, web_url: ("https://gitlab.test/issues/" + ($iid|tostring)),
    title: $title, description: $marker, author: {username: $author}
  };
  [range(1; 99) as $n | issue(1000+$n; 1; "2026-08-01"; "someone"; "Unrelated"; "none")]
  + [
    issue(1; 1; "2026-08-01"; "mills-bot"; "Audit advisory digest — 2026-08-01 (UTC)"; "<!-- mills-audit-digest:period=2026-08-01 -->"),
    issue(2; 1; "2026-08-02"; "mills-bot"; "Audit advisory digest — 2026-08-02 (UTC)"; "<!-- mills-audit-digest:period=2026-08-02 -->"),
    issue(3; 2; "2026-08-05"; "mills-bot"; "Audit advisory digest — 2026-08-05 (UTC)"; "<!-- mills-audit-digest:period=2026-08-05 -->"),
    issue(4; 2; "2026-08-06"; "mills-bot"; "Audit advisory digest — 2026-08-06 (UTC)"; "<!-- mills-audit-digest:period=2026-08-06 -->"),
    issue(5; 2; "2026-08-01"; "human"; "Audit advisory digest — 2026-08-01 (UTC)"; "<!-- mills-audit-digest:period=2026-08-01 -->"),
    issue(6; 2; "2026-08-01"; "mills-bot"; "Audit advisory digest — 2026-08-01 (UTC)"; "wrong marker"),
    issue(7; 2; "2026-08-01"; "mills-bot"; "Human audit note"; "<!-- mills-audit-digest:period=2026-08-01 -->"),
    issue(8; 2; "2026-08-01"; "mills-bot"; "Audit advisory digest — 2026-08-01 (UTC)"; null)
  ]
' >"$fixtures"

export PATH="$tmp_dir/bin:$PATH" MOCK_FIXTURES="$fixtures" MOCK_STATE="$state"
args=(--repo group/project --author mills-bot --before 2026-08-06)

dry_output="$($script "${args[@]}")"
grep -q 'DRY-RUN: 3 stale' <<<"$dry_output"
[[ ! -s "$state" ]]

execute_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 3 stale' <<<"$execute_output"
[[ "$(sort -n "$state" | tr '\n' ' ')" == "1 2 3 " ]]

repeat_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 0 stale' <<<"$repeat_output"
[[ "$(wc -l <"$state" | tr -d ' ')" == 3 ]]

: >"$state"
export MOCK_FAIL_PAGE=2
if "$script" "${args[@]}" --execute >/dev/null 2>&1; then
  echo "expected paginated fetch failure" >&2
  exit 1
fi
[[ ! -s "$state" ]]

echo "bulk-close audit advisory tests: passed"
