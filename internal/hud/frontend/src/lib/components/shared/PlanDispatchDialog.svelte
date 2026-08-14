<script lang="ts">
  /**
   * PlanDispatchDialog — the unified "hand off" chooser for a plan or a single
   * slice (Work → Plans). One affordance, three destinations:
   *   • Spawn a new agent session (its own worktree).
   *   • Hand to an existing live agent (POST /api/handoffs → their inbox).
   *   • Run autonomously in Mills (confirm-gated by the caller; spends budget).
   * Pure presentation: every action is a callback the PlansPanel owns, so this
   * component never touches a store or the network itself.
   */
  import { focusTrap } from '../../actions/focusTrap';
  import type { Plan, PlanSlice } from '../../utils/plansHelpers';

  interface DispatchAgent {
    agent_id: string;
    status?: string;
    agent_type?: string;
  }

  interface Props {
    open: boolean;
    target: { plan: Plan; slice?: PlanSlice } | null;
    agents?: DispatchAgent[];
    busy?: boolean;
    onSpawn: () => void;
    onHandoff: (targetAgentId: string) => void;
    onMills: () => void;
    onClose: () => void;
  }

  let {
    open,
    target,
    agents = [],
    busy = false,
    onSpawn,
    onHandoff,
    onMills,
    onClose,
  }: Props = $props();

  // Selected handoff target. Seed it from the first live agent on open (and
  // once agents arrive after an async fleet fetch), but never clobber a pick the
  // user already made while the dialog is open.
  let pickedAgent = $state('');
  $effect(() => {
    if (!open) return;
    if (!pickedAgent || !agents.some((a) => a.agent_id === pickedAgent)) {
      pickedAgent = agents[0]?.agent_id ?? '';
    }
  });

  let subtitle = $derived(
    target
      ? target.slice
        ? `${target.plan.title} · slice: ${target.slice.name}`
        : target.plan.title
      : '',
  );
  let unit = $derived(target?.slice ? 'slice' : 'plan');

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onClose();
  }
  function handleBackdropClick(event: MouseEvent): void {
    if ((event.target as HTMLElement)?.classList?.contains('dispatch-backdrop')) onClose();
  }
  function doHandoff(): void {
    const a = pickedAgent.trim();
    if (a) onHandoff(a);
  }
</script>

{#if open && target}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div class="dispatch-backdrop" onkeydown={handleKeydown} onclick={handleBackdropClick}>
    <div
      class="dispatch-dialog"
      role="dialog"
      aria-modal="true"
      aria-labelledby="dispatch-title"
      use:focusTrap
    >
      <div class="dispatch-head">
        <div class="dispatch-title" id="dispatch-title">Hand off work</div>
        <div class="dispatch-sub">{subtitle}</div>
      </div>

      <section class="opt">
        <div class="opt-text">
          <div class="opt-name">⟳ Spawn a new session</div>
          <div class="opt-desc">Start a fresh agent on its own worktree, scoped to this {unit}.</div>
        </div>
        <button class="btn-act" disabled={busy} onclick={onSpawn}>Spawn</button>
      </section>

      <section class="opt">
        <div class="opt-text">
          <div class="opt-name">→ Hand to an existing agent</div>
          <div class="opt-desc">Route a brief to a running agent's handoff inbox.</div>
        </div>
        {#if agents.length}
          <div class="opt-controls">
            <select class="inp" bind:value={pickedAgent} disabled={busy} aria-label="Target agent">
              {#each agents as a}
                <option value={a.agent_id}>{a.agent_id}{a.status ? ` (${a.status})` : ''}</option>
              {/each}
            </select>
            <button class="btn-act" disabled={busy || !pickedAgent} onclick={doHandoff}>Hand off</button>
          </div>
        {:else}
          <span class="opt-empty">No live agents — spawn instead.</span>
        {/if}
      </section>

      <section class="opt">
        <div class="opt-text">
          <div class="opt-name">❖ Run in Mills</div>
          <div class="opt-desc warn">Autonomous pipeline — spends budget, may open &amp; merge an MR.</div>
        </div>
        <button class="btn-act warn" disabled={busy} onclick={onMills}>Run…</button>
      </section>

      <div class="dispatch-actions">
        <button class="btn-cancel" onclick={onClose}>Close</button>
      </div>
    </div>
  </div>
{/if}

<style>
  .dispatch-backdrop {
    position: fixed;
    inset: 0;
    z-index: 9999;
    display: flex;
    align-items: center;
    justify-content: center;
    background: var(--scrim);
    backdrop-filter: blur(2px);
  }

  .dispatch-dialog {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    max-width: 440px;
    width: 92%;
    padding: var(--space-4);
    background: var(--bg-primary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow: 0 8px 32px rgba(0, 0, 0, 0.4);
  }

  .dispatch-head {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin-bottom: var(--space-1);
  }
  .dispatch-title {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
  }
  .dispatch-sub {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .opt {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
  }
  .opt-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    flex: 1;
    min-width: 0;
  }
  .opt-name {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
  }
  .opt-desc {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    line-height: 1.4;
  }
  .opt-desc.warn {
    color: var(--warning);
  }
  .opt-controls {
    display: flex;
    gap: var(--space-1);
    align-items: center;
  }
  .opt-empty {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-style: italic;
  }

  .inp {
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    color: var(--fg-primary);
    border-radius: var(--radius-sm);
    padding: 4px 8px;
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    max-width: 160px;
  }

  .btn-act,
  .btn-cancel {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-xs);
    font-size: var(--text-sm);
    font-family: var(--font-mono);
    cursor: pointer;
    white-space: nowrap;
    transition: background var(--transition-fast), border-color var(--transition-fast);
  }
  .btn-act {
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--accent);
  }
  .btn-act:hover:not(:disabled) {
    background: var(--accent-dim);
  }
  .btn-act.warn {
    border-color: color-mix(in srgb, var(--warning) 40%, var(--border));
    color: var(--warning);
  }
  .btn-act.warn:hover:not(:disabled) {
    background: var(--warning-dim);
    border-color: var(--warning);
  }
  .btn-act:disabled {
    opacity: 0.5;
    cursor: default;
  }

  .dispatch-actions {
    display: flex;
    justify-content: flex-end;
    margin-top: var(--space-1);
  }
  .btn-cancel {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-secondary);
  }
  .btn-cancel:hover {
    border-color: var(--border-active);
    color: var(--fg-primary);
  }
</style>
