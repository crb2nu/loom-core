# Planning incident runbook: storage health and external dependencies

Use this runbook when an operator or council planner needs to decide whether a
storage or outside-service symptom should produce repository work. It is a
planning boundary, not a storage recovery procedure: preserve the first
meaningful failure and classify it before proposing a slice.

For recovery thresholds and remediation, use
[incident hardening](incident-hardening.md). For the compact canonical class
definitions, use [storage health and incident classification](storage-health-and-incident-classification.md).

## Before planning

1. Pause autonomous writes or retries when the next mutation cannot be shown
   safe. Read-only inspection remains allowed.
2. Record the UTC window, first meaningful error, affected run or workload,
   commit SHA, pipeline/job/run IDs, retry history, and dependency or volume
   identity.
3. Resolve the live plan and slice from the shared store. The store is
   canonical; do not infer scope from a `.loom` mirror, an old prompt, or an
   MR description.

   ```text
   agent_plan_get{plan_id:"<plan-id>"}
   ```

4. Select one primary class. Do not replace an earlier storage or dependency
   error with a later timeout, failed retry, or `ci_watch` symptom.

## Symptom-to-classification contract

| First meaningful symptom | Classification | Evidence needed before a plan | Planning disposition |
| --- | --- | --- | --- |
| PVC/volume byte or inode use reaches 80% | `storage-warning` observation; not an incident by itself | Current byte and inode measurements, affected writable capability, and trend if available | Do not create a recovery slice solely for the observation. Monitor and propose local telemetry or documentation only if it is missing. |
| Byte or inode use reaches 90% or more | `storage-incident` | Current measurements, volume/PVC identity, affected writer, and confirmation that the more severe metric triggered the state | Fail closed for writes to that volume. Preserve diagnostics; plan only a repository-owned guardrail, telemetry, test, configuration, or runbook improvement. |
| Write error, read-only remount, SQLite lock/integrity error, or exhausted volume | `storage-incident` in `exhausted/failing` state | First error, volume identity, integrity/write check result, and the workload or run that attempted the write | Do not delete state, restart blindly, or plan external storage repair as loom-core work. Recovery must first restore capacity and integrity. |
| Missing token, invalid generated config, unavailable local file/socket, or rejected local endpoint before a request leaves the process | `local-config-fix` | Preflight error and proof that no remote request was sent | Plan a focused repository or operator configuration correction when the prerequisite is owned locally. Do not call it an external outage. |
| GitLab/API, runner preparation, registry, DNS/TLS, package mirror, auth/quota, model provider, Kubernetes API, or shared storage service fails outside the branch diff | `external-dependency-incident` | First service error, owner/status or cross-branch evidence, branch-diff check, and retry history | Stop autonomous retries. Create no proposal unless a file-backed local change can improve future classification or safe handling. |
| Branch diff deterministically breaks tests, contracts, CI config, or integration after the dependency boundary is excluded | `repo-fix` | Reproduction on the branch and a direct connection to changed repository input | Create a narrowly scoped repository fix with focused verification. |
| Empty/unpushed MR, stale plan state, wrong files, or a slice/branch mismatch | `branch-hygiene-fix` | Live plan/slice output, branch diff, staged-file list, and remote branch state | Correct the workflow state; do not mislabel it as storage or an external incident. |

`storage-warning` is an observation used for planning and escalation. The
canonical incident class for critical and exhausted/failing storage is
`storage-incident`. Capacity and inodes are independent: use the more severe
result. A write or integrity failure always wins over a lower percentage.

## External dependency planning boundary

An external incident blocking this repository does not, by itself, authorize
repository work. The following actions are outside an in-repository slice and
must be recorded as operational disposition instead:

- Restarting, resizing, repairing, or reconfiguring a provider-owned service
  or storage system.
- Contacting an owner, waiting for a status page, raising an external quota,
  or rotating an externally managed credential.
- Repeating CI until it passes without recovery evidence.
- Creating a generic "fix GitLab", "restore registry", or "clear PVC" task.

An in-repository follow-up is allowed only when it changes a Loom-owned file
and makes a future incident safer or easier to prove. Allowed follow-up types
are:

| Allowed local outcome | Required scope and validation |
| --- | --- |
| Classification or error-handling improvement | Name the first-failure signal it distinguishes and add focused tests when behavior changes. |
| Retry, backoff, or stop-policy guardrail | Bound retries/spend and test both the permitted and blocked paths. |
| Telemetry, logs, or audit evidence | Emit enough repository-owned context to prove the dependency boundary without exposing credentials. |
| Repository configuration or a guardrail | Change only a local control; verify its preflight or CI behavior. |
| Operator documentation | Name the symptom, class, evidence, allowed boundary, and manual validation steps. |

If none applies, emit no proposal:

```json
{
  "proposals": [],
  "omit_reason": "external dependency incident; no actionable in-repo follow-up"
}
```

## Planning checklist

Before creating a backlog item or slice, confirm all of the following:

1. The canonical class is stated and tied to the first meaningful symptom.
2. Storage proposals include capacity *and* inode measurements or the decisive
   write/integrity error. External proposals include branch-diff and shared or
   owner/status evidence.
3. The plan names a repository-owned outcome, not an external remediation.
4. The live slice file list matches the proposed diff; scope does not drift
   into unrelated recovery work.
5. The validation method proves the local change: focused tests for behavior,
   a guardrail command for configuration, or a documented manual check for a
   docs-only slice.
6. A single bounded retry is permitted only after recovery evidence; otherwise
   the retry loop remains paused.

Use this closeout record in the escalation or MR discussion:

```text
Class: <storage-incident|local-config-fix|external-dependency-incident|repo-fix|branch-hygiene-fix>
First failure: <job/workload and first meaningful error>
Evidence: <capacity+inode or diff/shared-service/owner-status evidence>
Repository follow-up: <file-backed local change | none>
Retry condition: <after recovery evidence | after pushed fix | none>
```

## Manual validation for this runbook

Review a storage write failure, a shared external CI failure, and a
branch-caused test failure against the table. Each must produce one primary
class, preserve its first-failure evidence, and reject any follow-up that only
asks an outside owner to act. For plan-linked work, verify that
`agent_plan_get` returns the slice whose files match the intended diff before
committing or pushing.
