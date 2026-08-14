<script lang="ts">
  /**
   * SpinningRoomTray — a popover that surfaces the live + recent async spins
   * from the operator's DURABLE spin-runs store. Replaces the old bare
   * "N spinning…" chip: each spin shows its frames, a status badge with derived
   * slow/stuck escalation, live elapsed time, the brief, and — once it lands —
   * links to the draft plan(s) it authored. A competitive spin shows as ONE
   * entry that fans out to its sibling drafts, so the operator sees "one spin →
   * two competing drafts" instead of two disconnected cards on the board.
   *
   * Anchored under its trigger by the parent's positioned wrapper; a transparent
   * fixed backdrop closes it on outside click.
   */
  import Badge from '../../widgets/Badge.svelte';
  import { focusTrap } from '../../actions/focusTrap';
  import { spinRunsStore } from '../../stores/spinRuns.svelte.ts';
  import { clockStore } from '../../stores/staleness.svelte.ts';
  import {
    spinPhase,
    spinPhaseLabel,
    spinPhaseVariant,
    elapsedMs,
    formatElapsed,
    briefHeadline,
    isLive,
    SPIN_STUCK_AFTER_MS,
    type SpinRun,
  } from '../../utils/spinRunsHelpers.ts';

  const STUCK_MIN = Math.round(SPIN_STUCK_AFTER_MS / 60000);

  interface Props {
    open: boolean;
    onClose: () => void;
    /** Open a resulting draft plan in the board drawer. */
    onOpenPlan: (planId: string) => void;
    /** Re-spin from a failed/timed-out/stuck run (seeds the Spin dialog). */
    onRetry: (run: SpinRun) => void;
    /** Open the compare/merge editor over a competitive spin's sibling drafts. */
    onCompare?: (planIds: string[]) => void;
  }
  let { open, onClose, onOpenPlan, onRetry, onCompare }: Props = $props();

  // Live clock (global 5s tick) drives elapsed + slow/stuck escalation without a
  // local timer.
  let now = $derived(clockStore.now);
  let visible = $derived(spinRunsStore.visible(now));

  function phaseOf(run: SpinRun) {
    return spinPhase(run, now);
  }
  function elapsedLabel(run: SpinRun): string {
    return formatElapsed(elapsedMs(run, now));
  }
  function canRetry(run: SpinRun): boolean {
    const p = phaseOf(run);
    return p === 'failed' || p === 'timeout' || p === 'stuck';
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onClose();
  }
</script>

<svelte:window onkeydown={open ? handleKeydown : undefined} />

{#if open}
  <!-- The tray's own Escape handler is the keyboard path off this backdrop. -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <!-- svelte-ignore a11y_click_events_have_key_events -->
  <div class="tray-backdrop" onclick={onClose}></div>
  <div class="tray" role="dialog" aria-modal="true" aria-label="Spinning Room status" use:focusTrap>
    <div class="tray-head">
      <span class="tray-title">Spinning Room</span>
      <span class="tray-sub">
        {#if spinRunsStore.liveCount > 0}
          {spinRunsStore.liveCount} live
        {:else}
          idle
        {/if}
      </span>
      <button class="tray-x" onclick={onClose} aria-label="Close">✕</button>
    </div>

    {#if !spinRunsStore.available}
      <div class="tray-empty">Async spins need the operator update (still deploying).</div>
    {:else if visible.length === 0}
      <div class="tray-empty">
        No spins yet. Hit <strong>⟳ Spin a plan</strong> to hand a brief to a model frame.
      </div>
    {:else}
      <ul class="run-list">
        {#each visible as run (run.id)}
          {@const phase = phaseOf(run)}
          <li class="run" class:live={isLive(run)} class:stuck={phase === 'stuck'}>
            <div class="run-top">
              <Badge text={spinPhaseLabel(phase)} variant={spinPhaseVariant(phase)} />
              <span class="run-frames">
                {#each run.frames as f}<span class="frame-chip">{f}</span>{/each}
                {#if run.competitive}<span class="competitive-tag" title="Competitive spin — one draft per frame">⚔ competing</span>{/if}
              </span>
              <span class="run-elapsed" title={isLive(run) ? 'Running time' : 'Total duration'}>
                {#if isLive(run)}<span class="live-dot" class:stuck={phase === 'stuck'}></span>{/if}
                {elapsedLabel(run)}
              </span>
            </div>

            <div class="run-brief" title={run.brief}>{briefHeadline(run.brief)}</div>

            {#if phase === 'stuck'}
              <div class="run-note warn">
                ⚠ Running past {STUCK_MIN}m — likely wedged (the operator caps a spin at 10m). Retry if it doesn't land.
              </div>
            {/if}

            {#if run.status === 'succeeded' && run.plan_ids.length > 0}
              <div class="run-drafts">
                <span class="drafts-label">
                  → {run.plan_ids.length} draft{run.plan_ids.length === 1 ? '' : 's'}{run.competitive ? ' (compare + advance the winner)' : ''}
                </span>
                <span class="drafts-links">
                  {#each run.plan_ids as pid}
                    <button class="draft-link" onclick={() => { onOpenPlan(pid); onClose(); }} title="Open this draft on the board">{pid}</button>
                  {/each}
                  {#if run.competitive && run.plan_ids.length > 1 && onCompare}
                    <button class="compare-link" onclick={() => { onCompare!(run.plan_ids); onClose(); }} title="Compare all {run.plan_ids.length} drafts side by side + merge">⚖ Compare all {run.plan_ids.length}</button>
                  {/if}
                </span>
              </div>
            {/if}

            {#if run.error && (phase === 'failed' || phase === 'timeout')}
              <div class="run-note err">{run.error}</div>
            {:else if run.error && run.status === 'succeeded'}
              <div class="run-note warn">{run.error}</div>
            {/if}

            {#if canRetry(run)}
              <div class="run-actions">
                <button class="retry-btn" onclick={() => { onRetry(run); onClose(); }} title="Re-spin from this brief + frames">
                  ⟳ Retry
                </button>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}

    <div class="tray-foot">
      Reads the operator's durable spin log — survives a refresh and shows spins from any session.
    </div>
  </div>
{/if}

<style>
  .tray-backdrop {
    position: fixed;
    inset: 0;
    z-index: 998;
    background: transparent;
  }
  .tray {
    position: absolute;
    top: calc(100% + 6px);
    right: 0;
    z-index: 999;
    width: 380px;
    max-width: min(380px, 92vw);
    max-height: 60vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow: 0 8px 28px rgba(0, 0, 0, 0.45);
    overflow: hidden;
    /* focusTrap's fallback focus lands on the container; no stray ring. */
    outline: none;
  }
  .tray-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }
  .tray-title {
    font-weight: 600;
    color: var(--fg-primary);
    font-size: var(--text-sm);
  }
  .tray-sub {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
  }
  .tray-x {
    margin-left: auto;
    background: none;
    border: none;
    color: var(--fg-secondary);
    cursor: pointer;
    font-size: var(--text-sm);
    line-height: 1;
    padding: 2px 4px;
  }
  .tray-x:hover {
    color: var(--fg-primary);
  }
  .tray-empty {
    padding: var(--space-4) var(--space-3);
    color: var(--fg-secondary);
    font-size: var(--text-sm);
    text-align: center;
  }
  .run-list {
    list-style: none;
    margin: 0;
    padding: 0;
    overflow-y: auto;
  }
  .run {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }
  .run:last-child {
    border-bottom: none;
  }
  .run.live {
    background: color-mix(in srgb, var(--accent) 5%, transparent);
  }
  .run.stuck {
    background: color-mix(in srgb, var(--status-error) 8%, transparent);
  }
  .run-top {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }
  .run-frames {
    display: flex;
    gap: 4px;
    flex-wrap: wrap;
    align-items: center;
    min-width: 0;
  }
  .frame-chip {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    padding: 0 5px;
  }
  .competitive-tag {
    font-size: var(--text-xs);
    color: var(--accent);
    font-family: var(--font-mono);
  }
  .run-elapsed {
    margin-left: auto;
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    white-space: nowrap;
  }
  .live-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--accent);
    animation: tray-pulse 1.2s ease-in-out infinite;
  }
  .live-dot.stuck {
    background: var(--status-error);
  }
  @keyframes tray-pulse {
    0%,
    100% {
      opacity: 0.35;
    }
    50% {
      opacity: 1;
    }
  }
  .run-brief {
    font-size: var(--text-sm);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .run-note {
    font-size: var(--text-xs);
    line-height: 1.4;
  }
  .run-note.err {
    color: var(--status-error);
  }
  .run-note.warn {
    color: var(--warning);
  }
  .run-drafts {
    display: flex;
    flex-direction: column;
    gap: 3px;
    font-size: var(--text-xs);
  }
  .drafts-label {
    color: var(--fg-secondary);
  }
  .drafts-links {
    display: flex;
    flex-wrap: wrap;
    gap: 4px;
  }
  .draft-link {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 1px 6px;
    background: color-mix(in srgb, var(--accent) 12%, var(--bg-tertiary));
    border: 1px solid color-mix(in srgb, var(--accent) 35%, var(--border));
    border-radius: var(--radius-sm);
    color: var(--accent);
    cursor: pointer;
  }
  .draft-link:hover {
    background: color-mix(in srgb, var(--accent) 22%, var(--bg-tertiary));
  }
  .compare-link {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 1px 6px;
    background: var(--accent-dim);
    border: 1px solid color-mix(in srgb, var(--accent) 45%, var(--border));
    border-radius: var(--radius-sm);
    color: var(--accent);
    cursor: pointer;
    font-weight: 600;
  }
  .compare-link:hover {
    background: color-mix(in srgb, var(--accent) 26%, var(--bg-tertiary));
  }
  .run-actions {
    display: flex;
    justify-content: flex-end;
  }
  .retry-btn {
    background: transparent;
    border: 1px solid var(--border);
    color: var(--fg-secondary);
    border-radius: var(--radius-sm);
    padding: 1px 8px;
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    cursor: pointer;
  }
  .retry-btn:hover {
    border-color: var(--accent);
    color: var(--accent);
  }
  .tray-foot {
    padding: var(--space-1) var(--space-3) var(--space-2);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    font-style: italic;
    border-top: 1px solid var(--border-subtle);
  }
</style>
