# External-Dependency Incident Runbook

Use this runbook when a Mills pipeline is blocked by a service outside the
repository boundary: GitLab, a CI runner, registry, package mirror, DNS/TLS,
an identity or quota service, a model provider, or a Kubernetes control plane.
Its purpose is to stop repeated attempts from consuming capacity while
preserving enough evidence to safely resume the intended work after the
dependency recovers.

This runbook describes the retry-exhaustion guardrail. It does not authorize
editing plan, backlog, or pipeline rows directly.

## Council brief threshold banner

The **External dependency incidents** section is rendered from the structured
threshold verdict for the affected ref and 24-hour window. Treat its verdict
code, severity, reasons, observed cluster count, and configured threshold as
the authoritative explanation of the auto-merge decision.

- **Auto-merge suppressed** means the incident count exceeded policy. Keep
  autonomous merge off while triaging and verifying recovery.
- **Auto-merge not suppressed by this guardrail** means the evaluated count is
  within this policy. Continue monitoring; this does not prove dependency
  health or override another auto-merge guardrail.

Record the council run, ref, evaluation window, and complete verdict. Do not
lower the count by rewriting a valid `external_dependency_incident`
classification.

### Threshold triage and recovery verification

1. Confirm each counted cluster has the canonical incident classification and
   belongs to the affected ref. Separate unrelated dependencies and
   repository-owned failures using the classification table below.
2. Group evidence by dependency and signature. Preserve upstream status links,
   first/latest observation times, affected run IDs, and a representative
   sanitized error.
3. Pause unbounded or paid retries and notify the dependency owner through the
   normal incident channel.
4. Obtain dependency-owner recovery evidence, then run a fresh bounded probe
   or pipeline for the affected ref. Replaying old evidence is insufficient.
5. Re-evaluate the complete 24-hour verdict. Require the new council brief to
   show **Auto-merge not suppressed by this guardrail**, no new matching
   cluster during the observation period, and internally consistent code,
   reasons, count, and threshold. Confirm all other merge guardrails
   independently.
6. Attach the recovery probe and new council run to the incident record before
   allowing normal automation to resume.

If any verification fails or the verdict remains suppressed, leave auto-merge
off and continue incident handling.

### Manual auto-merge override

A manual override accepts risk; it does not clear the structured verdict or
establish dependency recovery.

1. Obtain approval from the repository owner and incident commander (or
   designated dependency owner). Record approvers, justification, exact
   project/ref scope, and a short expiry.
2. Require a green current pipeline for the exact commit, human review, and a
   documented rollback path. Never override security, data-integrity, or
   migration uncertainty.
3. Perform only the approved merge manually through the normal GitLab control.
   Do not edit, delete, or falsify incident classifications or threshold
   evidence to make automation arm auto-merge.
4. Record the merge request, commit SHA, approvals, expiry, outcome, and
   rollback status. Monitor through expiry and revoke the exception on a
   matching recurrence.

After expiry, normal automation still requires the fresh passing verdict and
recovery verification above. Never use a global or indefinite override.

For the general external-dependency-incident classification criteria and the
broader operator disposition table, see `docs/mills-incident-runbook.md`. This
runbook narrows to the parking, resume, and retry-cap mechanics.

## Infrastructure workspace-signal classifier rules

During brief compilation, Mills compares both the signal's `Service` value and
its sampled log line with the rule table in
`pkg/mills/council/workspace_signal_classifier.go`. A match stamps
`class=external_dependency_incident` and the dependency below. It does not
authorize Mills to repair the outside service. If only the service or only the
log signature matches, the rule does not apply.

<!-- BEGIN INFRASTRUCTURE WORKSPACE SIGNAL RULES -->
| Stable rule ID | Matched infrastructure namespace or service | External dependency | Why remediation is outside loom-core | Required evidence | Escalation owner and path |
| --- | --- | --- | --- | --- | --- |
| `gitlab-service-unavailable` | A GitLab or GitLab Runner service reports a GitLab API/auth, quota, DNS, TLS, connection, or 5xx failure. | `gitlab` | GitLab availability, accounts, quotas, DNS, TLS, and runner-to-GitLab connectivity are operated outside this repository. | Preserve the service, UTC window, sanitized first error/status, affected GitLab host, and the same failure from unrelated work or an owner health check. | Escalate to the GitLab/platform owner through the shared-service incident path; include the GitLab incident or status reference. |
| `kubernetes-control-plane-unavailable` | A `kube-system`, Kubernetes, or k8s service reports an API-server connection, route, TLS, service-unavailable, or 5xx failure. | `kubernetes` | Control-plane availability, networking, certificates, and API-server repair belong to the Kubernetes platform, not the branch. | Preserve cluster and service identity, UTC window, sanitized first error, API health/ready evidence, and an unaffected-path comparison. | Escalate to the Kubernetes/platform owner and attach the cluster incident and control-plane health evidence. |
| `flux-upstream-unavailable` | A `flux-system` service reports GitLab, GitHub, or source-controller quota, DNS, TLS, connection, timeout, or 5xx failure. | `git_provider` | Flux's upstream Git provider, credentials, network path, and provider quota are shared GitOps dependencies outside loom-core. | Preserve the Flux object/controller, upstream host, UTC window, sanitized reconciliation error, provider health, and evidence from another source or branch. | Escalate to the GitOps/platform owner, who coordinates with the Git provider owner; include the Flux reconciliation and provider incident references. |
| `observability-backend-unavailable` | A monitoring, observability, Loki, Prometheus, or Grafana service reports a backend DNS, TLS, connection, timeout, service-unavailable, or 5xx failure. | `observability` | Backend availability, storage, networking, and tenant health belong to the observability service owner. | Preserve the backend/service and tenant identity, UTC window, sanitized first query/write error, owner health evidence, and an unaffected query comparison. | Escalate to the observability/platform owner through the shared-service incident path; attach the affected backend and health/dashboard reference. |
<!-- END INFRASTRUCTURE WORKSPACE SIGNAL RULES -->

The following source patterns are copied exactly from the classifier. They are
operator diagnostics and documentation-sync metadata, not patterns to edit
independently. Any classifier change must update this section in the same
change.

### `gitlab-service-unavailable` source patterns

Namespace/service:

```regexp
(?i)(^|[/_.-])(gitlab|gitlab-runner)([/_.-]|$)
```

Log signature:

```regexp
(?i)(?:\b(?:gitlab\.com|gitlab)\b.*(?:status (?:401|403|429|5\d\d)\b|connection refused|i/o timeout|tls handshake timeout|no such host)|(?:status (?:401|403|429|5\d\d)\b|connection refused|i/o timeout|tls handshake timeout|no such host).*\b(?:gitlab\.com|gitlab)\b)
```

### `kubernetes-control-plane-unavailable` source patterns

Namespace/service:

```regexp
(?i)(^|[/_.-])(kube-system|kubernetes|k8s)([/_.-]|$)
```

Log signature:

```regexp
(?i)(?:kubernetes|kube-apiserver|api server).*(?:connection refused|i/o timeout|tls handshake timeout|no route to host|service unavailable|status 5\d\d)\b
```

### `flux-upstream-unavailable` source patterns

Namespace/service:

```regexp
(?i)(^|[/_.-])flux-system([/_.-]|$)
```

Log signature:

```regexp
(?i)(?:gitlab|github|source-controller).*(?:connection refused|i/o timeout|tls handshake timeout|no such host|status (?:429|5\d\d))\b
```

### `observability-backend-unavailable` source patterns

Namespace/service:

```regexp
(?i)(^|[/_.-])(monitoring|observability|loki|prometheus|grafana)([/_.-]|$)
```

Log signature:

```regexp
(?i)(?:loki|prometheus|grafana).*(?:connection refused|i/o timeout|tls handshake timeout|no such host|service unavailable|status 5\d\d)\b
```

## Read the classification verdict before acting

The council classification contract is the stable operator input: its fields
are `class`, `disposition`, `dependency`, `evidence`, `reason`, `confidence`,
and `retry_allowed` (defined by `internal/contracts/incident_classification.go`
and produced by `pkg/mills/council/ci_incident_classifier.go`). Preserve the
verdict and its evidence with the run; do not substitute a later downstream
error for the first meaningful failure.

| Verdict (`class`) | Required disposition | Operator action |
| --- | --- | --- |
| `repository_regression` | `fix_branch_before_retry` | Fix and push the branch, then obtain a new CI result. |
| `ci_configuration_regression` | `fix_ci_config_before_retry` | Repair the repository-owned CI configuration before retrying. |
| `runner_infrastructure_incident` | `escalate_runner_owner` | Hand off to the runner owner; do not make speculative branch changes. |
| `external_dependency_incident` | `wait_for_dependency_recovery` | Park the run and use the dwell procedure below. |
| `dependency_update_ci_incident` | `fix_dependency_update_before_retry` | Repair or revert the branch-owned dependency update. |
| `flake_or_transient_dependency` | `retry_once` | Make at most the classified bounded retry, retaining the evidence. |
| `branch_or_plan_hygiene` | `fix_branch_or_plan_state` | Correct the empty/missing-push/stale-plan state. |
| `unclassified` | `human_triage_required` | Do not retry autonomously; collect evidence and triage manually. |

## Canonical external-dependency taxonomy

Use the following three operator families for this runbook. Each is an
`external_dependency_incident` with disposition
`wait_for_dependency_recovery` and `retry_allowed=false` when it is matched
as a known external incident. The family name is for triage; preserve the
stable classifier values in the incident record rather than replacing them
with prose.

| Operator family | Classification evidence | Stable classifier values | Ownership and escalation |
| --- | --- | --- | --- |
| `gitlab-ci` | A GitLab API/CI error is the first meaningful failure: authentication or token rejection, rate limiting, or an API/CI 5xx. Corroborate with a shared signature, an unrelated branch, `main`, or a pre-checkout failure whenever available. | `kind=gitlab_auth` for authentication failures, or `kind=gitlab_ci` with `dependency=gitlab` for rate-limit/service-unavailable signals. Known IDs include `external_dependency.gitlab.auth_failure`, `external_dependency.gitlab.rate_limit`, and `external_dependency.gitlab.service_unavailable`. | Escalate to the GitLab/platform owner with the affected project, pipeline/job, UTC window, sanitized first error, and upstream incident or status reference. Do not change branch CI configuration unless evidence makes it repository-owned. |
| `model-provider` | A model provider or rubric judge returns quota/rate-limit, timeout, or upstream 5xx evidence; the known ungradeable-envelope signature is a model-provider incident, not a code defect. Preserve provider/model identity without secrets and show whether another provider or unaffected path succeeds. | `kind=model_provider`, `dependency=llm_judge_provider`, and, for the ungradeable judge envelope, `id=external_dependency.model_provider.judge_ungradeable_envelope`. | Escalate to the model-provider/service owner. The pipeline owner may use configured fallback behavior, but must not repeatedly redial a parked or unavailable model or recast the provider result as a branch failure. |
| `storage` | An object/blob/registry-storage error is the first meaningful failure: manifest/cache write rejection, upload failure, storage timeout, or storage 5xx. For Longhorn or other volume failures, retain the affected PVC/volume and attach/mount/write evidence. | For registry blob storage, `kind=blob_storage`, `dependency=container_registry_blob_storage`, and `id=external_dependency.blob_storage.manifest_write`. CI classification may instead name `dependency=object_storage` or `dependency=longhorn` when that is the matching dependency. | Escalate to the storage/platform owner. The workload owner validates integrity after storage recovery; Mills must not repair volumes, replicas, capacity, or registry storage. |

### Persisted record and council visibility

For every matched incident, retain the classifier output verbatim: `class`,
`disposition`, `dependency`, `evidence`, `reason`, `confidence`, and
`retry_allowed`. For known signatures, retain the accompanying `id`, `kind`,
`dependency`, `summary`, and sanitized `evidence`. Link those values to the
plan ID, slice, backlog ID, run ID, first/last observation time, affected
pipeline/job, retry or dwell outcome, owner escalation, and recovery probe.
Do not store tokens, credentials, connection strings, or raw secret-bearing
logs.

The council brief exposes persisted CI escalation metadata in **Classified CI
failures** and **Incident classification planning context**. It renders the
run/backlog identity, class, disposition, classifier, failure/escalation
class, retry/free-retry/terminal state, external dependency, reason, and
evidence. Treat that context as an audit projection, not an authorization to
create upstream remediation work: an external incident with no repository-owned
follow-up must remain proposal-free with
`omit_reason="external dependency incident; no actionable in-repo follow-up"`.

### When an external verdict becomes `repository_regression`

Reclassification requires a branch fix when the branch has plausible
ownership, the failure is not shared, and the failure is deterministic; a
meaningful branch or slice diff with no shared-incident signal is also enough
to require `fix_branch_before_retry`. A branch-owned dependency update instead
uses `dependency_update_ci_incident` and
`fix_dependency_update_before_retry`. Do not reclassify merely because an
external incident is inconvenient or a dependency has recovered: retain the
original evidence and add the new, branch-owned evidence that supports the
new verdict.

Conversely, a known external signature, a shared failure, a pre-checkout
failure, or a repository-independent dependency must not be rewritten as a
repository regression. That boundary prevents a branch retry from hiding an
upstream incident.

## Disagreement and dwell reconciliation

A disagreement event is a fail-closed condition, not a tie to be resolved by
retrying. If merge reality contradicts Mills' autonomous merge claim, or its
evidence is unavailable, stop autonomous work and record the policy code
`merge_reality_diverged` or `merge_reality_unknown`. Likewise, an active or
unknown dependency health state yields `dependency_incident_active` or
`dependency_health_unknown`. Preserve the referenced MR/run and authoritative
observation, then obtain human triage; do not mark the branch healthy until
the disagreement is reconciled. These codes are defined by
`pkg/mills/council/autonomy_policy.go`.

### Dwell disposition

`wait_for_dependency_recovery` creates a durable external-incident dwell for
an `external_dependency_incident`. It is a bounded hold, not a new pipeline
state: the run and backlog item remain `escalated`. The ledger records the run
and dependency identities, start and deadline timestamps, completion timestamp,
completion reason, and elapsed duration. Reconciler restarts reuse the same
row and timestamps.

Configure the maximum dwell in `pipeline.auto_requeue`:

```yaml
pipeline:
  auto_requeue:
    external_incident_max_dwell_minutes: 360
```

An unset or zero value defaults to 360 minutes (six hours); a value above
seven days is invalid. A dwell may end only as `recovered`, `timeout`, or
`fast_kill`:

- `recovered` is written when recovery permits the normal requeue path.
- `timeout` is written when the deadline is reached; the item remains
  escalated and requires an explicit operator decision to requeue or retire.
- `fast_kill` is written when an operator manually escalates an open dwell.

The three competing terminal ledger writes — recovery, timeout, and fast-kill
— are reconciled with a first-writer-wins compare-and-swap. The winner is the
only authoritative completion, emits
`reconciler.external_incident_dwell_completed`, and supplies the metric label
for `mills_external_incident_dwell_duration_seconds{outcome=...}`. A losing
writer reads and reports the existing terminal record instead of publishing a
second, divergent outcome. Inspect that durable result before taking any
follow-up action.

## What "parked" means

Mills records a parked external-dependency run as a terminal pipeline run in
state `escalated`, with its associated backlog item also `escalated`. `parked`
is an operator term, not a separate pipeline-state value. The run history is
therefore the source of the prior attempts and the escalation reason; the
backlog item is the unit that is requeued.

This is distinct from a provider message such as "model is parked behind a
higher-priority primary." That message is incident evidence. It does not mean
an operator should immediately requeue the Mills backlog item.

For plan-derived work, resolve the live plan and slice from the plan store:

```text
agent_plan_get{plan_id:"<plan-id>"}
```

The plan store is canonical. Do not recover an incident by editing a checked
out `.loom` file, an old prompt, or a stale MR description.

## Detect and park

1. Inspect terminal runs and open the relevant run detail. The terminal list
   includes `done`, `escalated`, and `paused` runs; the detail includes stage
   results and gate outcomes.

   ```bash
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal" | jq
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run-id>" | jq
   ```

2. Correlate the run with pipeline history. In pipeline-recorded memory, retain
   the epoch and attempt, `outcome`, and the `last_message=` or log-tail line
   that first identifies the dependency failure. Preserve the backlog ID, run
   ID, plan ID, slice, failed stage, and any provider incident or status-page
   link.

3. Confirm that the first meaningful error belongs to the dependency rather
   than to the branch. Evidence is stronger when the signature occurs on
   unrelated work or before repository checkout. If the branch changed the
   failing endpoint, image, token name, job rule, package source, model, or
   Kubernetes target, treat it as a repository/configuration failure instead.

4. Stop retrying the affected item while the dependency remains unhealthy. A
   known external-dependency incident is escalated rather than repeatedly
   retried. If retries have already been exhausted, leave the terminal run and
   its evidence intact; the guardrail has parked it for operator review.

If a broad incident affects multiple items, pause new pipeline admission using
the policy procedure in `docs/MILLS_RUNBOOK.md` before investigating individual
runs. Do not delete pipeline, event, reservation, or outbox records to force
progress.

## Resume only after recovery

Before requeueing, all of the following must be true:

- The external dependency has recovery evidence: a successful owner health
  check, cleared provider incident, or a clean verification from an unaffected
  path.
- The original failure is no longer reproducible outside the branch diff.
- The current canonical plan/slice still matches the intended work and its
  file scope.
- The branch and MR state do not need a repository fix first. An open or
  closed MR, changed inputs, or an actionable code/configuration error needs
  human branch repair rather than a blind requeue.
- The operator has recorded why the retry is now justified and the run/backlog
  IDs that it supersedes.

Requeue the **backlog ID** associated with the escalated run. The start route
uses the same admission, dependency, budget, and policy checks as a normal
scheduler tick. It is admin-gated and requires the operator bearer token.

```bash
curl -sS -X POST \
  -H "Authorization: Bearer $LOOM_MILLS_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<backlog-id>/start?requeue=1" \
  | jq
```

A successful request returns `201` and includes a new `run_id`. A `403`
indicates policy or autonomy blocking; a `409` means the item is not in a
requeueable state or was changed concurrently. In either case, re-read the
canonical state and resolve the stated blocker rather than retrying the POST.
The `POST /api/mills/pipeline/runs/{id}/resume` route is not the recovery path
for this runner.

After the new run starts, watch its stage results and the dependency health.
If the same external signature returns, stop again, retain the new evidence,
and keep the item escalated. Do not loop manual requeues during an unresolved
incident.

## Retry-cap configuration

Retry settings live in the Mills policy under `pipeline.retry`:

```yaml
pipeline:
  retry:
    max_attempts: 3
    cooldown_seconds: 300
    transient_retry_cap: 5
    escalation_auto_retry_cap: 0
```

`max_attempts` caps real code and infrastructure failures. For transient and
transient-quota failures, `transient_retry_cap` adds free attempts; the
effective hard cap for one stage is `max_attempts + transient_retry_cap`.
When either value is omitted or non-positive, the runtime default is `3` and
`5` respectively, so the default hard cap is eight attempts.

`escalation_auto_retry_cap` is a separate cap on fresh pipeline runs after a
transient hard-cap escalation. The code default is `0` (disabled); deployments
may opt in with a policy value. It is not permission to override an active
external-dependency incident: recovery evidence is still required before a
human requeue, and automatic requeue eligibility is additionally constrained
by incident activity, cooldown, run-budget, and policy checks.

Change policy through the deployment's GitOps-managed policy configuration and
verify the operator logged the reload. Do not change retry values during an
active incident merely to make the current item progress; record the incident
and make any policy change as a separately reviewed operational decision.

## Handoff record

Before closing or handing off the incident, record:

- Dependency, incident link, first meaningful error, and recovery evidence.
- Plan ID, slice, backlog ID, old run ID, and (if resumed) new run ID.
- Attempts consumed, effective retry cap, final outcome, and whether the
  second run reproduced the signature.
- Any repository-owned follow-up, limited to classifiers, telemetry, retry
  policy, configuration, tests, or documentation.

An external incident with no repository-owned remediation should remain an
incident record, not a speculative backlog item.

## Targeted external incidents

Use the following procedures when the first meaningful failure identifies one
of these dependencies. They supplement, rather than replace, the detect, park,
and resume procedure above. Do not turn a storage, database, gateway, or
credential repair into a loom-core backlog item unless the follow-up changes a
repository-owned guardrail, configuration, telemetry, test, or this runbook.

### Mills behavior for every incident in this section

Mills must **classify, wait, and perform no paid retry** for an unresolved
incident in this section. It records the first failure and the dependency
evidence, escalates the affected run instead of repeating the failed stage,
and waits for recovery evidence. Mills does not restart the dependency, edit
its credentials, change storage/database state, or purchase/retry provider
work to make the incident disappear.

The service owner performs remediation. A human operator may only consider the
normal requeue path after the recovery checks below have passed and the
incident record explains why the requeue is safe. A recovery claim without
evidence is not a reason to spend a retry.

### Longhorn disk exhaustion

**Evidence.** Preserve the affected workload, namespace, PVC or volume
identity, UTC window, and the first write, attach, mount, or scheduling error.
Collect the Longhorn volume condition and capacity/inode evidence from the
storage owner, plus evidence that the same pressure is outside the branch
diff. A `DiskPressure` condition or a successful-looking pod alone does not
disprove a full Longhorn volume.

**Ownership and escalation.** The storage/Kubernetes platform owner owns
capacity, replica health, node placement, and any volume repair. The workload
owner owns application-level integrity decisions after storage is available.
Mills may classify the blocked run as an external dependency incident and park
it; it must hand off before cleanup, replica changes, volume expansion, or
workload restart. Escalate immediately to the platform owner when writes fail,
the volume cannot attach or mount, or capacity/inodes are exhausted.

**Recovery checks.** The storage owner must provide fresh healthy volume and
capacity/inode evidence, and the workload owner must confirm a safe write (or
the workload's equivalent integrity check) without a read-only or attach/mount
failure. Then confirm the original failure is absent on an unaffected path
before a human considers requeueing the backlog item.

### ClickHouse merge failures

**Signature.** This procedure includes a Langfuse-surfaced ClickHouse
MergeTree `Code 432` error. Preserve the complete exception text verbatim in
the restricted incident record; do not claim that the numeric code has one
canonical meaning. Treat parts/merge backlog, disk pressure, and
detached/replication state as hypotheses to be checked by the database owner,
not as conclusions drawn from the code alone.

**Evidence.** Preserve the ClickHouse error text, affected table/partition when
available, UTC window, query or job identity, and the first merge-related
failure. Obtain the database owner's evidence for merge backlog, replica or
disk health, detached/replication state, and any owner incident record.
Distinguish a shared merge failure from a query, schema, or data change
introduced by the branch.

**Ownership and escalation.** The ClickHouse/database owner owns merge
queues, replica repair, disk capacity, and server-side configuration. The
calling workload owner owns query/schema changes in its own deployment. Mills
classifies and parks only; it must hand off before killing merges, mutating
parts, changing table settings, or restarting ClickHouse. Escalate to the
database owner when the failure blocks pipeline history, telemetry, or another
shared workflow.

**Recovery checks.** Require owner confirmation that the affected merge path
is healthy, together with a successful read or write appropriate to the
blocked workflow. Re-run the original read-only diagnostic or an unaffected
workflow to show the signature is gone; a merely reduced backlog is not enough
when the original merge error remains.

### Langfuse Redis `ETIMEDOUT`

**Signature and blast radius.** A Langfuse worker, exporter, or backend reports
`ETIMEDOUT` while connecting to Redis. The observable impact can include
ingestion-queue backlog, delayed traces, or stale Langfuse UI data. Network
path trouble (DNS, firewall, or NAT), Redis saturation or slow commands, and a
blocked Redis instance are hypotheses only until the Langfuse/Redis owner
confirms them.

**Evidence and classification.** Preserve the exact sanitized error, first and
latest UTC timestamps, timeout duration, affected environment, Langfuse
component, Redis endpoint *identity* (never its credentials), and the affected
trace/export or run IDs. Check whether a recent repository change altered
telemetry volume, endpoint wiring, or request behavior; that evidence makes
the incident repository-owned until resolved. Otherwise, corroborate with an
unrelated workload or owner health observation and record
`class=external_dependency_incident`,
`disposition=wait_for_dependency_recovery`, and `retry_allowed=false`.

**Ownership, retries, and recovery.** Escalate through the observability
platform incident path to the Langfuse owner, who coordinates Redis and network
recovery. Stop autofix and monitor loops after external classification. Do not
restart, fail over, reconfigure, flush, or edit Redis/Langfuse to clear the
signal. Make no paid retry; after owner evidence shows Redis reachability and
Langfuse health from the same network boundary, allow one fresh bounded
trace/export probe. Resume normal automation only if that probe is clear and
the checklist in **Resume only after recovery** above is satisfied.

### GitLab CI runner death or termination

**Evidence and classification.** Preserve the GitLab project, pipeline and job
IDs, commit SHA, runner ID and description, executor when shown, exact
sanitized failure message, and first/latest UTC timestamps. Record whether the
job died before checkout and whether an unrelated job on the same runner or
pool has the same symptom. A runner-side termination, lost runner, executor or
Kubernetes-substrate failure is
`class=runner_infrastructure_incident` with
`disposition=escalate_runner_owner`; it is external infrastructure. A failing
script, image, rule, variable, or include owned in `ci/` is instead
`class=ci_configuration_regression` with
`disposition=fix_ci_config_before_retry`, and must be repaired in the
repository before retrying.

**Ownership, retries, and recovery.** Escalate runner death to the GitLab CI or
platform runner owner with the captured job and runner evidence. Do not
restart, register, drain, resize, reconfigure, or otherwise remediate a runner
outside approved owner processes. Stop automated retries after classification;
the sole permitted recovery attempt is one fresh bounded job after the owner
confirms the runner pool is healthy. Resume only when that job succeeds, the
checklist in **Resume only after recovery** above is satisfied, and there is
no evidence of a repository-owned `ci/` fault.

### LiteLLM or GitLab-agent authentication drift

**Evidence.** Preserve the HTTP/authentication status, sanitized error text,
endpoint identity, affected service account or token reference name (never the
secret value), and UTC window. Compare a known-good workload or control-plane
request using its own approved identity. For LiteLLM, first distinguish a
missing local gateway configuration from a request that reached the gateway;
the local preflight and supported environment names are documented in
`docs/incident-runbook-storage-and-dependency-failures.md`. For GitLab-agent,
capture the agent/job identity and whether the failure is shared across
unrelated work.

**Ownership and escalation.** The identity/platform owner owns credential
issuance, rotation, trust relationships, and gateway/agent authorization. The
application or pipeline owner owns the reference to an approved credential,
but must not expose or copy secrets into a Mills record. Mills may preserve a
sanitized classification and wait; it must hand off before rotating a token,
altering a service account, changing a trust policy, or bypassing
authentication. A branch-owned wrong variable name or missing configuration is
a repository/configuration repair, not an external incident.

**Recovery checks.** The owning team confirms the intended identity can perform
the minimum required authenticated operation, and the affected workload
completes its normal authenticated preflight without exposing credentials.
Confirm that the same configuration works from the affected deployment before
considering a human requeue.

### PostgreSQL root-role misconfiguration

**Evidence.** Preserve the sanitized authentication/authorization error, the
database endpoint or service identity, UTC window, and the operation that
required the role. Record role *names* only when they are non-sensitive; never
record passwords, connection strings, certificates, or SQL containing secrets.
Ask the database owner to confirm the expected role grants and role hierarchy.
Do not infer that a generic application failure requires a root role: the
failure must show that the configured root/administrative role is missing,
disabled, or lacks the required grant.

**Ownership and escalation.** The PostgreSQL/database owner owns root-role
creation, grants, credential rotation, and emergency access. The workload owner
owns using the least-privileged approved role and correcting its own reference.
Mills records the classification and waits; it must hand off before executing
role-changing SQL, granting privileges, resetting credentials, or changing
database authentication. Escalate directly to the database owner for failed
administrative bootstrap/migration work or any indication that role ownership
is ambiguous.

**Recovery checks.** The database owner verifies the intended role hierarchy
and grants, and the workload owner verifies the least-privileged expected
operation succeeds. Confirm that no root credential or broad grant was added
as a workaround, then show the original operation succeeds with the approved
identity before a human requeue.
