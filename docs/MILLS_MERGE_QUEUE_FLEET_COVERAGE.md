# Mills merge-queue fleet coverage

Fleet merge writes must preserve the serial queue's `(project, target_branch)` lane invariant.

| Path | Decision | Contract |
|---|---|---|
| Mills pipeline merge stage | Queued (phase 1) | Existing durable pipeline candidate. |
| mrwatch shepherd arm | Queue | Submit a SHA-pinned external candidate; fail closed if the operator is disabled, full, or unavailable. |
| `mcp-gitlab merge_merge_request` | Queue in explicit fleet mode | Resolve the MR, pin its observed SHA, and return queue state. Standalone mode retains direct GitLab behavior. |
| reconciler `AdoptGreenMR` | Accept for this phase | Conservative, low-volume, and serialized by one reconciler tick. |

External producers call authenticated `POST /api/mills/merge-queue/enqueue`. `producer` plus `idempotency_key` is the durable identity; retries return `duplicate`. Candidates also carry project, MR IID, source and target branches, and observed SHA. The endpoint returns distinct `enqueued`, `duplicate`, `disabled`, and `full` outcomes. The endpoint itself never merges directly; the ONE producer-side fallback is `mcp-gitlab` on the `disabled` outcome, which performs the pre-queue direct merge — mirroring the cluster merge stage's own policy-disable semantics (serialization is a policy choice, not an availability constraint). Full lanes and unreachable queues surface as errors, never as silent direct merges.

`GET /api/mills/merge-queue` (open read) lists active candidates plus a per-lane depth summary for HUD panels and lane-pressure checks.

## Local agents via loomd

Local agents (Claude Code / Codex / Gemini on a workstation) reach the queue through the loomd HUD proxy rather than holding the operator admin token:

```
mcp-gitlab merge_merge_request
  → MILLS_MERGE_QUEUE_URL=http://localhost:3333   (loomd HUD)
  → HUD /api/mills/merge-queue/enqueue            (HUD admin gate: token or trusted net)
  → operator /api/mills/merge-queue/enqueue       (HUD injects the operator bearer)
```

loomd needs `LOOM_MILLS_OPERATOR_URL` + `LOOM_MILLS_OPERATOR_TOKEN` in its environment (the same pair that powers every other `/api/mills/*` proxy route). The per-agent `mcp-gitlab` env sets only `MILLS_MERGE_QUEUE_URL` (and `MILLS_MERGE_QUEUE_TOKEN` with the HUD admin token when the HUD is not on a trusted network) — the cluster-wide operator credential never lands in per-agent config files.

## Evidence and attribution

Use this soak query, split by the durable candidate provenance when investigating individual rows:

```promql
sum(increase(mills_mergequeue_evictions_total{reason="head_moved"}[24h]))
```

Correlate the eviction timestamp and lane with the merge queue row's `detail.producer` and GitLab's merged-MR audit events for that target branch. Shepherd and fleet MCP merges are attributable through `producer=mrwatch_shepherd` and `producer=mcp_gitlab`; pipeline candidates retain their pipeline run identity.

The accepted `AdoptGreenMR` path remains parked while attributable `head_moved` evictions are zero. Any non-zero eviction attributable to adoption in a 24-hour soak unparks queue routing for that path; unrelated pipeline rebases do not.
