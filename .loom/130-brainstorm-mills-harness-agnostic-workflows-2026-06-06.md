# Brainstorm: Loom-native, harness-agnostic dynamic workflows for Mills

**Date**: 2026-06-06
**Triggered by**: "Let's build a loom native version of dynamic workflows for mills that is harness agnostic." The reference point is Anthropic's Claude-Code *dynamic workflows* (imperative `agent()`/`parallel()`/`pipeline()` JS that orchestrates subagents) — proprietary and Claude-only. The ask: a Loom-owned equivalent that any harness (claude-code/codex/gemini) can drive, wired into Mills.
**Constraints noted** (from clarifying answers): wants ALL four drivers — (1) harness portability, (2) per-task adaptive pipelines, (3) author/change workflows as data without recompiling the operator, (4) imperative power (runtime fan-out, loop-until-dry, budget-scaling). Build appetite: **ambitious — protocol + engine** (treat as foundational platform work).

## Grounding (verified against the codebase)

- **Mills today is a hardcoded Go state machine.** Stage sequence lives in `DefaultStages` (`pkg/mills/pipeline/runner.go:55-107`): `plan_slice → research → implement → post_implement_gate → tests → pr_self_review → merge → cleanup`. Driven by `Runner.Drive()` (`runner.go:255-436`). The sequence is **not** runtime-configurable; `policy.default_template` (`pkg/mills/policy.go:143`) exists but is a dead placeholder. A mirror YAML (`cmd/mcp-mentatlab/templates/mills-default-pipeline.yaml`) is documentation, not loaded.
- **A harness-agnostic DAG workflow engine already exists — and Mills does not use it.** `pkg/agentcontext/` has `WorkflowEngine` + `WorkflowDefinition`/`WorkflowStep` (`schema_workflow.go`, `workflow_engine.go`, `workflow_executor.go`): DAG with `depends_on`, step types `tool|approval|auto_verify|gate|parallel|subflow|map_reduce`, `${input.*}`/`${step.*}` interpolation, retries/backoff/timeouts/rollback, Qdrant persistence + checkpointing, YAML defs loaded from `.agents/workflows/*.yaml`. It is harness-agnostic **because it executes MCP tools via a `ToolExecutor` callback, not agent CLIs.** Built for short-lived tool sequences, not budget-governed days-long shipping.
- **MCP is the existing harness-agnostic seam.** Every spawned harness speaks the same MCP surface in-pod (`pkg/mills/clients/mcphub.go`). The pipeline's `SpawnClient`/`SpawnRequest`/`SpawnResponse` contract (`pkg/mills/pipeline/dispatcher.go:125-220`) already treats a spawn as a harness-neutral black box.
- **Harness-specifics live below the seam:** `buildAgentCommand` (`internal/hud/spawn.go:1152-1240`), per-harness JSONL parsers (`spawn_{claude,codex,gemini}_parser.go`), auth wiring, and multi-turn support (only claude-code via SDK driver today). These are the things a "harness-agnostic" claim must actually abstract.

**Reframed problem:** given two disconnected workflow systems and a clean MCP seam, what is the right shape for a Loom-native, harness-agnostic, *dynamic* workflow capability for Mills?

## Phase 1 — Framings

### F-A — Wire up what you already have
Make Mills a consumer of the existing `agentcontext` `WorkflowEngine`. `DefaultStages` becomes a YAML workflow definition; the `implement` stage becomes a `tool` step calling a `mills_spawn` MCP tool. Connect two existing systems instead of building a third.
- **Bet**: The cheapest path to dynamic + harness-agnostic is integration, not invention — the DAG engine already has gates, map-reduce, approval, persistence.
- **Risk**: That engine targets short MCP-tool sequences, not budget-governed, CI-gated, substrate-routed, resume-across-operator-restart shipping. The impedance mismatch could force rebuilding half of Mills inside it.

### F-B — Embed a real scripting runtime
Mirror Anthropic literally: workflows are imperative scripts (`agent()`, `parallel()`, `pipeline()`, `budget`, loop-until-dry) on an embedded interpreter in the operator (goja/Starlark-Go/Lua). `agent()` dispatches to the harness-agnostic `SpawnClient`; `agentType` is a parameter.
- **Bet**: The power of dynamic workflows is imperative control flow (runtime fan-out, budget-scaling, loop-until-converged) that no declarative DAG expresses. You need a Turing-complete authoring surface.
- **Risk**: A Turing-complete interpreter in an autonomous-merge operator is a brutal durability/safety surface. Checkpointing a *running script* across operator restart (journal/replay) is the hardest piece; getting it wrong corrupts in-flight merges.

### F-C — Declarative DAG, dynamic at the edges
No new language. Make `DefaultStages` *data*: wire up `policy.default_template`, support per-squad/per-item DAGs, conditional stages, and a `fan_out` (map-reduce) stage type. Execution stays in Go.
- **Bet**: 90% of the real "dynamic" need is "different pipelines for different work types" + "fan out over N slices" — a parameterized DAG, not a programming language.
- **Risk**: Hits the wall where a DAG can't express runtime-determined loops ("spawn reviewers until 2 rounds find nothing"); bolt-on config until it's a bad programming language (inner-platform effect). Explicitly insufficient for driver (4).

### F-D — Nail the worker contract first; orchestration is a red herring
The hard part of "harness-agnostic" isn't the workflow layer — it's CLI/parsing/telemetry/auth/multi-turn divergence per harness. Define one crisp `Worker` contract: `(prompt, mcp_tools, budget) → (structured_result, diff, telemetry)`, uniform across harnesses. Then any orchestrator can drive it.
- **Bet**: Once workers are fungible, dynamic workflows fall out cheaply on whatever orchestrator. Harness-agnosticism is a worker-contract problem.
- **Risk**: Necessary but not sufficient — ship a beautiful contract and still have no workflow engine. Reads as yak-shaving that defers the ask.

### F-E — MCP-tool orchestration, not agent orchestration
Most Mills steps (plan, gate, MR, CI-watch, merge) are deterministic glue = MCP-tool calls (`git_diff`, `quality_check`, `gitlab_*`, flexinfer LLM-judge). A workflow is a program over MCP tools; only the creative "write code" step spawns an agent.
- **Bet**: Treating MCP tools as the instruction set makes the workflow inherently harness-agnostic, faster, more reliable than spawning agents for glue.
- **Risk**: The "spawn an agent to write code" step is the load-bearing 80% of value and complexity. Optimizing glue may be optimizing the wrong thing — and agentcontext already runs MCP-tool DAGs.

### F-F — Let the council author the workflow at runtime
The Mills council already plans slices. Extend it to emit an executable workflow per backlog item — choosing harness, fan-out width, gate strictness from the work (typo → 1-agent fast path; risky refactor → 5-agent adversarial council).
- **Bet**: The leverage is adaptive orchestration where each task gets a bespoke pipeline the planner generated.
- **Risk**: LLM-generated control flow in an auto-merge system is a determinism/safety nightmare. Still needs a constrained, verifiable target representation — so you're back to designing a DSL *and* trusting the LLM to emit it.

### F-G — Adopt the vendor DX, invert the runtime
Adopt Anthropic's `agent()/parallel()/pipeline()` shape as Loom's authoring DSL, compiled/interpreted against a Loom runtime where `agent()` hits the harness-agnostic `SpawnClient`. Authored once, runs with any harness; could even be authored *by* a Claude agent and handed to Mills.
- **Bet**: Don't reinvent the DX everyone's learning; make the popular shape portable *off* Claude.
- **Risk**: API-compatibility with a proprietary, fast-moving vendor surface is a treadmill. Inherit their semantics (journal/resume, concurrency caps) without their runtime; may diverge anyway.

### F-H — Build a workflow *protocol*, not an engine
Define a durable step/event contract in the Mills store (`step_requested → worker_assigned → result → gate_verdict`). The Go state machine, a JS script, the agentcontext DAG engine, or an external service are all just *clients* appending steps to the log.
- **Bet**: The lasting asset is the durable step/event log — it gives resume, audit, and HUD replay for free regardless of which engine wins.
- **Risk**: Protocol-first is high-ceiling but slow to first value. Over-abstract and ship a spec with no engine; YAGNI bites if there's only ever one orchestrator.

## Phase 2 — Cross-Pollinations & Tensions

### Combinations

- **F-H + F-B + F-D (the load-bearing combination)**: H *neutralizes B's killer risk.* B alone dies on "how to checkpoint a running script across operator restart." If every side-effect (`agent()`, `tool()`, `gate()`, budget debit) is an append-only logged step (H), then resume = re-run the script, and each effect-primitive checks the log first (recorded → return cached instantly; absent → execute live). That is Anthropic's "longest unchanged prefix returns cached results," achieved with the Mills store instead of a private journal. D makes `agent({agentType})` return a uniform shape across harnesses. Net: **a capability-confined imperative runtime whose only way to touch the world is appending gated, budgeted steps to a durable log, executed against fungible workers.**
- **F-D + F-G**: The portable authoring surface (G) only works if the worker contract (D) is solid — `agent({agentType})` needs a uniform return regardless of harness. G without D is a façade.
- **F-F as a mode of F-C, not an architecture**: Per-task adaptivity does not require LLM-written control flow. The council *selects + parameterizes* a constrained workflow (fan-out width, gate strictness, harness) — F's adaptivity with C's safety. Adaptivity = parameter-binding, not code-generation.

### Tensions

- **F-B (imperative) vs F-C/F-A (declarative) — dissolved by the log**: Feels like "scripting language vs DAG." Once H is the contract, the engine above it is pluggable: linear pipelines stay declarative DAGs (cheap, auditable); loop-until-converged councils are imperative scripts; both append to the same log. Pick the **log as the invariant**, let authors choose form per workflow.
- **Expressiveness vs auto-merge safety — NOT dissolvable, must be designed**: A Turing-complete script can do anything; auto-merge must gate every effect. Resolution = **capability confinement**: pure compute (loops/conditionals/budget math) runs free in the sandbox; every world-touching effect routes through the protocol where policy/gates/budget intercept. This boundary is the thing the kill-test must prove.

## Phase 3 — Convergence

### Recommended: **F-H + F-B + F-D, built bottom-up** (protocol → worker contract → imperative runtime)

The only framing that delivers all four drivers without a fatal risk, given the protocol+engine appetite:

1. **Layer 1 — Worker contract (D)**: formalize the already-neutral `SpawnClient`/`SpawnResponse` into a stable `Worker` interface `(prompt, mcp_tools, budget, agentType) → (structured_result, diff, telemetry)`. → harness portability (driver 1).
2. **Layer 2 — Step/event protocol + durable log (H)** in the Mills SQLite store: append-only `(spawn_requested, worker_assigned, result, gate_verdict, budget_debit)`. → resume/audit (driver 3 substrate); becomes the journal.
3. **Layer 3 — Capability-confined imperative runtime (B)**: `agent()/parallel()/pipeline()/budget/loop-until-dry`, embedded interpreter, all effects routed through Layer 2. → imperative power (driver 4) + author-without-recompile (driver 3). Per-task adaptivity (driver 2) via the council parameterizing which workflow runs (F-as-mode).

Decisive property: **each layer ships standalone value before the next exists.** Layer 1 makes Mills truly multi-harness immediately. Layer 2 gives audit/resume even to the *current* hardcoded `DefaultStages`. Layer 3 — the risky part — lands last on a foundation already earning its keep. This sequencing kills the riskiest assumption in week one rather than after a large MR.

### Runner-up: **F-A — wire Mills into the existing `agentcontext` `WorkflowEngine`**

What tips it: if the existing engine (already has `map_reduce`/`subflow`/`approval`/`gate`/retries/persistence and is harness-agnostic via its MCP `ToolExecutor`) can swap its in-memory/Qdrant execution for the Mills SQLite store and survive operator-restart resume **without a rewrite**, reuse beats build on time-to-value. The tell is durability under Mills' non-functionals (budget, substrate, CI, days-long runs). This makes Layer-2's *first task* a fork in the road: a spike evaluating whether `agentcontext`'s engine can back onto a durable store decides recommended-vs-runner-up.

### Open question

**What is the durable unit of resume?** (a) Replay the script against the step log (Anthropic's model) — maximal expressiveness but requires deterministic scripts (no `Date.now()`/`Math.random()`/uncontrolled I/O in control flow); or (b) Resume a materialized state/DAG — simpler/safer but loses imperative expressiveness exactly on the resume path. This determines whether Layer 3 is viable and is the load-bearing decision.

## Riskiest assumption + kill-test

**Load-bearing assumption**: In the Loom Mills operator, an imperative workflow script's side-effects can be fully captured as append-only steps in the Mills SQLite store such that **re-executing the same script after an operator process restart deterministically resumes from the log** — previously-recorded effect-primitive calls (`agent()`/`tool()`/`gate()`) return their cached results instantly and only not-yet-recorded calls execute live — while every effect remains interceptable by policy/gate/budget (capability confinement). (This is the Go-embedded-interpreter analogue of Anthropic Claude-Code Workflow's "longest unchanged prefix of `agent()` calls returns cached results" resume model.)

**Kill test** (≤30 min, observable, end-to-end): Build a throwaway spike — a 3-call script `agent("A") → gate("G") → agent("B")` on a minimal Go interpreter (goja or Starlark-Go) whose `agent`/`gate` primitives read-through/append to a real SQLite `workflow_steps` table keyed by `(run_id, call_index, arg_hash)`. Run it; `kill -9` the process after step G is logged but before B; restart and re-run the same script with the same `run_id`. **Pass** iff: (1) calls A and G return their logged results without re-executing their side-effect (assert via a side-effect counter persisted in the row, not re-incremented on resume), (2) only B executes live, (3) the final result is identical to an uninterrupted run. Pair with a disconfirming search: "goja/Starlark determinism limitations", "durable workflow replay non-determinism pitfalls", "Temporal/DBOS replay constraints" — to surface where non-deterministic script constructs break the prefix-cache model before committing.

**Failure mode if wrong**: If scripts can't be made deterministically replayable (hidden non-determinism, non-idempotent effects, or capability confinement can't intercept all side-effects), Layer 3 (imperative runtime, F-B) collapses. Fallback is F-C (declarative DAG with `fan_out`) on top of the same Layer-1/Layer-2 foundation — losing runtime loop-until-dry/imperative fan-out (driver 4) but keeping portability, audit/resume, and author-as-data. ~70% of the foundation (Layers 1–2) is reusable regardless; only the top layer's form changes.

**Status**: not run

> The downstream slice plan is BLOCKED until this kill-test passes. The spike doubles as the runner-up fork: while building it, evaluate whether `agentcontext`'s `WorkflowEngine` can back onto the same SQLite step log instead of a new interpreter.

## Handoff

- If chosen → next step is: `plan-loom-core` to produce a multi-slice implementation plan. **Slice 1 = the kill-test spike** (resume-from-log + agentcontext-reuse evaluation); Slice 2 (Worker contract / Layer 1) and the protocol/log schema (Layer 2) can proceed in parallel since they don't depend on the resume decision. Layer 3 (imperative runtime) is gated on the kill-test passing.
- **DONE (2026-06-06)** → planning artifacts produced via a 15-agent research+design+adversarial-verify workflow:
  - Research dossier: `131-research-mills-dynamic-workflows-2026-06-06.md` (interpreter = **Starlark-Go**; durability = **memoized-step-output / DBOS-Restate model**; full citations)
  - Product spec: `132-product-spec-mills-dynamic-workflows-2026-06-06.md`
  - Implementation plan: `133-implementation-plan-mills-dynamic-workflows-2026-06-06.md` (slices S1→S7; S1+S1c kill-tests gate Layer 3)
  - Key refinements from the adversarial pass: goja dropped from the auto-merge path; two-tier confinement scoping; structured drift-tolerant step keys (not flat call_index); client-side spawn idempotency is its own prerequisite slice; the real de-risk is a **deployed** pod-crash kill-test, not the in-process one.
- Related prior art in `.loom/`: `90-product-spec-agent-swarm-council-pipeline`, `93-product-spec-mills-v2-hierarchical-swarm`, `45-product-spec-mills-harvester-vm-substrate`, `113-product-spec-llm-fsm-substrate`.
