# Mills Operational Guardrails

Operator-facing guidance for keeping Loom Mills in a safe state when telemetry, storage, or authentication dependencies degrade.

This document complements `docs/MILLS.md` and `docs/MILLS_RUNBOOK.md`. Use it when `/api/mills/status`, `/api/mills/capabilities`, HUD, or alerts show that unattended Mills work may no longer be trustworthy.

`LOOM_MILLS_OPERATOR_URL` is the REST + MCP base URL. `/healthz`, `/readyz`,
and raw `/metrics` live on the separate `:9090` metrics listener; use a cluster
connection or port-forward for those routes rather than appending them to the
public REST URL.

## Guardrail model

Mills composes several practical safety layers:

| Layer | Where to check | Safe state |
|---|---|---|
| Policy kill switch | `policy.enabled` in the Mills policy ConfigMap | `false` closes work-creating REST admissions and background schedulers. |
| Quiescence proof | `GET /api/mills/safety/quiescence` | all durable and in-memory activity counts are zero before maintenance or shared-pod fault injection. |
| Destructive-action lease | `POST /api/mills/safety/crash-lease` + token renewal | blocks new admissions across the last quiescence read, identity checks, and bounded UID delete. |
| Capability breaker | `GET /api/mills/capabilities` | Any required red/stub row makes `autonomy_ready=false`. |
| Workflow interpreter drain gate | CI: `scripts/ci/check_workflow_drain_gate.sh` | MRs changing the interpreter replay-compatibility surface fail until in-flight imperative runs are drained and the MR carries `[workflow-drain-confirmed]`. |
| Pipeline continuation breaker | Pipeline stage events and escalation reason | A running pipeline checks autonomy before each autonomous stage; a blocked verdict escalates the item instead of continuing toward MR, CI watch, or merge. |

`/readyz` only says the service initialized. It is not an autonomy signal. Treat `autonomy_ready=false` as the authoritative fail-closed state for unattended writes.

## Breaker thresholds

These thresholds are the operator-facing defaults to know during incident response.

| Guardrail | Default threshold | Failure behavior | Operator action |
|---|---:|---|---|
| Mills policy kill switch | `policy.enabled=false` | New queued work is not picked up. In-flight runs continue under the policy captured at start, then hit the continuation breaker if readiness is blocked. | Use for any ambiguous telemetry, storage, or auth incident where autonomous safety cannot be proven. |
| Capability readiness | All required rows green/real | `autonomy_ready=false`; blockers list the red/stub capability rows. | Remediate the named row, then verify `/api/mills/capabilities`. |
| Pipeline retry policy | `max_attempts: 3`, `cooldown_seconds: 300` | Retryable stage failures cool down and retry; exhausted items escalate. | Do not manually restart the same failure loop until the dependency is fixed. |
| Pipeline budget policy | `$5` per pipeline run, `$75` per day, `4` concurrent runs, `20` runs/day | Budget/concurrency gates stop new work from exceeding policy. | Raise only by policy change with an explicit reason and rollback point. |
| Council budget policy | `$15` per council run, `$50` per day | Council demand generation is held when budget is exhausted. | Check whether queue starvation is budget-driven before treating it as a model or storage failure. |
| Local MCP recv timeout breaker | `3` consecutive local recv timeouts | The daemon tears down the stale local stdio transport so the next request can reconnect. | Inspect server logs after the third timeout; one timeout alone is not enough to declare the server dead. |
| Embedding provider breaker | `3s` per call, `3` consecutive failures, `30s` cooldown | Embedding calls fail fast with `embedder unavailable`; callers should use fallback/non-vector paths where implemented. | Fix the provider or route around it; expect degraded semantic search until recovery. |

## Workflow interpreter drain gate

Imperative workflow runs (`engine=imperative`) replay their journal on every
resume, so a journal written by one operator binary must stay replayable by the
next. The replay-compatibility surface — universe builtins, canonical-args
encoding, step-key scheme, idempotency-key derivation, host ceilings, and the
`go.starlark.net` engine itself — is pinned in
`pkg/mills/workflow/testdata/interp_surface.golden` (enforced by
`TestInterpreterSurfaceGolden`; `TestInterpreterVersionMatchesGoMod`
additionally binds `HostInterpreterVersion` to the go.mod engine version).

CI runs `scripts/ci/check_workflow_drain_gate.sh` on every MR. An MR that
changes the golden or the `go.starlark.net` requirement fails until the final
commit message carries `[workflow-drain-confirmed]`, which asserts the author
ran the drain procedure:

1. `GET /api/mills/safety/quiescence` on the deployed operator shows no
   running/paused `engine=imperative` workflow runs (pipeline DAG runs do not
   replay through the interpreter and do not count), or the in-flight runs are
   deliberately paused and allowed to abort-and-escalate on the version-pin
   refusal after deploy.
2. The golden is regenerated
   (`UPDATE_INTERP_SURFACE=1 go test ./pkg/mills/workflow -run TestInterpreterSurface`).

## Workflow template registry (S7)

Imperative workflow programs come from a CLOSED, compiled-in registry
(`pkg/mills/workflow/registry`): named + versioned templates with clamped
numeric parameters and closed enum vocabularies. There is no runtime mutation
surface — adding a template is a reviewed code change.

- **Selection** is authored on a backlog item
  (`policy.workflow_template` / `workflow_template_version` /
  `workflow_params` / `workflow_enums`). The council's backlog mutator strips
  invalid selections at authoring (the item lands as a normal DAG item);
  admission-side `ResolveItemSelection` is the backstop and fails closed on an
  unknown template — it never falls back to a default program. A selection
  while `workflows.enabled: false` defers the item rather than silently
  running the DAG.
- **Freezing**: a resolved selection stamps engine, template, version,
  interpreter version, and the clamped/validated params (with the template's
  content hash) onto the run row. Started runs are never re-resolved; editing
  the item cannot re-route an in-flight run, and the imperative interpreter
  refuses DAG-engine rows outright.
- **Drift**: at execution the interpreter re-derives the template's content
  hash from the compiled-in registry and terminalizes the run on mismatch —
  the deploy-time analog of the interpreter version pin.
- **Start kernel** (`ClaimWorkflowStart`): a frozen selection routes the item
  through a transactional start mirroring `ClaimPipelineStart` — queued→running
  CAS, admission against the SHARED pipeline budget tier (reservations are
  cross-lane visible, so the two kernels cannot jointly oversubscribe the
  daily cap), an imperative run stamped with the frozen identity, and the
  aggregate transition (`workflow_start`). No pipeline run and no dispatch
  intent exist: the workflow scheduler discovers the run directly.
- **Terminal settle**: when a claim-started imperative run reaches any
  terminal state, the SAME transaction that commits the lifecycle CAS
  releases the budget reservation, escalates the backlog item
  (`workflow_terminal` transition), and records a `workflow.terminal_settle`
  event on the item pointing the reviewer at the run outcome and the
  `mills-wf/<run-id>` work-product branch — v1 templates stop pre-merge, so
  even a `done` run's work product needs human review. The active reservation
  is the claim-provenance discriminator: admin/test canaries carry no
  reservation and can never release or escalate an item they did not claim.
- **Wall-clock bound**: `workflows.max_run_minutes` (default 180) bounds an
  imperative run's lifetime; the scheduler terminalizes an over-age run as
  error, and the settle frees its reservation and item — a wedged run can
  never hold quiescence (or future canary windows) hostage.
- **Template immutability**: shipped template versions are pinned by content
  hash in `pkg/mills/workflow/registry/testdata/templates.golden`; editing a
  shipped version fails CI with instructions to ship a new version instead
  (in-flight runs frozen to an edited version terminalize fail-closed).
- **Metrics**: `mills_workflow_start_claims_total`,
  `mills_workflow_selection_outcomes_total`, and
  `mills_workflow_runs_terminal_total{state,cause}` mirror the pipeline
  kernel's observability.

## Safe state procedure

1. Freeze new autonomous work:

   ```bash
   # In platform/gitops/k3s/mills/configmap-policy.yaml:
   #   enabled: false
   flux reconcile kustomization apps -n flux-system --with-source
   kubectl logs -n loom-mills deploy/loom-mills-operator --since=2m | grep "policy reloaded"
   ```

   Apply the ConfigMap-only closure first. Do not update a Deployment checksum
   or otherwise restart the operator until quiescence is proven; rollouts happen
   behind the already-closed barrier.

2. Confirm Mills is fail-closed:

   ```bash
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/status" | jq '{policy_enabled, autonomy_ready, autonomy_blockers}'
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/safety/quiescence" | jq
   loom mills pipelines list
   ```

3. If a running pipeline is already past the point you trust, force-escalate it:

   ```bash
   curl -sf -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
     "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>/escalate"
   ```

4. Remediate the dependency class below.

5. Resume only after `/api/mills/capabilities` shows `autonomy_ready=true`, recent logs are clean, and any escalated item has a human disposition.

## Telemetry failures

Telemetry failures are visibility failures first. They become autonomy blockers when they hide required capability state or make it impossible to verify what Mills is doing.

Common symptoms:

| Symptom | Likely source | Safe interpretation |
|---|---|---|
| Metrics-listener `/metrics` missing `mills_*` series | Metrics listener, scrape, or KPI writer path | Mills may still run, but trend-based decisions are blind. Pause if you cannot inspect status another way. |
| `/api/mills/kpis?window=1d` returns `404` immediately after restart | KPI writer has not recorded its first post-start snapshot | Wait one scheduler tick, then recheck. Persistent `404` is a telemetry incident. |
| HUD shows stale Mills panels while CLI/status endpoints are fresh | HUD proxy/cache path | Prefer direct operator endpoints during the incident. |
| Missing cost or spawn telemetry on a failed stage | Spawn/harness telemetry path | Treat the stage result as incomplete; inspect the run event log and spawn logs before retrying. |

Remediation workflow:

1. Check direct operator surfaces before derived dashboards:

   ```bash
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/status" | jq
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | jq
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/telemetry/stages?window=1d" | jq
   kubectl logs -n loom-mills deploy/loom-mills-operator --tail=200
   ```

2. If direct status is healthy but HUD is stale, keep Mills paused only if operators depend on HUD for approval. Otherwise continue using direct status during the HUD fix.

3. If KPI snapshots or metrics are missing across more than one scheduler tick, keep or set `policy.enabled=false` until visibility is restored.

4. After repair, verify a fresh KPI snapshot:

   ```bash
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/kpis?window=1d" | jq '{SnapshotAt, Metrics}'
   ```

## Storage failures

The canonical store is SQLite on the `mills-state` Longhorn PVC. Storage is a hard autonomy dependency.

Common symptoms:

| Symptom | Breaker row | Safe state |
|---|---|---|
| `/api/mills/capabilities` shows `sqlite_store` red | `sqlite_store` | `autonomy_ready=false`; do not resume until DB liveness and integrity are known. |
| Operator logs show `database is locked`, failed migrations, or integrity errors | `sqlite_store` or `policy_loaded` | Pause, capture diagnostics, and follow the DB recovery runbook. |
| Repo root checkout or `.loom` is missing/unwritable | `repo_root` | Read-only APIs can stay up, but unattended backlog execution must remain blocked. |

Remediation workflow:

1. Capture the current state:

   ```bash
   kubectl get deploy,po,pvc -n loom-mills
   kubectl logs -n loom-mills deploy/loom-mills-operator --previous --tail=500
   kubectl logs -n loom-mills deploy/loom-mills-operator --tail=500
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | jq '.autonomy_blockers, .capabilities[] | select(.status!="green")'
   ```

2. For DB integrity errors, use the recovery procedure in `docs/MILLS_RUNBOOK.md` before replaying work.

3. For repo-root errors, verify the mounted checkout:

   ```bash
   kubectl exec -n loom-mills deploy/loom-mills-operator -- \
     sh -lc 'git -C "$LOOM_MILLS_REPO_ROOT" status --short && test -w "$LOOM_MILLS_REPO_ROOT/.loom"'
   ```

4. Resume only after `sqlite_store`, `policy_loaded`, and `repo_root` are green and `autonomy_ready=true`.

## Authentication failures

Auth failures are fail-closed. Missing admin auth blocks mutating endpoints; missing or invalid downstream credentials block the required capability rows for autonomous work.

Common symptoms:

| Symptom | Breaker row | Safe state |
|---|---|---|
| Admin endpoints return `401`/`403` | `admin_auth` | Mutating endpoints fail closed; rotate or restore `LOOM_ADMIN_TOKEN`. |
| MR, CI watch, merge, or cleanup stages fail with GitLab auth errors | `gitlab` | Autonomy blocked; do not retry until token/project/API URL are fixed. |
| Spawn stages fail with unauthorized responses | `hud_spawn` | Plan-slice, implement, and self-review stages cannot be trusted. |
| Agent-context session cannot initialize | `mcp_hub_session` | Handoff, plan-slice, and coordination paths are degraded; autonomy blocked. |
| FlexInfer returns auth/model access errors | `flexinfer` | LLM-judged gates and research/council paths are not safe for unattended operation. |

Remediation workflow:

1. Identify the failing auth boundary from capabilities:

   ```bash
   curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/capabilities" | \
     jq '.capabilities[] | select(.status!="green") | {id, status, mode, config_key, message}'
   ```

2. Rotate or restore the specific secret through GitOps when possible. Use direct cluster patches only for an emergency, and follow with a GitOps commit so Flux does not revert the fix.

3. Restart or roll the dependent deployment when the secret is env-backed rather than dynamically read.

4. Verify with a read-only probe first, then a dry run:

   ```bash
   loom mills status
   loom mills council dryrun
   ```

5. Resume policy only after the capability row is green and any failed pipeline run has been escalated, retried deliberately, or replaced by a new backlog item.

## Resume checklist

Before setting `policy.enabled=true` again:

- `/api/mills/capabilities` returns `autonomy_ready=true`.
- `autonomy_blockers` is empty.
- `loom mills pipelines list` contains only active runs you intend to keep.
- Recent operator logs contain no repeated storage, telemetry, or auth failures.
- Any forced escalation has a linked human note, GitLab issue, or agent-context handoff.
- The policy change has been applied through GitOps or reconciled back into GitOps after an emergency patch.
