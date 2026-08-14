# GitLab Agent Unauthenticated

Use this runbook for classifier pattern
`external_dependency.gitlab.auth_failure` when the failing component is a
GitLab agent or another GitLab client. Treat credentials as secrets throughout
triage: record secret names and versions, never token values.

## Detection

The classifier matches a GitLab-scoped authentication error such as HTTP 401,
`unauthorized`, `authentication failed`, `invalid token`, or an
`unauthenticated` GitLab agent. Preserve the first error, UTC timestamp,
pipeline/run ID, agent identity, GitLab host, and affected project.

Safe diagnostics:

1. Check the GitLab service status and whether unrelated agents using different
   credentials can authenticate. A shared outage or broad 401 burst points to
   GitLab or identity infrastructure.
2. Inspect the agent pod status and sanitized logs. Confirm which Kubernetes
   Secret name and key are referenced without printing the Secret value.
3. From the same runtime identity, make the least-privileged read-only API probe
   available (for example, the GitLab user or project endpoint). Capture only
   the status code and request correlation ID.

## Classification

Classify as an external dependency incident when a previously valid agent
credential is rejected without a repository change to the GitLab endpoint,
agent configuration, RBAC, or secret reference, or when the same rejection is
seen across unrelated work.

Likely false positives are a branch-owned endpoint change, a newly introduced
secret name/key, insufficient project membership, an expired credential whose
rotation is owned by this repository, or a request to a project the identity
was never authorized to access. Handle those as configuration or repository
regressions. HTTP 403 may indicate valid authentication with inadequate
authorization; verify membership and scope before calling it an auth outage.

## Operator Action

1. Stop automatic retries; repeating a rejected credential can trigger further
   throttling or lockout.
2. Escalate to the GitLab/identity owner with sanitized evidence. Have that
   owner restore access or rotate the credential through the approved secret
   management path. Do not patch token values directly into workloads or Git.
3. If rotation is required, verify the agent references the intended Secret,
   then use the normal deployment reconciliation path to roll the workload.
4. From the same runtime identity, repeat the read-only probe and confirm the
   intended project is accessible. Confirm the agent reports connected/healthy
   and that new logs contain no 401 or unauthenticated event.
5. Run one affected stage. Resume normal automation only when it succeeds and
   the authentication error does not recur during the observation window.

If the probe still fails, leave the Mills item escalated and return ownership to
the GitLab/identity team with the new correlation ID.
