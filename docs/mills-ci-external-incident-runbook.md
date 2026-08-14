# `ci/main` External-Dependency Incident Triage Runbook

Use this runbook when a `ci/main` validation pipeline has a suspected
`external_dependency_incident`. Its purpose is to contain a shared-service
failure without spending retry capacity on unchanged work, while preserving the
evidence needed to resume safely. It documents the current Mills policy; it
does not authorize changing the affected dependency.

For single-branch CI diagnosis, use `docs/ci-incident-triage.md`. For
dependency-specific diagnostics and recovery checks, use
`docs/runbooks/external-dependency-incidents.md`.

## Identify and record the cluster

Freeze manual and autonomous retries for the affected job family while the
class is unknown. Start with the first meaningful failed job, rather than a
downstream `ci_watch` result. Record:

- project, branch, commit, merge request, pipeline, job, stage, and runner IDs;
- job timestamps, the first meaningful sanitized error, and bounded surrounding
  log context;
- the branch diff and live plan/slice scope for Mills-managed work;
- matching failures on `main`, unrelated branches, or before checkout; and
- dependency status/owner evidence and every prior retry result.

Classify as `external_dependency_incident` only when the first useful evidence
identifies a shared dependency outside the branch diff and the failure is
independent of the candidate change: for example, it recurs on unrelated work,
also affects `main`, or happens before checkout. GitLab/registry/package
mirror/DNS/TLS/auth/model-provider/Kubernetes API symptoms are not sufficient by
name alone. A changed endpoint, token reference, image tag, CI rule, package,
or target remains a repository or CI-configuration failure.

If the evidence is ambiguous, retain the normal fail-closed classification and
escalate for human triage; do not relabel a failure as external to obtain a
retry. Mills records the decision as
`class=external_dependency_incident`, with disposition
`wait_for_dependency_recovery`, and `retry_allowed=false` in GitLab-CI incident
classification.

## Triage and ownership

1. Group matching failures by dependency and signature. Preserve the error
   excerpt, first/latest observation time, affected IDs, and a correlation or
   status-page link.
2. Check whether the branch could own the failure. If it can, route it to
   `repo-fix` or `ci-config-fix` and require a corrective commit before rerun.
3. For a confirmed external cluster, record an escalation with the dependency,
   signature, evidence, owner, and retry state. The Mills operator/on-call owns
   classification and containment; the dependency owner owns recovery.
4. Do not create a repository item whose only action is to restart, reconfigure,
   or otherwise remediate the external service. Repository follow-up is limited
   to its classifiers, retry policy, telemetry, configuration, tests, or
   documentation.

Use read-only diagnostics until the owning team accepts the incident. Do not
change credentials, quotas, storage, or the dependency as a workaround.

## Retry decision

The classifier record is the source of truth. Inspect its `class`,
`retryable`, `free_retry`, `terminal`, `external_dependency_id`, and
`external_dependency` fields before retrying.

| Classification outcome | Retry action |
| --- | --- |
| `free_retry=true` (`transient` or `transient_quota`) | A retry may proceed through the bounded transient/backoff policy. It does not consume the normal max-attempts budget, but it is not an unbounded retry permission. Stop when the transient cap is exhausted or cross-branch recurrence proves an active incident. |
| `external_dependency_incident` with `free_retry=false` | Stop. The retry policy denies paid/non-free retries and returns `wait_for_dependency_recovery`. Configuration-class external signatures are terminal for unchanged work. |
| `free_retry=false` and branch-owned code, CI config, plan, or hygiene evidence | Stop and fix the branch/state first; rerun only after the required corrective change is pushed. |
| Unknown or mixed evidence | Stop and escalate for human triage. Unknown failure classes fail closed rather than becoming free retries. |

The default escalation policy has three normal attempts and five transient
attempts; the latter is a bounded addition, not a reason to bypass an active
dependency incident. A one-off transient may be closed as `flake-retried` only
when one clean retry passes with the same diff and the signature does not recur.
If it repeats, restore the hold and classify the shared failure.

## Hold kill-tests and S2 soaks

Hold a live kill-test or overseer promotion soak whenever its evidence window
contains an unresolved external incident, recovery is not proven, or the
incident makes the observation stream incomplete or unreliable. Preserve the
current report and incident record. Do not treat a status-page green signal or
a single historical success as recovery, and do not promote an action class by
changing an allow flag.

The S2 overseer soak remains dry-run and fail-closed until a fresh, closed
evidence window meets all of these requirements:

- seven complete UTC days (`168h`) are present;
- every day has at least one dry-run decision;
- the window contains at least one would-have-acted decision;
- reviewed divergences are exactly zero; and
- evidence is readable and internally consistent, with no unexpected committed
  action in the dry-run window.

For a promotion report, also require actor prefix `overseer.`, a nonempty
closed window, and nonempty action evidence. Missing evidence, a short window,
an executed action, or any divergence is a failed soak: keep dry-run enabled
and escalate. The machine-readable verdict is `promotable=false` with
`fail_closed=true`; the relevant measurements are
`mills_overseer_soak_elapsed_days`, `mills_overseer_soak_dry_run_decisions`,
`mills_overseer_soak_would_have_acted`, and
`mills_overseer_soak_divergences`.

## Resume criteria and closeout

Resume a held retry, kill-test, or soak only after the dependency owner confirms
recovery **and** a fresh dependent probe or pipeline succeeds using the intended
identity, route, and configuration. Re-evaluate the incident classification;
do not replay old success evidence. For an S2 soak, begin or continue only with
new dry-run evidence and require a newly complete seven-day window—time spent
while evidence was incomplete does not satisfy the gate.

Close the incident note with the disposition, dependency and signature,
affected pipeline/run IDs, evidence links, retry decision, owner recovery
acknowledgement, fresh-probe result, and any repository-owned follow-up. If the
same signature recurs, reapply the hold immediately.

## Policy references

- `pkg/mills/council/ci_incident_classifier.go` — CI classification and
  `wait_for_dependency_recovery` disposition.
- `pkg/mills/pipeline/failure_classifier.go` and
  `pkg/mills/budget/retry_policy.go` — retry semantics and paid-retry denial.
- `pkg/mills/council/escalation_policy.go` — bounded normal and transient retry
  defaults.
- `pkg/mills/overseer/overseer.go` — S2 persisted-soak and promotion-report
  criteria.
- `docs/MILLS.md` — operator-facing external-incident and S2 policy.
