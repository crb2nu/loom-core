#!/bin/sh
# Live, opt-in MR-awareness crash/restart proof. The summary file doubles as
# the recovery token, so rerunning this command adopts the recorded run/MR.
set -u

summary=${MILLS_MR_AWARENESS_SUMMARY:-mr-awareness-summary.json}
binary=${MILLS_KILLTEST_BIN:-./bin/mills-workflow-killtest}

write_wrapper_failure() {
    code=$1
    detail=$2
    if [ ! -s "$summary" ]; then
        escaped=$(printf '%s' "$detail" | sed 's/\\/\\\\/g; s/"/\\"/g')
        printf '{"mode":"mr-awareness","verdict":"FAIL","passed":false,"reason_code":"%s","detail":"%s","mr_count":0,"worker_restarts":0,"recovered":false}\n' "$code" "$escaped" >"$summary"
    fi
}

for name in LOOM_MILLS_ADMIN_TOKEN GITLAB_TOKEN GITLAB_PROJECT MILLS_MR_AWARENESS_WORKER_COMMAND MILLS_MR_AWARENESS_STATE_DIR MILLS_MR_AWARENESS_BACKLOG_ID MILLS_MR_AWARENESS_SOURCE_BRANCH; do
    eval "value=\${$name-}"
    if [ -z "$value" ]; then
        write_wrapper_failure invalid_configuration "required environment variable $name is unset"
        cat "$summary"
        exit 2
    fi
done

if [ ! -x "$binary" ]; then
    write_wrapper_failure invalid_configuration "kill-test binary is not executable: $binary"
    cat "$summary"
    exit 2
fi

"$binary" \
    -mode mr-awareness \
    -operator-url "${MILLS_OPERATOR_URL:-http://localhost:8090}" \
    -admin-token "$LOOM_MILLS_ADMIN_TOKEN" \
    -gitlab-api-url "${GITLAB_API_URL:-https://gitlab.flexinfer.ai/api/v4}" \
    -gitlab-token "$GITLAB_TOKEN" \
    -gitlab-project "$GITLAB_PROJECT" \
    -mr-awareness-worker-command "$MILLS_MR_AWARENESS_WORKER_COMMAND" \
    -mr-awareness-state-dir "$MILLS_MR_AWARENESS_STATE_DIR" \
    -mr-awareness-backlog-id "$MILLS_MR_AWARENESS_BACKLOG_ID" \
    -mr-awareness-source-branch "$MILLS_MR_AWARENESS_SOURCE_BRANCH" \
    -mr-awareness-timeout "${MILLS_MR_AWARENESS_TIMEOUT:-20m}" \
    -mr-awareness-poll "${MILLS_MR_AWARENESS_POLL:-2s}" \
    -mr-awareness-recovery-max-age "${MILLS_MR_AWARENESS_RECOVERY_MAX_AGE:-24h}" \
    -evidence "$summary"
status=$?
if [ "$status" -ne 0 ]; then
    write_wrapper_failure killtest_failed "kill-test binary exited with status $status"
fi
exit "$status"
