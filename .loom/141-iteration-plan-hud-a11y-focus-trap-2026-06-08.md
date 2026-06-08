# RALPH Iteration Plan — HUD A11y Focus Trap (slice 7a) (2026-06-08)

## Review

- Source backlog: `.loom/hud-audit-plan-2026-06-06.md` — 12-slice HUD audit remediation board.
- Verified live (origin/main): slices 1, 3, 4, 5, 6 merged (commits `32121a31`, `ee30a768`,
  `aa110e9b`, `c30a8d23`); `src/lib/actions/focusTrap.ts` absent + no `use:focusTrap` usages →
  **Slice 7 (focus trap + dialog focus management) is the next-open slice.**
- Slice 7 is M-effort across ~13 files and the plan's proof gate is vitest, but the HUD
  frontend has **no test harness** (no `vitest`/`svelte-check`). So this ships the clean,
  low-risk **core (7a)** and defers the long tail to 7b.

## Riskiest assumption + kill-test

**Load-bearing assumption**: A single reusable `focusTrap` Svelte action can manage focus for
the shared dialogs without a test harness, verified by build + bundle inspection.

**Kill test**: `make hud-frontend` compiles the new `.ts` action + both dialog consumers
(`use:focusTrap`) and the bundle contains the action's behavior. → Verified 2026-06-08: build
succeeded; the focusable-selector literal `button:not([disabled])` and the new `aria-live` /
`aria-label` strings are present in `dist/assets/*.js`.

**Failure mode if wrong**: a focus bug (trap that strands focus, or no restore) shipping
unverified. Mitigated by keeping Escape-to-close owned by each dialog (always an exit) and a
deterministic ~50-line action.

## Align

- Slice name: **focusTrap action + core dialog focus management + toast live region (7a).**
- Scope in:
  - `src/lib/actions/focusTrap.ts` (new) — snapshot/focus-first/Tab-wrap/restore action.
  - `Modal.svelte` — `use:focusTrap`; backdrop `role=button`→presentational; close-btn `aria-label`.
  - `ConfirmDialog.svelte` — `use:focusTrap` on the dialog.
  - `Toast.svelte` — persistent `aria-live="polite"` live-region container; dismiss `aria-label`.
  - Rebuild + commit the `go:embed`'d `dist/`.
  - CHANGELOG `### Fixed` entry.
- Scope out (→ 7b follow-up): `AuditDrawer`, `PipelineRunDetail`, `DetailDrawer`, App
  keyboard-help overlay, `OverlayShell` `aria-expanded`, broader icon-button/`aria-expanded` sweep.
- Acceptance criteria:
  1. `focusTrap` action exists and is wired (`use:focusTrap`) on `Modal` + `ConfirmDialog`.
  2. Modal backdrop is no longer a `role="button"`; close button has `aria-label`.
  3. Toast container is a persistent polite live region; dismiss button labelled.
  4. `make hud-frontend` builds; the action + aria attributes are present in the bundle.
- Dependencies/blockers: none. Branch `feat/hud-a11y-focus-trap` off `main` (`51e33451`).

## Prove

- `make hud-frontend` (Svelte compile / esbuild transpile) — succeeds.
- Bundle grep: focusable selector literal + `aria-live` + new `aria-label`s present.
- No vitest/svelte-check harness in repo → documented exception (build + deterministic logic).
- CI: GitLab pipeline to terminal; fix-and-retry on red.

## Handoff/Harvest

- Docs: CHANGELOG `### Fixed` entry (satisfies the docs guardrail — `internal/` src is a code path).
- Agent-context: decision (7a scope + no-harness exception), finding (next-open = slice 7).
- Next: HUD audit slice 7b (remaining dialog/drawer surfaces), or slices 9–12.
