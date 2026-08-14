# External Dependency Incident Runbook

Use this runbook when a GitLab-origin CI failure is likely caused by GitLab or
another shared dependency outside the repository boundary. Its purpose is to
preserve evidence, stop wasteful retries, and resume normal work only after
there is fresh recovery evidence.

This document uses the persisted incident contract exactly:

- `external_dependency_incident` is the incident **class**.
- `wait_for_dependency_recovery` is its required **disposition**.
- `external-dependency-incident` is the required GitLab/Mills follow-up
  **label**. Do not substitute the class value for this label.
- `retry_allowed` is `false` for this classification.
- When no repository-owned follow-up exists, the stable `omit_reason` is
  `external dependency incident; no actionable in-repo follow-up`.

For the fuller council workflow, see
[`council-external-dependency-incidents.md`](council-external-dependency-incidents.md).
For threshold parking and recovery mechanics, see
[`runbook-external-dependency-incidents.md`](runbook-external-dependency-incidents.md).

## GitLab-origin CI triage

1. Identify the affected project, merge request or branch, commit SHA,
   pipeline ID, first failed job, stage, runner ID or pool, and UTC time
   window. Start with the earliest meaningful failure, not a later `ci_watch`
   or downstream symptom.
2. Preserve the first relevant GitLab job trace and status/error code. Look for
   failures before checkout, during runner preparation, GitLab API access,
   image or artifact registry access, package resolution, DNS/TLS, auth,
   quota, or shared networking.
3. Compare the signature with `main`, an unrelated branch, or another pipeline
   in the same window. A shared signature, a pre-checkout failure, or evidence
   that the branch did not change the implicated endpoint, token, runner
   selector, image tag, job rule, or package source supports an external
   classification.
4. Rule out repository ownership. A deterministic compile, test, lockfile,
   generated-code, CI-configuration, or branch-diff failure is not an external
   dependency incident merely because GitLab reported it.
5. Record the classification as `external_dependency_incident` with
   `disposition=wait_for_dependency_recovery` and `retry_allowed=false`.
   Group matching failures into one dependency/signature cluster rather than
   treating every branch as a separate incident.

If the evidence is incomplete or conflicts, stop autonomous retries and obtain
human triage. Do not relabel a repository regression as external to avoid
investigating it.

## Required label and incident record

For every follow-up, escalation, or GitLab discussion created for this class,
apply the exact label `external-dependency-incident`. Keep the class,
disposition, dependency, evidence, reason, confidence, and retry decision with
the record. The label is a routing marker; the class remains
`external_dependency_incident`.

When there is no safe repository-owned action, do not create speculative
backlog work. Record the evidence and use the stable empty-proposal result:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

Repository follow-up is appropriate only for a file-backed change to local
classification, retry limits, telemetry, configuration, tests, or operator
documentation. Apply `external-dependency-incident` to that follow-up as well.

## Evidence and escalation

Capture enough sanitized evidence for another operator to reproduce the
classification decision without rerunning the failed work:

- project, MR IID or branch, commit SHA, pipeline ID, job/stage, and UTC
  observation window;
- dependency identity and, when available, the runner, pool, host, registry,
  package mirror, or GitLab endpoint involved;
- the earliest meaningful error, HTTP status, and a representative job-trace
  excerpt with secrets redacted;
- comparable failures from `main` or unrelated branches, plus the branch-diff
  check that establishes the boundary;
- retry history, incident/status-page reference, dependency owner, and the
  exact classification fields and label.

Escalate through the shared-service incident path to the GitLab, runner,
platform, registry, network, or other dependency owner. Send the evidence
bundle and state that the run is parked with
`wait_for_dependency_recovery`. Escalation does not transfer repository access
or authorize changes to the external system.

## Recovery verification

Resume only after the dependency owner reports recovery or an equivalent
authoritative health signal is available. Then:

1. Run one fresh, bounded probe or pipeline for the affected commit or ref.
   Do not treat an old green result or a replayed log as recovery evidence.
2. Confirm the original dependency signature is absent and the relevant GitLab
   job progresses past the previous failure point.
3. Re-check the current incident/threshold verdict and confirm no new matching
   cluster is active in the observation window. Evaluate other merge guardrails
   independently.
4. Attach the recovery probe, result, and UTC time to the incident record
   before clearing the hold or resuming automation.

If the probe fails with the same signature, keep the incident classified,
labeled, and parked; update the evidence and escalation rather than broadening
the retry loop.

## Explicit non-actions

While an `external_dependency_incident` is active, operators must not:

- make speculative code, dependency, CI, or configuration fixes just to change
  the next pipeline result;
- run unbounded, paid, or repeated retries; only the recovery-verification
  probe above may be attempted after fresh recovery evidence;
- restart, repair, reconfigure, rotate credentials for, increase quota on, or
  otherwise remediate GitLab, runners, registries, networks, or any other
  external system from this repository workflow;
- rewrite or delete valid incident evidence, the classification, or the label
  to make a pipeline appear healthy.

The only permitted local work is a repository-owned, evidence-backed follow-up
that improves classification, retry policy, telemetry, configuration, tests, or
documentation. It must not be represented as remediation of the external
service.
