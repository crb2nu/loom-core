# Storage Health and Incident Classification Guardrails

Use this guide before restarting workloads, retrying CI, creating a backlog
item, or extracting roadmap work from an incident. It is the canonical compact
classification for storage health and incident handling in loom-core. Classify
the first meaningful failure, retain its evidence, and do not replace it with a
later downstream symptom.

For detailed recovery procedures, see [incident hardening](incident-hardening.md),
[Mills incident response](mills-incident-runbook.md), and [council handling for
external dependency CI incidents](council-external-dependency-incidents.md).

## First response

1. Pause autonomous retries or mutations when the failure may affect more than
   one run or a safe precondition cannot be verified.
2. Capture the UTC window, affected workload or branch, first meaningful error,
   commit SHA, pipeline/job/run identifiers, and retry history.
3. Keep safe read-only inspection available; do not delete data, restart a
   healthy service, or make speculative branch changes to clear an alert.
4. Select exactly one primary incident class below for roadmap extraction and
   operator follow-up. Reclassify only when new evidence disproves the original
   first-failure interpretation.

## Canonical incident classes

| Class | Boundary and evidence | Immediate operator action | Closeout evidence |
| --- | --- | --- | --- |
| `storage-incident` | Volume/PVC byte or inode pressure, write failure, read-only remount, or SQLite lock/integrity error. Use the more severe of byte and inode usage. | Pause autonomous writes; preserve diagnostics and state before recovery or cleanup. | Fresh capacity/inode measurement, successful write/integrity check, and affected capability health. |
| `local-config-fix` | A token reference, generated config, endpoint, tunnel, daemon, checkout, or filesystem prerequisite is rejected **before** a remote request is sent. | Repair the local prerequisite and rerun its preflight; do not treat it as a provider outage. | Successful local preflight without exposing credentials. |
| `external-dependency-incident` | The earliest failure is in GitLab, runner/cluster, registry, DNS/TLS, package mirror, auth/quota provider, model provider, Kubernetes API, or another shared service; the branch did not change the failing input; and the signature is shared, pre-checkout, or otherwise outside the diff. | Stop autonomous retries, record the owner/status evidence, and wait for recovery evidence before at most one bounded retry. | Dependency owner/status evidence plus a successful retry of the same affected operation, or a documented no-retry disposition. |
| `repo-fix` | The active branch deterministically caused a build, test, guardrail, configuration, or integration failure. | Fix the repository-owned diff and run focused verification. | Changed files and successful relevant verification. |
| `branch-hygiene-fix` | The live plan/slice and branch disagree, the MR is empty/unpushed, wrong files are staged, or stale `.loom`/prompt state drove the work. | Resolve the canonical plan store, correct the branch, commit only intended files, and push. | Live slice file list matches the branch diff and the remote branch has the commit. |
| `flake-retried` | One bounded retry succeeds without a relevant diff or policy change, and the signature does not recur in comparable work. | Record the retry and stop treating it as a repository regression. | Failed and successful job/run IDs with the same diff SHA. |

Adjacent runbooks may use narrower closeout dispositions. Map them to the
primary classes before extracting roadmap work: `ci-config-fix` is a
`repo-fix` when it changes this repository's CI files, `runner-incident` is an
`external-dependency-incident` unless the branch changed runner selection or CI
configuration, and `operator-escalated` is a pause state that still needs one
of the primary classes above before follow-up is proposed.

Storage health is `normal` below 80% used, `warning` from 80% to less than
90%, and `critical` at 90% or more for either capacity or inodes. A write
failure, read-only mount, SQLite lock/integrity failure, or exhausted volume is
an `exhausted/failing` storage condition regardless of the reported percentage.
At critical or exhausted/failing state, fail closed for work that writes to the
affected volume; a restart alone is not recovery evidence.

## External incidents: repository boundary

An external incident does not become loom-core work merely because it blocks a
loom-core branch. Do not create a repository item whose only action is to
restart or reconfigure an outside service, contact its owner, wait for a status
page, raise an outside quota, rotate an outside credential, or rerun CI until
it becomes green.

In-repository follow-up is allowed only when it changes a repository-owned
artifact that improves future handling. Valid follow-up classes are:

- Classification, error handling, or guardrails that prevent misclassification.
- Retry, backoff, or stop-policy configuration that bounds autonomous spend.
- Telemetry, logs, or observability that makes the dependency boundary provable.
- Repository configuration or tests for one of those local behaviors.
- Operator documentation and runbooks.

When no allowed change exists, do not extract a proposal. Record the incident
instead:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

## Roadmap extraction validation

Planners must complete every check before turning an incident into a roadmap
proposal:

1. Resolve the live plan and slice with `agent_plan_get`; the shared plan store
   is canonical. Do not rely on `.loom` mirrors, an old prompt, or an MR
   description.
2. State the canonical incident class and cite the first meaningful error,
   time window, affected run(s), and shared-service or branch-diff evidence.
3. For an external incident, prove that the proposed change is one of the
   allowed in-repo follow-up classes above. If it is not, emit the empty
   proposal result and `omit_reason` instead.
4. Give each proposal a repository-owned outcome, explicit file list, and
   narrowly named slices. Do not use external remediation as a slice goal.
5. Include validation that demonstrates the proposed local behavior: focused
   tests for code/config changes, relevant guardrail checks, or a manual
   operator checklist for documentation-only changes.
6. Ensure labels and disposition preserve the boundary, including
   `external-dependency-incident` when applicable.

Reviewers must reject or return an extraction when any of the following is
missing:

- A canonical class, first-failure evidence, or rationale that distinguishes an
  external incident from a repository regression.
- Proof that a claimed external incident is outside the branch diff or shared
  across comparable work.
- A file-backed, repository-owned follow-up within the allowed classes.
- A live-plan/slice check, scoped file list, or validation method.
- The empty-proposal `omit_reason` when the only next action belongs to an
  outside service.

Before approving a roadmap item, compare the planned files against the live
slice and then run the applicable narrow checks. For a documentation-only
extraction, verify the document names the class, boundary, evidence, allowed
follow-up, and planner/reviewer checks; run the documentation guardrail and
the changelog-fragment validation when a fragment is included.

## Operator closeout template

Record a concise note in the escalation, MR discussion, or incident tracker:

```text
Class: <canonical class>
First failure: <stage/job and first meaningful error>
Evidence: <diff check, shared-run evidence, owner/status link>
Disposition: <repo-fix|external-dependency-incident|...>
Repository follow-up: <file-backed local change | none>
Retry condition: <none | after recovery evidence | after pushed fix>
```
