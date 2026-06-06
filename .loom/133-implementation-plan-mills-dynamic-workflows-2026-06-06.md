# Implementation Plan: Capability-Confined Durable Workflow Runtime for Mills

**Date**: 2026-06-06
**Spec**: `132-product-spec-mills-dynamic-workflows-2026-06-06.md`
**Research**: `131-research-mills-dynamic-workflows-2026-06-06.md`
**Brainstorm**: `130-brainstorm-mills-harness-agnostic-workflows-2026-06-06.md`
**Gate**: Slices S6/S7 are BLOCKED until **S1 (in-process) AND S1c (deployed crash) kill-tests pass.**

---

## Slice DAG

```
S1 (kill-test spike, IN-PROCESS) ──┐
                                   ├─► S5 (reuse/build decision gate)
S1c (DEPLOYED crash kill-test) ────┤        │
                                   │        ▼
S2 (Layer 1 Worker contract) ──────┼─► S2b (client-side idempotency key) ─┐
                                   │                                       │
S3 (Layer 2 step journal) ─────────┼───────────────────────────────────────┼─► S6 (Layer 3 runtime, KILL-TEST-GATED)
                                   │                                       │        │
S4 (HUD step-log panel) ◄── S3     │                                       │        ▼
                                                                           └─► S7 (council template select + clamp)
```

**Kill-test gates:** S1 (in-process) and **S1c (deployed crash)** gate the entire Layer-3 build. S6 cannot ship until S1c passes on the deployed k8s operator. **S1 green alone does NOT authorize S2–S5 to assume substrate idempotency.**

**Ordering rationale:** Layer 1 (S2) and Layer 2 (S3) ship standalone value to the live pipeline before the interpreter exists. The reuse/build decision (S5) is an explicit early gate with a control experiment. The client-side idempotency prerequisite (S2b) — under-scoped in the original designs — is its own slice with its own deployed kill-test, sequenced before S6.

**Suggested parallelization:** S1 (spike) ∥ S2 (Worker contract) ∥ S3 (journal) can start together — none depends on another's code. S2b follows S2; S1c follows S2b. S5 needs S1+S2+S3. S6 needs everything + S1c green. S7 follows S6.

---

### S1 — Kill-test spike: deterministic replay-from-journal (in-process)

**Goal:** Prove the Tier-1 replay mechanism in isolation, exercising the HARD cases the verdict requires, before any production wiring.
**Deliverables:** Throwaway `pkg/mills/workflow/spike/` embedding Starlark-Go; `:memory:` SQLite journal with `StepKey`+`Get`+`Append`; structured drift-tolerant step keys; replay short-circuit with effect-counter. Script: `agent('a'); gate('g'); agent('b')` + 2-child `parallel()` + a `loop_until_dry()`.
**Pass/fail (all must hold):** (1) replay short-circuits A,G without incrementing counter, only B+parallel run live; (2) **divergent loop-iteration-count replay** survives via structured keys; (3) **concurrent `parallel()` key assignment under scheduling jitter** is deterministic; (4) injected `call_hash` mismatch → **quarantine+escalate**, not double-execute, not mass-abort; (5) **interpreter-version-bump** replay refuses + escalates; (6) **hostile params** (`fan_out_width`/`max_iter` absurd) → host ceiling clamps; (7) script `import os/time/random` fails to resolve. FAIL → fall back to declarative-DAG (spec §7 option b).
**filesAffected:** `pkg/mills/workflow/spike/replay_spike_test.go` (new), `pkg/mills/workflow/spike/journal.go` (new), `go.mod` (add `go.starlark.net`).
**Standalone value:** Go/no-go signal for the entire Layer-3 bet in <30 min, zero operator changes.
**Verification:** `go test ./pkg/mills/workflow/spike/ -race -v`.
**Effort:** M (3–5 days). **Risks:** Starlark integration friction; if it blocks the spike in-budget, that itself is signal (do NOT silently fall back to goja — record it).

### S1c — DEPLOYED crash kill-test (the real de-risk)

**Goal:** Prove exactly-once-across-operator-crash on the **deployed k8s operator** — the property the in-process S1 cannot exercise. *(S1 is necessary but insufficient; the genuine de-risk must run live.)*
**Deliverables:** A minimal real workflow deployed behind `policy.workflows.enabled`; a harness to kill the spawn pod and the operator process mid-B; assertion on the persisted global side-effect counter across restart.
**Pass/fail:** resume re-attaches by idempotency key (pod/Job name = key → `AlreadyExists` no-op); counter exact across operator-process restart; no second pod; no double-merge. Exercise Codex + Gemini + a harvester-vm buffered spawn; assert `CostSource` recorded correctly and empty-telemetry+`exit!=0` buffered run recorded as failure.
**dependsOn:** S2b (needs the client-side key).
**killTestGated:** this IS the gate for S6.
**filesAffected:** `pkg/mills/workflow/killtest/` (new), operator deploy config (gitops).
**Standalone value:** Definitive deployed evidence; converts the riskiest-assumption status to passed/FAILED with live evidence.
**Verification:** deployed run + `kubectl delete pod` + operator restart; counter assertion. (k3s context per workspace deployment rules.)
**Effort:** L. **Risks:** harvester-vm may lack name dedupe → imperative runtime stays k8s-only.

### S2 — Layer 1 Worker contract (zero behavior change)

**Goal:** Extract the stable `WorkerRunner`/`WorkerRequest`/`WorkerResult` contract; add `AgentType` (required+validated), `IdempotencyKey` (plumbing), `CostSource` + `Telemetry`.
**Deliverables:** `pkg/mills/worker` package; `spawnClientAdapter` (field-map, byte-identical wiring); `CostSource` enum surfaced through `hudSpawnTelemetry` (fix the lossy subset, `clients/spawn.go:173-179`); `AgentType` validation at the dispatcher boundary (reject unknown, stop relying on `Model`-string inference, `clients/spawn.go:389-396`); per-harness capability flags.
**filesAffected:** `pkg/mills/worker/worker.go` (new), `pkg/mills/worker/adapter.go` (new), `pkg/mills/pipeline/dispatcher.go:139-183`, `pkg/mills/clients/spawn.go:136-154,173-179`, `internal/hud/spawn.go:1542-1695`.
**Standalone value:** The live pipeline gets structured per-stage telemetry **with cost provenance** on `StageOutput` — improving HUD cost attribution immediately, with no runtime dependency.
**Verification:** round-trip contract test asserting field parity + `CostSource` survives the operator boundary for all three harnesses. `go test ./pkg/mills/worker/ ./pkg/mills/clients/`.
**Effort:** M (3–4 days). **Risks:** None to runtime behavior (adapter preserves wiring).

### S2b — Client-side deterministic spawn idempotency key (PREREQUISITE)

**Goal:** Move spawn-id generation client-side and make the substrate create the pod with a name/label derived from the key, so a duplicate create is `AlreadyExists`. *(Spans three components, not one field — `spawnID` is server-minted via `crypto/rand` today, `controller.go:497`.)*
**Deliverables:** Client generates `IdempotencyKey`; `/api/mobile/v1/agent/spawn` accepts a client key (id-ownership moves client-side, `clients/spawn.go:65,287-306`); `internal/spawn` controller derives pod/Job name from key; record-before-dispatch ordering so the `spawn_requested` step is committed before the dispatch HTTP call; resume re-attaches by key. Its **own deployed crash kill-test** (crash in the record-before-dispatch window asserts the `AlreadyExists` no-op fires).
**dependsOn:** S2.
**filesAffected:** `pkg/mills/clients/spawn.go:65,220-228,287-306`, `internal/hud/spawn.go:662`, `internal/spawn/controller.go:111,497-500`.
**Standalone value:** Closes the pre-existing double-spawn window (`clients/spawn.go:220-228`) that the current pipeline already has — a safety fix independent of Layer 3.
**Verification:** deployed crash test; assert single pod after restart. `go test ./internal/spawn/ ./pkg/mills/clients/`.
**Effort:** L. **Risks:** harvester-vm name dedupe unproven → gate imperative to k8s.

### S3 — Layer 2 durable step/event journal (migration 004)

**Goal:** Land `workflow_steps` + `workflow_runs` tables and DAO as the stable journal; no runtime yet.
**Deliverables:** `004_workflow_steps.sql` (forward-only, with immutable `engine` discriminator, `template_version`, `interpreter_version`); `WorkflowStep`/`WorkflowRun` types + enums; `WorkflowDAO` (`StepKey`-based `GetStep`, `AppendStep` idempotent `INSERT OR REPLACE`, `ListPending`, `ListByRun`); wired into `Store.Open`. **Dual source-of-truth resolution:** legacy `dag` runs do NOT write `workflow_steps`; `imperative` runs use it exclusively; documented record-before-result invariant; `workflow_steps` wins on conflict; a test crashes between `PutRun`/`PutStage`/`PutWorkflowStep`.
**dependsOn:** S1.
**filesAffected:** `pkg/mills/store/migrations/004_workflow_steps.sql` (new), `pkg/mills/store/dao_workflow.go` (new), `pkg/mills/store/types.go`, `pkg/mills/store/store.go`.
**Standalone value:** A durable per-call audit journal + the foundation Layer 3 needs. *(Corrected: the existing pipeline ALREADY resumes via `stage_results`, so the incremental value here is the per-call audit channel + the imperative journal — NOT "newly enables resume.")*
**Verification:** idempotent `AppendStep`, structured-key drift test, crash-between-writes reconciliation test. `go test ./pkg/mills/store/ -run Workflow`.
**Effort:** L (4–5 days). **Risks:** dual-write skew (mitigated by exclusive-table-per-engine rule).

### S4 — HUD step-log replay panel

**Goal:** Surface the journal: detail endpoint, monitor, frontend timeline/replay.
**Deliverables:** `GET /api/mills/workflow/runs` + `/{id}` (run+steps+events nested); `WorkflowMonitor` SSE; KPI additions (incl. `workflow_quarantined_runs`, cost branched on `CostSource`); frontend timeline with cache-hit/live/quarantine badges; `dist/` rebuilt + committed.
**dependsOn:** S3.
**filesAffected:** `cmd/loom-mills-operator/main.go:184-220`, `internal/hud/monitor/mills.go:35-95`, `pkg/mills/kpi_writer.go:80-142`, `internal/hud/embed.go` + HUD frontend.
**Standalone value:** Operators see/replay per-step execution + cost provenance with no runtime dependency.
**Verification:** endpoint tests; visual check. `go test ./internal/hud/...` + `make hud-frontend` then commit `dist/`.
**Effort:** L (1.5–2 weeks incl. frontend). **Risks:** `go:embed` dist rebuild gotcha (CI doesn't rebuild — must commit).

### S5 — agentcontext reuse/build decision gate (with control experiment)

**Goal:** Record the BUILD verdict as observed evidence, not narrative.
**Deliverables:** Decision doc + ADR in `.loom/` scored against C1–C5 (spec §7); **the S1 recorded-effect-counter kill-test run against agentcontext UNMODIFIED**, capturing the observed failure (no per-call row, stale snapshot, Running-crash deadlock); explicit conditional-dismissal branch (if S1 fails or declarative target accepted → agentcontext DAG becomes ~70% reuse candidate, BUILD revisited); coexistence ADR; `pkg/mills/workflow` package skeleton.
**dependsOn:** S1, S2, S3.
**filesAffected:** `.loom/` decision doc + ADR (new), `pkg/agentcontext/workflow_persist.go` + `schema_workflow.go` (reference only), `pkg/mills/workflow/` skeleton (new).
**Standalone value:** Falsifiable BUILD evidence prevents a wasted reuse attempt; clear package boundary.
**Verification:** control-experiment test recorded. **Effort:** S (1–2 days).

### S6 — Layer 3 capability-confined Starlark runtime (KILL-TEST-GATED)

**Goal:** Embed Starlark with the whitelist universe + `EffectHost`, behind `policy.workflows.enabled` (k8s-only), DAG remaining default.
**Deliverables:** `interp.go` (whitelist builtins; while/recursion disabled unless bounded; `loop_until_dry` host-bounded); `host.go` (`EffectHost` routing `agent()`→`WorkerRunner` w/ idempotency key, `gate()`→`policy.Evaluate`, `tool()`→bridge; read-through cache; quarantine-on-hash-mismatch; canonical parallel ordering); **host-level resource envelope** (wall-clock deadline, concurrent-spawn semaphore, total-spawn cap, loop ceiling, mem limit — council params only lower); **pre-flight budget reservation** + secondary 5s watcher + turn/time fallback when `CostSource != real`; **mechanical 100%-interception enforcement** (test-time fake EffectHost fails on un-recorded IO + import ban on net/os/time); merge idempotency at `GitLabClient.Merge`; `WorkflowScheduler` honoring `AutonomyGate` + `paused_at`; interpreter/template-version pinning with hard resume-abort-and-escalate on drift. **Gate: S1c deployed crash kill-test passes**, including the hostile-param clamp case.
**dependsOn:** S1, S1c, S2, S2b, S3, S5.
**killTestGated:** yes (S1c).
**filesAffected:** `pkg/mills/workflow/{interp,host,scheduler,budget,parallel}.go` (new), `cmd/loom-mills-operator/main.go:338-391`, `pkg/mills/policy.go`, `internal/hud/clients/gitlab.go:271-284`.
**Standalone value:** A real `loop_until_dry` review workflow runs end-to-end on one canary behind a flag with deterministic deployed resume — imperative power without touching the default DAG.
**Verification:** S1c deployed crash test + hostile-param clamp + import-ban vet check + merge-idempotency test. `go test ./pkg/mills/workflow/ -race`.
**Effort:** XL (2.5–3 weeks). **Risks:** harvester-vm gating; multi-turn Claude-only restriction.

### S7 — Council template selection + closed registry + clamping

**Goal:** Per-task adaptivity by selecting+parameterizing a named template; never raw control flow.
**Deliverables:** `TemplateRegistry` (named template → Starlark source + params schema + defaults; content-hashed, ref-counted, immutable; startup fail-fast on unknown/unresolvable pinned version); `ResolveWorkflowTemplate` in `reconciler.tryStart` after squad routing, **before** `run.Put`, writing immutable `engine`, frozen `template_version`, pinned `interpreter_version`, **clamped** `workflow_params` to the row; runtime reads params from the row, never re-resolves on resume; **numeric clamping with fuzz test** (`fan_out_width`/`max_iter` bounds); extension points (`WorkflowHint`, `ManifestWorkflow`, `PerLabelWorkflowTemplates`); engine-discriminator guard so selection never re-routes started runs.
**dependsOn:** S6.
**filesAffected:** `pkg/mills/workflow/registry.go` (new), `pkg/mills/reconciler.go:608-649`, `pkg/mills/council/backlog_mutator.go:17-42`, `pkg/mills/squads/types.go:52-85`, `pkg/mills/policy.go:141-166`, `pkg/mills/store/types.go`.
**Standalone value:** Different work types get different confined workflows, selected safely and recorded for audit.
**Verification:** clamp fuzz test; unknown-template rejection; in-flight-run-not-re-routed test. `go test ./pkg/mills/...`.
**Effort:** L (1–1.5 weeks). **Risks:** stale-param fallback; mitigated by clamp + freeze.

## Verification & rollout

**Per-layer feature-flagging:**
- Layer 1 (S2/S2b): no flag — adapter preserves behavior; `IdempotencyKey` empty = legacy path until S2b lands.
- Layer 2 (S3/S4): `WorkflowStepLogReady` capability flag; `dag` runs never write `workflow_steps`, so the journal is inert for the default pipeline.
- Layer 3 (S6/S7): `policy.workflows.enabled`, default OFF, **gated to `substrate==k8s`**, one canary template.

**Dual-run vs DefaultStages:** the hardcoded DAG stays default and authoritative. In-flight runs resume under their immutable `engine` discriminator (never the mutable `Template`). Imperative runs are observable side-by-side in the HUD (S4) before any default flips. No flip is proposed in this plan — the canary proves the path; broader rollout is a follow-on decision.

**Rollback:** `workflow.enabled=false` returns to pure-DAG behavior instantly; forward-only tables are inert when unused (no schema rollback). S2b's client-key path falls back to server-minted ids if reverted. Each slice is independently revertible; later slices fail-closed to the DAG.

**Deploy-safety gate:** bumping `go.starlark.net` or changing any `EffectHost` builtin signature requires **draining in-flight imperative runs first** (CI gate), because `interpreter_version` drift forces a resume-abort-and-escalate on every active run.

## Open questions to resolve before/within S6

(from spec §11) emergency-stop endpoint vs between-step pause; exact host ceilings (load-test the operator); `result_blob` size cap + offload; harvester-vm name-dedupe feasibility; conservative Codex cost multiplier.

## Handoff

- **S1 / S1c** are spikes → use the `research` skill + a throwaway worktree; do **not** start `feature-dev` on S6 until S1c is green.
- **S2 / S2b / S3 / S4** → `feature-dev` (worktree-isolated per slice), shippable independently; follow the workspace auto-ship policy (commit/push/MR/auto-merge) since each is a self-contained, behavior-preserving increment.
- **S5** → `decision-journal` + ADR.
- Register slices as tracked tasks via `agent_task_add` (namespace `mills/dynamic-workflows`) so multi-agent progress is visible in the HUD.
- Next concrete action: **scaffold S1** (`pkg/mills/workflow/spike/`, add `go.starlark.net` to `go.mod`) and run the seven in-process pass/fail checks.
