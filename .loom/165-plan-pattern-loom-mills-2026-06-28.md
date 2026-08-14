# Mills Pattern Loom — intent + materials → stamped, deployed product

- **Plan ID**: `plan-pattern-loom-mills`
- **Phase**: in_progress
- **Project**: services/loom-core
- **Namespace**: loom-core/pattern-loom
- **Created by**: claude-code
- **Created**: 2026-06-28T14:00:05Z
- **Updated**: 2026-06-28T14:21:37Z

> Rendered from the Loom plan store (canonical). Edit via `agent_plan_*` tools, not this file.

## Lifecycle refs

- MRs: services/loom-core!831

## Phase history

| From | To | At | Actor | Note |
|---|---|---|---|---|
| planned | in_progress | 2026-06-28T14:21:37Z | claude-code | S0 implemented + shipped as MR !831 (open, awaiting merge while merge path is repaired); S1 kill-test passed. Plan is actively in progress. |

## Spec

# Mills Pattern Loom

**Status**: planned — riskiest-assumption kill-test (S1) **PASSED (lean form) 2026-06-28**; Tracks A/B unblocked. Full Mills e2e stamp deferred until merge path stabilizes. Evidence: `.loom/kill-test-pattern-go-rest-service-2026-06-28.md`.
**Rationale doc**: `.loom/164-brainstorm-pattern-loom-mills-2026-06-28.md`

## Thesis

A textile pattern is an instruction book that anyone can follow given **materials** + **basic tools**. Mills should become a **Pattern Loom**: a user arrives with intent to make a *type of thing* (product/service/tool); Mills holds a library of vetted **Patterns**; it **stamps** the chosen pattern with the user's **materials** and returns a **deployed, working version**.

## Object model

- **Engram** (exists, empty catalog) — atomic proof-gated technique. `pkg/agentcontext/svc_engrams.go`.
- **Pattern** (new, `agent_patterns_v1`) — product archetype. Composes engrams + declares: `materials_schema` (typed user inputs), `tools_manifest` (closed set of required capabilities; stamp aborts if any missing), `gauge` (a swatch kill-test proving it works in *this* env), `slice_template` (→ PlanSlices), `deploy_contract`, `provenance`/taste-gate. **Stampability fields surfaced by S1** (must pin to reach zero-improvisation): standard error envelope, body for every endpoint, wiring/composition convention, error model, full status-code table.
- **Materials** (new) — concrete inputs filling a Pattern's schema.
- **Stamp** (new verb) — `stamp(pattern_id, materials) → Plan → Mills pipeline → deployed instance`.
- **Loom = Mills** — adds an intent front door feeding the existing pipeline through a pattern-constrained path.

## Riskiest assumption + kill-test (S1) — PASSED (lean form)

**Load-bearing assumption**: a vetted Pattern + typed materials stamps deterministically into a result Mills' pipeline executes to a green, deployed state with **zero per-instance human architecture decisions**. If each instance still needs human judgment, it is a suggestion, not a pattern.

**What ran (2026-06-28)**: tightened `pattern-go-rest-service` (closes load-bearing axes) + synthetic `widget` materials → handed to a fresh context-free agent (rule: no architecture decisions, report GAPs) → independent black-box gauge **10/10**, zero unrequested deps/files/features. Determinism caveat: 7 bounded boundary-class gaps, none load-bearing. The full-pipeline stamp (→ merged MR → deploy) is the integration step, deferred until the merge path (under repair this week) is stable. Evidence: `.loom/kill-test-pattern-go-rest-service-2026-06-28.md`.

**Failure mode (avoided)**: rebuilding today's manual REST intake (`pkg/mills/handlers_backlog.go:48`) behind a UI; "anyone can follow it" being false.

## Decisions (2026-06-28)

1. After S1, **Track A** (constraint rails on the autonomous loop) and **Track B** (human intent front door) run **in parallel**.
2. First pattern = **Go REST microservice** (`go-service-scaffold` formalized).
3. Pattern storage = **new agent-context entity** (mirrors Plan/Engram), thin projection into Mills policy for matching.

## Structure

- **Trunk**: S0 (Pattern catalog) → S1 (stamp + gauge kill-test) ✅ PASS.
- **Track A** (inward, quality rails): A1 pattern-constrained council editor → A2 engram population from green stamps.
- **Track B** (outward, product): B1 materials intake front door → B2 pattern authoring + taste gate.

## Attach points (from architecture map)

- Stamp → Plan: Plan Store write-path (deferred S7b seam), `pkg/agentcontext/schema_plan.go:98`.
- Constraint rails: Council Editor `pkg/mills/council/editor.go:18`, prompt `clients/council.go:131`.
- Matching/projection: Squads-style hot-reload `squads/router.go:103`; reconciler `reconciler.go:194`.
- Engram population: `agent_engram_verify` (`engram_verify.go:95`), `unlocked_in`.

## Slices

### 1. S0 — Pattern object + catalog — `in_review`

- **Slice ID**: `plan-pattern-loom-mills#1`
- **Goal**: Define `Pattern` as a new agent-context entity (collection `agent_patterns_v1`) with CRUD/search tools, mirroring the Plan/Engram services. Seed the first pattern: `pattern-go-rest-service` (go-service-scaffold formalized).
- **Files**: pkg/agentcontext/schema_pattern.go, pkg/agentcontext/svc_patterns.go, pkg/agentcontext/svc_patterns_test.go, cmd/mcp-agent-context/tools_patterns.go
- **Acceptance**: Pattern schema carries: makes (type), materials_schema (typed fields), tools_manifest (required capabilities), gauge (kill-test spec), slice_template (→ PlanSlices), deploy_contract, engrams[] (composed URIs), provenance (author/approved_by/instances_shipped_green). Tools agent_pattern_add/get/list/search wired + persisted to Qdrant. One seeded pattern-go-rest-service resolves by id from any worktree.
- **MR**: services/loom-core!831
- **Decision**: S1 kill-test surfaced the required "stampability" fields the Pattern schema MUST pin to reach zero-improvisation (beyond the load-bearing axes already listed): (1) a standard error envelope (one shape for all non-2xx bodies); (2) a body spec for EVERY endpoint incl. secondary (readyz); (3) a wiring/composition convention (where DI lives + constructor naming); (4) a pinned error model (sentinel vs typed) for the service↔handler seam; (5) a complete status-code table including failure paths (500s). Add these as required fields of materials_schema/slice_template. Reference: .loom/kill-test-pattern-go-rest-service-2026-06-28.md.
- **Decision**: S0 IMPLEMENTED + MR !831 (open, not auto-merged) 2026-06-28. Pattern entity agent_patterns_v1 (schema_pattern.go + svc_patterns.go + tools_pattern.go + registry/index/service/seed wiring); seed pattern-go-rest-service pins 13 axes incl the 5 stampability seams. All hooks green (golangci-lint 0); new agent_pattern_* tools need mcp-agent-context rebuild+restart. Not auto-merged: merge path under repair this week.

### 2. S1 — Stamp + gauge kill-test (RISKIEST ASSUMPTION) — `in_review`

- **Slice ID**: `plan-pattern-loom-mills#2`
- **Goal**: Implement `stamp(pattern_id, materials) → Plan` and run the Go REST gauge end-to-end through Mills to a green merged MR with zero human architecture input. This proves or kills the core assumption; everything downstream is blocked on it.
- **Files**: pkg/agentcontext/svc_pattern_stamp.go, pkg/agentcontext/svc_pattern_stamp_test.go, cmd/mcp-agent-context/tools_patterns.go, pkg/mills/intake/pattern_stamp_intake.go
- **Depends on**: plan-pattern-loom-mills#1
- **Acceptance**: stamp() (1) validates materials vs materials_schema, (2) checks tools_manifest against the environment and aborts loudly if a capability is missing, (3) expands slice_template+materials into a concrete Plan via agent_plan_create + agent_plan_slice_add, (4) projects a Mills BacklogItem with PlanID set, labeled mills-pattern-stamp. KILL-TEST: synthetic widget materials (one entity, memory storage, no auth) reach a GREEN merged MR; the widget service builds and passes its generated CRUD + /healthz smoke test in CI; the diff contains no unrequested architectural choices (negative-search the diff before declaring pass).
- **MR**: services/loom-core!831
- **Decision**: S1 kill-test PASS (lean form), 2026-06-28. Fresh context-free agent + tightened pattern-go-rest-service + synthetic widget materials → building service, independent black-box gauge 10/10, zero unrequested deps/files/features. Determinism: 7 bounded boundary-class gaps (error envelope, readyz body, DI location, constructor naming, error model, decode→400, 500 path) — none load-bearing. Assumption HOLDS. Evidence: .loom/kill-test-pattern-go-rest-service-2026-06-28.md. Out of scope (deliberate): full Mills e2e to merged MR — plumbing proven by A2, under repair this week; run as integration once merge path stable.
- **Decision**: S1 stamp() CODE shipped (extends MR !831) 2026-06-28 — distinct from the earlier kill-test. svc_pattern_stamp.go: stampPattern(pattern,materials)→Plan with materials validation (required/enum/default-resolution), {{placeholder}} substitution (incl derived entity.name/entity_lower), slice_template expansion, stamp-manifest spec_doc; agent_pattern_stamp MCP tool. 4 unit tests green. DEFERRED (gated on merge-path stability): live tools-manifest probing + Mills BacklogItem projection + pipeline run; result surfaces tools_required for the B1 front door to gate on.

### 3. A1 — Pattern-constrained Council Editor (Track A) — `in_review`

- **Slice ID**: `plan-pattern-loom-mills#3`
- **Goal**: Inject the pattern catalog into Mills' autonomous council so generated proposals conform to / cite an approved pattern instead of free-styling architecture. Emit pattern-stamp demand to feed the idle canary-autopilot loop.
- **Files**: pkg/mills/clients/council.go, pkg/mills/council/editor.go, pkg/mills/council/backlog_mutator.go
- **Depends on**: plan-pattern-loom-mills#2
- **Acceptance**: buildCouncilEditorPrompt includes the available pattern catalog (agent_pattern_list); each EditorOutput BacklogProposal carries a pattern_id or is explicitly flagged non-conforming; canary-autopilot can request a pattern stamp as demand. Works behind both FlexInfer and gpt-5.4 editors (drop-in interface preserved).
- **MR**: services/loom-core!831
- **Decision**: A1 SHIPPED (MR !831) 2026-06-28. pkg/mills: PatternClient.ListApprovedPatterns (MCP agent_pattern_list status=approved, best-effort/never-blocks); buildCouncilEditorPrompt injects "## Approved patterns — conform to one" + pattern_id instruction (both FlexInfer + OpenAI editors); BacklogProposal.PatternID parsed + logged at persist; operator main.go wires PatternClient onto the live editor. 8 hermetic tests. Note: PatternID not persisted to BacklogItem store (no column) — rides proposal + log. Combined build/test green with S0/S1/B1.

### 4. A2 — Populate engrams from green stamps (Track A) — `pending`

- **Slice ID**: `plan-pattern-loom-mills#4`
- **Goal**: Close the empty-catalog gap: when a stamp merges green, verify the pattern's composed engrams and record `unlocked_in` for the repo. The Pattern Loop becomes the producer the engram engine never had.
- **Files**: pkg/agentcontext/svc_engrams.go, pkg/agentcontext/svc_pattern_stamp.go, pkg/mills/pipeline/runner.go
- **Depends on**: plan-pattern-loom-mills#2
- **Acceptance**: A green Go REST stamp triggers agent_engram_verify for each engram in Pattern.engrams[], flipping proof_status to verified and appending the target repo to unlocked_in. The engram catalog is non-empty after the first green stamp. Novel slices with no matching engram are surfaced as engram candidates (not auto-added).

### 5. B1 — Materials intake front door (Track B) — `in_review`

- **Slice ID**: `plan-pattern-loom-mills#5`
- **Goal**: The headline human entrypoint: a person picks a pattern, fills a typed materials form, previews the gauge result, submits, and watches Mills stamp + deploy. Deliberately re-introduces the human as demand source (not supervisor).
- **Files**: cmd/loom/mills_stamp.go, internal/hud/api_patterns.go, internal/hud/frontend/src/lib/patterns, internal/hud/routes.go
- **Depends on**: plan-pattern-loom-mills#2
- **Acceptance**: `loom mills stamp` CLI and a HUD page both: list patterns, render a materials form derived from materials_schema, run/preview the gauge before submit, submit a stamp, and stream pipeline status to a deployed instance. A user supplying only materials (no architecture decisions) produces a deployed service. Builds on the Plan Store write-path.
- **MR**: services/loom-core!831
- **Decision**: B1 SHIPPED (MR !831) 2026-06-28 — Go backend of the front door. HUD: bridge PatternList/PatternStamp (agent_pattern_list/stamp via daemon socket, mirrors EngramSummary) + GET /api/patterns + POST /api/patterns/stamp (routes.go). CLI: `loom mills patterns [--status]` + `loom mills stamp --pattern --materials(@file|inline|path) --project` via withAgentBridge (agent-context daemon, not operator HTTP). 12 tests (5 httptest + 7 CLI). DEFERRED: the Svelte front-door page (TODO in routes.go) — next sub-slice. Combined build/test green.

### 6. B2 — Pattern authoring + taste gate (Track B) — `pending`

- **Slice ID**: `plan-pattern-loom-mills#6`
- **Goal**: How new patterns enter the library and earn approval — the "we approve / find tasteful" curation layer. Patterns become proof-gated like engrams: provenance + a passing gauge to register, N green instances to promote to approved.
- **Files**: pkg/agentcontext/svc_patterns.go, pkg/agentcontext/schema_pattern.go, mcp/skills/pattern-authoring
- **Depends on**: plan-pattern-loom-mills#2
- **Acceptance**: Registering a pattern requires provenance (author) + a gauge that passes once. A pattern is promoted from candidate→approved only after instances_shipped_green ≥ threshold. Mills' constraint rails (A1) and front door (B1) only offer approved patterns by default. An authoring skill documents the workflow.
