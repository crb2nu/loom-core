# One-time stale audit-advisory bulk close

Use this procedure once to close pre-existing stale Mills audit digest issues.
The script is dry-run by default, reads its selection identity from the Go
producer contract, discovers all pages before mutation, and excludes current,
nonmatching, and already-closed issues.

## Prerequisites

- Authenticate `glab` to the target GitLab project with issue update access.
- Install `jq` and use GNU `date`.
- Reserve an operator window and retain command output as the audit record.
- Confirm the expected author (`mills-bot` by default), project, and 30-day
  staleness window. Override the window only with an explicit positive
  `--stale-after DAYS` value.

## Dry-run and review

Run without `--execute`:

```bash
scripts/close-stale-audit-advisories.sh \
  --project services/loom-core | tee audit-bulk-close-dry-run.log
```

Review every `WOULD_CLOSE` line. Each selected issue must be open, older than
the printed cutoff, authored by the exact digest bot, and carry the producer's
label, dated title, and matching body marker. Stop if the selection includes a
current digest, a human issue, or another advisory type.

The command fetches and validates every page before printing or mutating its
selection. An API error, malformed page, invalid timestamp, or selector error
exits nonzero. Do not execute when discovery fails or the dry-run is incomplete.

## Execute and verify

Repeat the reviewed command unchanged and add `--execute`:

```bash
scripts/close-stale-audit-advisories.sh \
  --project services/loom-core \
  --execute | tee audit-bulk-close-execute.log
```

For each selected issue, the script first adds
`audit-bulk-close-2026-08-14`, verifies that API call succeeded, and only then
closes the issue. It stops at the first failure. Rerun the original dry-run;
it must select zero issues. Then query the rollback label and compare its IIDs
exactly with the `CLOSED` lines:

```bash
glab issue list \
  --repo services/loom-core \
  --all \
  --label audit-bulk-close-2026-08-14
```

No issue outside the execution log should carry this one-time label.

## Partial failure

If discovery fails, no mutation occurred; correct the API, authentication, or
JSON problem and restart with a dry-run. If labeling fails, that issue remains
open and the script stops. If closing fails after a `LABELED` line, the issue
remains open but marked; retain that line as the record, correct the cause, and
rerun the reviewed command.
Already-closed issues are excluded, making an unchanged rerun idempotent.

## Roll back by label

Closing may send notifications or trigger automation that reopening cannot
undo. If rollback is approved, first confirm that the label query equals the
saved `CLOSED` set. Reopen precisely that set:

```bash
glab issue list \
  --repo services/loom-core \
  --state closed \
  --label audit-bulk-close-2026-08-14 \
  --output json | jq -r '.[].iid' | while read -r iid; do
    glab issue reopen "$iid" --repo services/loom-core
  done
```

Verify every labeled issue is open, then remove the rollback label only after
the incident record has captured the affected IIDs. Never broaden rollback to
unlabeled issues or reuse this label for another cleanup run.
