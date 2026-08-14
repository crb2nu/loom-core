# Mills Incident Response Runbook

Use this runbook when a Mills backlog item, pipeline run, council output, or
merge-request gate needs operator intervention. It focuses on three recurring
failure modes:

- `external_dependency_incident` handling.
- Stale plan or slice warnings.
- Gate flakiness and bounded retry decisions.

The goal is to preserve the boundary between repository fixes and operational
incidents. Classify the first meaningful failure before retrying.

## First Response

Pause autonomous retries when the failure can affect more than one run, when
the branch diff is unverifiable, or when a dependency incident is suspected.

```bash
git status --short --branch
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
```

For any plan-linked backlog item, resolve the live plan from the shared store:

```text
agent_plan_get{plan_id:"<plan-id>"}
```

The plan store is canonical. Do not use checked-out `.loom` files, stale MR
descriptions, or an old spawn prompt as the source of truth.

Record these fields before changing state:

- Backlog item ID, plan ID, slice name, branch, commit SHA, MR IID, pipeline ID,
  and run ID.
- First failed stage, first failed gate, job ID, runner ID, and the first
  meaningful error line.
- Branch diff summary and live slice file list.
- Retry count, prior dispositions, and any shared incident or status-page link.

## Classification

| Signal | Disposition | Operator action |
| --- | --- | --- |
| Failure points at code, tests, docs, CI config, generated files, or package inputs changed by the branch | `repo-fix` or `ci-config-fix` | Patch the branch, run the narrow check, commit, and rerun |
| Same GitLab, runner, registry, DNS, TLS, package mirror, model provider, Kubernetes API, auth, or quota failure appears outside the branch diff or across unrelated work | `external_dependency_incident` | Stop autonomous retries, record evidence, retry only after recovery or bounded backoff |
| Live plan or slice differs from the worker prompt, `.loom` mirror, branch diff, or MR content | `branch-hygiene-fix` | Escalate the stale run and requeue from the canonical plan |
| A single timeout, connection reset, or judged-gate miss passes on one clean retry and does not recur in comparable runs | `flake-retried` | Record retry evidence and close the loop |
| Gate repeatedly fails with the same reason across unrelated clean branches | `operator-escalated` or `external_dependency_incident` | Pause affected work and inspect gate dependencies before retrying |

Use the first meaningful failure, not later downstream symptoms. A failed
`ci_watch` stage, empty MR, or missing diff often follows an earlier runner,
push, stale-plan, or dependency failure.

## External Dependency Incidents

Use this path when the error belongs to GitLab, runner infrastructure, a
registry, package mirror, DNS, TLS, auth provider, quota system, model provider,
Kubernetes API, or another shared service.

Classify as `external_dependency_incident` when all of these are true:

- The branch did not introduce the failing endpoint, token name, image tag, job
  rule, package source, model name, runner selector, or Kubernetes target.
- The same signature appears on `main`, unrelated branches, multiple Mills
  runs, or before repository checkout.
- The earliest useful error points to a shared service or dependency.
- Immediate retry would spend runner, model, or operator capacity without new
  recovery evidence.

Operator response:

1. Stop autonomous retries for the affected job family or backlog cluster.
2. Capture the evidence fields from the first response section.
3. Link external incident evidence when available, such as a GitLab status
   issue, registry status page, runner owner note, or provider incident.
4. Add a Mills escalation note or MR discussion with the disposition:
   `external_dependency_incident`.
5. Create loom-core work only when the follow-up changes this repository's
   classifier, retry policy, telemetry, configuration, tests, or docs.
6. Allow at most one bounded retry after recovery evidence exists. If the same
   signature returns, restore the pause and keep the incident external.

Do not create a repository backlog item whose only action is to restart a
runner, contact a provider, rotate an outside credential, raise quota, or wait
for an external service.

Council output with no repo-local remediation should use an empty proposal
list:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

See `docs/council-external-dependency-incidents.md` and
`docs/external-dependency-incidents.md` for the council-specific contract.

## Stale Plan Warnings

Use this path when Mills, a spawned agent, a gate, or an MR indicates stale plan
state. Common signals:

- The prompt references a `.loom` plan or slice that differs from
  `agent_plan_get`.
- The branch changes files outside the live slice file list.
- A retry produces no diff after an earlier discarded attempt changed files.
- Scope or path gates mention missing slices, fictional files, or unexpected
  paths.
- The MR has no commits, an empty diff, or content from a previous run.

Operator response:

1. Resolve the live plan:

   ```text
   agent_plan_get{plan_id:"<plan-id>"}
   ```

2. Compare the returned slice file list with:

   ```bash
   git diff --name-only origin/main...HEAD
   git status --short
   ```

3. If the live plan is correct but the worker used stale context, escalate the
   current run and requeue a fresh item from the canonical plan.
4. If the plan store is wrong, patch or abandon the plan in the store before
   rerunning. Do not hand-edit `.loom` mirrors as recovery.
5. If the branch is wrong, fix branch hygiene before retrying: stage only the
   intended files, commit with a conventional commit header, and push the
   branch.
6. Add the disposition `branch-hygiene-fix` unless the corrected live plan also
   requires repository code, docs, tests, or config changes.

Verification before handing back to automation:

```bash
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
git push -u origin HEAD
```

The pushed branch must contain at least one intentional commit, and the diff
must match the live slice plus required documentation updates.

## Gate Flakiness

Treat gate flakiness as an exception, not the default explanation. A gate is
flaky only when the first failure is transient, the branch diff does not explain
it, and a single clean retry passes without recurrence.

Allowed bounded retry cases:

- One network timeout, connection reset, 502, 503, or 504 from a shared service.
- One runner startup, image pull, artifact upload, or GitLab API hiccup.
- One LLM-judged gate miss where the rerun sees the same diff and passes, with
  no prompt, model, policy, or code change.
- One test timeout in a suite with no changed test inputs and no matching
  failure on neighboring runs.

Do not classify as flakiness when:

- The same gate fails twice with the same reason.
- The failure points at files changed by the branch.
- The gate fails across unrelated branches or `main`.
- The retry used a different diff, regenerated plan, changed policy, changed
  model, or changed test selection.
- The gate is fail-closed because evidence is missing, unknown, or stale.

Operator response:

1. Record the failed gate name, reason, run ID, job ID, and diff SHA.
2. Run one retry only when the allowed bounded retry criteria are met.
3. If the retry passes, record `flake-retried` with the retry job ID and close
   the incident.
4. If the retry fails, stop retrying and reclassify as `repo-fix`,
   `ci-config-fix`, `external_dependency_incident`, or `operator-escalated`.
5. If repeated valid work is blocked by a noisy gate, create repo-local
   follow-up for gate policy, telemetry, tests, or documentation.

## Closeout Note

Every operator intervention should end with a short note in the Mills
escalation, MR discussion, or incident tracker:

```text
Disposition: <repo-fix|ci-config-fix|external_dependency_incident|branch-hygiene-fix|flake-retried|operator-escalated>
Plan: <plan-id>, slice <slice-name>
Branch/MR: <branch>, !<iid>
Pipeline/run: <pipeline-id>, <run-id>
First failure: <stage/gate/job and first meaningful error>
Evidence: <links or commands used>
Action: <patch, pause, retry, requeue, or no repo-local follow-up>
Next retry condition: <none|after recovery evidence|after pushed fix>
```

References:

- `docs/mills-incident-triage.md`
- `docs/operational-fault-runbooks.md`
- `docs/ci-incident-triage.md`
- `docs/ci-renovate-and-image-gate-guardrails.md`
- `docs/MILLS_RUNBOOK.md`
