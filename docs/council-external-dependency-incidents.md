# Council Handling for External Dependency CI Incidents

Use this runbook when repeated Renovate or GitLab CI failures reach Mills
council review and the failure may belong to GitLab, runner infrastructure, the
registry, networking, package mirrors, auth, quota, or another shared
dependency instead of the branch diff.

The council's job is to preserve the boundary between repository work and
outside-service incidents. Repeated CI failures are not automatically backlog
items. They become loom-core work only when the follow-up changes this
repository's classification, retry policy, telemetry, configuration, tests, or
operator documentation.

For the broader incident workflow, see `docs/ci-incident-triage.md` and
`docs/mills-incident-triage.md`. For the council output contract, see
`docs/mills-escalation-and-dependency-failures.md`.

## Classification Rule

Classify repeated Renovate or GitLab CI failures as an
`external-dependency-incident` when all of these are true:

- The branch diff did not introduce the failing endpoint, token name, runner
  selector, job rule, image tag, dependency, package source, or Kubernetes
  target. For Renovate branches, the package update may introduce a dependency
  version; the failure is external only when the first meaningful error is in
  package infrastructure, GitLab API behavior, runner preparation, registry
  access, networking, auth, or quota rather than the updated dependency's
  repository-owned behavior.
- The same error signature appears on `main`, unrelated branches, multiple
  Mills runs, or before repository checkout.
- The first meaningful error points to GitLab, a runner, a container registry,
  DNS, TLS, auth, package infrastructure, Kubernetes API, quota, or another
  shared service.
- Another immediate retry would only spend CI or operator capacity without new
  recovery evidence.

Use the first meaningful CI failure, not the last downstream symptom. A failed
`ci_watch` stage can be a symptom of an earlier GitLab API, runner, registry, or
network incident.

## Repeated Incident Clusters

Treat a cluster as one incident when several Renovate branches, Mills branches,
or `main` pipelines fail with the same external signature during the same
window. The council summary should group the failures instead of creating one
proposal per failed branch.

Use a cluster when the failures share at least two of these signals:

- Same failing service, such as GitLab API, package proxy, container registry,
  runner pool, DNS, TLS, Kubernetes API, or auth provider.
- Same first meaningful error line or HTTP status class.
- Same pipeline stage or job family, such as Renovate dependency resolution,
  `build:image:*`, runner preparation, checkout, or `ci_watch`.
- Same runner, runner pool, Kubernetes namespace, registry endpoint, package
  mirror, or dependency source.
- Recurrence across unrelated branches, Renovate MRs, or default-branch
  pipelines.

Do not split a shared outage into branch-specific backlog unless the branch has
its own deterministic repository failure after the dependency incident is
removed.

### Renovate Clusters

Renovate clusters are external only when the repeated failures occur outside
the package update's repository-owned behavior. A cluster is still a `repo-fix`
when the updated package deterministically breaks compile, tests, contracts,
lockfile integrity, or generated code.

Summarize Renovate clusters with:

- Dependency manager, package family, affected branches or MR IIDs, and target
  versions when relevant.
- First failed job and error signature for the cluster.
- Whether unrelated Renovate branches or `main` show the same signature.
- Package registry, checksum database, package proxy, or GitLab API evidence.
- Retry state: not retried, one bounded retry, or stopped after repeat.

Allowed in-repo follow-up is limited to Renovate policy, dependency pinning,
test adaptation, CI classification, retry guardrails, telemetry, or docs. If
the only action is to wait for the registry, package mirror, or GitLab API, the
council must emit no backlog proposal.

### GitLab CI Clusters

GitLab CI clusters are external when repeated failures happen before checkout,
before repository scripts run, during runner preparation, while pulling or
pushing shared images, or in GitLab API operations that the branch did not
change.

Summarize GitLab CI clusters with:

- Project, branch or MR list, pipeline IDs, first failed job names, and runner
  IDs where available.
- The earliest shared error signature and the first pipeline where it appeared.
- The affected job family, such as checkout, runner preparation, image build,
  artifact upload, GitLab API polling, or `ci_watch`.
- Evidence that the branch diff did not introduce the failing endpoint, token,
  runner selector, image tag, job rule, or dependency source.
- Recovery or retry condition, including the dependency owner or incident link
  if one exists.

Allowed in-repo follow-up is limited to CI rules, scripts, classifiers, retry
limits, telemetry, configuration, tests, or docs. If the only action is to
restart a runner, wait for GitLab, rotate an outside credential, or contact an
infrastructure owner, the council must emit no backlog proposal.

## Council Output

When the incident is external and has no repo-local remediation, the council
should not mint a backlog item. Emit an empty proposal list with an
`omit_reason` that names the incident class:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; repeated GitLab CI failures have no actionable in-repo follow-up"
}
```

When failures form a repeated cluster, include the cluster evidence in the
operator-facing summary before the JSON sidecar:

```text
Incident class: external-dependency-incident
Cluster: repeated Renovate package-proxy failures
Window: 2026-07-09 04:10-05:35 UTC
Affected work: !123, !124, !126; pipeline 98765 on main
First shared error: package proxy returned HTTP 503 before repository tests ran
Branch-diff check: no branch changed the package proxy endpoint or CI rules
Disposition: no actionable in-repo follow-up; stop autonomous retries until recovery evidence exists
```

The summary must identify the shared signature and explain why any follow-up is
or is not actionable in this repository. Avoid generic summaries such as "CI is
failing" or "Renovate is broken"; they do not give the next operator a boundary
for retry or repo work.

When there is a real loom-core follow-up, the proposal must be file-backed and
labelled:

```json
{
  "proposals": [
    {
      "title": "Document repeated GitLab CI external dependency handling",
      "labels": ["external-dependency-incident"],
      "slices": [
        {
          "name": "operator-runbook-doc",
          "goal": "Document the external dependency classification and local follow-up boundary.",
          "files": ["docs/council-external-dependency-incidents.md"]
        }
      ]
    }
  ]
}
```

The council may propose repo work for:

- CI failure classifiers or escalation wording.
- Retry, backoff, or stop conditions that prevent repeated autonomous retries.
- Telemetry or logging that makes future external incidents easier to prove.
- Configuration or guardrails that prevent known dependency incidents from
  being misclassified as repository regressions.
- Tests for any changed classifier, guardrail, retry, or telemetry behavior.
- Operator runbooks and documentation.

The council must not propose repo work whose only action is to restart,
reconfigure, contact, unblock, grant quota for, rotate credentials for, or wait
on an outside service.

## Operator Follow-Up Allowed Locally

Operators may perform local follow-up that does not pretend the repository can
fix the dependency:

1. Record the evidence in the Mills escalation note or MR discussion: project,
   MR IID, branch, pipeline ID, first failed job, runner ID, commit SHA, first
   meaningful error, retry history, and any shared incident or status-page link.
2. Resolve the live plan from the shared store before acting on plan-linked
   work:

   ```text
   agent_plan_get{plan_id:"<plan-id>"}
   ```

3. Confirm the branch contains the intended slice diff and has been pushed.
   Missing pushes, empty MRs, stale plans, and unrelated staged files are
   branch-hygiene issues, not external dependency incidents.
4. Pause autonomous retries for the affected job family until recovery evidence
   exists, or use one bounded retry after the dependency owner reports recovery.
5. Create a loom-core follow-up only when it changes files in this repository
   for detection, retry policy, telemetry, configuration, tests, or docs.

Do not hide an external dependency incident behind speculative branch changes.
If the first meaningful failure is outside the branch diff, document the
incident, stop the retry loop, and keep repo work limited to the allowed
follow-up classes above.

## Closeout Dispositions

Close the council review with one of these dispositions:

- `external-dependency-incident`: shared dependency evidence recorded; no
  repo-local change required.
- `external-dependency-incident-with-follow-up`: shared dependency evidence
  recorded and a file-backed loom-core follow-up created for classifier, retry,
  telemetry, config, tests, or docs.
- `repo-fix`: the branch diff caused the CI failure and was fixed locally.
- `branch-hygiene-fix`: the issue was an empty MR, missing push, stale live
  plan, wrong worktree, unrelated staged files, or missing required docs update.

Include the disposition in the Mills escalation note or MR discussion so the
next council run can distinguish unresolved dependency incidents from
repository defects.
