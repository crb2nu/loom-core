// confirm — one confirmation contract for every gated mutation surface.
//
// Before this module each surface declared its own inline confirm shape, and
// they had already drifted: BulkToolbar's carried no `variant`, the inbox
// CardAction's required `confirmLabel`. Drift is not cosmetic — MemoryPanel's
// bulk action array was typed with a local literal that had no `confirm` field
// at all, so gating the destructive Delete was not even expressible at the
// callsite, and it shipped unconfirmed. Shared types keep that from recurring.

/** Tone of the confirm button. Mirrors ConfirmDialog's own variants. */
export type ConfirmVariant = 'danger' | 'warn' | 'default';

/**
 * Copy for a ConfirmDialog gate on a single destructive action.
 *
 * `confirmLabel` and `variant` are optional: ConfirmDialog defaults the label
 * to "Confirm", and a surface that already knows an action is destructive
 * (BulkToolbar reads `variant: 'danger'`) can infer the tone instead of
 * repeating it.
 */
export interface ConfirmSpec {
  title: string;
  message: string;
  confirmLabel?: string;
  variant?: ConfirmVariant;
}

/**
 * One button in a BulkToolbar.
 *
 * A bulk pass is N sequential mutations, so destructive entries want both
 * guards: `confirm` gates the pass through ConfirmDialog, and the toolbar's
 * top-level `busy` locks every button while a pass drains.
 */
export interface BulkAction {
  label: string;
  variant?: string;
  onclick: () => void;
  confirm?: ConfirmSpec;
}
