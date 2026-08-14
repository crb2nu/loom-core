# One-time audit-advisory sweep

Use this runbook once to close stale rolling `audit_digest` issues. The sweep
is dry-run by default. It does not schedule recurring cleanup, and it cannot
reopen an issue after an incorrect apply.

## Prerequisites

- Use an operator command that wires the GitLab issue client to
  `audit.SweepAuditAdvisories` and passes its arguments through
  `audit.ParseAdvisorySweepFlags`. Set its path as `AUDIT_SWEEP_COMMAND` below.
- Authenticate that client to the target project with permission to list and
  update issues. Confirm the project and bot username before continuing.
- Ensure listing follows every results page. The client contract requires a
  complete open-issue list; a listing error aborts before any close call.
- Reserve an operator window and capture stdout/stderr as the execution record.

The default author is `mills-bot`. The default cutoff is exactly 30 days before
the command's current time. Selection is strictly before the resolved cutoff,
so an issue created exactly at the cutoff is not selected. Override either
default explicitly with `-author` or an RFC3339 `-cutoff`.

## Review the dry-run

Run without `-apply` and save the result:

```bash
"$AUDIT_SWEEP_COMMAND" \
  -author mills-bot \
  -cutoff 2026-07-14T00:00:00Z | tee audit-advisory-dry-run.log
```

Record the resolved cutoff and review every selected IID. A selectable issue
must satisfy every condition: state `opened`, exact author, label
`audit-digest`, exact dated digest title, matching body marker, and creation
time strictly before the cutoff. Reject the run if a human-authored issue, an
`audit-followup`, a malformed marker, or a current/boundary-age digest appears.

Omit `-cutoff` only when the rolling 30-day default is intentional. Never move
to apply without retaining and reviewing the dry-run selection.

## Apply once

Repeat the reviewed command unchanged and add the explicit mutation flag:

```bash
"$AUDIT_SWEEP_COMMAND" \
  -author mills-bot \
  -cutoff 2026-07-14T00:00:00Z \
  -apply | tee audit-advisory-apply.log
```

The implementation completes selection before mutation and closes issues in
the listed order. Keep the output: on failure it identifies the successfully
closed prefix and the IID that failed.

## Verify

Rerun the original dry-run command. It must select zero issues because closed
issues are excluded:

```bash
"$AUDIT_SWEEP_COMMAND" \
  -author mills-bot \
  -cutoff 2026-07-14T00:00:00Z
```

Also inspect the recorded IIDs in GitLab and confirm they are closed. A second
successful `-apply` with the same arguments is a no-op.

## Failure recovery

If listing fails, do not apply: no complete selection exists and the sweep has
made no mutations. Correct authentication, pagination, network, or rate-limit
problems and start again with a dry-run.

If closing fails partway through, preserve the error and closed-prefix output,
correct the cause, and rerun the same `-apply`. Already-closed issues disappear
from the open list, so only the failed issue and later selected issues remain.
Review a fresh dry-run first if the cutoff or author changes.

## Rollback limitation

There is no automatic rollback. Closing an issue may trigger notifications or
other automation that reopening does not undo. If selection was wrong, use the
captured apply log to manually reopen exactly the affected IIDs in GitLab, then
record the incident and verify each issue's state. Do not bulk-reopen issues
that were already closed before this operation.
