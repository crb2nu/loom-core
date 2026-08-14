---
name: mills-ops
description: "Day-2 operations for the loom-mills-operator deployed in k3s: inspect status, pause/resume autonomy, replay council runs, audit merges, hot-reload policy, recover from a corrupted DB, and stage feature flag rollouts. Use when an operator asks about mills state, a council run is paused/stuck, audit findings need triage, policy needs a hot-reload, or a v2 feature flag needs to flip."
version: 0.2.0
---

# Mills Ops

> **Note:** This source SKILL.md mirrors the registry entry in
> `mcp/context/skills-registry.yaml` (entry: `mills-ops`). The published
> per-platform SKILL.md bundles (Codex / Claude / Gemini / Kilocode) are
> generated from the registry by `loom sync`. Edit the registry's
> `instructions:` block, then regenerate.

## Purpose

Day-2 operations for the always-on `loom-mills-operator` cluster controller
that runs the Loom Mills Council + Pipeline. Audience is a cluster operator
working with the `loom mills` CLI on a Mac and the operator deployment in
k3s under namespace `loom-mills`. Read-only inspection is safe; every
mutating endpoint (`/run`, `/escalate`, `/sync`, kill switch edits, DB
restore Jobs) is gated behind explicit admin confirmation and, where
possible, GitOps.

`LOOM_MILLS_OPERATOR_URL` targets the REST + MCP listener (`:8090` in the
pod). Lifecycle probes and Prometheus metrics are served separately on the
metrics listener (`:9090`) and are not assumed to be exposed by the public
operator URL. The bundled snapshot therefore reads headline KPIs and stage
telemetry from `/api/mills/kpis` and `/api/mills/telemetry/stages`.
The public ingress's Cloudflare Access service token is distinct from the
operator bearer token. The snapshot and CLI resolve it from
`LOOM_MILLS_CF_ACCESS_ID` / `LOOM_MILLS_CF_ACCESS_SECRET`, then the generic
`CF_ACCESS_CLIENT_ID` / `CF_ACCESS_CLIENT_SECRET`, then shared loom config;
they never send those edge credentials to a loopback target.

## When to use

- User asks about mills status, council/pipeline progress, or eval scores.
- A council run is paused, stuck, or producing regressions.
- A pipeline run is hung in `running` and needs a force-escalate.
- Audit findings (Loop A artifact judge, Loop B merge attribution) need
  triage or a replay against a different ensemble.
- Policy needs to change (kill switch, ensemble, gates, budgets) and
  reload via fsnotify without restarting the pod.
- A v2 feature flag (`squads`, `audit`, `debate`, `cross-repo`,
  `adaptive-policy`) is staging from local → dev → prod.
- Operator state file is suspected corrupt and needs MinIO restore.

## Workflow

1. **Read current state.** Capture a snapshot before changing anything:
   - `loom mills status`
   - `loom mills pipelines list` (active/non-terminal runs)
   - `loom mills eval list` (or `--since=<RFC3339>`)
   - `bash mcp/skills/mills-ops/scripts/mills_status_snapshot.sh > /tmp/mills-status.md`
2. **Inspect the deployment in k3s.** Use `kc-k3s` (i.e. `KUBECONFIG=platform/gitops/.kube/k3s.yaml`):
   - `kubectl -n loom-mills get pods,cm,pvc,svc`
   - `kubectl -n loom-mills logs deploy/loom-mills-operator --tail=200`
   - For `/healthz`, `/readyz`, or raw `/metrics`, port-forward the separate
     listener: `kubectl -n loom-mills port-forward svc/loom-mills-operator 9090:9090`.
   - Look for the `policy reloaded` log line after any ConfigMap change.
3. **Decide action class.** Match symptom → action:
   - autonomy too aggressive → **pause** via kill switch
   - council outputs regressing → **replay** with a different ensemble
   - merge needs review → **audit** by MR iid (Loop B attribution)
   - pipeline run stuck → **force-escalate** the specific run
   - state file corrupt → **recover** from MinIO backup
   - new v2 capability ready → **rotate** feature flag (staged rollout)
4. **Apply via GitOps.** All policy edits live in
   `platform/gitops/k3s/mills/configmap-policy.yaml`. Edit, commit, push,
   then `flux reconcile kustomization apps -n flux-system --with-source`.
   **Never `kubectl edit configmap`** — Flux will revert it on the next
   reconcile and you will fight the controller.
5. **Verify.** Operator hot-reloads policy via fsnotify within seconds.
   Confirm the new posture: status snapshot, log line, and (for
   v2 flips) `mills_regression_count_total` over the next 24h.

## Common scenarios

**Pause autonomy (kill switch)**
```bash
# Edit platform/gitops/k3s/mills/configmap-policy.yaml: policy.enabled: false
git -C platform/gitops commit -am "ops(mills): pause autonomy"
git -C platform/gitops push
flux reconcile kustomization apps -n flux-system --with-source
kubectl -n loom-mills logs deploy/loom-mills-operator --tail=20 | grep "policy reloaded"
```

**Replay council with new ensemble**
```bash
loom mills council dryrun                 # safe: scratch DB, no commits
# Edit policy.council.ensemble in configmap-policy.yaml, commit, reconcile
loom mills council run                    # real replay under new policy
# Compare in HUD: Mills → Eval panel (Loop A scores)
```

**Audit a merged change by MR iid**
```bash
curl -sf \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?mr_iid=<iid>"
# Loop B attribution returns the originating council run id; inspect scores at
# /api/mills/eval/scores and filter SubjectKind/SubjectID with jq.
```

**Force-escalate a stuck pipeline run**
```bash
loom mills pipelines list
curl -X POST -H "Authorization: Bearer $LOOM_ADMIN_TOKEN" \
  "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs/<run_id>/escalate"
curl -sf "$LOOM_MILLS_OPERATOR_URL/api/mills/pipeline/runs?state=terminal&limit=50" | \
  jq '.[] | select(.State == "escalated")'
```

**Recover state from MinIO backup**
```bash
# 1) Confirm corruption inside the pod
kubectl -n loom-mills exec deploy/loom-mills-operator -- \
  sqlite3 /var/lib/loom-mills/state.db 'PRAGMA integrity_check;'
# 2) Scale to 0, run the restore Job from docs/MILLS_RUNBOOK.md, scale to 1
# Items committed since the last nightly backup must be re-run; council
# is idempotent and the backlog regenerates from .loom/backlog/*.yaml.
```

**Rollout staging (v2 feature flag flip)**
Local → dev cluster (kc-k3s) → production. One-week soak between flips.
Watch `mills_regression_count_total` after each flip; pause via
`enabled: false` if non-zero in the first 24h. Sequence is
`squads → audit (advisory) → debate (incident only) → cross-repo →
adaptive policy (manual-apply)`. Detail in
`.loom/94-implementation-plan-mills-v2-hierarchical-swarm-2026-05-02.md`
Phase 8.

## Reference

- Architecture and policy reference: `docs/MILLS.md`
- Full day-2 procedures (pause/resume, escalate, replay, recover, rollout):
  `docs/MILLS_RUNBOOK.md` — do not duplicate, refer.
- Companion skill notes (sources, common commands, telemetry, kill
  switch, eval framework, rollout staging): `mcp/skills/mills-ops/references/workflow.md`
- Read-only snapshot script: `mcp/skills/mills-ops/scripts/mills_status_snapshot.sh`
- v2 plan and acceptance criteria:
  `.loom/94-implementation-plan-mills-v2-hierarchical-swarm-2026-05-02.md`

## Out of scope

Implementation work for mills v2 (Phase 4+ slices: squads, audit
follow-ups, debate, cross-repo, adaptive policy) is owned by
`feature-dev` or `parallel-slice-ship`, not by `mills-ops`. This skill
covers operating what is already deployed; landing new code goes
through the standard worktree → tests → MR loop.
