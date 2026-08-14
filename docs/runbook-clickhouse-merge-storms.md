# ClickHouse Merge Storms

Use this runbook when the first meaningful failure is a ClickHouse-scoped
`merge task failed`, `merge task failure`, or `failed to execute merge task`
error. Record the UTC window, cluster, database/table, replica, part names,
and job or query ID; redact query data and parameters.

## Classification

This is an external dependency incident when background merges fail outside the
branch diff, the same symptom affects unrelated writers or replicas, or the
ClickHouse owner confirms a service incident. A branch-owned schema, partition,
or SQL change, a healthy server with only a client timeout, or a generic
application message containing “merge” is repository-owned until disproved.

## Fail closed and escalate

Pause affected ingestion or write-heavy jobs when continued writes can increase
part, replication, or capacity pressure. Do not retry autonomously, kill or
force merges, detach/drop/repair parts, restart ClickHouse, or alter table
settings. Collect only read-only evidence: replica and queue health, active
merges, disk/inode headroom, recent part errors, and coordination-service
health.

Escalate the sanitized evidence bundle to the ClickHouse/database owner. That
owner alone owns merge queues, replica repair, capacity, and server-side
configuration. Record `external_dependency_incident`,
`disposition=wait_for_dependency_recovery`, `retry_allowed=false`, and the
`external-dependency-incident` label. If no repository follow-up is justified,
record `external dependency incident; no actionable in-repo follow-up`.

## Verify recovery

Require fresh owner evidence that replicas are healthy, queues are draining,
disk and inode headroom is sufficient, and background merges complete without
a new error. Then run one small representative read or write and one affected
job. Resume only if both succeed and monitoring shows no recurrence; otherwise
keep writes paused and the incident escalated.
