# Product Spec: Capability-Confined Durable Workflow Runtime for Loom Mills

**Date**: 2026-06-06
**Status**: Draft — BLOCKED on Slice-1 + Slice-1c kill-tests (§3)
**Owner**: Cody Blevins
**Repo**: `services/loom-core`
**Lineage**: `130-brainstorm-mills-harness-agnostic-workflows` → `131-research-mills-dynamic-workflows` (interpreter + durability decisions) → this spec → `133-implementation-plan-mills-dynamic-workflows`
**Provenance**: synthesized from a 15-agent workflow (5 codebase maps with file:line evidence, 2 cited research dossiers, 2 competing designs, 5 adversarial verdicts — all `holds-with-caveats`, every must-change folded in).

> Convention note: this is a Loom-native engine, distinct from Anthropic's Claude-Code "dynamic workflows" (the JS `agent()/parallel()/pipeline()` Workflow tool). We port the *authoring shape* to a harness-agnostic, durably-replayable runtime that any spawned harness (claude-code/codex/gemini) can be a worker for.

---

## 1. Summary & goals

Mills today runs every backlog item through a single hardcoded pipeline. This spec replaces the implicit, un-resumable orchestration with three independently shippable layers: a named **Worker contract**, a durable **step/event protocol**, and a capability-confined **imperative runtime** for the minority of work that needs runtime-determined control flow.

**The four drivers:**

1. **Durable resume** — an operator restart must not re-spawn completed work or lose hours of in-flight progress.
2. **Per-task adaptivity** — different work types (and squads) run different, still-confined workflows, selected and parameterized at launch.
3. **Workflows as data** — workflow shape is authored/selected, not hardcoded in `DefaultStages`.
4. **Imperative power** — express loop-until-dry review and runtime-determined fan-out that a static DAG cannot.

**What success looks like:**

- The existing pipeline gains a durable, memoized step journal and HUD replay with **zero runtime behavior change** (Layers 1–2 ship value before the interpreter exists).
- A canary imperative workflow runs end-to-end behind a flag, survives an operator-process crash with **exactly-once agent spawn proven on the deployed target substrate**, and never double-merges.
- The hardcoded `DefaultStages` DAG remains the default and the proven resume model for the ~90% fixed-pipeline majority. The imperative runtime is opt-in per template, with the burden of proof on each imperative template.

## 2. Background & current state

**Mills runs one hardcoded pipeline.** Every `PipelineRun` is created with `policy.Pipeline.DefaultTemplate` (`pkg/mills/reconciler.go:615`), and the runner ignores `run.Template`, always walking the compiled `DefaultStages` array (`pkg/mills/pipeline/runner.go:1-107`, `45-55`). The `Template` field is decorative today.

**Resume already works — via a materialized DAG, name-keyed.** `resumeIndex()` (`runner.go:442`) returns an integer position by name-lookup over the static stage array; `loadPriorOutputs()` (`runner.go:465`) rehydrates a map keyed by stage **name**, not positional index; `pendingStage()` (`runner.go:531-546`) detects in-flight spawns by `outcome=null` + `spawn_id != ""`; `withResumeSpawnID`/`ResumeSpawnIDFromContext` (`runner.go:149-174`) re-attach a pending spawn after restart. Idempotent stage retrieval comes from `stage_results` with `UNIQUE(pipeline_run_id, stage, attempt)` and `ON CONFLICT` upsert (`dao_pipeline.go:487-548`). **This shipping model is the baseline we preserve, not replace.** (Verified fact.)

**Two disconnected workflow systems exist.** `pkg/agentcontext` has a `WorkflowEngine` — an in-memory, goroutine-driven **declarative DAG** executor (`workflow_executor.go:14-71`) persisted as a single Qdrant snapshot point per workflow (`workflow_persist.go:43-54`), with events not persisted (`workflow_persist.go:124`) and `CheckpointWorkflow` called only from MCP handlers, never the executor loop (`service_workflows.go:272,301,328`). Mills has no such engine — it has the hardcoded pipeline above. The two share vocabulary (`Workflow`, `WorkflowStep`) but no execution code.

**The MCP/harness seam is already neutral.** `SpawnRequest`/`SpawnResponse` (`pkg/mills/pipeline/dispatcher.go:139-183`) are harness-agnostic; `SpawnEventSink` (`internal/hud/spawn_parser.go:14-60`) fans divergent Claude/Codex/Gemini JSONL into one accumulator; diff/files/commit data are reconstructed operator-side by `git diff baseBranch...origin/branch` (`pkg/mills/clients/spawn.go:481-506`), making them parser-independent and substrate-opaque. (Verified facts.)

**What does NOT exist today (verified absent):** any spawn idempotency-key infrastructure — `spawnID` is minted server-side via `crypto/rand` (`internal/spawn/controller.go:497-500`); the pod name is `"spawn-"+spawnID` (`internal/hud/spawn.go:662`); grep across `internal/spawn`, `internal/hud/spawn.go`, `pkg/mills/clients/spawn.go` returns **0** idempotency-key hits. The `/api/mobile/v1/agent/spawn` endpoint mints the id and returns it (`clients/spawn.go:65,287-306`). There is no per-run wall-clock deadline, goroutine cap, or memory limit on the Drive loop. The kill-switch (`policy.enabled`) is an eventually-consistent GitOps→Flux→ConfigMap-poll flow (`handlers_policy.go:47-110`, `config.go:91-104`) that blocks new ticks but cannot abort an in-flight spawn.

## 3. Riskiest assumption + kill-test (slice-1 gated)

**Load-bearing assumption (two tiers, scoped):**

> **Tier 1 (interpreter):** A capability-confined **Starlark-Go** workflow script's world-touching effects are all routed through host-provided builtins (whitelist-by-construction predeclared environment), and the script can be **deterministically replayed from an append-only step log keyed by a structured, drift-tolerant step key** — recorded effect-calls return cached results without re-executing, only the first un-recorded call runs live — such that **agent spawn is exactly-once across an operator-process crash on the deployed target substrate (k8s)**.
>
> **Tier 2 (spawned agent):** The agent that `agent()` dispatches runs with bypassed permissions (`claude -p ... --dangerously-skip-permissions` / `codex exec --dangerously-bypass-approvals-and-sandbox` / `gemini --yolo`, `internal/hud/spawn.go:1176,1232,1236`) and has unmediated filesystem/network/clock access inside its pod and the NFS-shared worktree. Tier 2 is **NOT** host-effect-mediated — it is contained by pod resource limits + downstream git-diff review + CI gates. The auto-merge safety boundary is the post-spawn diff/gate/CI review, not in-flight effect mediation of the agent.

*(This two-tier scoping is mandatory: the verdict on the confinement claim refuted the unqualified "ALL side-effects... no escape (filesystem, network)" phrasing — it is false for Tier 2 by `internal/hud/spawn.go:1176/1232/1236`. The claim holds only for Tier 1 + the decision-to-dispatch gate.)*

**The engine is Starlark-Go, not goja.** Determinism is a language guarantee (no clock/random, insertion-ordered iteration, whitelist-by-construction confinement, per `github.com/google/starlark-go` spec). goja is **dropped from the auto-merge path entirely** — the deterministic-replay claim is true for Starlark and false-by-default for goja (determinism is four standing obligations: `SetRandSource`, `SetTimeSource`, no Go-map iteration, strip globals; Go-map iteration order silently leaks into control flow). Any future goja adoption is a **separate riskiest-assumption requiring its own kill-test**. (See `131-research` §1.)

**Kill test (≤30 min, must run on the DEPLOYED operator against the real k8s substrate — an in-process test is necessary but NOT sufficient):**

A spike workflow with effects A=`agent()`, G=`gate()`, B=`agent()`, plus a parallel() of two children and a `loop_until_dry()`. A global atomic effect-counter increments only on **live** effect execution. Procedure and pass/fail:

1. **Short-circuit + exactly-once (in-process gate):** Run to completion past A, G. Persist the journal. Drop the interpreter + counter. Re-run the same pinned script. PASS: A and G return cached results (counter does **not** increment); only B + parallel run live; counter increments by exactly the un-recorded count.
2. **Drift tolerance (the hard case the verdict requires):** Replay with a `loop_until_dry` that runs a **different iteration count** because an input changed. PASS: structured step keys (loop-label/iteration/branch-scoped + arg_hash) survive — no sibling-call key shift, no wrong-result consumption.
3. **Concurrent key assignment:** Replay an N-way `parallel()` under artificial goroutine-scheduling jitter. PASS: branch effects replay in canonical key order regardless of which goroutine finishes first; step-key assignment across concurrent branches is deterministic (not a flat shared counter).
4. **Non-determinism tripwire:** Inject a mutated `call_hash` on a recorded step. PASS: the run **quarantines + escalates** (freeze, alert, await operator decision) — never silent-continue, never silent mass-abort.
5. **Interpreter-version drift:** Bump `go.starlark.net` (or change an `EffectHost` builtin signature) between the original run and replay. PASS: replay **refuses** and escalates (the run pins interpreter/builtin-ABI version, not just template version).
6. **Hostile council params:** Resolve a template with `fan_out_width` and `max_iter` set absurdly high. PASS: the **host resource ceiling clamps** them (fan-out ≤ host constant, loop ≤ host max) regardless of council input.
7. **DEPLOYED exactly-once (the real de-risk, Slice 1c):** On the operator, kill the spawn pod and then the operator process mid-B. PASS: resume re-attaches by idempotency key (k8s Job/pod name = key → duplicate create is `AlreadyExists` no-op); the global side-effect counter stays exact across the operator-process restart; no second pod, no double-merge.
8. **Non-Claude / non-streaming fidelity:** Exercise at least Codex and Gemini, and a harvester-vm (buffered-exec) spawn. PASS: cost provenance is recorded as `real`/`estimated`/`unavailable` (not silently 0); an empty-telemetry buffered run with `exit != 0` is recorded as **failure**, never cached as authoritative success.

**Failure mode if the assumption is wrong:** We would build a positional-replay runtime that silently consumes the wrong cached result (corrupting an autonomous merge) or mass-aborts every in-flight run on benign control-flow drift or an operator upgrade. If the kill-test fails, fall back to the materialized-DAG resume model (option (b), §7): keep Layers 1–2 (~70% reuse) and drive the runtime declaratively, conceding drivers 2/4.

**Status:** **Tier-1 in-process: PASSED 2026-06-06.** The S1 spike (`feat/mills-workflow-killtest-spike`, commit `0fe0e8d3`, standalone module `pkg/mills/workflow/spike/`) proves checks **1–6 + capability confinement** — all 7 in-process scenarios green under `-race`, independently re-run by an adversarial verifier (`honest: true`) with falsification probes (a panicking live-fn confirms replay never re-executes; a positive-control confirms the confinement check is non-vacuous). Verified: `go.starlark.net v0.0.0-20260522144826-ec58d4b459e2`, `ExecFileOptions` + `FileOptions{While:true, TopLevelControl:true}`, `thread.Load=nil`; step_key = scope-path (`root` / `loop:<label>#<iter>` / `par:<site>/b<branch>`) + leaf `<primKind>~<callHash[:16]>#<dupOrdinal>`. **Checks 7 (DEPLOYED exactly-once / Slice 1c) and 8 (non-Claude / non-streaming fidelity on real substrates) NOT run** — these remain the gate for Layer 3 (§Plan S6).

Findings to fold into the real engine (from the spike + verifier):
1. **The real test suite MUST add an explicit sibling-INSERT and sibling-DELETE drift test** in the same scope frame. The spike's scenarios catch a *fully-global* flat counter, but a *scope-preserving flat-leaf* counter would still pass them; only an insert/delete-then-replay test (asserting zero sibling key shift) fully locks down drift-tolerance.
2. **The durable (cross-process) journal is unproven.** Only the in-memory map was exercised; the SQLite path (`UNIQUE(run_id, step_key)` + `ON CONFLICT` idempotent `pending→success` upsert) needs its own follow-up spike before the real engine (§Plan S3).
3. **`call_hash` needs a proper recursive arg canonicalizer.** The spike collapses non-scalar Starlark values (list/dict/function) to `.String()`; effects taking structured args require real canonicalization (§Plan S6).
4. **Concurrency model confirmed:** fresh `*starlark.Thread` + forked ScopeStack per `parallel()` branch, branch index assigned pre-launch in slice order; journal (mutex) + effect-counter (atomic) the only shared mutable state. `thread.SetLocal` must not be called concurrently — hence forked stacks.

## 4. Architecture: three layers

Dependencies point inward. Layer 3 depends on Layer 2's `StepJournal` interface and Layer 1's `WorkerRunner` interface; neither lower layer imports the interpreter. The capability-confinement boundary **is** the Starlark predeclared environment.

### Layer 1 — Worker contract (harness-neutral)

Promote the existing `pipeline.SpawnRequest`/`SpawnResponse` (`dispatcher.go:139-183`, all-neutral) into a named `WorkerRunner` port so the runtime never imports `spawn.go` or the HUD mobile API directly. `AgentType` becomes a **required, validated** first-class field (today inferred from the overloaded `Model` string via `agentTypeOrDefault`, `clients/spawn.go:389-396` — a fragile interim). Reject/normalize unknown `AgentType` at the dispatcher boundary.

```go
package worker

type WorkerRequest struct {
    AgentType       string            // REQUIRED, validated: claude-code|codex|gemini
    Prompt          string            // reuse SpawnRequest.Prompt
    Model           string            // reuse
    WorkingDir      string            // reuse
    Env             map[string]string // reuse
    BudgetUSD       float64           // reuse
    BudgetTurns     int               // reuse
    BudgetMinutes   int               // reuse
    ParentSessionID string            // reuse
    BacklogID       string            // reuse
    Project         string            // reuse
    Branch          string            // reuse
    BaseBranch      string            // reuse
    Namespace       string            // reuse
    Substrate       string            // reuse (k8s|harvester-vm)
    IdempotencyKey  string            // NEW — deterministic, client-generated; substrate derives pod/Job name from it
}

type WorkerResult struct {
    SpawnID        string          // reuse SpawnResponse.SpawnID
    CostUSD        float64         // reuse
    CostSource     CostSource      // NEW — real|estimated|unavailable (provenance, MUST survive operator boundary)
    LogTail        string          // reuse
    FilesChanged   []string        // reuse (operator-side git diff; truly uniform)
    LinesAdded     int             // reuse
    LinesRemoved   int             // reuse
    DiffPatch      []byte          // reuse
    CommitMessages []string        // reuse
    Artifacts      map[string]any  // reuse
    Telemetry      *SpawnTelemetry // NEW — full snapshot incl. CostSource, TurnCount semantics
}

type CostSource int // real, estimated, unavailable

// The ONLY door from the runtime to a harness.
type WorkerRunner interface {
    Run(ctx context.Context, req WorkerRequest) (WorkerResult, error)
}

// Re-attach to an accepted spawn after restart, by idempotency key.
type WorkerResumer interface {
    Resume(ctx context.Context, idempotencyKey string) (WorkerResult, error)
}
```

A thin `spawnClientAdapter` maps `WorkerRequest ⇄ pipeline.SpawnRequest` and `WorkerResult ⇄ SpawnResponse` so existing wiring is byte-identical; `pipeline.SpawnWorker` becomes a `WorkerRunner` adapter.

### Layer 2 — Step/event protocol + durable append-only log

`workflow_steps` is the **sole write path for side-effects** and the source of truth for resume. The existing `events` table stays advisory-audit-only (`dao_events.go:19-42`, fire-and-forget); a documented invariant declares **`workflow_steps` wins on any conflict**.

**Schema (migration `004_workflow_steps.sql`, goose-flavored, forward-only — safe per the append-only precedent of `002_v2.sql` ALTER TABLE and `003_research_diff.sql`):**

```sql
CREATE TABLE workflow_runs (
  id                  TEXT PRIMARY KEY,
  backlog_id          TEXT REFERENCES backlog_items(id),
  engine              TEXT NOT NULL,            -- IMMUTABLE discriminator: dag|imperative (NOT the mutable Template)
  template            TEXT NOT NULL,
  template_version    TEXT NOT NULL,            -- content hash; replay aborts if registry drifts
  interpreter_version TEXT NOT NULL,            -- pinned go.starlark.net + builtin-ABI version
  workflow_params     TEXT,                     -- JSON: fan_out_width, gate_strictness, harness (clamped at resolve)
  state               TEXT NOT NULL,            -- running|paused|done|escalated|error|quarantined
  paused_at           TEXT,
  resumed_at          TEXT,
  started_at          TEXT,
  ended_at            TEXT,
  cost_usd            REAL DEFAULT 0,
  parent_session_id   TEXT
);

CREATE TABLE workflow_steps (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  run_id          TEXT NOT NULL REFERENCES pipeline_runs(id),
  step_key        TEXT NOT NULL,                -- STRUCTURED drift-tolerant key (NOT a flat counter); see below
  event_type      TEXT NOT NULL,               -- enum below
  call_hash       TEXT NOT NULL,               -- SHA256(prim-name + canonical-sorted-key-args); mismatch => quarantine
  idempotency_key TEXT,                         -- set for spawn_requested; substrate derives pod/Job name
  status          TEXT NOT NULL,               -- pending|success|error|gate_fail|skipped
  spawn_id        TEXT,                         -- re-attach handle (same role as stage_results.spawn_id)
  started_at      TEXT,
  ended_at        TEXT,
  result_blob     TEXT,                         -- JSON cached value returned on replay
  cost_usd        REAL DEFAULT 0,
  cost_source     TEXT,                         -- real|estimated|unavailable (mirrors WorkerResult)
  effect_count    INTEGER DEFAULT 0,           -- kill-test counter: live execution increments, replay does NOT
  UNIQUE(run_id, step_key)
);
CREATE INDEX idx_workflow_pending ON workflow_steps(run_id) WHERE status='pending';
CREATE INDEX idx_workflow_replay  ON workflow_steps(run_id, step_key);
```

**Step key — structured, NOT a flat positional counter.** *(Verdict mustChange: a flat `call_index`/`NextCallIndex` is the single biggest correctness risk — it shifts every sibling key when control flow drifts, causing silent wrong-result consumption or availability-destroying mass-abort.)* The key is a deterministic hierarchical path mirroring the proven name-keyed `stage_results` scheme: `(scope_path, primitive_kind, arg_hash)` where `scope_path` encodes loop label + iteration index + parallel branch key. Inserting/removing an unrelated call does not shift sibling keys. Branch keys are computed deterministically **before** branches run; concurrent branches never race on a shared counter.

**`call_hash` canonicalization is a hard, tested invariant:** sorted-key JSON, stable field order, version-tagged. A test proves two structurally-distinct effects never collide and the same effect hashes identically across two builds.

**Event enum:** `spawn_requested`, `spawn_result`, `spawn_resumed`, `gate_eval`, `budget_reserved`, `budget_debit`, `tool_call`, `ctx_now`, `ctx_uuid`, `parallel_branch`, `parallel_join`, `loop_iter`, `step.cache_hit`, `step.budget_exhausted`, `step.paused`, `step.resumed`, `step.nondeterminism_quarantine`, `workflow_done`.

### Layer 3 — Capability-confined imperative runtime (gated on kill-test)

`WorkflowEngine` implements `pipeline.Worker` and registers via `Dispatcher.Register` (`dispatcher.go:33-95`). It embeds Starlark-Go whose **only** predeclared builtins are host-mediated effect primitives: `agent()`, `gate()`, `tool()`, `parallel()`, `pipeline()`, `budget_debit()`, `ctx_now()`, `ctx_uuid()`, `loop_until_dry()`. No fs/net/clock/random exist in the universe.

**Uniform primitive flow (record-before-result):**

1. `key := scopedStepKey(scope, primName, canonicalArgs)` — deterministic, drift-tolerant.
2. `hash := callHash(primName, canonicalArgs)`.
3. `prior := journal.Get(run_id, key)`.
4. **Replay short-circuit:** if `prior != nil`: if `prior.call_hash != hash` → **quarantine + escalate** (never silent); else return decoded `result_blob`, `effect_count` **not** incremented.
5. **Live path:** append `{status:pending, hash, idempotency_key}` (recorded **before** dispatch) → run interception stack → on success append `{status:success, result_blob, effect_count:+1}` → return.

`ctx_now()`/`ctx_uuid()` are recorded with the same record-before-return + idempotency rigor as spawn, since their values flow into control flow and a crash-in-window re-derivation would diverge replay. *(Verdict mustChange.)*

**Capability confinement + interception stack** (every effect passes through before touching the world): (a) policy gate (`PolicyManager.Current`, matching `reconciler.AutonomyGate`); (b) **pre-flight budget reservation** against a per-run ledger (recorded `budget_reserved` step) — a spawn cannot start if remaining budget cannot cover its declared `BudgetUSD`; the 5s `runBudgetWatcher` (`spawn.go:1787`) stays as a secondary kill, never the primary. For Codex `estimated` cost, apply a conservative multiplier; **when `CostSource != real` (Gemini = `unavailable`/hard-zero, `spawn_gemini_parser.go:239`), budget is enforced by `MaxTurns`/`MaxMinutes`, and `MaxCostUSD` is advisory-only.** *(Verdict mustChange: cross-harness cost is not comparable; the worker-contract claim is reframed to "uniform envelope + uniform diff/success, harness-specific telemetry fidelity.")* (c) the gate registry for `gate()`.

**Host-level resource envelope (independent of council params, enforced before §6 selection can lower them):** per-run wall-clock deadline (`context.WithTimeout` on the Drive context), a max-concurrent-spawn semaphore (host constant, e.g. 16, regardless of `fan_out_width`), a max-total-spawns-per-run counter, a host max-loop-iteration ceiling overriding any council `max_iter`, and an operator memory limit. **Council params may only LOWER these, never raise them.** *(Verdict mustChange: a hostile/buggy council param set is in-scope and currently unguarded.)*

**100%-interception enforced mechanically, not asserted:** a test-time fake `EffectHost` fails on any un-recorded IO, plus a `go vet`-style import ban on `net`/`os`/`time` inside `pkg/mills/workflow/interp` except via `EffectHost`. *(Verdict mustChange.)*

**Exactly-once agent spawn:** the `IdempotencyKey` is a deterministic hash of `(run_id, step_key, call_hash)` generated **inside** a recorded step and persisted on the `spawn_requested` row **before** dispatch. The substrate derives the k8s Job/pod name from it; a replay-induced duplicate create is an `AlreadyExists` no-op. Re-attach reuses `SpawnResumeClient.Resume` + `withResumeSpawnID` keyed off `workflow_steps.spawn_id`. **This requires moving id-generation client-side** (today server-minted via `crypto/rand`, `controller.go:497`) and is delivered by a dedicated prerequisite slice (§Plan S2b), not a one-field add. The imperative runtime is **gated to `substrate==k8s`** until exactly-once dedupe is proven on harvester-vm via a deployed kill-test.

**Merge idempotency:** `merge()` records `merge_done(run_id, MR_iid)` and pre-checks MR merged-state before the PUT; "already merged" is success on replay. Added at `GitLabClient.Merge` (`gitlab.go:271-284`), which today has neither.

**Parallel ordering:** one Starlark thread per goroutine, frozen shared values; each branch tagged `parallel_branch` with a canonical branch key; the journal merges branch effects in **key order**, not finish order.

## 5. Harness-agnostic contract: uniform vs normalized

**Truly uniform across claude-code/codex/gemini** (verified):

- **Request envelope** — every `WorkerRequest` field is neutral (`dispatcher.go:139-169`).
- **Diff / files / commits** — reconstructed operator-side by `git diff baseBranch...origin/branch` (`clients/spawn.go:481-506`); parser-independent, substrate-opaque.
- **Success/fail** — exit-code / terminal-state driven.
- **Telemetry struct shape** — `SpawnEventSink` fans divergent JSONL into one accumulator (`spawn_parser.go:14-60`).

**Normalized / harness-specific FIDELITY (the central caveat, elevated from footnote):**

- **Cost** — Claude reports real SDK cost (`spawn_claude_parser.go:322`); Codex is **estimated** from a pricing table (`spawn_codex_parser.go:159-172`); Gemini is **hard-zero/unavailable** — `SetResult(0, 0, ...)` with no pricing entry (`spawn_gemini_parser.go:237-239`). The new `CostSource` enum makes provenance first-class and **must survive `hudSpawnTelemetry`** (today a lossy subset that drops the estimated-cost marker, `clients/spawn.go:173-179`).
- **Turn count** — Claude from SDK `num_turns`; Codex per `turn.started`; Gemini counts non-delta messages but its terminal `result` self-inconsistently zeroes turns (`handleResult:239`). The contract defines `TurnCount` as "agent-reported terminal turns" and each parser populates it consistently at the terminal event.
- **Multi-turn** — claude-only today; `UseSDKDriver`/`MultiTurn` are silently ignored on Codex/Gemini. Imperative workflows using `loop_until_dry` that rely on multi-turn are **restricted to Claude** until SDK drivers ship for Codex/Gemini, OR the template's harness selection is capability-checked.
- **Buffered (harvester-vm) telemetry** — best-effort post-hoc `StdoutTail` parsing (`spawn.go:879-886`); an empty-telemetry run with `exit != 0` is recorded as **failure**, never cached as authoritative success in `result_blob`.

The contract declares per-harness capability flags (`supports_real_cost`, `supports_multi_turn`, `supports_streaming`) so the §6 template selector can refuse or down-rank a harness lacking a needed capability instead of silently degrading.

## 6. Per-task adaptivity

The council **selects + parameterizes** a pre-validated workflow template from a **closed registry**; it never emits raw control flow. *(council-policy-adaptivity map.)* Hooks, in priority order:

1. `BacklogProposal.WorkflowHint` (advisory) — `pkg/mills/council/backlog_mutator.go:17-42`.
2. Squad `ManifestSpec.WorkflowTemplate` — `pkg/mills/squads/types.go:52-85`.
3. `Policy.PerLabelWorkflowTemplates` (label fallback) — `pkg/mills/policy.go:141-166`.
4. `policy.Pipeline.DefaultTemplate` (final fallback).

Resolution runs in `reconciler.tryStart` **after** squad routing, **before** `PipelineRun.Put` (`reconciler.go:634`). It writes the immutable `engine` discriminator, the frozen `template_version`, the pinned `interpreter_version`, and the **clamped** `workflow_params` to the `workflow_runs` row. The runtime reads params from the row on resume — **never re-resolves** (re-resolution would change the call sequence and break replay).

**Numeric clamping is a hard requirement** at `ResolveWorkflowTemplate`: `fan_out_width ∈ [1, HOST_MAX]`, `max_iter ∈ [1, HOST_MAX]`, treat council params as untrusted LLM-generated input, fuzz-test the clamp. A closed template enum is **insufficient** because params are free numeric inputs. *(Verdict mustChange.)*

**Registry version immutability:** template versions are content-hashed and ref-counted — retained as long as any run references them. An edit creates a **new** version; in-flight runs keep their pinned content. Startup fails fast if any in-flight `run_id`'s pinned `template_version` no longer resolves. *(Verdict mustChange.)*

## 7. agentcontext reuse decision

**Decision: BUILD NEW** in `pkg/mills/workflow`; **REUSE agentcontext only as a workflow-definition/UI reference**, never as the execution engine. This is the verdict from the reuse-vs-build adversarial review, which attempted to refute BUILD by steelmanning REUSE and failed: agentcontext is the wrong **shape**, not merely under-featured.

Decisive criteria (each maps to a finding; the agentcontext `WorkflowEngine` fails all):

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| C1 | Append-only per-call journal, idempotent retrieval | FAIL | Qdrant snapshot, one point/workflow; events not persisted (`workflow_persist.go:43-54,124`) |
| C2 | Per-call idempotent replay by key | FAIL | No per-call logging; Running-crash deadlocks (`workflow_executor.go:161-173`) |
| C3 | Harness-neutral dispatch | FAIL | `ToolExecutor` is MCP-only (`workflow_engine.go:34-35`); Mills uses `SpawnClient(agentType)` |
| C4 | Storage-scoped lifecycle | FAIL | Goroutine-scoped; subflow waiters hang on restart (`workflow_persist.go:202-217`) |
| C5 | Budget/policy/gate first-class | FAIL | No budget tracking; gates are condition-expressions only |

**Decisive caveat — BUILD ≠ from-scratch.** The build reuses **Mills' own** proven primitives, not a new persistence engine: `stage_results` `UNIQUE`-upsert idempotency (`dao_pipeline.go:487-548`) and resume machinery (`resumeIndex`/`seedAttempts`/`pendingStage`/`withResumeSpawnID`, `runner.go:149-174,305-312,439-590`), generalized to `workflow_steps` as migration 004 (002 already proved append-only ALTER TABLE is safe). The faster reuse is of Mills internals — which the plan does. The only portable assets from agentcontext are **conceptual** (step-type taxonomy, approval-gate UX).

**The dismissal is CONDITIONAL** *(verdict mustChange)*: it holds only while the imperative-replay target is committed AND the S1 kill-test passes. If the kill-test fails or the org accepts the declarative-DAG fallback (option (b)), agentcontext's DAG executor becomes a legitimate ~70% reuse candidate for that fallback and the BUILD verdict must be revisited. **Control experiment:** run the S1 recorded-effect-counter kill-test against agentcontext **unmodified** and record the observed failure (no per-call row, stale snapshot, Running-crash deadlock) as falsifiable evidence.

**Coexistence ADR:** `pkg/mills/workflow` is the durable autonomous-merge runtime; `pkg/agentcontext` stays the interactive MCP-driven DAG; they share no execution code; only step-type vocabulary is borrowed. Prevents mis-wiring two engines in one binary.

## 8. Operational safety

- **Exactly-once agent spawn on replay** — deterministic client-generated `IdempotencyKey`, recorded before dispatch, substrate dedupes on pod/Job name. **k8s-only** until harvester-vm dedupe is proven by a deployed kill-test. Merge idempotent on `(run_id, MR_iid)`.
- **Pre-existing double-spawn window fixed** — today `startSpawn` (pod created, fresh random id) runs **before** `OnAccepted(spawnID)` persists the id (`clients/spawn.go:220-228`); a crash in that window loses the re-attach handle and re-spawns on resume. The record-before-dispatch invariant closes it — but only with the client-side key (S2b).
- **Live-store migration** — migration 004 is forward-only, idempotent, transactional per migration (`migrate.go:30-59`); safe on the live operator per the 003 precedent.
- **Dual source-of-truth resolved** — legacy `dag`-engine runs keep `stage_results` as sole truth and do **not** write `workflow_steps`; `imperative`-engine runs use `workflow_steps` exclusively. Branch on the immutable `engine` discriminator in Drive. Single record-before-result ordering invariant; `workflow_steps` wins on conflict. A test crashes between `PutRun`/`PutStage`/`PutWorkflowStep` (separate non-transactional `ExecContext`, `dao_pipeline.go:48,526`) and asserts correct reconciliation. *(Verdict mustChange.)*
- **Dual-run vs DefaultStages** — the hardcoded DAG remains default; in-flight runs resume under the **engine they started with** (immutable discriminator, never the mutable `Template`); §6 template-selection never re-routes started runs.
- **Feature-flagging** — `policy.workflows.enabled` (separate from the global kill-switch), default OFF, gated to `substrate==k8s`.
- **Kill-switch** — global `policy.enabled` blocks **new** ticks only (eventually-consistent GitOps→Flux→ConfigMap-poll, `config.go:91-104`); it **cannot** abort an in-flight spawn. A run-level `paused_at` between-step check is the fast stop. If true emergency stop of a running merge is required, an in-process exec-context cancel endpoint is needed (flagged in Open Questions). *(Verdict mustChange: state the latency truth, scope the claim.)*
- **Rollback** — each layer is independently revertible; `workflow.enabled=false` returns to pure-DAG behavior with no schema rollback needed (forward-only tables are inert when unused).

## 9. Observability

- **HUD step timeline / replay** — `GET /api/mills/workflow/runs` (list), `GET /api/mills/workflow/runs/{id}` (run + steps + events nested, per `handlers_pipeline.go:103-126`), `POST .../pause` + `.../resume`. Step timeline shows cache-hit vs live badges, cost with provenance, quarantine state.
- **`WorkflowMonitor`** — mirrors `MillsMonitor` (`internal/hud/monitor/mills.go:35-95`), broadcasts `hud.workflows` SSE every 15s.
- **Metrics** — `KPIWriter.snapshot()` (`kpi_writer.go:80-142`) gains `workflow_active_runs`, `workflow_completed_steps`, `workflow_failed_steps`, `workflow_avg_cost_per_step_usd`, `workflow_quarantined_runs`. Cost rollups must branch on `CostSource` so dashboards never sum real + estimated + zero as if comparable.
- **Frontend** — workflow run detail view; `dist/` rebuilt + committed (`go:embed` gotcha — CI does not rebuild it).

## 10. Out of scope / non-goals

- Replacing the `DefaultStages` DAG for the ~90% fixed-pipeline majority. The imperative runtime is opt-in for the imperative minority only.
- goja / JavaScript authoring on the auto-merge path (separate future kill-test).
- Interpreter-state (VM continuation) serialization — by design we replay effects from the log, never checkpoint the VM.
- harvester-vm imperative workflows until exactly-once dedupe is proven there.
- Step-level pause mid-primitive (only between-step pause in v1).
- Multi-turn imperative workflows on Codex/Gemini until SDK drivers ship.

## 11. Open questions

1. Does true emergency-stop of a running merge require a new in-process exec-context-cancel operator endpoint, or is between-step pause + the diff/CI safety boundary sufficient?
2. What are the exact host ceilings (`HOST_MAX` fan-out, max-loop-iter, per-run wall-clock, operator memory)? Needs a load test on the operator.
3. Should `result_blob` cap at a size (e.g. 32 KiB like `DiffPatch`) and offload larger payloads, to keep `ListPending` scans fast under the single-writer WAL?
4. Does harvester-vm have any name-based create dedupe primitive, or is exactly-once structurally impossible there (forcing k8s-only permanently)?
5. Conservative Codex cost multiplier value — what factor errs safely toward under-spending without over-throttling?
