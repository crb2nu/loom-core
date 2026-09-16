#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture_dir=$(mktemp -d "${TMPDIR:-/tmp}/mr-awareness.XXXXXX")
trap 'rm -rf "$fixture_dir"' EXIT HUP INT TERM
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)

write_fixture() {
    fixture=$1
    captured_at=$2
    recognitions=$3
    cat >"$fixture_dir/$fixture.json" <<EOF
{"scenario":"mr-awareness","captured_at":"$captured_at","deadline":"9999-12-31T23:59:59Z","mr_awareness":{"repo":"services/loom-core","iid":42,"source_branch":"feat/mr-awareness","recognitions":$recognitions}}
EOF
}

run_fixture() {
    go run ./cmd/mills-workflow-killtest \
        -scenario mr-awareness \
        -scenario-max-age 24h \
        -evidence "$fixture_dir/$1.json"
}

cd "$repo_dir"
valid_recognition="[{\"repo\":\"services/loom-core\",\"iid\":42,\"source_branch\":\"feat/mr-awareness\",\"state\":\"pipeline_running\",\"recognized\":true,\"observed_at\":\"$now\"}]"
write_fixture happy "$now" "$valid_recognition"
write_fixture absent "$now" '[]'
write_fixture contradictory "$now" "[{\"repo\":\"services/loom-core\",\"iid\":43,\"source_branch\":\"feat/mr-awareness\",\"state\":\"pipeline_running\",\"recognized\":true,\"observed_at\":\"$now\"}]"
write_fixture stale '2000-01-01T00:00:00Z' "$valid_recognition"

run_fixture happy

for fixture in absent contradictory stale; do
    case "$fixture" in
        absent) expected_error='"reason_code":"duplicate_or_ambiguous_evidence"' ;;
        contradictory) expected_error='"reason_code":"contradictory_identity"' ;;
        stale) expected_error='"reason_code":"stale_evidence"' ;;
    esac
    log_file="$fixture_dir/$fixture.log"
    if run_fixture "$fixture" >"$log_file" 2>&1; then
        echo "MR-awareness kill-test failed: ${fixture} fixture was accepted" >&2
        exit 1
    fi
    if ! grep -F "$expected_error" "$log_file" >/dev/null; then
        echo "MR-awareness kill-test failed: ${fixture} fixture failed for an unexpected reason" >&2
        cat "$log_file" >&2
        exit 1
    fi
done

echo 'MR-awareness kill-test passed: pipeline recognized valid MR state; absent, contradictory, and stale evidence rejected'
