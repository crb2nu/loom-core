# RALPH Iteration Plan — HUD A11y overlay + accordions (slice 7c) (2026-06-08)

## Review

- Backlog: `.loom/hud-audit-plan-2026-06-06.md` slice 7. **7a** (dialogs, `!673`) and **7b**
  (drawers, `!674`) merged; the shared `focusTrap` action is in place.
- 7c closes the remaining slice-7 surfaces: the keyboard-help overlay + accordion
  `aria-expanded` — completing Slice 7.

## Riskiest assumption + kill-test

**Load-bearing assumption**: The keyboard-help overlay and the OverlayShell/CatalogPanel
accordions are the only remaining slice-7 surfaces, and the "icon-only buttons missing labels"
finding is already satisfied by 7a/7b on these files.

**Kill test**: grep the three named files (`CatalogPanel`, `PresencePanel`, `CreateTaskModal`)
for unlabelled icon-only buttons → **none found** (visible icon controls already carry
`aria-label`). Escape already closes the help (`App.svelte` keydown handler line 113). →
Verified 2026-06-08.

**Failure mode if wrong**: an unlabelled control or a help dialog that strands focus.
Mitigated: Escape always closes; the new close button is labelled.

## Align

- Slice name: **keyboard-help overlay a11y + accordion `aria-expanded` (7c).**
- Scope in:
  - `App.svelte` keyboard-help: drop backdrop `role="presentation"`; `use:focusTrap` on the
    dialog; labelled `✕` close button + `.help-close` CSS.
  - `OverlayShell.svelte`: `aria-expanded` on the section / namespace / session toggles.
  - `CatalogPanel.svelte`: `aria-expanded` on the server-row expand button.
  - Rebuild + commit `dist/`; CHANGELOG `### Fixed` entry.
- Scope out: none remaining for Slice 7 (this completes it). Next is slices 9–12 or MBL-8.
- Acceptance criteria:
  1. keyboard-help backdrop no longer `role="presentation"`; dialog has `use:focusTrap` + a
     labelled close button; Escape still closes.
  2. All three accordion toggle families expose `aria-expanded` bound to their open state.
  3. `make hud-frontend` builds; `aria-expanded` present + `role="presentation"` gone from bundle.
- Dependencies/blockers: none. Branch `feat/hud-a11y-overlay-labels-7c` off `main` (`12bd4d2e`).

## Prove

- `make hud-frontend` build + bundle grep (`aria-expanded` present; help `role="presentation"` gone).
- No vitest/svelte-check harness → documented exception (carried from 7a/7b).
- CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: CHANGELOG `### Fixed` entry (satisfies docs guardrail — `internal/` src is a code path).
- Agent-context: decision (7c scope completes Slice 7), finding (icon-button sweep was a no-op).
- Next: HUD audit slices 9 (mobile breakpoints), 10 (legacy palette), 11 (polish), 12 (token
  codemod) — or MBL-8 to formally close the mobile milestone.
