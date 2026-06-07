<script lang="ts">
  // WorkflowRunDetail — right-edge drawer rendering the durable step-log
  // timeline for the selected workflow run (plan .loom/134 §S4b, the S1c
  // observation surface). Driven entirely off millsStore.selectedWorkflowID
  // + millsStore.openWorkflowDetail so it stays reactive to the panel's
  // background refresh ticks. Read-only: the workflow endpoints are GET-only,
  // so there are no actions here (contrast PipelineRunDetail's escalate).
  //
  // The timeline is the point of this view. Each step shows its event_type +
  // status, a colour-coded badge (the server-derived replay/live/quarantined
  // hint), cost WITH provenance (real / estimated / unavailable — never
  // blended), and the spawn / effect / timing facts an operator needs to
  // judge "did this step actually run, or replay from the journal?".

  import {
    millsStore,
    type WorkflowStep,
    type WorkflowStepBadge,
  } from '../../stores/mills.svelte.ts';

  let load = $derived(millsStore.openWorkflowDetail);
  let open = $derived(millsStore.selectedWorkflowID !== null);
  let detail = $derived(load && load.status === 'loaded' ? load.detail : null);

  function close(): void {
    millsStore.closeWorkflowDetail();
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape' && open) close();
  }

  function fmtTime(ts?: string | null): string {
    if (!ts) return '—';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toLocaleTimeString();
  }

  function fmtDuration(start?: string | null, end?: string | null): string {
    if (!start) return '—';
    const s = new Date(start).getTime();
    if (isNaN(s)) return '—';
    const e = end ? new Date(end).getTime() : Date.now();
    if (isNaN(e) || e < s) return '—';
    const ms = e - s;
    if (ms < 1000) return `${ms}ms`;
    if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
    const mins = Math.floor(ms / 60_000);
    const secs = Math.floor((ms % 60_000) / 1000);
    return `${mins}m ${secs}s`;
  }

  // fmtCost mirrors the operator's 4-decimal journal precision so a sub-cent
  // step doesn't round to $0. Display-only — provenance is rendered
  // separately (see costSourceLabel) and never folded into this number.
  function fmtCost(usd?: number): string {
    if (usd == null || !Number.isFinite(usd)) return '—';
    if (usd === 0) return '$0';
    if (usd < 0.0001) return '<$0.0001';
    return `$${usd.toFixed(4)}`;
  }

  // costSourceLabel surfaces provenance VERBATIM. An estimated cost is not a
  // real one and must never be implied as such — when the source is unknown
  // or unavailable we say so plainly rather than dressing the number up.
  function costSourceLabel(src?: string): string {
    switch (src) {
      case 'real':
        return 'real';
      case 'estimated':
        return 'estimated';
      case 'unavailable':
        return 'unavailable';
      case undefined:
      case '':
        return 'unspecified';
      default:
        return src;
    }
  }

  // badgeLabel is the human caption for the server-derived render hint.
  // cache_hit reads as "replayed" — a memoized step the durable journal
  // short-circuited on resume (effect_count == 0), the replay==cache-hit
  // signal §S4 calls for.
  function badgeLabel(badge: string): string {
    switch (badge as WorkflowStepBadge) {
      case 'quarantined':
        return 'quarantined';
      case 'failed':
        return 'failed';
      case 'pending':
        return 'pending';
      case 'live':
        return 'live';
      case 'cache_hit':
        return 'replayed';
      default:
        return badge || 'unknown';
    }
  }

  function badgeTitle(badge: string): string {
    switch (badge as WorkflowStepBadge) {
      case 'quarantined':
        return 'Run quarantined for nondeterminism (call_hash mismatch) — every step is flagged.';
      case 'failed':
        return 'Step failed (error or gate_fail).';
      case 'pending':
        return 'Record-before-result row whose effect has not completed (or was interrupted).';
      case 'live':
        return 'Step ran a live side effect this execution (effect_count > 0).';
      case 'cache_hit':
        return 'Replayed from the durable journal — no live side effect this run (effect_count == 0).';
      default:
        return 'Step badge';
    }
  }

  // A run carries a quarantine banner if any of its steps came back flagged
  // (the operator stamps every step under a quarantined run). Surfaced at
  // the top so an operator reads "this whole run is suspect" before scanning.
  let runQuarantined = $derived(
    !!detail && detail.steps.some((s) => s.badge === 'quarantined'),
  );

  function copyRunID(): void {
    const id = detail?.run.id;
    if (!id) return;
    void navigator.clipboard?.writeText(id).catch(() => {
      /* swallow — clipboard perms aren't worth a toast */
    });
  }

  // stepDotClass / stepBadgeClass map the badge to its colour family. Kept
  // as a single mapping so the timeline dot, the rail segment, and the badge
  // pill always agree on the colour.
  function badgeKind(badge: string): string {
    switch (badge as WorkflowStepBadge) {
      case 'quarantined':
      case 'failed':
        return 'danger';
      case 'pending':
        return 'warn';
      case 'live':
        return 'live';
      case 'cache_hit':
        return 'muted';
      default:
        return 'muted';
    }
  }

  function stepEffectHint(s: WorkflowStep): string {
    if (s.effect_count === 0) return 'no live effect (replay / memoized)';
    if (s.effect_count === 1) return '1 effect';
    return `${s.effect_count} effects`;
  }
</script>

<svelte:window onkeydown={handleKeydown} />

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
  <div class="wf-scrim" onclick={close}></div>
  <aside class="wf-drawer" role="dialog" aria-label="Workflow run detail" aria-modal="true">
    <header class="wf-header">
      <div class="wf-title">
        <span class="wf-kicker">Workflow Run</span>
        {#if detail}
          <span class="wf-state state-{detail.run.state}">{detail.run.state}</span>
        {/if}
      </div>
      <button type="button" class="wf-close" onclick={close} aria-label="Close workflow detail">✕</button>
    </header>

    {#if load?.status === 'loading' && !detail}
      <div class="wf-loading">Loading step timeline…</div>
    {:else if load?.status === 'error'}
      <div class="wf-error">
        <div class="wf-error-title">Couldn't load workflow run</div>
        <div class="wf-error-msg">{load.message}</div>
        <button
          type="button"
          class="wf-retry"
          onclick={() => millsStore.openWorkflowRunDetail(millsStore.selectedWorkflowID ?? '')}
        >
          Retry
        </button>
      </div>
    {:else if detail}
      {#if runQuarantined}
        <div class="wf-quarantine-banner" role="alert">
          <span class="wf-quarantine-glyph" aria-hidden="true">⚠</span>
          <div>
            <div class="wf-quarantine-title">Run quarantined</div>
            <div class="wf-quarantine-sub">
              A call-hash mismatch flagged this run for nondeterminism. Every step
              below is shaded; the journal is not safe to replay as-is.
            </div>
          </div>
        </div>
      {/if}

      <section class="wf-summary">
        <dl class="wf-meta">
          <div class="wf-meta-row">
            <dt>Run ID</dt>
            <dd class="mono wf-id">
              <span title={detail.run.id}>{detail.run.id}</span>
              <button type="button" class="wf-copy" onclick={copyRunID} title="Copy run id">⧉</button>
            </dd>
          </div>
          <div class="wf-meta-row">
            <dt>Engine</dt>
            <dd class="mono">{detail.run.engine || '—'}</dd>
          </div>
          <div class="wf-meta-row">
            <dt>Template</dt>
            <dd class="mono">{detail.run.template || '—'}</dd>
          </div>
          {#if detail.run.backlog_id}
            <div class="wf-meta-row">
              <dt>Backlog</dt>
              <dd class="mono">{detail.run.backlog_id}</dd>
            </div>
          {/if}
          <div class="wf-meta-row">
            <dt>Started</dt>
            <dd class="mono">{fmtTime(detail.run.started_at)}</dd>
          </div>
          <div class="wf-meta-row">
            <dt>Ended</dt>
            <dd class="mono">{fmtTime(detail.run.ended_at)}</dd>
          </div>
          <div class="wf-meta-row">
            <dt>Duration</dt>
            <dd class="mono">{fmtDuration(detail.run.started_at, detail.run.ended_at)}</dd>
          </div>
          <div class="wf-meta-row">
            <dt>Total cost</dt>
            <dd class="mono cost">{fmtCost(detail.run.cost_usd)}</dd>
          </div>
          <div class="wf-meta-row">
            <dt>Steps</dt>
            <dd class="mono">{detail.run.step_count ?? detail.steps.length}</dd>
          </div>
        </dl>
      </section>

      <section class="wf-section">
        <h3 class="wf-section-title">
          Step Timeline
          <span class="section-count">{detail.steps.length}</span>
        </h3>
        {#if detail.steps.length === 0}
          <div class="section-empty">No steps journaled yet for this run.</div>
        {:else}
          <ol class="timeline">
            {#each detail.steps as step (step.id)}
              {@const kind = badgeKind(step.badge)}
              <li class="tl-step" data-kind={kind}>
                <div class="tl-rail" aria-hidden="true">
                  <span class="tl-dot kind-{kind}"></span>
                </div>
                <div class="tl-card">
                  <div class="tl-head">
                    <span class="tl-event mono">{step.event_type}</span>
                    <span class="tl-status status-{step.status}">{step.status}</span>
                    <span class="tl-badge kind-{kind}" title={badgeTitle(step.badge)}>
                      {badgeLabel(step.badge)}
                    </span>
                  </div>
                  <div class="tl-key mono" title="step key">{step.step_key}</div>
                  <div class="tl-facts">
                    <span class="fact">
                      <span class="fact-label">cost</span>
                      <span class="mono cost">{fmtCost(step.cost_usd)}</span>
                      <span class="cost-src src-{costSourceLabel(step.cost_source)}" title="cost provenance — never blended with real cost">
                        {costSourceLabel(step.cost_source)}
                      </span>
                    </span>
                    <span class="fact">
                      <span class="fact-label">effects</span>
                      <span class="mono">{step.effect_count}</span>
                      <span class="fact-hint">{stepEffectHint(step)}</span>
                    </span>
                    {#if step.spawn_id}
                      <span class="fact">
                        <span class="fact-label">spawn</span>
                        <span class="mono spawn" title={step.spawn_id}>{step.spawn_id}</span>
                      </span>
                    {/if}
                    <span class="fact">
                      <span class="fact-label">when</span>
                      <span class="mono">{fmtTime(step.started_at)} → {fmtTime(step.ended_at)}</span>
                      <span class="fact-hint">{fmtDuration(step.started_at, step.ended_at)}</span>
                    </span>
                  </div>
                  {#if step.call_hash}
                    <div class="tl-hash mono" title="memoization key (call_hash) — replay is keyed on this">
                      <span class="fact-label">call_hash</span> {step.call_hash}
                    </div>
                  {/if}
                </div>
              </li>
            {/each}
          </ol>
        {/if}
      </section>
    {/if}
  </aside>
{/if}

<style>
  .wf-scrim {
    position: fixed;
    inset: 0;
    background: rgba(6, 12, 16, 0.55);
    z-index: 900;
    animation: fadeIn 0.15s ease-out;
  }

  .wf-drawer {
    position: fixed;
    top: 0;
    right: 0;
    bottom: 0;
    width: min(680px, 96vw);
    background: var(--bg-secondary);
    border-left: 1px solid var(--border);
    box-shadow: -16px 0 32px rgba(0, 0, 0, 0.4);
    z-index: 901;
    display: flex;
    flex-direction: column;
    animation: slideIn 0.18s ease-out;
    overflow-y: auto;
  }

  .wf-header {
    position: sticky;
    top: 0;
    z-index: 1;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
    background: var(--bg-secondary);
  }
  .wf-title { display: flex; align-items: center; gap: var(--space-2); }
  .wf-kicker {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }
  .wf-state {
    padding: 0.1rem 0.45rem;
    border-radius: 3px;
    font-size: 0.72rem;
    font-family: ui-monospace, monospace;
    background: var(--bg-tertiary);
    color: var(--fg-muted);
  }
  .wf-state.state-pending  { background: var(--bg-subtle, #233); color: var(--text-muted, #aab); }
  .wf-state.state-running  { background: rgba(64, 144, 240, 0.15); color: rgb(120, 180, 240); }
  .wf-state.state-done,
  .wf-state.state-completed,
  .wf-state.state-succeeded { background: rgba(72, 200, 128, 0.15); color: rgb(120, 220, 160); }
  .wf-state.state-failed,
  .wf-state.state-error    { background: rgba(220, 80, 80, 0.15); color: rgb(240, 130, 130); }
  .wf-state.state-quarantined {
    background: rgba(220, 70, 70, 0.18);
    color: rgb(245, 140, 140);
  }

  .wf-close {
    padding: 4px 10px;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--fg-secondary);
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    cursor: pointer;
  }
  .wf-close:hover {
    color: var(--fg-primary);
    border-color: var(--border-active);
    background: var(--bg-tertiary);
  }

  .wf-loading,
  .wf-error,
  .section-empty {
    padding: var(--space-4);
    font-size: var(--text-sm);
    color: var(--fg-muted);
  }
  .wf-error-title { color: var(--error, rgb(240, 130, 130)); font-weight: 600; }
  .wf-error-msg { margin-top: 0.4rem; font-family: ui-monospace, monospace; font-size: var(--text-xs); }
  .wf-retry {
    margin-top: 0.8rem;
    padding: 4px 10px;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--fg-secondary);
    border-radius: var(--radius-xs);
    cursor: pointer;
  }

  .wf-quarantine-banner {
    display: flex;
    gap: 0.6rem;
    align-items: flex-start;
    margin: var(--space-3) var(--space-4) 0;
    padding: var(--space-3);
    border: 1px solid rgba(220, 80, 80, 0.45);
    border-radius: var(--radius-lg, 8px);
    background: color-mix(in srgb, rgb(220, 80, 80) 10%, transparent);
  }
  .wf-quarantine-glyph { color: rgb(245, 140, 140); font-size: 1.1rem; line-height: 1.2; }
  .wf-quarantine-title { color: rgb(245, 150, 150); font-weight: 700; font-size: 0.85rem; }
  .wf-quarantine-sub { color: var(--fg-secondary); font-size: 0.78rem; line-height: 1.45; margin-top: 0.2rem; }

  .wf-summary {
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }
  .wf-meta {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.35rem 0.9rem;
    margin: 0;
  }
  .wf-meta-row { display: contents; }
  .wf-meta dt {
    color: var(--text-muted, #889);
    font-size: 0.78rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .wf-meta dd {
    margin: 0;
    color: var(--fg-primary);
    font-size: 0.85rem;
    min-width: 0;
  }
  .wf-id {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    word-break: break-all;
  }
  .wf-copy {
    background: transparent;
    border: 1px solid var(--border-subtle);
    color: var(--text-muted);
    border-radius: var(--radius-xs);
    padding: 1px 6px;
    cursor: pointer;
    font-size: 0.8rem;
  }
  .wf-copy:hover { color: var(--fg-primary); border-color: var(--border-active); }
  .cost { color: rgb(220, 200, 140); }
  .mono { font-family: ui-monospace, monospace; }

  .wf-section {
    padding: var(--space-3) var(--space-4);
  }
  .wf-section-title {
    margin: 0 0 0.8rem;
    font-size: 0.85rem;
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .section-count {
    padding: 0.05rem 0.45rem;
    border-radius: 999px;
    font-size: 0.7rem;
    background: var(--bg-tertiary, #233);
    color: var(--text-muted, #889);
    font-family: ui-monospace, monospace;
  }

  /* --- Timeline -------------------------------------------------------- */
  .timeline {
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .tl-step {
    display: grid;
    grid-template-columns: 1.5rem 1fr;
    gap: 0.6rem;
  }
  .tl-rail {
    position: relative;
    display: flex;
    justify-content: center;
  }
  /* The continuous rail line behind the dots. Drawn as a pseudo-element so
     it runs through the full height of each step and visually connects the
     timeline; the last step's line is trimmed by the card's own margin. */
  .tl-rail::before {
    content: '';
    position: absolute;
    top: 0;
    bottom: 0;
    left: 50%;
    width: 2px;
    transform: translateX(-50%);
    background: var(--border-subtle, #233);
  }
  .tl-step:first-child .tl-rail::before { top: 0.55rem; }
  .tl-step:last-child .tl-rail::before { bottom: calc(100% - 0.95rem); }
  .tl-dot {
    position: relative;
    z-index: 1;
    margin-top: 0.45rem;
    width: 0.7rem;
    height: 0.7rem;
    border-radius: 50%;
    background: var(--text-muted, #889);
    box-shadow: 0 0 0 3px var(--bg-secondary);
  }
  .tl-dot.kind-danger { background: rgb(240, 110, 110); }
  .tl-dot.kind-warn   { background: rgb(225, 190, 90); }
  .tl-dot.kind-live   { background: rgb(110, 215, 150); }
  .tl-dot.kind-muted  { background: rgb(130, 160, 210); }

  .tl-card {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs, 4px);
    background: var(--bg-primary, #0d1417);
    padding: 0.55rem 0.7rem;
    margin-bottom: 0.55rem;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .tl-step[data-kind='danger'] .tl-card { border-color: rgba(220, 80, 80, 0.4); }
  .tl-step[data-kind='warn'] .tl-card   { border-color: rgba(220, 180, 60, 0.35); }
  .tl-step[data-kind='live'] .tl-card   { border-color: rgba(72, 200, 128, 0.3); }

  .tl-head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .tl-event {
    font-size: 0.82rem;
    color: var(--fg-primary);
    font-weight: 600;
  }
  .tl-status {
    padding: 0.05rem 0.4rem;
    border-radius: 3px;
    font-size: 0.7rem;
    font-family: ui-monospace, monospace;
    background: var(--bg-subtle, #233);
    color: var(--text-muted, #aab);
  }
  .tl-status.status-success,
  .tl-status.status-succeeded { background: rgba(72, 200, 128, 0.15); color: rgb(120, 220, 160); }
  .tl-status.status-error,
  .tl-status.status-gate_fail { background: rgba(220, 80, 80, 0.15); color: rgb(240, 130, 130); }
  .tl-status.status-pending   { background: rgba(220, 180, 60, 0.12); color: rgb(225, 200, 120); }
  .tl-status.status-skipped   { background: var(--bg-subtle, #233); color: var(--text-muted, #889); }

  /* Badge — color-coded per §S4b: quarantined/failed → red, pending →
     amber, live → green, cache_hit (replayed) → muted blue. The dot, card
     border, and this pill all read off the same badgeKind() family. */
  .tl-badge {
    margin-left: auto;
    padding: 0.05rem 0.5rem;
    border-radius: 999px;
    font-size: 0.68rem;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    font-family: ui-monospace, monospace;
    cursor: help;
  }
  .tl-badge.kind-danger {
    background: rgba(220, 80, 80, 0.18);
    color: rgb(245, 140, 140);
    border: 1px solid rgba(220, 80, 80, 0.4);
  }
  .tl-badge.kind-warn {
    background: rgba(220, 180, 60, 0.16);
    color: rgb(230, 200, 110);
    border: 1px solid rgba(220, 180, 60, 0.4);
  }
  .tl-badge.kind-live {
    background: rgba(72, 200, 128, 0.16);
    color: rgb(130, 225, 165);
    border: 1px solid rgba(72, 200, 128, 0.4);
  }
  .tl-badge.kind-muted {
    background: rgba(120, 160, 220, 0.14);
    color: rgb(160, 190, 235);
    border: 1px solid rgba(120, 160, 220, 0.35);
  }

  .tl-key {
    font-size: 0.74rem;
    color: var(--fg-secondary, #9ab);
    word-break: break-all;
  }

  .tl-facts {
    display: flex;
    flex-wrap: wrap;
    gap: 0.3rem 1rem;
    font-size: 0.75rem;
  }
  .fact { display: inline-flex; align-items: baseline; gap: 0.3rem; }
  .fact-label {
    font-size: 0.66rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted, #889);
  }
  .fact-hint { color: var(--text-muted, #889); font-size: 0.7rem; }
  .spawn {
    max-width: 16ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--fg-secondary);
  }

  /* Cost provenance pill — VERBATIM, never blended. real = green, estimated
     = amber (explicitly NOT the same as real), unavailable/unspecified =
     muted. The visual distance between real and estimated is deliberate. */
  .cost-src {
    padding: 0.02rem 0.4rem;
    border-radius: 3px;
    font-size: 0.64rem;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .cost-src.src-real {
    background: rgba(72, 200, 128, 0.16);
    color: rgb(130, 225, 165);
  }
  .cost-src.src-estimated {
    background: rgba(220, 180, 60, 0.16);
    color: rgb(230, 200, 110);
  }
  .cost-src.src-unavailable,
  .cost-src.src-unspecified {
    background: var(--bg-subtle, #233);
    color: var(--text-muted, #889);
  }

  .tl-hash {
    font-size: 0.7rem;
    color: var(--text-muted, #889);
    word-break: break-all;
  }

  @keyframes fadeIn {
    from { opacity: 0; }
    to   { opacity: 1; }
  }
  @keyframes slideIn {
    from { transform: translateX(100%); }
    to   { transform: translateX(0); }
  }
  @media (max-width: 480px) {
    .wf-drawer { width: 100vw; }
  }
</style>
