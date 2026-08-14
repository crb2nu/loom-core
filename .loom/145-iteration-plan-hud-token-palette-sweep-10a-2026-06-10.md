# RALPH Iteration Plan — HUD Token Palette Sweep 10a (2026-06-10)

## Review

- Roadmap milestone: HUD audit remediation plan, `.loom/hud-audit-plan-2026-06-06.md`.
- Prior slices complete on `main`: Slice 8 (SpawnTelemetry empty/error states), Slice 9 (mobile breakpoints), and the live-events listener drift fix.
- Source finding: Slice 10 still had several shared/widget-level hardcoded legacy palette literals (`rgba(129, 240, 254, ...)`, `rgba(231, 179, 18, ...)`, old graph hex ramps, `top: 52px`) plus two panels still importing the older widget `ConfirmDialog` instead of the focus-trapped shared dialog.

## Align

- Slice name: **shared HUD palette/token cleanup 10a**.
- Scope in:
  - Replace shared/widget stale palette literals in `MetricCard`, `DataTable`, `AgentCard`, `ConfirmDialog`, `ErrorBanner`, `Toast`, and `EntityGraph` with semantic tokens.
  - Migrate the remaining `MemoryPanel` and `GraphFullView` call sites from `widgets/ConfirmDialog.svelte` to `shared/ConfirmDialog.svelte`.
  - Delete the unused widget `ConfirmDialog` wrapper.
  - Rebuild embedded HUD `dist`.
- Scope out:
  - No PresencePanel or SpawnDetailPanel palette pass; those remain Slice 10b because they touch larger, more data-dense surfaces.
  - No broad `font-size: 9px` codemod outside the touched shared/widget files.
  - No visual redesign or layout changes.
- Acceptance criteria:
  1. The touched shared/widget files no longer contain the stale cyan/orange/red rgba or entity graph hex ramp targeted by Slice 10a.
  2. No source file imports `widgets/ConfirmDialog.svelte`.
  3. `make hud-frontend` succeeds and regenerated `dist/` contains the tokenized CSS.
  4. `git diff --check` is clean.
- Dependencies/blockers: none.

## Land

- Planned file areas:
  - `internal/hud/frontend/src/lib/components/shared/{MetricCard,DataTable,ConfirmDialog}.svelte`
  - `internal/hud/frontend/src/lib/widgets/{AgentCard,EntityGraph,ErrorBanner,Toast}.svelte`
  - `internal/hud/frontend/src/lib/components/{MemoryPanel,graph/GraphFullView}.svelte`
  - `internal/hud/frontend/dist/`
  - `CHANGELOG.md`
- Implementation steps:
  1. Tokenize the shared/widget palette literals.
  2. Migrate final ConfirmDialog imports and delete the stale widget.
  3. Rebuild HUD dist and run source/bundle greps.

## Prove

- Tests to run:
  - `make hud-frontend`
- Lint/static checks:
  - `git diff --check`
  - targeted `rg` checks for stale literals and stale `widgets/ConfirmDialog` imports.
- CI checks:
  - GitLab MR pipeline to terminal state.

## Handoff/Harvest

- Docs to update:
  - `CHANGELOG.md`.
- Agent-context entries to add:
  - Not available in this session beyond recall tooling; this iteration plan records the durable decision.
- Next-slice candidates:
  - Slice 10b: PresencePanel + SpawnDetailPanel palette cleanup with a real visual review.
  - Slice 11: Lazy-panel error recovery + Mills typography/copy rhythm.
