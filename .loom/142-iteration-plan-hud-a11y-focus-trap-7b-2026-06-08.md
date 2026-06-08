# RALPH Iteration Plan — HUD A11y Focus Trap, drawers (slice 7b) (2026-06-08)

## Review

- Backlog: `.loom/hud-audit-plan-2026-06-06.md` slice 7 (focus trap). Slice **7a** (Modal/
  ConfirmDialog/Toast) merged as `!673`/`61de9a49`. The reusable `focusTrap` action now
  exists at `src/lib/actions/focusTrap.ts`.
- 7b applies that action to the slide-in **drawers**.

## Riskiest assumption + kill-test

**Load-bearing assumption**: `DetailDrawer`'s hand-rolled focus `$effect` can be replaced by
`use:focusTrap` without regressing its 6 consumers, and the action's Tab-wrap is the only
behavior actually missing.

**Kill test**: Read `DetailDrawer.svelte` — confirmed it does initial-focus (rAF +
querySelector) + restore + Escape but **no Tab handler**, so the action is a strict superset
(adds wrap, keeps focus-in/restore). `make hud-frontend` compiles all 3 drawers + the 6
DetailDrawer consumers. → Verified 2026-06-08: build succeeds; the bundle no longer contains
DetailDrawer's old "Focus first focusable element inside drawer" path.

**Failure mode if wrong**: a drawer that fails to focus-in or strands focus. Mitigated:
Escape-to-close is retained on every drawer (always an exit).

## Align

- Slice name: **focus-trap the three slide-in drawers (7b).**
- Scope in:
  - `shared/DetailDrawer.svelte` — refactor: drop `$effect`/`previousFocus`/`drawerEl`, apply
    `use:focusTrap` on the `.drawer` aside (covers MemoryPanel, TaskDetail, GraphFullView,
    SessionDetail, ServerDetail, BacklogDetail).
  - `shared/action/AuditDrawer.svelte` — `use:focusTrap` on `.audit-drawer`.
  - `Mills/PipelineRunDetail.svelte` — `use:focusTrap` on `.run-drawer`.
  - Rebuild + commit `dist/`; CHANGELOG `### Fixed` entry.
- Scope out → 7c: App keyboard-help overlay, `OverlayShell` `aria-expanded` toggles, broader
  icon-button `aria-label`/`aria-expanded` sweep.
- Acceptance criteria:
  1. All 3 drawers carry `use:focusTrap`; DetailDrawer's hand-rolled focus logic removed.
  2. `make hud-frontend` builds; old DetailDrawer focus path absent from the bundle.
  3. Each drawer keeps Escape + a labelled close button.
- Dependencies/blockers: none. Branch `feat/hud-a11y-focus-trap-7b` off `main` (`61de9a49`).

## Prove

- `make hud-frontend` build + bundle grep (old path gone; shared action present).
- No vitest/svelte-check harness → documented exception (carried from 7a).
- CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: CHANGELOG `### Fixed` entry (satisfies docs guardrail — `internal/` src is a code path).
- Agent-context: decision (7b drawer scope), finding (DetailDrawer overclaimed its trap).
- Next: slice 7c (keyboard-help overlay + OverlayShell aria-expanded + icon-button sweep), or
  slices 9–12.
