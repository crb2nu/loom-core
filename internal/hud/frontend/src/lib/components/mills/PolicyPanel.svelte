<script lang="ts">
  /**
   * PolicyPanel — the Mills ▸ Policy tab: adaptive policy proposals emitted by
   * the Sunday job. Reads /api/mills/policy/proposals via the shared MillsStore
   * so proposals refresh at the standard 15s cadence; Apply/Reject hit
   * POST /api/mills/policy/proposals/{id}/{apply|reject}.
   *
   * Named PolicyProposalsCard until it was promoted to a first-class panel:
   * it is registered as a top-level Mills tab, so it now follows the sibling
   * panel conventions end to end — PanelShell header/count/empty/error states
   * (already in place) plus the shared ConfirmDialog + runAdminAction path for
   * mutations. It was the last Mills surface still confirming a policy write
   * with a raw globalThis.confirm() and reporting the outcome ad hoc.
   */
  import { millsStore } from '../../stores/mills.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import { runAdminAction } from './shared/millsActions.ts';
  import { createPoller } from '../../utils/poller.ts';

  // This card owns its refresh: no other surface fans out to
  // /api/mills/policy/proposals, so a mount-only fetch left the list frozen
  // at first paint until the tab was reloaded. createPoller adds the standard
  // 15s Mills cadence with visibility-pause and an overlap guard; it fires no
  // initial tick, so the explicit first fetch stays.
  $effect(() => {
    void millsStore.fetchPolicyProposals();
    const poller = createPoller(() => millsStore.fetchPolicyProposals(), 15000);
    poller.start();
    return () => poller.stop();
  });

  let proposals = $derived(millsStore.policyProposals);
  let disabled = $derived(millsStore.disabled);
  let loading = $derived(millsStore.loading && millsStore.policyProposals.length === 0);
  let error = $derived(millsStore.error);

  // Pending confirmation. Both verbs mutate policy, so both are stray-click
  // guarded — through the shared ConfirmDialog, matching every other Mills
  // mutation surface, rather than a native confirm() box.
  let pending = $state<{ id: number; verb: 'apply' | 'reject' } | null>(null);
  let busy = $state<number | null>(null);

  const CONFIRM_COPY = {
    apply: {
      title: 'Apply this proposal?',
      message: 'Applies the diff to live policy. It can be reverted within 24h.',
      confirmLabel: 'Apply',
      variant: 'warn' as const,
    },
    reject: {
      title: 'Reject this proposal?',
      message: 'Discards the proposal. The Sunday job may re-emit it next week.',
      confirmLabel: 'Reject',
      variant: 'danger' as const,
    },
  };

  async function runPending(): Promise<void> {
    const req = pending;
    pending = null;
    if (!req) return;
    busy = req.id;
    await runAdminAction(
      () =>
        req.verb === 'apply'
          ? millsStore.applyPolicyProposal(req.id)
          : millsStore.rejectPolicyProposal(req.id),
      {
        success: req.verb === 'apply' ? `Proposal ${req.id} applied` : `Proposal ${req.id} rejected`,
        failurePrefix: req.verb === 'apply' ? 'Apply proposal failed' : 'Reject proposal failed',
      },
    );
    busy = null;
  }
</script>

<PanelShell
  title="Policy proposals"
  icon="⚙"
  count={proposals.length}
  loading={loading}
  error={!disabled && error && proposals.length === 0 ? error : null}
  errorHeading="Couldn't load policy proposals"
  empty={!error && proposals.length === 0}
  emptyIcon={disabled ? '◯' : '✓'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'No pending proposals'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'The Sunday adaptive job emits proposals when policy looks too tight or too loose.'}
  emptyTone={disabled ? 'disabled' : 'ready'}
>
  {#if error && proposals.length > 0}
    <ErrorBanner prefix="Policy proposals refresh failed" message={error} />
  {/if}

  <ul class="proposals">
    {#each proposals as p (p.ID)}
      <li class="proposal kind-{p.Kind}">
        <header class="proposal-header">
          <span class="kind kind-{p.Kind}">{p.Kind}</span>
          <span class="target mono">{p.Target}</span>
          <span class="date">{p.ProposalDate}</span>
        </header>
        <pre class="diff mono">{p.Diff}</pre>
        <p class="rationale">{p.Rationale}</p>
        <footer class="proposal-actions">
          <button
            type="button"
            class="apply"
            disabled={busy === p.ID}
            onclick={() => (pending = { id: p.ID, verb: 'apply' })}
          >{busy === p.ID ? 'Working…' : 'Apply'}</button>
          <button
            type="button"
            class="reject"
            disabled={busy === p.ID}
            onclick={() => (pending = { id: p.ID, verb: 'reject' })}
          >Reject</button>
        </footer>
      </li>
    {/each}
  </ul>
</PanelShell>

<ConfirmDialog
  open={pending !== null}
  title={pending ? CONFIRM_COPY[pending.verb].title : ''}
  message={pending ? CONFIRM_COPY[pending.verb].message : ''}
  confirmLabel={pending ? CONFIRM_COPY[pending.verb].confirmLabel : 'Confirm'}
  variant={pending ? CONFIRM_COPY[pending.verb].variant : 'default'}
  onConfirm={runPending}
  onCancel={() => (pending = null)}
/>

<style>
  .proposals {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }
  .proposal {
    border: 1px solid var(--border-subtle);
    border-left-width: 3px;
    border-radius: var(--radius-sm);
    padding: 0.6rem 0.75rem;
    background: var(--bg-subtle);
  }
  /* Kind colors mirror the semantic intent of the Sunday job. */
  .proposal.kind-relax           { border-left-color: var(--success); }
  .proposal.kind-tighten         { border-left-color: var(--warning); }
  .proposal.kind-rotate_ensemble { border-left-color: var(--tier-short); }

  .proposal-header {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: var(--text-xs);
    margin-bottom: 0.35rem;
  }
  .kind {
    padding: 0.05rem 0.45rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-weight: 600;
    text-transform: lowercase;
    letter-spacing: 0.02em;
  }
  .kind-relax           { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .kind-tighten         { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .kind-rotate_ensemble { background: color-mix(in srgb, var(--tier-short) 15%, transparent); color: var(--tier-short); }
  .target { color: var(--text); flex: 1; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .date { color: var(--text-muted); font-size: var(--text-2xs); }

  .mono { font-family: ui-monospace, monospace; }
  .diff {
    margin: 0 0 0.4rem 0;
    padding: 0.45rem 0.6rem;
    background: rgba(0, 0, 0, 0.25);
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    color: var(--text);
    white-space: pre-wrap;
    word-break: break-word;
    max-height: 8rem;
    overflow-y: auto;
  }
  .rationale {
    margin: 0 0 0.5rem 0;
    color: var(--text-muted);
    font-size: var(--text-xs);
    line-height: 1.35;
  }
  .proposal-actions {
    display: flex;
    gap: 0.4rem;
    justify-content: flex-end;
  }
  button {
    padding: 0.25rem 0.7rem;
    border-radius: var(--radius-xs);
    border: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
    cursor: pointer;
    background: var(--bg-subtle);
    color: var(--text);
    transition: background 0.12s ease, border-color 0.12s ease;
  }
  button:hover:not(:disabled) { border-color: var(--border); }
  button:disabled { opacity: 0.6; cursor: progress; }
  button.apply  { background: rgba(var(--success-rgb), 0.18); color: var(--success); border-color: rgba(var(--success-rgb), 0.35); }
  button.apply:hover  { background: rgba(var(--success-rgb), 0.28); }
  button.reject { background: rgba(var(--error-rgb), 0.15);  color: var(--error); border-color: rgba(var(--error-rgb), 0.35); }
  button.reject:hover { background: rgba(var(--error-rgb), 0.25); }
</style>
