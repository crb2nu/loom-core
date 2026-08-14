<script lang="ts">
  /**
   * SpinningRoomCard — the Spinning Room surfaced INLINE on the Mills Overview
   * (mirrors the iOS companion's Mills screen). The same capability the Plans
   * panel exposes as a header button + popover tray, but as a dashboard card:
   * a "⟳ Spin a plan" action plus the live + recent spins read from the durable
   * spin-runs store (frames, status with derived slow/stuck escalation, live
   * elapsed, brief, links to the draft plan(s), retry). Competitive spins show
   * as one entry that fans out to its sibling drafts.
   *
   * Reuses the shared store + pure helpers (spinRuns.svelte.ts /
   * spinRunsHelpers.ts) — only the dashboard-flavoured presentation is local.
   * Resulting drafts live in the Plan Store, so opening one routes to
   * Work → Plans; there is no board to refresh here.
   */
  import Badge from '../../widgets/Badge.svelte';
  import SpinPlanDialog from '../shared/SpinPlanDialog.svelte';
  import { spinRunsStore } from '../../stores/spinRuns.svelte.ts';
  import { clockStore } from '../../stores/staleness.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
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

  // The Mills operator is unreachable (proxy down / rolling) — hide the spin
  // action so the card degrades to a calm "unavailable" note like the rest of
  // the overview rather than offering a button that will just error.
  interface Props {
    disabled?: boolean;
  }
  let { disabled = false }: Props = $props();

  let now = $derived(clockStore.now);
  let visible = $derived(spinRunsStore.visible(now));

  // Spin dialog state. A fresh spin has a null seed; a Retry seeds brief +
  // frames + scope from the run it redoes.
  interface SpinSeed {
    brief: string;
    project: string;
    namespace: string;
    priority: string;
    label: string;
    frames: string[];
  }
  let showSpin = $state(false);
  let seed = $state<SpinSeed | null>(null);

  function openFreshSpin(): void {
    seed = null;
    showSpin = true;
  }

  function retry(run: SpinRun): void {
    seed = {
      brief: run.brief,
      project: run.project ?? '',
      namespace: run.namespace ?? '',
      priority: run.priority ?? '',
      label: 'Retry spin',
      frames: run.frames,
    };
    showSpin = true;
  }

  function onQueued(spin: { spinId: string; frames: string[] }): void {
    spinRunsStore.track(spin.spinId);
  }

  // Drafts land in the Plan Store — open them on the Work → Plans board.
  function openPlan(planId: string): void {
    router.navigate('tasks', 'plans', planId);
  }

  // Open the side-by-side compare + merge editor over a competitive spin's
  // sibling drafts.
  function comparePlans(planIds: string[]): void {
    router.navigateCompare(planIds);
  }

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

  // Poll the durable spin log while the overview is mounted.
  $effect(() => {
    spinRunsStore.start();
    return () => spinRunsStore.stop();
  });
</script>

<section class="spin-card" aria-label="Spinning Room">
  <div class="spin-head">
    <div class="head-text">
      <span class="spin-title">Spinning Room</span>
      <span class="spin-sub">
        {#if spinRunsStore.liveCount > 0}{spinRunsStore.liveCount} live{:else}idle{/if}
      </span>
    </div>
    <button
      type="button"
      class="spin-btn"
      onclick={openFreshSpin}
      disabled={disabled}
      title="Hand a frame a brief — it spins a draft plan into the Plan Store"
    >
      ⟳ Spin a plan
    </button>
  </div>
  <p class="spin-copy">Hand a frame a brief — it spins a draft plan onto the board.</p>

  {#if !spinRunsStore.available}
    <div class="spin-empty">Async spins need the operator update (still deploying).</div>
  {:else if visible.length === 0}
    <div class="spin-empty">No spins yet — hand a frame a brief to spin a draft plan.</div>
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
                  <button class="draft-link" onclick={() => openPlan(pid)} title="Open this draft on the Plans board">{pid}</button>
                {/each}
                {#if run.competitive && run.plan_ids.length > 1}
                  <button class="compare-link" onclick={() => comparePlans(run.plan_ids)} title="Compare all {run.plan_ids.length} drafts side by side + merge">⚖ Compare all {run.plan_ids.length}</button>
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
              <button class="retry-btn" onclick={() => retry(run)} title="Re-spin from this brief + frames">⟳ Retry</button>
            </div>
          {/if}
        </li>
      {/each}
    </ul>
  {/if}
</section>

<SpinPlanDialog open={showSpin} onClose={() => { showSpin = false; seed = null; }} {onQueued} {seed} />

<style>
  .spin-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding: var(--space-4);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background:
      linear-gradient(180deg, rgba(255, 255, 255, 0.018), transparent 48%),
      var(--bg-secondary);
    min-width: 0;
  }
  .spin-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }
  .head-text {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    min-width: 0;
  }
  .spin-title {
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .spin-sub {
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }
  .spin-btn {
    border: 1px solid var(--border-focus);
    background: transparent;
    color: var(--accent);
    border-radius: var(--radius-sm);
    padding: 4px var(--space-3);
    font: inherit;
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    cursor: pointer;
    white-space: nowrap;
  }
  .spin-btn:hover:not(:disabled) {
    background: var(--accent-dim);
  }
  .spin-btn:disabled {
    opacity: 0.5;
    cursor: default;
  }
  .spin-copy {
    margin: 0;
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
  }
  .spin-empty {
    padding: var(--space-3);
    color: var(--fg-muted);
    font-size: var(--text-sm);
    text-align: center;
    border: 1px dashed var(--border-subtle);
    border-radius: var(--radius-sm);
  }
  .run-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }
  .run {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: var(--space-2);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--bg-tertiary) 68%, transparent);
  }
  .run.live {
    border-color: color-mix(in srgb, var(--accent) 32%, var(--border-subtle));
    background: color-mix(in srgb, var(--accent) 5%, transparent);
  }
  .run.stuck {
    border-color: color-mix(in srgb, var(--status-error) 42%, var(--border-subtle));
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
    animation: spin-card-pulse 1.2s ease-in-out infinite;
  }
  .live-dot.stuck {
    background: var(--status-error);
  }
  @keyframes spin-card-pulse {
    0%, 100% { opacity: 0.35; }
    50% { opacity: 1; }
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
</style>
