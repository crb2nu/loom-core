#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/close_stale_audit_advisories.sh"
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
  printf '%s\n' "${path##*/}" >>"$MOCK_STATE"
  exit 0
fi
page="$(sed -nE 's/.*[?&]page=([0-9]+).*/\1/p' <<<"$path")"
if [[ "${MOCK_FAIL_PAGE:-}" == "$page" ]]; then exit 1; fi
jq --argjson page "$page" --slurpfile closed <(jq -Rsc 'split("\n")[:-1]' "$MOCK_STATE") '
  map(select(.page == $page)) |
  map(select((.iid | tostring) as $iid | ($closed[0] | index($iid) | not))) |
  map(del(.page))
' "$MOCK_FIXTURES"
MOCK
chmod +x "$tmp_dir/bin/glab" "$script"

fixtures="$tmp_dir/issues.json"
state="$tmp_dir/closed"
: >"$state"
jq -n '
  def issue($iid; $page; $state; $labels; $created): {
    iid: $iid, page: $page, state: $state, labels: $labels, created_at: $created,
    web_url: ("https://gitlab.test/issues/" + ($iid | tostring)), title: ("issue " + ($iid | tostring))
  };
  [range(1; 100) as $n | issue(1000 + $n; 1; "opened"; ["other"]; "2026-01-01T00:00:00Z")]
  + [
    issue(1; 1; "opened"; ["audit_digest"]; "2026-08-01T00:00:00Z"),
    issue(2; 2; "opened"; ["other", "audit_digest"]; "2026-08-04T23:59:59Z"),
    issue(3; 2; "opened"; ["audit_digest"]; "2026-08-05T00:00:00Z"),
    issue(4; 2; "closed"; ["audit_digest"]; "2026-08-01T00:00:00Z"),
    issue(5; 2; "opened"; ["other"]; "2026-08-01T00:00:00Z")
  ]
' >"$fixtures"

export PATH="$tmp_dir/bin:$PATH" MOCK_FIXTURES="$fixtures" MOCK_STATE="$state"
args=(--project group/project --cutoff 2026-08-05)

dry_output="$($script "${args[@]}")"
grep -q 'DRY-RUN: 2 stale' <<<"$dry_output"
[[ ! -s "$state" ]]

execute_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 2 stale' <<<"$execute_output"
[[ "$(sort -n "$state" | tr '\n' ' ')" == "1 2 " ]]

repeat_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 0 stale' <<<"$repeat_output"
[[ "$(wc -l <"$state" | tr -d ' ')" == 2 ]]

: >"$state"
export MOCK_FAIL_PAGE=2
if "$script" "${args[@]}" --execute >/dev/null 2>&1; then
  echo "expected paginated fetch failure" >&2
  exit 1
fi
[[ ! -s "$state" ]]
unset MOCK_FAIL_PAGE

if "$script" --project group/project --cutoff not-a-date --execute >/dev/null 2>&1; then
  echo "expected invalid cutoff failure" >&2
  exit 1
fi
[[ ! -s "$state" ]]

echo "stale audit advisory close tests: passed"
