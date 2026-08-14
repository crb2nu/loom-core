// Shared helper for Mills day-2 admin actions (plan 42 Slice 1).
//
// Mills mutations are DOUBLE-gated. The HUD proxy injects the *operator* admin
// bearer before forwarding (internal/hud/domain/mills/proxy.go), but the HUD's
// own handleProxyAdminPost gate (requireAdminToken) runs FIRST and requires the
// browser to present the *HUD* admin token (X-Admin-Token) — so the store's
// postJSON must attach it (via adminFetch from the Labs access bar). This helper
// only wraps the mutation with toast feedback; the token attach lives in the
// store. (Earlier this comment claimed "the browser never handles a token",
// which was the mental-model bug behind mills mutations 401ing at the HUD gate.)
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
