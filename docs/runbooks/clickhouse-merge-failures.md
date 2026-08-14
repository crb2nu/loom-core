# ClickHouse Merge Failures

Use this runbook for classifier pattern
`external_dependency.clickhouse.merge_task`. ClickHouse part repair can destroy
or conceal evidence; Mills operators must not detach, delete, or rewrite parts.

## Detection

The classifier requires a ClickHouse-scoped error containing `merge task
failed`, `merge task failure`, or `failed to execute merge task`. Preserve the
first error, UTC window, cluster/database/table, replica, failed part names, and
query or job ID. Consult ClickHouse server logs and system tables using
read-only queries.

Safe diagnostics include checking replica health and queues, active/background
merges, free disk and inode capacity, recent part errors, and the health of the
coordination service. Redact query parameters or table data that may contain
sensitive values.

## Classification

Classify as an external ClickHouse incident when background merges fail on the
service independently of the branch, multiple unrelated writers or replicas
show the same storage/replication symptom, or the ClickHouse owner confirms a
cluster incident.

Likely false positives are invalid branch-owned SQL, a schema or partition
change introduced by the candidate commit, a client-side timeout with healthy
server merges, or an application error that merely contains the word `merge`.
The dependency name and distinctive merge-task phrase must both be present.

## Operator Action

1. Pause affected ingestion or write-heavy jobs when the ClickHouse owner says
   continued writes could worsen capacity, replication, or part pressure.
2. Escalate with the sanitized error, table/replica identity, capacity figures,
   and relevant server log timestamps. Do not run `DETACH`, `DROP`, `KILL`,
   forced merge, or filesystem repair commands from Mills.
3. The ClickHouse owner diagnoses disk pressure, corrupt or oversized parts,
   replication backlog, coordination failures, and merge-pool saturation, then
   performs any repair under the database change process.
4. Require replicas to be healthy, queues to be draining, sufficient disk and
   inode headroom, and background merges to complete without a fresh error.
5. Run a small representative query or write and then one affected job. Resume
   normal load only if both succeed and monitoring shows no new merge failure.

If replica state, capacity, or data integrity remains uncertain, keep writes
paused and the Mills item escalated.
