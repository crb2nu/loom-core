# Mills Degraded Mode Semantics

Use this reference when a Mills run depends on GitLab, CI runners, model
providers, registries, Kubernetes, package mirrors, telemetry, or another
shared service that can fail independently of the branch diff.

The goal is to keep autonomous work moving when recovery is possible, stop it
when retrying would waste capacity, and avoid turning an outside incident into
speculative repository work. For incident triage, see
`docs/mills-incident-runbook.md`. For council output rules, see
`docs/mills-escalation-and-dependency-failures.md` and
`docs/council-external-dependency-incidents.md`.

## Run States

| State | Meaning | Operator action |
| --- | --- | --- |
| `healthy` | Required dependencies are reachable, recent gates are trustworthy, and the branch diff explains any repository-owned failures. | Continue normal Mills execution. Patch, test, commit, and push when the failure is in repo-owned files. |
| `degraded` | A required dependency is unreliable, slow, rate-limited, or partially unavailable, but there is bounded evidence that another attempt may succeed without hiding a repo defect. | Allow only bounded retries with backoff. Record the dependency signal and recovery condition before spending more CI, model, or operator capacity. |
| `blocked` | Work cannot proceed because a dependency is unavailable, credentials or quota are missing, the live plan is stale, or the branch cannot be verified. | Stop autonomous retries. Escalate with evidence and resume only after the blocking condition is removed. |
| `external_dependency_incident` | The first meaningful failure belongs to a shared service outside the branch diff and has no immediate repo-local remediation. | Preserve the incident boundary. Do not create repo backlog unless the follow-up changes loom-core classifiers, retry policy, telemetry, config, tests, or docs. |

`external_dependency_incident` is a disposition for the failure, not proof that
the branch is correct. Operators still need to confirm the live plan, intended
slice files, branch diff, commit state, and push state before closing the run.

## Healthy

Classify a run as `healthy` when all required services for the current step are
available and the earliest useful failure is explained by the branch or by a
single expected local check.

Healthy examples:

- `go test ./...` fails in a package changed by the branch.
- `make changelog-check` fails because the branch forgot a fragment.
- A contract golden diff appears after an intentional API shape change.
- A merge request gate fails because the branch was not pushed.

Healthy does not mean every gate is passing. It means Mills has a trustworthy
local signal and should continue with repository-owned remediation.

## Degraded

Classify a run as `degraded` when a shared dependency is impaired but the
failure is transient enough to permit a bounded retry. Degraded mode requires a
specific dependency signal and a retry budget.

Common degraded signals:

- One HTTP 502, 503, 504, timeout, or connection reset from GitLab, a model
  provider, a registry, package infrastructure, or Kubernetes.
- One runner startup, image pull, artifact upload, or checkout failure before
  repository scripts ran.
- Slow or partial responses from a dependency where a later call succeeds in
  the same window.
- A judged gate miss that passes on one repeat over the same pushed diff.

Degraded-mode rules:

1. Identify the first meaningful dependency error before retrying.
2. Confirm the branch did not introduce the failing endpoint, token, model,
   image tag, dependency source, runner selector, or Kubernetes target.
3. Use one bounded retry by default. Add backoff when several runs share the
   same service or job family.
4. Do not mutate unrelated files, regenerate stale plans, or change retry
   inputs just to make a transient dependency pass.
5. If the same signature repeats, reclassify as `blocked` or
   `external_dependency_incident`.

Record the degraded state in the Mills escalation note or MR discussion with
the dependency, job or gate, first error, retry count, and next retry condition.

## Blocked

Classify a run as `blocked` when Mills cannot make reliable forward progress.
Blocked is appropriate even when the cause is local branch hygiene rather than
an outside outage.

Blocked examples:

- The live plan from `agent_plan_get` does not match the worker prompt, branch
  diff, or intended slice files.
- The branch has no pushed commits, an empty MR, unrelated staged files, or a
  missing required docs or changelog update.
- Required credentials, quota, runner capacity, registry access, or Kubernetes
  access are absent.
- The same dependency error recurs after the degraded-mode retry budget is
  spent.
- The first meaningful failure is unknown because logs, jobs, or plan state are
  missing.

Operator response:

1. Stop autonomous retries for the affected run or cluster.
2. Capture the plan ID, slice, branch, commit SHA, pipeline or run ID, first
   failed job or gate, and first meaningful error.
3. Fix local branch hygiene when that is the blocker.
4. Escalate outside dependency blockers to the dependency owner or incident
   channel with the evidence needed to reproduce the failure.
5. Resume only after the live plan, branch state, credentials, quota, service
   availability, or recovery evidence is valid.

## External Dependency Incident

Classify a failure as `external_dependency_incident` when all of these are true:

- The branch did not introduce the failing endpoint, credential name, runner
  selector, image tag, dependency source, model, Kubernetes target, or CI rule.
- The earliest useful error points to a shared service such as GitLab, CI
  runners, a registry, package mirror, DNS, TLS, auth, quota, model provider,
  storage, networking, or Kubernetes API.
- The same signature appears on `main`, unrelated branches, multiple Mills
  runs, multiple Renovate branches, or before repository checkout.
- Another immediate retry would spend capacity without new recovery evidence.

The council must not create repo work whose only action is to restart a runner,
contact a provider, rotate an outside credential, raise quota, wait for a
service, or reconfigure an external system.

Allowed loom-core follow-up is limited to changes that improve future handling
inside this repository:

- Failure classifiers or council prompt/output guardrails.
- Retry limits, backoff policy, or stop conditions.
- Telemetry, logs, and evidence capture.
- Configuration or CI rules owned by this repository.
- Tests for the changed classifier, retry, telemetry, or config behavior.
- Operator documentation and runbooks.

When there is no repo-local follow-up, council output should use an empty
proposal list with an `omit_reason`:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

When there is repo-local follow-up, it must be file-backed and labelled:

```json
{
  "proposals": [
    {
      "title": "Document degraded dependency handling for Mills runs",
      "labels": ["external-dependency-incident"],
      "slices": [
        {
          "name": "degraded-mode-doc",
          "goal": "Define healthy, degraded, blocked, and external_dependency_incident behavior for Mills runs.",
          "files": ["docs/mills-degraded-mode.md"]
        }
      ]
    }
  ]
}
```

## State Transitions

Use the narrowest state that preserves evidence:

| From | To | Trigger |
| --- | --- | --- |
| `healthy` | `degraded` | One bounded transient dependency failure appears and the branch diff does not explain it. |
| `healthy` | `blocked` | Required live plan, branch, credential, quota, or dependency state is missing or unverifiable. |
| `degraded` | `healthy` | A bounded retry succeeds over the same intended diff and dependency evidence does not recur. |
| `degraded` | `blocked` | The retry budget is spent, logs are missing, or the run cannot distinguish dependency failure from repo failure. |
| `degraded` | `external_dependency_incident` | Repeated or cross-branch evidence proves the failure belongs to an outside service. |
| `blocked` | `healthy` | The blocker is removed and the next run has a current live plan, clean branch state, and reachable dependencies. |
| `external_dependency_incident` | `healthy` | Recovery evidence exists and a bounded retry over the same intended diff succeeds. |

Do not move from `external_dependency_incident` to repository remediation unless
the new work changes loom-core behavior or documentation. The external service
may still need operational follow-up, but that work should not be represented
as a repository defect.

## Evidence Checklist

Every degraded, blocked, or external dependency closeout should include:

- Disposition: `degraded`, `blocked`, or `external_dependency_incident`.
- Plan ID, slice name, branch, commit SHA, MR IID, pipeline ID, and Mills run
  ID when available.
- First failed stage, gate, job, runner, and first meaningful error.
- Branch-diff check: whether the branch changed the failing dependency input.
- Shared-signal check: related failures on `main`, unrelated branches, other
  Mills runs, or before checkout.
- Retry state: not retried, one bounded retry, retry succeeded, retry failed,
  or retries paused.
- Next retry condition: no retry, after pushed repo fix, after recovery
  evidence, or after dependency owner confirmation.

Suggested closeout note:

```text
Disposition: <healthy|degraded|blocked|external_dependency_incident>
Plan: <plan-id>, slice <slice-name>
Branch/MR: <branch>, !<iid>, <commit-sha>
Pipeline/run: <pipeline-id>, <run-id>
First failure: <stage/gate/job and first meaningful error>
Dependency signal: <service/status/signature or none>
Branch-diff check: <why the branch did or did not cause it>
Retry state: <count and result>
Next retry condition: <none|after pushed fix|after recovery evidence>
```
