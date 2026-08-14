<script lang="ts">
  /**
   * BootstrapRepoDialog — mint a new GitLab project from a Spinning Room plan.
   *
   * A spin (or the compare/merge editor) can author a draft plan for a product
   * that has no repo yet — it lands with project="" and the plan-slice emitter
   * can never source it. This dialog closes that gap: it POSTs to
   * /api/mills/projects/bootstrap, which creates the repo with the operator's
   * group token, seeds an initial commit (README from the plan spec + a
   * self-contained green CI skeleton), records the project in the demand
   * registry, and re-scopes the plan onto the new path. The plan's lifecycle is
   * untouched — advance it to `planned` afterwards to feed the beam.
   *
   * Admin-gated at the HUD (submitBootstrap routes through adminFetch so the
   * Labs access-bar token rides along). Gated again in policy at the operator
   * (cross_repo.enabled + cross_repo.allow_bootstrapped), which surfaces here as
   * a 503 with a clear hint.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { submitBootstrap, type BootstrapResult } from './bootstrapActions.ts';

  interface Props {
    open: boolean;
    onClose: () => void;
    /** The plan being given a home. planId is required; title + a suggested
     * group seed the fields. */
    planId: string;
    planTitle?: string;
    /** Default group the leaf slug lands in (e.g. "services"). */
    defaultGroup?: string;
    /** Called after a successful mint so the board can reload + link the repo. */
    onBootstrapped?: (res: BootstrapResult) => void;
  }

  let { open, onClose, planId, planTitle = '', defaultGroup = 'services', onBootstrapped }: Props =
    $props();

  const VISIBILITIES = ['private', 'internal', 'public'];

  let group = $state(defaultGroup);
  let slug = $state('');
  let visibility = $state('private');
  let busy = $state(false);
  // Persistent, inline failure message. Held here (not a transient toast) so a
  // failed mint stays readable while the dialog is open — the operator can see
  // the reason, fix the path/token, and retry. Cleared on retry, on edit, and
  // when the dialog re-opens for a new plan.
  let errorMsg = $state('');

  // Suggest a slug from the plan title the first time the dialog opens for a
  // plan (kebab-case, GitLab-path-safe). The operator can override it.
  let seededFor = $state('');
  $effect(() => {
    if (open && planId && seededFor !== planId) {
      seededFor = planId;
      group = defaultGroup;
      visibility = 'private';
      slug = slugify(planTitle);
      errorMsg = '';
    }
  });

  function slugify(s: string): string {
    return s
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '-')
      .replace(/^-+|-+$/g, '')
      .slice(0, 48);
  }

  let fullPath = $derived(`${group.trim().replace(/\/+$/, '')}/${slug.trim()}`);
  // GitLab path segments: lowercase alnum + . _ - , not leading with a sep.
  let pathValid = $derived(
    group.trim().length > 0 &&
      slug.trim().length > 0 &&
      /^[a-z0-9][a-z0-9._/-]*[a-z0-9]$/.test(fullPath) &&
      !fullPath.includes('//'),
  );
  let canSubmit = $derived(pathValid && !busy);

  async function submit(): Promise<void> {
    if (!canSubmit) return;
    busy = true;
    errorMsg = '';
    try {
      const res = await submitBootstrap({ plan_id: planId, path: fullPath, visibility });
      if (res.plan_rescoped) {
        toastStore.success(`Bootstrapped ${res.project} — plan re-scoped; advance it to feed the beam`);
      } else {
        // The repo exists but the plan write failed — surface the server's
        // remediation hint rather than a plain success.
        toastStore.warning(res.warning || `Repo ${res.project} created, but the plan re-scope failed`);
      }
      onBootstrapped?.(res);
      onClose();
    } catch (e) {
      // Persist the failure INLINE rather than via a toast: the dialog stays
      // open so the operator can read the reason and correct + retry. The
      // previous toast auto-dismissed in 5s AND rendered behind this dialog
      // (z-index 1000 vs the 9999 backdrop), so the error was effectively
      // invisible.
      errorMsg = e instanceof Error ? e.message : String(e);
    } finally {
      busy = false;
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape' && !busy) onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if (!busy && (event.target as HTMLElement)?.classList?.contains('bs-backdrop')) onClose();
  }
</script>

{#if open}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="bs-backdrop" onkeydown={handleKeydown} onclick={handleBackdropClick}>
    <div class="bs-dialog" role="dialog" aria-modal="true" aria-labelledby="bs-title" use:focusTrap>
      <div class="bs-head">
        <div class="bs-title" id="bs-title">🧵 Spin up a repo</div>
        <div class="bs-sub">
          Mint a new GitLab project for <strong>{planTitle || planId}</strong>, seed it, and
          re-scope this plan onto it. Advance the plan to <em>planned</em> afterwards to feed the beam.
        </div>
      </div>

      <div class="fld-row">
        <label class="fld narrow">
          <span class="fld-label">Group</span>
          <input class="inp" placeholder="services" bind:value={group} disabled={busy} oninput={() => (errorMsg = '')} />
        </label>
        <label class="fld">
          <span class="fld-label">Repo slug</span>
          <input class="inp" placeholder="procmodel" bind:value={slug} disabled={busy} oninput={() => (errorMsg = '')} />
        </label>
        <label class="fld narrow">
          <span class="fld-label">Visibility</span>
          <select class="inp" bind:value={visibility} disabled={busy}>
            {#each VISIBILITIES as v}<option value={v}>{v}</option>{/each}
          </select>
        </label>
      </div>

      <div class="bs-note" class:err={!pathValid && slug.trim().length > 0}>
        {#if pathValid}
          Creates <code>{fullPath}</code> — an empty repo with a README, a self-contained green
          <code>.gitlab-ci.yml</code>, and an AGENTS.md pointing back at this plan.
        {:else}
          Path must be a valid GitLab path like <code>services/procmodel</code> (lowercase; the
          group must already exist).
        {/if}
      </div>

      {#if errorMsg}
        <div class="bs-error" role="alert">
          <span class="bs-error-icon" aria-hidden="true">✕</span>
          <span class="bs-error-msg">{errorMsg}</span>
        </div>
      {/if}

      <div class="bs-actions">
        <button class="btn-cancel" onclick={onClose} disabled={busy}>Cancel</button>
        <button class="btn-act" onclick={submit} disabled={!canSubmit}>
          {busy ? 'Minting…' : 'Create repo + re-scope plan'}
        </button>
      </div>
      <div class="bs-note dim">
        Requires the Mills admin token (Labs access bar) and the two-key policy gate
        (<code>cross_repo.enabled</code> + <code>cross_repo.allow_bootstrapped</code>).
      </div>
    </div>
  </div>
{/if}

<style>
  .bs-backdrop {
    position: fixed;
    inset: 0;
    z-index: 9999;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--scrim);
    backdrop-filter: blur(2px);
  }
  .bs-dialog {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    max-width: 560px;
    width: 94%;
    padding: var(--space-4);
    background: var(--bg-primary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
  }
  .bs-head {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .bs-title {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
  }
  .bs-sub {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    line-height: 1.4;
  }
  .bs-note {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
  }
  .bs-note.err {
    color: var(--status-error);
    border-color: color-mix(in srgb, var(--status-error) 40%, var(--border));
  }
  .bs-note.dim {
    background: none;
    border: none;
    padding: 0;
    font-style: italic;
  }
  .bs-note code {
    font-family: var(--font-mono);
  }
  /* Persistent inline failure banner — stays until retry/edit so the operator
     can actually read a failed mint (replaces the transient error toast that
     auto-dismissed and rendered behind this dialog). */
  .bs-error {
    display: flex;
    align-items: flex-start;
    gap: var(--space-2);
    font-size: var(--text-sm);
    color: var(--status-error);
    background: color-mix(in srgb, var(--status-error) 12%, transparent);
    border: 1px solid color-mix(in srgb, var(--status-error) 45%, var(--border));
    border-radius: var(--radius-sm);
    padding: var(--space-2);
  }
  .bs-error-icon {
    flex-shrink: 0;
    font-weight: 700;
    line-height: 1.4;
  }
  .bs-error-msg {
    flex: 1;
    min-width: 0;
    word-break: break-word;
  }
  .fld {
    display: flex;
    flex-direction: column;
    gap: 3px;
    flex: 1;
    min-width: 0;
  }
  .fld.narrow {
    flex: 0 0 140px;
  }
  .fld-row {
    display: flex;
    gap: var(--space-2);
  }
  .fld-label {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }
  .inp {
    width: 100%;
    padding: var(--space-1) var(--space-2);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    color: var(--fg-primary);
    font-size: var(--text-sm);
  }
  .bs-actions {
    display: flex;
    justify-content: flex-end;
    gap: var(--space-2);
  }
  .btn-cancel,
  .btn-act {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
    cursor: pointer;
    border: 1px solid var(--border);
  }
  .btn-cancel {
    background: var(--bg-secondary);
    color: var(--fg-secondary);
  }
  .btn-act {
    background: var(--accent);
    color: var(--accent-fg, #fff);
    border-color: var(--accent);
  }
  .btn-act:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
</style>
