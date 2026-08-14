- **Escape closes a HUD detail drawer again after the focused control unmounts
  underneath it** (`internal/hud/frontend/src/lib/components/shared/DetailDrawer.svelte`):
  `DetailDrawer` scoped its Escape handler to the panel, which held only while
  focus stayed inside the subtree `focusTrap` seeded on mount. That focused child
  can disappear while the drawer is still open — watching an in-flight Mills run
  reach a terminal state unmounts the header's Escalate button, and `focusTrap`'s
  restore is a no-op when the previously-focused control is already gone — at
  which point `document.activeElement` falls back to `<body>`, the `<aside>` is no
  longer on the event path, and Escape silently stopped closing the drawer (✕ and
  the backdrop still worked, so it degraded rather than stranded). A
  window-level fallback re-arms the key, gated on focus being *orphaned*
  (`<body>`/`<html>`/detached) rather than merely outside the panel, so a
  `ConfirmDialog` or the audit drawer layered over the drawer keeps sole ownership
  of Escape and one keypress still dismisses exactly one surface. Re-homing focus
  on `focusout` was rejected for the same reason: it would yank focus back out of
  those nested dialogs. Applies to all nine `DetailDrawer` consumers, not just the
  two Mills run drawers.
