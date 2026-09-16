#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="$repo_root/scripts/close-stale-audit-advisories.sh"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin"

cat >"$tmp_dir/bin/glab" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
method=GET
path=""
field=""
while (($#)); do
  case "$1" in
    api) shift ;;
    -X) method="$2"; shift 2 ;;
    -f) field="$2"; shift 2 ;;
    *) path="$1"; shift ;;
  esac
done
if [[ "$method" == PUT ]]; then
  printf '%s %s\n' "${path##*/}" "$field" >>"$MOCK_STATE"
  printf '{}\n'
  exit 0
fi
page="$(sed -nE 's/.*[?&]page=([0-9]+).*/\1/p' <<<"$path")"
if [[ "${MOCK_FAIL_PAGE:-}" == "$page" ]]; then
  echo 'mock upstream failure' >&2
  exit 1
fi
closed="$(sed -nE 's/^([0-9]+) state_event=close$/\1/p' "$MOCK_STATE" | jq -Rsc 'split("\n")[:-1]')"
jq --argjson page "$page" --argjson closed "$closed" '
  map(select((.page == $page) and ((.iid | tostring) as $iid | ($closed | index($iid) | not))))
  | map(del(.page))
' "$MOCK_FIXTURES"
MOCK
chmod +x "$tmp_dir/bin/glab" "$script"

# The script's --stale-after cutoff is relative to the current clock, so the
# fixtures compute their timestamps relative to now as well.
stale_ts="$(jq -rn 'now - (40 * 86400) | floor | todate')"
fresh_ts="$(jq -rn 'now - 86400 | floor | todate')"
stale_frac_ts="${stale_ts%Z}.123456Z"

fixtures="$tmp_dir/issues.json"
state="$tmp_dir/closed"
: >"$state"
# Titles, markers, label, author, and rollback label are spelled literally so
# this test doubles as a cross-check of the producer contract in
# pkg/mills/audit/audit.go and pkg/mills/audit/advisory.go.
jq -n --arg stale "$stale_ts" --arg fresh "$fresh_ts" --arg stale_frac "$stale_frac_ts" '
  def issue($iid; $page; $st; $labels; $author; $created; $title; $desc): {
    iid: $iid, page: $page, state: $st, labels: $labels,
    author: {username: $author}, created_at: $created,
    web_url: ("https://gitlab.test/issues/" + ($iid | tostring)),
    title: $title, description: $desc
  };
  def digest_title($p): "Audit advisory digest — " + $p + " (UTC)";
  def digest_marker($p): "<!-- mills-audit-digest:period=" + $p + " -->";
  [range(1; 98) as $n
    | issue(1000 + $n; 1; "opened"; ["other"]; "someone"; $stale; "Unrelated"; "none")]
  + [
    issue(1; 1; "opened"; ["audit-digest"]; "mills-bot"; $stale;      digest_title("2026-07-01"); digest_marker("2026-07-01")),
    issue(2; 1; "opened"; ["audit-digest"]; "mills-bot"; $stale_frac; digest_title("2026-07-02"); digest_marker("2026-07-02")),
    issue(3; 1; "opened"; ["audit-digest"]; "mills-bot"; $fresh;      digest_title("2026-07-03"); digest_marker("2026-07-03")),
    issue(4; 2; "opened"; ["audit-digest"]; "human";     $stale;      digest_title("2026-07-04"); digest_marker("2026-07-04")),
    issue(5; 2; "opened"; ["other"];        "mills-bot"; $stale;      digest_title("2026-07-05"); digest_marker("2026-07-05")),
    issue(6; 2; "opened"; ["audit-digest"]; "mills-bot"; $stale;      "Human audit note";         digest_marker("2026-07-06")),
    issue(7; 2; "opened"; ["audit-digest"]; "mills-bot"; $stale;      digest_title("2026-07-07"); "wrong marker"),
    issue(8; 2; "opened"; ["audit-digest"]; "mills-bot"; $stale;      digest_title("2026-02-30"); digest_marker("2026-02-30")),
    issue(9; 2; "closed"; ["audit-digest"]; "mills-bot"; $stale;      digest_title("2026-07-09"); digest_marker("2026-07-09"))
  ]
' >"$fixtures"

export PATH="$tmp_dir/bin:$PATH" MOCK_FIXTURES="$fixtures" MOCK_STATE="$state"
args=(--project group/project)

dry_output="$($script "${args[@]}")"
grep -q 'DRY-RUN: 2 stale' <<<"$dry_output"
grep -q 'WOULD_CLOSE #1 ' <<<"$dry_output"
grep -q 'WOULD_CLOSE #2 ' <<<"$dry_output"
[[ ! -s "$state" ]]

# --repo must select identically to --project.
alias_output="$($script --repo group/project)"
grep -q 'DRY-RUN: 2 stale' <<<"$alias_output"
[[ ! -s "$state" ]]

execute_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 2 stale' <<<"$execute_output"
grep -q 'LABELED #1 ' <<<"$execute_output"
grep -q 'CLOSED #2 ' <<<"$execute_output"
# The rollback invariant: each issue is labeled strictly before it is closed.
expected=$'1 add_labels=audit-bulk-close-2026-08-14\n1 state_event=close\n2 add_labels=audit-bulk-close-2026-08-14\n2 state_event=close'
[[ "$(cat "$state")" == "$expected" ]]

repeat_output="$($script "${args[@]}" --execute)"
grep -q 'EXECUTE: 0 stale' <<<"$repeat_output"
[[ "$(wc -l <"$state" | tr -d ' ')" == 4 ]]

: >"$state"
export MOCK_FAIL_PAGE=2
if "$script" "${args[@]}" --execute >/dev/null 2>&1; then
  echo "expected paginated fetch failure" >&2
  exit 1
fi
[[ ! -s "$state" ]]
unset MOCK_FAIL_PAGE

if "$script" "${args[@]}" --stale-after 0 --execute >/dev/null 2>&1; then
  echo "expected invalid --stale-after failure" >&2
  exit 1
fi
[[ ! -s "$state" ]]

echo "close-stale-audit-advisories tests: passed"
