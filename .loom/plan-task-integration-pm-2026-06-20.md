# Implementation Plan: Task Integration for the Unified PM View (Claude + Codex)

**Date**: 2026-06-20
**Status**: DRAFT — slice 1 is a kill-test gate (blocks slices 2–5)
**Scope decision (user, 2026-06-20)**: **"Make it actionable"** — accuracy (both
agents emit project-stamped tasks) + an agent-facing rollup tool + risk↔task
cross-link surfacing. Dispatch-actually-spawns is explicitly **out of scope** (later).
**Builds on**: flexdeck unified project tracking, SHIPPED 2026-06-19
([services/flexdeck/.loom/plan-unified-project-tracking-2026-06-19.md](../../flexdeck/.loom/plan-unified-project-tracking-2026-06-19.md)).

---

## 1. Problem (one sentence)

The flexdeck `/projects` view federates work by a shared `project` key, but task
integration is asymmetric and fragile: **Codex emits zero tasks**, and a degraded
session namespace silently orphans a task from every project bucket — so the
"unified" view shows a partial, misleading picture of what the fleet is doing.

## 2. Architecture recap (what already exists)

```
flexdeck /projects (SolidJS)  ──GET /api/projects[/<id>]──►  reads Qdrant READ-ONLY,
                                                              federates by `project`:
   agent tasks (agent_tasks_v1) │ GitLab issues │ milestones │ risks (pm_risks) │ decisions
```

- The correlation key is `project` = GitLab `path_with_namespace` (e.g. `services/flexdeck`).
- Tasks are stamped at creation: `agent_task_add` inherits `session.Project`, then
  re-canonicalizes via `canonicalProject(project, namespace, pipelineRef)`
  ([pkg/agentcontext/svc_tasks.go:92,107](pkg/agentcontext/svc_tasks.go);
  helper at [pkg/agentcontext/linking.go:61](pkg/agentcontext/linking.go)).
  **The project key is derived from the session namespace** — no namespace, no project.
- Claude path works today: native `TaskCreate`/`TodoWrite` → PostToolUse
  `loom agent task-sync` → HUD resolves the agent's **active session** and writes the
  task under it ([internal/hud/domain/fleet/handler_task_sync.go:42-57](internal/hud/domain/fleet/handler_task_sync.go)),
  wired via the `postToolUse_taskSync` extra
  ([pkg/generator/platform_profiles.yaml:38](pkg/generator/platform_profiles.yaml)).
- `mcp-pm` owns risk **writes** (Qdrant `pm_risks`); flexdeck reads everything directly.

## 3. The gaps (evidence-backed)

| # | Gap | Evidence | Why it breaks the view |
|---|-----|----------|------------------------|
| G1 | **Codex emits no tasks** — no native todos, no `postToolUse_taskSync`; only `sessionStart` + `notify` events | [platform_profiles.yaml:118-184](pkg/generator/platform_profiles.yaml) | Any project worked by Codex shows 0 tasks → view is blind to half the fleet |
| G2 | **Project-stamping is fragile** — `canonicalProject("","",nil)` → `""`; the known Codex `////main` namespace-minting bug yields a degraded namespace | [linking.go:61](pkg/agentcontext/linking.go); MEMORY: "Agent namespace minting (!711/!712)" | A real task lands with `project=""` and never appears in any bucket — silent loss |
| G3 | **No agent-facing rollup** — agents have only `agent_task_list` (session-scoped) and `pm_risk_list`; no single "state of project X" call | [cmd/mcp-agent-context/tools_tasks.go:138](cmd/mcp-agent-context/tools_tasks.go), [cmd/mcp-pm/tools.go:56](cmd/mcp-pm/tools.go) | Agents can't *consume* the unified view; the MCP is write-only (risks) + the view is read-only → no author→see loop |
| G4 | **Risk↔task link is one-way string** — `pm_risk_link(id, ref=task_id)`; no back-reference, not surfaced | [cmd/mcp-pm/tools.go:122](cmd/mcp-pm/tools.go), [pkg/pm/service.go:202](pkg/pm/service.go) | A risk and the task mitigating it read as disconnected lanes |

---

## 4. Riskiest assumption + kill-test (Slice 1 gate)

**Load-bearing assumption**: a task created during a **real Codex run** and a **real
Claude run** on a real repo each lands in Qdrant `agent_tasks_v1` stamped with the
correct `project` (= `path_with_namespace`), such that flexdeck
`GET /api/projects/<id>` returns it under that project — for BOTH agents, on live infra.

**Kill test** (≤30 min, both legs green):
1. *Claude leg*: in a real Claude Code session on `services/loom-core`, create a task
   (native `TaskCreate` or `agent_task_add`). Scroll `agent_tasks_v1` by
   `project=services/loom-core`; assert the task is present with non-empty `project`,
   and that `GET /api/projects/services%2Floom-core` (flexdeck) includes it.
2. *Codex leg*: same, from a real Codex run. **This leg is expected to FAIL today**
   (G1) — that failure IS the gate result that justifies Slice 3. Record whether Codex
   produced any task at all, and if so whether `project` was stamped or empty (G2).

**Failure mode if wrong**: we wire skills + a rollup tool against a `project` key that
doesn't actually correlate Codex/Claude tasks on live data → we ship a "useful" PM view
that silently omits real work (the exact MCP-Apps-widget precedent: green tests, nothing
rendered). Stamping must be proven on real agent output, not a hand-built task.

**Status**: ❌ **FAILED 2026-06-20** — both legs blocked, plus a higher-priority outage
surfaced that reorders the plan.

- *Claude leg — FAILED (unexpected cause)*: `agent_task_add(session_id=47b655c57ca99d06,
  …)` returned `morph API HTTP 500: … gte-qwen2-1.5b … type must be string, but is null`.
  The task could not be created at all. Root cause is NOT stamping — it's that
  `agent_task_add` **hard-fails on embedder error** ([svc_tasks.go:118-120](pkg/agentcontext/svc_tasks.go):
  `vectors, err := ts.embedr.EmbedDocuments(...); if err != nil { return ErrorResult }`),
  with **no best-effort fallback** — unlike `mcp-pm` risks, which persist a deterministic
  fallback vector on embed failure ([pkg/pm/service.go:97](../pkg/pm/service.go)).
- *Codex leg — FAILED (expected, G1)*: rendered `~/.codex/hooks.json` + `config.toml`
  carry **no** task-sync wiring (grep: no `task-sync`/`agent_task`). Moot anyway — the
  write path is down for every agent.
- *Headline finding*: `agent_tasks_v1` has **`points_count: 0`** (live
  `qdrant_get_collection`). The PM view's entire task lane is empty **platform-wide**,
  for all agents and all projects — the embedder outage + reconciler GC drained it and
  writes can't refill. **Task integration isn't "asymmetric" right now; it's fully down.**
- *Stamping logic is structurally sound* (couldn't be exercised E2E, but verified):
  - My active session already carries `project=services/loom-core` (canonicalized from
    the branch-qualified namespace `services/loom-core/fix/...`) — `agent_session_list`.
  - The `project` payload index exists on `agent_tasks_v1` (keyword) — ready to receive.
  - **G2 confirmed by code**: `projectmeta.Canonical("","")` → `FromNamespace("")` → `""`,
    and a degraded `////main` namespace hits `if strings.HasPrefix(ns,"/") { return "" }`
    ([projectmeta.go:34-37,67-72](pkg/projectmeta/projectmeta.go)). The orphan risk is
    real for degraded namespaces — but it is NOT the acute breakage.

**Verdict**: the riskiest assumption as framed (stamping correctness) was the *wrong*
risk. The load-bearing failure is **task writes are hostage to the embedder**, and the
embedder is down. This must be fixed FIRST (new Slice 2a below) before any
stamping/Codex/rollup work — none of it is testable while task creation 500s.

---

## 5. Slices (scope = "actionable")

### Slice 1 — Kill-test gate (THROWAWAY) — ~0.5 day
Run §4 against live Claude + Codex. Capture both legs' evidence in §4 Status. Decision:
confirms G1 (Codex blind) and characterizes G2 (stamping) on real data before any code.

### Slice 2a — Decouple task write from embed (UNBLOCK) — ~0.5 day **[NEW, top priority]**
**Goal**: a task always persists, even when the embedder is down — so the PM view's task
lane can refill. This is the acute breakage found by the Slice-1 kill-test.
- Port the exact best-effort pattern `mcp-pm` already uses to `agent_task_add`: on
  `EmbedDocuments` error, log + fall back to a deterministic vector and still upsert,
  rather than returning an error ([svc_tasks.go:117-156](pkg/agentcontext/svc_tasks.go)
  vs [pkg/pm/service.go:53-101](pkg/pm/service.go)). Apply the same to any other
  embed-coupled agent-context write on the task/decision hot path.
- Regression test: `EmbedDocuments` returns an error → task still upserts, is
  payload-filterable by `project`, and `agent_task_list` returns it.
- **Acceptance**: with the embedder forced to 500, `agent_task_add` succeeds and the task
  appears in `agent_tasks_v1`; `make test` green. (Re-run the Slice-1 Claude leg → PASS.)
- *Pairs with* the upstream embedder fix (flexinfer `--pooling last`, flexdeck plan §5#3)
  — but the decoupling is the durable fix; embedding is an enrichment, never a gate.

### Slice 2 — Harden project-stamping (no orphans) — ~1 day
**Goal**: a task for a real repo can never land with `project=""`.
- In `canonicalProject` ([linking.go:61](pkg/agentcontext/linking.go)) /
  `projectmeta.Canonical`, add a final fallback: when explicit+namespace+pipelineRef
  all yield empty, derive `project` from the session's git remote / cwd
  (`path_with_namespace`) captured at `session-start`.
- Prefer fixing at the **source** (session namespace minting) so decisions + tasks +
  sessions all inherit it — re-check the `////main` / muted-namespace path from
  MEMORY "Agent namespace minting (!711/!712)".
- Regression test: `agent_task_add` with an empty/degraded namespace still produces a
  non-empty `project` for a known repo; assert via `payloadToTask`.
- **Acceptance**: `make test` green; a degraded-namespace task is project-bucketed.

### Slice 3 — Codex task parity — ~1.5 days
**Goal**: a real Codex run produces project-stamped tasks in the PM view.
- Codex has the `agent_task_*` tools via the loom proxy already; the gap is *reliable
  emission* under Codex's event model (`sessionStart` + `notify` only — no PostToolUse,
  so the Claude `task-sync` bridge is not portable).
- **Two-part fix**:
  1. *Skill enforcement (both agents)*: promote project-stamped task tracking to a
     first-class, explicit step in the cross-platform `agent-context` skill
     ([skills-registry.yaml:1107-1146](mcp/context/skills-registry.yaml)) — "record
     each unit of work via `agent_task_add(session_id=<active>)`; mark `in_progress`/
     `completed`." `loom sync --regen` propagates to Codex + Claude.
  2. *Codex hook fallback*: on `sessionStart`, surface the project's open/dispatched
     tasks (auto-recall already establishes namespace → project); on `notify`
     (turn-complete) run a `loom agent task-sync` variant that reconciles the active
     session's task state. (notify carries no todo list, so this is reconcile-only, not
     per-tool — matches the deliberate no-PostToolUse decision at
     [platform_profiles.yaml:122-149](pkg/generator/platform_profiles.yaml).)
- **Acceptance**: re-run the Slice-1 Codex leg → task appears under the right project;
  `make test` green; generated Codex config carries the new wiring (grep rendered output).

### Slice 4 — Agent-facing project rollup tool — ~1.5 days
**Goal**: make the MCP *consumable* — one call returns the unified project state.
- New `pm_project_status{project}` in `mcp-pm` (mirrors flexdeck `/api/projects/<id>`
  but for agents): reads `agent_tasks_v1` + `pm_risks` (+ best-effort decisions from
  `agent_context_v1`) by `project` via the shared Qdrant client, returns
  `{open_tasks, in_progress, blocked, open_risks, risks[], tasks[]}`.
  `mcp-pm` already holds an `agentcontext.QdrantClient`; add read clients for `CollTasks`
  /`CollContext` ([pkg/pm/store.go:27](pkg/pm/store.go), [pkg/pm/config.go:42](pkg/pm/config.go)).
- Per-source error isolation (one dead collection ⇒ `[]` + `partial`), matching the
  flexdeck federation contract.
- **Acceptance**: `pm_project_status` callable via the loom proxy returns the same
  rollup flexdeck shows for ≥2 real projects; `pkg/pm` stays >80% (hermetic fake Qdrant).

### Slice 5 — Risk↔task cross-link surfacing — ~1 day
**Goal**: close the author loop — a risk and its mitigating tasks read as connected.
- Include linked tasks (resolved from `Risk.Links[]`) in `pm_project_status` and in
  `pm_risk_list` output; add a convenience so an agent can attach a freshly-created task
  to a risk in one step.
- **Acceptance**: a risk linked to a task surfaces that task in the rollup; round-trips
  through daemon restart.

---

## 6. Sequencing

```
Slice 1 (gate) ──► Slice 2a ──► Slice 2 ──► Slice 3 ──► Slice 4 ──► Slice 5
   FAILED          UNBLOCK       stamping     Codex                    ▲
   2026-06-20      (embed)         │                                   │
                                   └─ Slice 4 (read-only) can overlap after 2a
```
- Slice 1 ran and FAILED — it correctly surfaced the real risk (embed coupling), not the
  assumed one (stamping). Per the riskiest-assumption rule, that re-scopes the plan.
- **Slice 2a now BLOCKS all** — task creation 500s until embed is decoupled; nothing
  downstream is testable before it. It is also the smallest, highest-leverage change.
- Slice 2 (stamping) precedes Slice 3 (Codex), so Codex's first tasks land stamped.
- Slice 4 is independent read-only work; can overlap once 2a is green.
- **Total estimate**: ~6 days, all in loom-core (no flexdeck changes required; the view
  already federates — we fix the *inputs* it reads).

## 7. Out of scope (deferred)

- **Dispatch-actually-spawns** (assigning a task starts a Mills/handoff agent run).
  Today dispatch is record-only ([internal/hud/bridge/agent_task.go:175-247](internal/hud/bridge/agent_task.go)).
  Pulls in Mills orchestration; revisit as its own cycle.
- New flexdeck UI — none needed; this cycle fixes the data the view consumes.

## 8. Sources / evidence

- Federation architecture + frozen contract: flexdeck plan §1–3b (link above).
- Project stamping: [svc_tasks.go:92,107](pkg/agentcontext/svc_tasks.go),
  [linking.go:61](pkg/agentcontext/linking.go).
- Claude task-sync: [handler_task_sync.go:42-57](internal/hud/domain/fleet/handler_task_sync.go),
  [platform_profiles.yaml:38](pkg/generator/platform_profiles.yaml).
- Codex event model: [platform_profiles.yaml:118-184](pkg/generator/platform_profiles.yaml).
- mcp-pm storage/env: [pkg/pm/config.go:42-70](pkg/pm/config.go),
  [pkg/pm/store.go:27](pkg/pm/store.go), [cmd/mcp-pm/tools.go](cmd/mcp-pm/tools.go).
- Namespace-minting risk: MEMORY "Agent namespace minting (!711/!712)".
</content>
</invoke>
