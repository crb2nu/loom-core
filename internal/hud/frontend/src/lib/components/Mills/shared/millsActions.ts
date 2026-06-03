// Shared helper for Mills day-2 admin actions (plan 42 Slice 1).
//
// All Mills mutations route through the HUD proxy, which injects the
// operator admin bearer before forwarding (internal/hud/domain/mills/
// proxy.go) — the browser never handles a token. So this helper only
// needs to surface success/failure feedback, not authentication.
import { toastStore } from '../../../stores/toasts.svelte.ts';

export interface AdminActionOpts {
  /** Toast shown on success. */
  success: string;
  /** Prefix for the error toast (the underlying message is appended). */
  failurePrefix?: string;
}

// runAdminAction wraps a Mills store mutation with toast feedback and a
// boolean result so callers can drive their own button state. It never
// throws: failures become an error toast and `false`.
export async function runAdminAction(
  action: () => Promise<unknown>,
  opts: AdminActionOpts,
): Promise<boolean> {
  try {
    await action();
    toastStore.success(opts.success);
    return true;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    toastStore.error(`${opts.failurePrefix ?? 'Action failed'}: ${msg}`);
    return false;
  }
}
