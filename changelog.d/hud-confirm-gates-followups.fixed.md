- **Every destructive HUD mutation is now confirmation-gated** (`internal/hud/frontend/src`):
  the Memory panel's bulk `Delete` fired immediately on click while the
  single-row delete beside it had always been gated, and the File Claims tab's
  per-row `Release` force-released another agent's claim with no prompt. Both
  now route through the shared `ConfirmDialog`, and the Memory bulk toolbar
  locks while a pass drains so a second click cannot start an overlapping pass.
  A partly-rejected bulk delete now reports what actually landed instead of
  claiming the whole selection was deleted.
- **One confirmation contract across surfaces** (`internal/hud/frontend/src/lib/utils/confirm.ts`):
  `BulkToolbar` and the inbox `CardAction` each declared their own inline
  `confirm` shape and had already drifted. Both now share `ConfirmSpec`, and
  `BulkAction` is a shared type — the Memory panel's local action-array literal
  had no `confirm` field at all, so gating its destructive entry was not
  expressible at the callsite, which is how it shipped unconfirmed.
  `CardAction.run` widened to `() => void | Promise<unknown>` so a store
  mutation that reports its outcome by return value can wire in directly.
