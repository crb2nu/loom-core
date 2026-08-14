# Incident hardening: storage, local preflight, and external dependencies

Use this runbook to classify an incident before restarting workloads, rerunning CI, or creating a repository backlog item. It defines the operator boundary for capacity pressure on repository-owned or Mills state storage, local configuration preflight failures, and GitLab/GitLab-agent failures.

For the broader Mills fail-closed procedure, see [Mills operational guardrails](mills-operational-guardrails.md). For CI evidence and council output, see [Mills incident triage](mills-incident-triage.md) and [council dependency incidents](council-external-dependency-incidents.md).

## First response

1. Stop automatic retries for the affected work family. Do not restart a healthy component merely to make an alert disappear.
2. Capture the first meaningful error, UTC time window, affected branch or run, and the relevant storage or dependency identity.
3. Classify the failure below. Later errors are often downstream symptoms and must not overwrite the first meaningful failure.
4. Keep read-only inspection available where it is safe. Block mutations whose preconditions cannot be verified.

| First meaningful signal | Classification | Owner | Immediate boundary |
| --- | --- | --- | --- |
| Filesystem/PVC capacity or inode threshold crossed, write fails, or SQLite reports lock/integrity trouble | Storage incident | Storage and Mills operator | Pause autonomous writes; preserve evidence before cleanup or recovery. |
| Missing/invalid local setting, generated config, credential, tunnel, daemon, or checkout is rejected before a request is sent | Local configuration preflight failure | Affected workstation or daemon operator | Fix and re-run the local preflight; do not call it a GitLab or provider outage. |
| GitLab API, GitLab-agent, runner, registry, checkout, artifact, or CI operation fails outside the branch diff or on unrelated work | External dependency incident | GitLab/platform dependency owner | Record evidence, pause autonomous retries, and retry only after recovery evidence. |
| A changed repository file deterministically causes a build, test, guardrail, or integration failure | Repository regression | Branch implementer | Fix the branch and run the narrow local verification. |

## Storage thresholds and actions

Use the percentage of byte capacity used and the percentage of inodes used for the volume that holds the affected data. Apply the more severe state when byte capacity and inode usage disagree. These are operator escalation thresholds, not an instruction to delete data automatically.

| State | Used capacity or inodes | Required action | Automation state |
| --- | ---: | --- | --- |
| Normal | below 80% used | Continue monitoring. Record the volume and growth trend if an alert fired. | Normal policy may remain enabled. |
| Warning | 80% to less than 90% used | Identify the owning volume, largest consumers, retention path, and projected exhaustion time. Open or schedule capacity remediation; do not delete state without an approved recovery path. | Keep writes only while integrity and headroom are known; pause discretionary/bulk work. |
| Critical | 90% used or more | Freeze new autonomous work that writes to the volume. Capture diagnostics and notify the storage owner. | `policy.enabled=false` for Mills work that depends on the volume; block deploy, spawn, and cleanup writes. |
| Exhausted/failing | Write error, read-only remount, SQLite lock/integrity failure, or 100% capacity/inodes | Preserve the first error and current state. Follow the storage recovery runbook; do not retry mutations or remove evidence. | Fail closed until liveness and integrity checks pass. |

For the Mills canonical SQLite store (`mills-state` Longhorn PVC), an unhealthy `sqlite_store`, `policy_loaded`, or `repo_root` capability row means `autonomy_ready=false`; it is not safe to resume unattended work. Check the actual mount rather than inferring free space from a container image layer:

```bash
kubectl get pvc,pv -n loom-mills
kubectl get events -n loom-mills --sort-by=.lastTimestamp | tail -80
kubectl logs -n loom-mills deploy/loom-mills-operator --tail=500
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
```

At warning or critical state, record the volume/PVC, namespace, byte and inode measurements, growth trend, workload, and any write error. Recovery requires fresh capacity evidence plus the affected capability checks; a pod restart alone is not proof of recovery.

## Local configuration preflight boundary

A local preflight validates settings before Loom sends a request to GitLab, Kubernetes, a model provider, or another remote service. Typical failures are a missing token, malformed endpoint, absent generated configuration, disconnected SSH tunnel, unhealthy `loomd`, or unwritable local checkout. These are local configuration incidents when the preflight rejects them before a remote request is attempted.

Treat the following as required evidence before escalating a local failure:

```bash
git status --short --branch
make build
./bin/loom generate configs --target all --hub-mode
curl http://localhost:9876/health
./bin/loom tunnel status
```

Then regenerate and sync the affected profile using the standard commands in repository `AGENTS.md`. For a local credential error, verify only that the expected variable or secret reference is present; do not paste credentials into logs, tickets, or merge requests.

Use this decision rule:

- A preflight failure means **no remote request was sent**. Repair the local setting, config generation, daemon, tunnel, checkout, or credential source, then re-run the preflight.
- A request that reached GitLab and received an API, runner, registry, or GitLab-agent error is **not** a local preflight failure. Continue with the external-dependency path unless branch evidence proves a repository regression.
- Do not fix a local failure by repeatedly rerunning CI. CI cannot validate a missing local config or an unpushed/incorrect local branch.

## GitLab and GitLab-agent dependency incidents

Treat GitLab and GitLab-agent failures as external dependency incidents when the first meaningful failure is in a GitLab-owned or platform-owned operation and the branch did not change that operation's input. This includes GitLab API 5xx/timeouts, runner preparation before project scripts, GitLab-agent connectivity or reconciliation, registry pull/push failures, checkout failures outside branch inputs, artifact upload/download failures, and shared CI polling failures.

Before declaring the incident external, capture both boundary checks:

```bash
git diff --name-only origin/main...HEAD
git log --oneline origin/main..HEAD
```

Also record the project, branch/MR, pipeline and job IDs, commit SHA, runner or agent identity, first failed stage, first meaningful error, timestamps, retry history, and matching evidence from `main` or an unrelated branch where available. GitLab-agent evidence should include the cluster/agent name, namespace, reconciliation object, and the first connection or authentication error—not only a downstream Flux or deployment symptom.

The response is deliberately bounded:

1. Pause autonomous retries for the affected job family and preserve read-only inspection.
2. Escalate to the GitLab, runner, registry, or GitLab-agent/platform owner with the captured evidence.
3. Allow one bounded retry only after an owner/status signal or a successful equivalent preflight provides new recovery evidence.
4. Resume automation only when the same affected operation succeeds; an unrelated green pipeline does not prove recovery.

Do not create a loom-core backlog item whose only action is restarting GitLab, a runner, or a GitLab agent; contacting an owner; waiting for a status page; or changing an external quota or credential. A repository follow-up is valid only when it changes repository-owned classification, retry/stop policy, telemetry, configuration, tests, or operator documentation.

When there is no such follow-up, record the outcome explicitly:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

## Closeout checklist

Close the incident with one disposition and its evidence:

- `storage-incident`: capacity/inode or storage integrity issue; include the recovery measurement and capability result.
- `local-config-fix`: preflight failed before a remote request; include the repaired local prerequisite and successful recheck.
- `external-dependency-incident`: GitLab/GitLab-agent or another outside dependency failed; include owner/escalation, recovery evidence, and bounded retry result.
- `repo-fix`: the active diff owned the failure; include the changed files and narrow verification.

For any disposition that resumes Mills autonomy, verify `autonomy_ready=true`, empty autonomy blockers, and no repeating first meaningful error in recent operator logs.
