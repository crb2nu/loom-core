# Plan — HUD UX for Work (Plan/Slice/Task), Project lens & Mills

**Date**: 2026-06-26
**Owner**: cblevins (+ Claude)
**Repo**: services/loom-core (`internal/hud/frontend`, Svelte 5 + Vite; Go bridge/domain)
**Status**: in progress

## Goal

The Plan Store (S7b) shipped a richly-linked work model — `Plan → PlanSlice → Task`,
plans born-linked to Mills backlog items, plans carrying MR/pipeline refs — but the
HUD renders it as **disconnected lists**. Make the new work model usable and visible:

1. **Unify Work** — plan→slice→task hierarchy with clickable cross-links.
2. **Wire Mills → Plan Store** — surface council-authored (born-linked) plans.
3. **Add a Project lens** — new first-class per-project rollup nav surface.
4. **Mills operator polish** — escalation root-cause, trends, consistent states.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The HUD's existing API surface can serve the
cross-linking data (task↔plan, backlog↔plan, project rollup) with only thin,
well-understood additions — not a deep backend rework.

**Kill test**: Trace each link end-to-end through the live code path.

**Status**: **PASSED 2026-06-26**. Evidence:

- **Plans API exists**: `internal/hud/domain/plans/plans.go:18-21` —
  `GET /api/plans` (filter by project/namespace/phase), `GET /api/plans/{id}`
  (returns slices, mr_refs, pipeline_refs, mills_backlog_id, phase_history via
  `bridge/agent_plans.go` PlanInfo).
- **Task↔Plan round-trips in backend**: `pkg/agentcontext/schema.go:338-342`
  (`PlanID`/`SliceID` json tags); stored + read in
  `pkg/agentcontext/svc_tasks.go:126-127,485-489,521-522`; returned by
  `agent_task_list` (`svc_tasks.go:271` → emits full `[]Task`).
  **GAP (thin)**: HUD bridge `TaskInfo` (`bridge/agent_dto.go:39`) drops
  `plan_id`/`slice_id`. Fix = add 2 fields + frontend interface. LOW effort.
- **Backlog↔Plan linkage**: deterministic id `plan-mills-<slug>` via
  `pkg/mills/clients/plan.go:122 PlanIDForBacklog`; plus `mills_backlog_id` on
  Plan and `PlanID`/`CouncilRunID` on backlog item (`pkg/mills/store/types.go:70-74`).
  Frontend resolves backlog→plan by computed id or `mills_backlog_id` match. LOW-MED.
- **Project rollup**: `pm_project_status` federation exists (`pkg/pm/project.go:30-118`)
  but is NOT yet exposed via a HUD endpoint. Bridge can call arbitrary daemon tools
  (`bridge/agent.go:112 callAgentTool`). v1 Project lens can aggregate existing
  endpoints client-side (`/api/plans?project=`, `/api/tasks`, `/api/sessions?project=`,
  mills data) with no new backend; optional `pm__pm_project_status` enrichment later. MED.

**Failure mode if wrong**: we'd build UI against data that isn't reachable without
a backend rewrite. Kill-test shows the opposite — work is ~90% frontend + thin DTO.

## Build/CI constraints

- `internal/hud/frontend/dist` is **git-tracked** and embedded via
  `//go:embed frontend/dist` (`internal/hud/app.go:31`).
- CI gate `make hud-dist-check` runs `pnpm build` and fails if committed dist is
  stale. **Every frontend MR must rebuild + commit dist.**
- No JS unit tests (no vitest). Verify via `go build ./...`, `make hud-dist-check`,
  Go HUD tests (`internal/hud/...`), and visual smoke.

## Slices (each independently shippable)

### Slice 1 — Work hierarchy foundation (Task↔Plan linking + clickable refs)
- Backend: add `PlanID`/`SliceID` to `bridge/agent_dto.go TaskInfo` (passthrough).
- Frontend: add `plan_id`/`slice_id` to tasks store; show plan/slice chip in
  TaskDetail + rows; make `blocked_by` IDs clickable (→ task); make agent clickable.
- Acceptance: a task linked to a plan shows the plan slug and deep-links to it.

### Slice 2 — Plans panel upgrade (tree + clickable refs + filters)
- Render `mr_refs`/`pipeline_refs` as links; per-slice task rollup (fetch by plan_id);
  add filter bar + project grouping toggle; slice drill-down; show born-linked
  backlog id. Bring PlansPanel to parity with TasksPanel patterns.

### Slice 3 — Mills ↔ Plan Store wiring
- BacklogDetail: "Authored plan" link (computed `plan-mills-<slug>` → Plans detail).
- CouncilPanel: council run → backlog items → plans.
- Overview: "last plan authored" in activity card.

### Slice 4 — Project lens (new first-class nav surface)
- New `projects` view in router; per-project rollup (plans by phase, open/blocked
  tasks, active agents/sessions, recent mills activity) with deep-links.
- v1 = client-side aggregation; optional `/api/projects/{id}/status` enrichment.

### Slice 5 — Mills operator polish
- Escalation root-cause summary on BacklogDetail; trend sparklines on Overview;
  consistent stale/error/empty handling; workflow→backlog + council→audit links.

## Sequencing notes

- Slices 1→2→3 share the plan/task DTO surface; do in order.
- Slice 4 depends on Slice 1 (task linkage) + Slice 2 (plan filters) for deep-links.
- Slice 5 is independent of the Plan Store work; can interleave.
