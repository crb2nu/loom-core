# PostgreSQL Role Drift

Use this runbook when a PostgreSQL authentication or authorization failure
shows that the configured administrative/root role is missing, disabled, or
lacks the required grant. Preserve the UTC window, database endpoint or service
identity, failed operation, and sanitized error. Record role names only when
non-sensitive; never record passwords, connection strings, certificates, or
SQL containing secrets.

## Classification

This is an external dependency incident when a previously approved role
hierarchy or grant changes outside the branch, unrelated workloads fail with
the same database identity, or the database owner confirms role drift. Do not
infer a root-role incident from a generic application failure. A branch-owned
connection/role reference, migration, privilege requirement, or use of an
unapproved role is repository-owned and must be fixed locally.

## Fail closed and escalate

Stop the affected bootstrap or migration work and do not retry autonomously.
Do not execute role-changing SQL, grant privileges, reset credentials, alter
database authentication, add a broad grant, or use a root credential as a
workaround. The only safe checks are read-only, sanitized confirmation of the
expected role hierarchy and grants by the database owner.

Escalate to the PostgreSQL/database owner with the endpoint identity, failed
operation, and sanitized evidence; that owner owns role creation, grants,
credential rotation, and emergency access. Record
`external_dependency_incident`, `disposition=wait_for_dependency_recovery`,
`retry_allowed=false`, and `external-dependency-incident`. If no local work is
justified, record `external dependency incident; no actionable in-repo follow-up`.

## Verify recovery

Require fresh owner confirmation of the intended hierarchy and grants. The
workload owner must show its approved least-privileged operation succeeds, with
no root credential or broad grant added as a workaround. Re-run the original
operation using the approved identity; only then may a human requeue the work.
