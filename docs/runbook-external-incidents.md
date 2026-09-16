# External Incident Signatures and Fail-Closed Response

Use this runbook when Mills detects one of the signatures below. Preserve a
sanitized first error, run and stage IDs, commit SHA, and UTC time window.
Never include tokens, API keys, or connection strings in the record.

## Classification boundary

The first four signatures are classified as
`external_dependency_incident`. Their disposition is
`wait_for_dependency_recovery`: autonomy fails closed, records and escalates
the incident, and does **not** retry unchanged work. It must not repair or
change the external system.

The fifth signature, a GitLab CI push failure, is intentionally different.
It is a terminal CI-configuration or branch-hygiene condition, not an
`external_dependency_incident`. It is still fail-closed and not retried, but
the operator fixes the branch or repository CI configuration before requeueing.
Do not relabel it as an external outage merely to defer the required repair.

| Signature | Classification and autonomous response | Operator escalation and safe recovery |
| --- | --- | --- |
| ClickHouse `Code: 432` with a merge error (for example, `Cannot merge parts`) | `external_dependency_incident`; park the run and wait for ClickHouse recovery. Do not retry, force a merge, detach/delete parts, or change table settings to make the failure disappear. | Escalate to the ClickHouse/database owner with the sanitized error, table/replica/part identity, and UTC window. After the owner confirms replica and background-merge health, run one fresh bounded probe, then requeue only if it succeeds. |
| Longhorn replica scheduling error, such as `Longhorn volume scheduling failed: no available disk` | `external_dependency_incident`; park the run and wait for storage recovery. Do not retry unchanged work, delete replicas or snapshots, change disk/node scheduling, or expand storage outside the approved platform process. | Escalate to the Longhorn/storage owner with the sanitized error, PVC/PV and volume identity, requested replica count/size, candidate node details, and UTC window. Resume only after the owner confirms healthy volume placement and a fresh bounded mount or affected-job probe succeeds. |
| Postgres missing-role error, such as `FATAL: role "loom_reader" does not exist` | `external_dependency_incident`; park the run and wait for the database/identity dependency to recover. Do not retry with an unchanged identity, create roles ad hoc, or alter database privileges outside the approved access-management flow. | Escalate to the Postgres/database and identity owner with the role name, database/service identity, and sanitized error. Resume only after the owner restores the approved role mapping and a least-privileged fresh connection probe succeeds. |
| LiteLLM missing-key error, such as `LiteLLM ... 401 ... No API Key Passed In` | `external_dependency_incident`; park the run and wait for the LiteLLM credential dependency to recover. Do not retry, paste a key into logs or configuration, or rotate credentials outside the approved secrets flow. | Escalate to the LiteLLM/secret owner with the provider, secret reference (not its value), route, and sanitized error. Resume after the owner confirms the secret reference and a minimal request through the same route succeeds. |
| GitLab CI push failure: an MR head has **no push pipeline** after the bounded watch | Terminal CI configuration/branch-hygiene failure; it is not an external dependency incident. Autonomy does not retry the identical watch, force-merge, or treat a merge-request-only pipeline as evidence that the required push pipeline exists. | Escalate to the repository CI/branch owner with the MR, head SHA, source branch, and observed pipeline sources. Check `workflow:rules`, CI enablement, and the pushed source branch; repush or correct the CI configuration, then requeue. |

## Required fail-closed workflow

1. Stop autonomous retries at the first classified result and retain the
   classifier evidence with the escalation.
2. For an `external_dependency_incident`, wait for the named dependency owner
   to provide recovery evidence; do not perform unsafe recovery or upstream
   remediation from Mills.
3. For the GitLab push failure, correct the repository-owned branch or CI
   state; waiting for GitLab service recovery is not a remedy for a project
   that does not create push pipelines.
4. Verify recovery with one fresh bounded probe for the same affected path.
   Only then requeue the pipeline.

The code-backed disposition is deliberately conservative: a known external
signature escalates as “not retried” and waits for dependency recovery, while
an unavailable GitLab push pipeline escalates as terminal CI configuration
error “not retried.”

## Related procedures

- [External-dependency incident procedures](runbooks/external-dependency-incidents.md)
- [ClickHouse merge failures](runbooks/clickhouse-merge-failures.md)
- [Longhorn disk exhaustion](runbooks/longhorn-disk-exhaustion.md)
- [LiteLLM authentication failures](runbooks/litellm-auth-missing.md)
- [Infra incidents (ClickHouse, LiteLLM, and PostgreSQL missing-role detection, escalation, and recovery verification steps)](runbooks/infra-incidents.md)
