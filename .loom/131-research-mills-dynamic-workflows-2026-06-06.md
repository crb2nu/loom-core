# Research: Embedded interpreter + durable-execution substrate for Mills dynamic workflows

**Date**: 2026-06-06
**For**: `132-product-spec-mills-dynamic-workflows` / `133-implementation-plan-mills-dynamic-workflows`
**Lineage**: `130-brainstorm-mills-harness-agnostic-workflows` (converged on F-H + F-B + F-D)
**Method**: 15-agent workflow (`wf_0e37143a-01a`) — 5 codebase maps (file:line evidence) + 2 external research dossiers + 2 competing designs + 5 adversarial verdicts. All claims below are either workspace file references or cited URLs; assumptions are marked **[ASSUMPTION]**.

---

## 1. Decision: interpreter = Starlark-Go (not goja)

**Recommendation: adopt `go.starlark.net` (google/starlark-go).** goja is the JS-ergonomics runner-up but is **dropped from the auto-merge path** and would require its own riskiest-assumption kill-test if ever revisited.

The discriminator for a deterministically-replayable, capability-confined runtime is whether the **language core** can inject nondeterminism. Starlark cannot; the others can.

| Engine | Determinism | Confinement | Verdict |
|---|---|---|---|
| **Starlark-Go** | **Language guarantee** — "no sources of random numbers, clocks, or unspecified iterators"; dict iteration insertion-ordered | **Whitelist-by-construction** — host supplies the predeclared universe; fs/net/clock/random never exist to deny | **CHOSEN** |
| goja (JS) | Opt-in plumbing: `SetRandSource`+`SetTimeSource`, avoid Go-map iteration, strip globals — 4 standing obligations, silently forgettable | Blacklist — broad ES5.1 global surface, "no documented whitelist-only model" | Runner-up; already an indirect dep, but determinism is discipline not guarantee |
| risor | No language guarantee; stdlib wraps `rand`/`time`/`os` | Secure-empty default (good) but convenience modules re-introduce nondeterminism | Rejected — younger, single-vendor, recently re-homed |
| expr-lang | Deterministic but **single-expression only — no statements/loops** | n/a | Rejected as host; OK for leaf gate conditions only |
| gopher-lua | `math.random`/`os.time`/`os.clock` leak; PRNG | **Blacklist sandbox with documented escape vectors** (string metatables, `load`, `debug`, `string.rep` memory exhaustion) | Rejected — weakest confinement for a security-critical operator |

**Key facts (cited):**
- Determinism is spec-level: *"Starlark execution is deterministic: … there are no sources of random numbers, clocks, or unspecified iterators."* — https://github.com/google/starlark-go/blob/master/doc/spec.md
- Confinement is whitelist: the host supplies a `StringDict` predeclared environment; all added values must be immutable; "Starlark programs cannot modify the dictionary." — same spec.
- `FREEZE` makes module/global values immutable-and-race-free across threads — clean substrate for `parallel()`/`pipeline()`.
- **No engine can serialize/resume interpreter state across a process restart** (goja states async/await is blocked precisely because it cannot "save and restore the execution context" — https://github.com/dop251/goja/issues/460). This is *by design acceptable here*: we replay effects from the log, we never checkpoint the VM. So the universal suspend/resume gap does **not** disqualify any engine — confinement + core determinism become the deciders, where Starlark dominates.

**Standing obligation that survives the choice (host-side, not the engine's job):** goroutine scheduling is nondeterministic in *every* engine, so the wall-clock interleaving of side-effecting host calls inside `parallel()` is not replayable on its own. **A canonical ordering of parallel-branch effects must be imposed at the event-log layer** (deterministic keyed merge, not finish-order). Starlark keeps the language core deterministic; ordering of concurrent effects is ours to enforce.

**[ASSUMPTION]** Authoring in a Python-dialect (Starlark) rather than JS is an acceptable DX cost for internal workflow templates. Revisit only if external/community authoring becomes a goal (then re-open goja with its own kill-test).

---

## 2. Decision: durability = memoized-step-output (DBOS/Restate model), honoring the full-replay determinism contract

Three industry models exist; we adopt the one that is simplest to embed single-process while preserving imperative power.

| Model | Mechanism | Fit |
|---|---|---|
| **Memoized-step-output** (DBOS / Restate `ctx.run`) | Re-run script top-down; each effect looks up its recorded output by `(run_id, step_key, arg_hash)` and short-circuits if present | **CHOSEN** — architecturally identical to the brainstorm's SQLite `workflow_steps` table; embeds in-process against the existing DB; no server/task-queue/sharding |
| Full-history replay (Temporal / Cadence / Azure Durable Functions) | Positional command-stream match against append-only history; mismatch → loud `NonDeterministicOrchestrationException` | We **borrow its determinism CONTRACT** (below) and its loud-mismatch tripwire, but not its positional matching or its server |
| Materialized state-machine (Conductor / Step Functions) | Declarative DAG; decider re-drives from stored state; no author code on replay path | This is the **fallback** (option b) if the kill-test fails — concedes drivers 2/4, keeps ~70% of Layers 1–2 |

**Why memoized-step-output over Temporal-style positional replay:** matching by an explicit, structured `step_key` (not positional command order) is more forgiving of pure-compute reordering between effects, and it maps 1:1 onto a SQLite table Mills can own in-process. Temporal's server, task queues, sharding, and quorum replication are scale-only additions to omit. (DBOS confirms single-process durable execution is a real shipping pattern: in-process library + existing DB.) — https://www.dbos.dev/blog/why-postgres-durable-execution , https://www.restate.dev/blog/building-a-modern-durable-execution-engine-from-first-principles

### 2.1 The determinism contract workflow scripts MUST obey

Each maps to a documented nondeterminism source (Temporal/Azure/Vanlightly/Resonate). The interpreter enforces these by *construction* (Starlark) plus host guards:

- **No wall-clock time** in control flow → inject a recorded `ctx_now()` (value logged on first call, replayed thereafter). cf. Temporal `workflow.Now`, Azure `CurrentUtcDateTime`.
- **No randomness/UUIDs** → recorded `ctx_uuid()` / fixed-seed RNG seeded from the log. cf. Azure `context.NewGuid` (Type-5), DBOS records RNG output as a step.
- **No direct IO / network / fs / MCP calls** from control-flow code → every world-touching call is an append-only effect step.
- **No env-var or external mutable-state reads in branching** → pass as run input or recorded step (a DB that changed between retries makes replay branch differently — Vanlightly).
- **No unordered iteration** → iterate sorted keys / ordered lists only.
- **No nondeterministic concurrency** → single logical thread; parallelism only via a recorded `parallel()` that fixes child order in the log.
- **No blocking sleep** → recorded durable timer.
- **No mutable global/static state across calls** → only locals + log-derived state.
- **Pin workflow-code version per run** → mid-flight edits that add/remove/reorder effect calls break replay; gate behind version pinning + drain.

Sources: https://docs.temporal.io/workflow-definition , https://docs.temporal.io/encyclopedia/event-history/event-history-go , https://learn.microsoft.com/en-us/azure/azure-functions/durable/durable-functions-code-constraints , https://jack-vanlightly.com/blog/2025/11/24/demystifying-determinism-in-durable-execution , https://journal.resonatehq.io/p/from-where-do-deterministic-constraints

### 2.2 Exactly-once agent-spawn / merge on replay (auto-merge-critical)

An agent spawn and a git merge are **non-transactional external effects** → at-most-once-per-attempt **+ an idempotency key**, not free exactly-once:

1. Generate the idempotency key **inside a recorded step** (deterministic hash of `(run_id, step_key, call_hash)`) so it is stable across every replay.
2. Record `spawn_requested(key)` **before** dispatch. On replay, if logged, return the recorded `spawn_result` without dispatching a second pod.
3. The substrate **deduplicates on the key** — k8s Job/pod name derived from it → duplicate create is `AlreadyExists` no-op (mirrors DBOS `INSERT … ON CONFLICT DO NOTHING`, Restate idempotency-key dispatch).
4. Make GitLab merge idempotent on `(run_id, MR_iid)` — pre-check "already merged", record `merge_done`.

Source: https://docs.aws.amazon.com/durable-execution/patterns/best-practices/idempotency/

**Verdict on the brainstorm open-question (replay-script vs materialized-DAG):** choose **replay-the-script** because it is the only option preserving driver-4 imperative power, and it is proven viable single-process by DBOS/Restate — **provided** capability confinement intercepts 100% of effects and the determinism contract is enforced. If the kill-test shows confinement leaks or the hazards can't be banned, fall back to the materialized-DAG (concedes drivers 2/4, keeps Layers 1–2).

---

## 3. Codebase grounding (maps, file:line verified)

### 3.1 Mills persistence + resume (store+migrations map)
- **Migrations are embedded goose-flavored forward-only SQL** with version tracking; new tables append as `00N_name.sql`, one transaction per migration. `002_v2.sql` already proved `ALTER TABLE` is safe on the live store. — `pkg/mills/store/migrate.go:16-30`, `migrations/002_v2.sql:121-129`, `003_research_diff.sql:16`
- **Resume already works via a materialized, name-keyed DAG.** `resumeIndex()` reads `current_stage`; `loadPriorOutputs()` rehydrates by stage **name**; `pendingStage()` detects in-flight spawns by `outcome=null` + `spawn_id`; `withResumeSpawnID`/`ResumeSpawnIDFromContext` re-attach after restart. Idempotency from `stage_results UNIQUE(pipeline_run_id, stage, attempt)` + `ON CONFLICT` upsert. — `pkg/mills/pipeline/runner.go:149-174,305-312,439-450,531-590`, `dao_pipeline.go:487-548`. **This is the baseline we preserve, not replace.**
- **Concurrency: SQLite WAL + `busy_timeout=5000`, one writer + many readers, `SetMaxOpenConns(8)`.** Writes to `pipeline_runs`/`stage_results` are **separate non-transactional `ExecContext` calls** — a crash between them is possible. — `store.go:54-75`, `runner.go:568-590`, `dao_pipeline.go:48,526`. *(Implication: a new `workflow_steps` write must define a record-before-result ordering invariant and a reconciliation test.)*
- The existing `events` table is **fire-and-forget audit** with no re-execution semantics — distinct from a durable step log. — `dao_events.go:19-42`.

### 3.2 Worker / harness seam (worker-contract map)
- **`SpawnRequest`/`SpawnResponse` are already harness-neutral.** — `pkg/mills/pipeline/dispatcher.go:139-183`
- **Diff/files/commits are reconstructed operator-side** via `git diff baseBranch...origin/branch` after terminal, capped (DiffPatch 32 KiB) — parser-independent, substrate-opaque, truly uniform. — `clients/spawn.go:464-506`
- **Telemetry diverges in FIDELITY, not shape.** Cost: Claude real (SDK), Codex **estimated** from a pricing table (`AddEstimatedCost`), Gemini **hard-zero/unavailable** (`SetResult(0,0,…)`). Multi-turn is **claude-only** today (`UseSDKDriver`/`MultiTurn` silently ignored on Codex/Gemini). `AgentType` is inferred from the overloaded `Model` string (`agentTypeOrDefault`). — `spawn_claude_parser.go:322`, `spawn_codex_parser.go:159-172`, `spawn_gemini_parser.go:237-239`, `spawn.go:751-790`, `clients/spawn.go:389-396`
- **No spawn idempotency-key infrastructure exists.** `spawnID` is server-minted via `crypto/rand`; pod name is `"spawn-"+spawnID`; 0 idempotency hits across the spawn code. `startSpawn` creates the pod **before** `OnAccepted` persists the id → a crash in that window already re-spawns on resume today. — `internal/spawn/controller.go:497-500`, `internal/hud/spawn.go:662`, `clients/spawn.go:220-228,287-306`
- **Buffered (harvester-vm) exec** parses only `StdoutTail` post-hoc; an empty-telemetry run with `exit!=0` must be recorded as **failure** (this was the A2 kill-test root cause). — `spawn.go:879-895,926-935`

### 3.3 agentcontext WorkflowEngine — REUSE rejected for this target (agentcontext-reuse map)
- **In-memory goroutine-driven declarative DAG**; state snapshots to **Qdrant only on explicit checkpoint calls from MCP handlers, never the executor loop**; events not persisted. A mid-execution crash loses step progress and **deadlocks Running steps** on reload. — `workflow_executor.go:14-71,161-173`, `workflow_persist.go:43-54,124,189`, `service_workflows.go:272,301,328`
- Dispatch is **MCP-`ToolExecutor`-only** (no `SpawnClient(agentType)`); lifecycle is **goroutine-scoped** (subflow waiters hang on restart). No budget/policy/gate as first-class.
- **Verdict: wrong SHAPE, not merely under-featured → BUILD new** in `pkg/mills/workflow`, reusing **Mills' own** proven primitives (`stage_results` upsert, `resumeIndex`/`pendingStage`/`withResumeSpawnID`). Dismissal is **conditional** on the kill-test passing; if it fails, agentcontext's DAG becomes a legitimate ~70% reuse candidate for the declarative fallback.

### 3.4 Adaptivity + operator/HUD seams (council-policy + operator-hud maps)
- Workflow selection slots into `reconciler.tryStart` **after squad routing, before `PipelineRun.Put`** (`reconciler.go:608-649`). Hooks: `BacklogProposal.WorkflowHint` → squad `ManifestSpec.WorkflowTemplate` → `Policy.PerLabelWorkflowTemplates` → `DefaultTemplate`. `policy.default_template` is the existing **dead placeholder**.
- Kill-switch (`policy.enabled`) is **eventually-consistent** GitOps→Flux→ConfigMap-poll — blocks new ticks, **cannot abort an in-flight spawn**. — `handlers_policy.go:47-110`, `config.go:91-104`
- HUD: KPI via `KPIWriter.snapshot()`; new metrics branch on `CostSource`. `dist/` is `go:embed`'d — **must be rebuilt + committed** (CI doesn't rebuild it).

---

## 4. What the adversarial pass changed (all 5 verdicts = holds-with-caveats)

1. **Determinism:** flat `call_index` → **structured drift-tolerant `step_key`** (loop/branch-scoped path + arg_hash); hash-mismatch → **quarantine+escalate** (never silent); pin **interpreter/builtin-ABI version** per run + drain-before-bump.
2. **Confinement:** scope to **two tiers** — Tier-1 interpreter (mediated, true) vs Tier-2 spawned agent (`--dangerously-skip-permissions` etc. — **not** mediated; contained by pod limits + diff/CI). Add a **host resource envelope** (wall-clock deadline, spawn semaphore, loop ceiling) that council params may only *lower*; enforce 100% interception **mechanically** (fake EffectHost + import ban on `net`/`os`/`time`).
3. **Harness uniformity:** reframe to **uniform envelope + diff/success, harness-specific telemetry fidelity**; add `CostSource` and make it survive the operator boundary; `MaxCostUSD` is advisory for zero-cost harnesses (use `MaxTurns`/`MaxMinutes`); kill-test must exercise a **non-Claude, non-streaming** harness.
4. **Reuse:** dismissal is **conditional**; add a control experiment (run the kill-test against agentcontext unmodified, capture the failure); coexistence ADR.
5. **Operational safety:** client-side idempotency key is a **dedicated prerequisite slice** (3 components); the real de-risk is a **deployed pod-crash kill-test**, not the in-process one; persist an **immutable `engine` discriminator** (not the mutable `Template`); merge idempotency at `GitLabClient.Merge`; state the kill-switch latency truth.

---

## 5. Sources

Interpreters: starlark-go spec & repo; dop251/goja + issue #460; expr-lang; deepnoodle-ai/risor; yuin/gopher-lua (+ issues #55/#59/#348, HackTricks Lua sandbox bypass). Durable execution: Temporal (workflow-definition, event-history-go, versioning); Azure Durable Functions code-constraints; DBOS; Restate; AWS durable-execution idempotency; Conductor; Vanlightly determinism; Resonate. Full URL list in §1–§2 above. Codebase: file:line references throughout §3.
