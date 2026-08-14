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
  //
  // The drawer chrome (scrim, panel, focus trap, Escape, ✕) belongs to the
  // shared DetailDrawer; this file owns only the workflow-specific body.

  import {
    millsStore,
    workflowRunBranch,
    type WorkflowStep,
    type WorkflowStepBadge,
  } from '../../stores/mills.svelte.ts';
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import { elapsedMs, fmtCostExact, fmtDuration, fmtRunTime } from './shared/format.ts';

  let load = $derived(millsStore.openWorkflowDetail);
  let open = $derived(millsStore.selectedWorkflowID !== null);
  let detail = $derived(load && load.status === 'loaded' ? load.detail : null);

  function close(): void {
    millsStore.closeWorkflowDetail();
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

  function copyBranch(): void {
    const id = detail?.run.id;
    if (!id) return;
    void navigator.clipboard?.writeText(workflowRunBranch(id)).catch(() => {
      /* swallow — clipboard perms aren't worth a toast */
    });
  }

  // Frozen selection / canary params, pretty-printed. Opaque to the store;
  // for S7 registry runs this is the exact identity the run replays under
  // (content hash + clamped params + validated enums).
  let prettyParams = $derived.by(() => {
    const raw = detail?.workflow_params;
    if (!raw) return null;
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  });

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

<DetailDrawer
  {open}
  title="Workflow run detail"
  width="min(680px, 96vw)"
  contentPadding="0"
  closeLabel="Close workflow detail"
  onClose={close}
>
  {#snippet titleContent()}
    <div class="wf-title">
      <span class="wf-kicker">Workflow Run</span>
      {#if detail}
        <span class="wf-state state-{detail.run.state}">{detail.run.state}</span>
      {/if}
    </div>
  {/snippet}

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
          <dd class="mono">{detail.run.template || '—'}{#if detail.run.template_version}<span class="wf-dim">@{detail.run.template_version}</span>{/if}</dd>
        </div>
        {#if detail.run.interpreter_version}
          <div class="wf-meta-row">
            <dt>Interpreter</dt>
            <dd class="mono wf-dim" title="version pin: replay refuses under a drifted interpreter">{detail.run.interpreter_version}</dd>
          </div>
        {/if}
        <div class="wf-meta-row">
          <dt>Branch</dt>
          <dd class="mono wf-id">
            <span title="deterministic work-product branch (agent spawns commit here)">{workflowRunBranch(detail.run.id)}</span>
            <button type="button" class="wf-copy" onclick={copyBranch} title="Copy branch name">⧉</button>
          </dd>
        </div>
        {#if detail.run.backlog_id}
          <div class="wf-meta-row">
            <dt>Backlog</dt>
            <dd class="mono">{detail.run.backlog_id}</dd>
          </div>
        {/if}
        <div class="wf-meta-row">
          <dt>Started</dt>
          <dd class="mono">{fmtRunTime(detail.run.started_at)}</dd>
        </div>
        <div class="wf-meta-row">
          <dt>Ended</dt>
          <dd class="mono">{fmtRunTime(detail.run.ended_at)}</dd>
        </div>
        <div class="wf-meta-row">
          <dt>Duration</dt>
          <dd class="mono">{fmtDuration(elapsedMs(detail.run.started_at, detail.run.ended_at))}</dd>
        </div>
        <div class="wf-meta-row">
          <dt>Total cost</dt>
          <dd class="mono cost">{fmtCostExact(detail.run.cost_usd)}</dd>
        </div>
        <div class="wf-meta-row">
          <dt>Steps</dt>
          <dd class="mono">{detail.run.step_count ?? detail.steps.length}</dd>
        </div>
      </dl>
    </section>

    {#if prettyParams}
      <section class="wf-section">
        <h3 class="wf-section-title">Frozen identity params</h3>
        <!-- The run replays under exactly this frozen blob: registry runs
             carry content_hash + clamped params + validated enums; canary
             runs carry agent_type (+ merging). Immutable for the run's
             life — a drifted template hash terminalizes fail-closed. -->
        <pre class="wf-params mono">{prettyParams}</pre>
      </section>
    {/if}

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
                    <span class="mono cost">{fmtCostExact(step.cost_usd)}</span>
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
                    <span class="mono">{fmtRunTime(step.started_at)} → {fmtRunTime(step.ended_at)}</span>
                    <span class="fact-hint">{fmtDuration(elapsedMs(step.started_at, step.ended_at))}</span>
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
</DetailDrawer>

<style>
  /* Drawer chrome (scrim, panel, header row, ✕, slide-in, mobile sheet) lives
     in shared/DetailDrawer.svelte. What remains here is the run-specific
     header content it renders through the titleContent snippet, plus the
     body. */
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
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    background: var(--bg-tertiary);
    color: var(--fg-muted);
  }
  .wf-state.state-pending  { background: var(--bg-subtle); color: var(--text-muted); }
  .wf-state.state-running  { background: rgba(var(--info-rgb), 0.15); color: var(--info); }
  .wf-state.state-done,
  .wf-state.state-completed,
  .wf-state.state-succeeded { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .wf-state.state-failed,
  .wf-state.state-error    { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .wf-state.state-quarantined {
    background: rgba(var(--error-rgb), 0.18);
    color: var(--error);
  }

  .wf-loading,
  .wf-error,
  .section-empty {
    padding: var(--space-4);
    font-size: var(--text-sm);
    color: var(--fg-muted);
  }
  .wf-error-title { color: var(--error); font-weight: 600; }
  .wf-error-msg { margin-top: 0.4rem; font-family: var(--font-mono); font-size: var(--text-xs); }
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
    border: 1px solid rgba(var(--error-rgb), 0.45);
    border-radius: var(--radius-lg);
    background: color-mix(in srgb, var(--error) 10%, transparent);
  }
  .wf-quarantine-glyph { color: var(--error); font-size: var(--text-base); line-height: 1.2; }
  .wf-quarantine-title { color: var(--error); font-weight: 700; font-size: var(--text-12); }
  .wf-quarantine-sub { color: var(--fg-secondary); font-size: var(--text-xs); line-height: 1.45; margin-top: 0.2rem; }

  .wf-dim { color: var(--text-muted); }
  .wf-params {
    margin: 0;
    padding: 0.5rem 0.65rem;
    background: var(--bg-subtle);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
    font-size: var(--text-2xs);
    line-height: 1.5;
    overflow-x: auto;
    white-space: pre;
  }
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
    color: var(--text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .wf-meta dd {
    margin: 0;
    color: var(--fg-primary);
    font-size: var(--text-12);
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
    font-size: var(--text-xs);
  }
  .wf-copy:hover { color: var(--fg-primary); border-color: var(--border-active); }
  .cost { color: var(--fg-muted); }
  .mono { font-family: var(--font-mono); }

  .wf-section {
    padding: var(--space-3) var(--space-4);
  }
  .wf-section-title {
    margin: 0 0 0.8rem;
    font-size: var(--text-12);
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
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    background: var(--bg-tertiary);
    color: var(--text-muted);
    font-family: var(--font-mono);
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
    background: var(--border-subtle);
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
    background: var(--text-muted);
    box-shadow: 0 0 0 3px var(--bg-secondary);
  }
  .tl-dot.kind-danger { background: var(--error); }
  .tl-dot.kind-warn   { background: var(--warning); }
  .tl-dot.kind-live   { background: var(--success); }
  .tl-dot.kind-muted  { background: var(--mills); }

  .tl-card {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    background: var(--bg-primary);
    padding: 0.55rem 0.7rem;
    margin-bottom: 0.55rem;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .tl-step[data-kind='danger'] .tl-card { border-color: rgba(var(--error-rgb), 0.4); }
  .tl-step[data-kind='warn'] .tl-card   { border-color: rgba(var(--warning-rgb), 0.35); }
  .tl-step[data-kind='live'] .tl-card   { border-color: rgba(var(--success-rgb), 0.3); }

  .tl-head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    flex-wrap: wrap;
  }
  .tl-event {
    font-size: var(--text-xs);
    color: var(--fg-primary);
    font-weight: 600;
  }
  .tl-status {
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    background: var(--bg-subtle);
    color: var(--text-muted);
  }
  .tl-status.status-success,
  .tl-status.status-succeeded { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .tl-status.status-error,
  .tl-status.status-gate_fail { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .tl-status.status-pending   { background: rgba(var(--warning-rgb), 0.12); color: var(--warning); }
  .tl-status.status-skipped   { background: var(--bg-subtle); color: var(--text-muted); }

  /* Badge — color-coded per §S4b: quarantined/failed → red, pending →
     amber, live → green, cache_hit (replayed) → muted blue. The dot, card
     border, and this pill all read off the same badgeKind() family. */
  .tl-badge {
    margin-left: auto;
    padding: 0.05rem 0.5rem;
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.03em;
    font-family: var(--font-mono);
    cursor: help;
  }
  .tl-badge.kind-danger {
    background: rgba(var(--error-rgb), 0.18);
    color: var(--error);
    border: 1px solid rgba(var(--error-rgb), 0.4);
  }
  .tl-badge.kind-warn {
    background: rgba(var(--warning-rgb), 0.16);
    color: var(--warning);
    border: 1px solid rgba(var(--warning-rgb), 0.4);
  }
  .tl-badge.kind-live {
    background: rgba(var(--success-rgb), 0.16);
    color: var(--success);
    border: 1px solid rgba(var(--success-rgb), 0.4);
  }
  .tl-badge.kind-muted {
    background: rgba(var(--mills-rgb), 0.14);
    color: var(--mills);
    border: 1px solid rgba(var(--mills-rgb), 0.35);
  }

  .tl-key {
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    word-break: break-all;
  }

  .tl-facts {
    display: flex;
    flex-wrap: wrap;
    gap: 0.3rem 1rem;
    font-size: var(--text-xs);
  }
  .fact { display: inline-flex; align-items: baseline; gap: 0.3rem; }
  .fact-label {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--text-muted);
  }
  .fact-hint { color: var(--text-muted); font-size: var(--text-2xs); }
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
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.03em;
  }
  .cost-src.src-real {
    background: rgba(var(--success-rgb), 0.16);
    color: var(--success);
  }
  .cost-src.src-estimated {
    background: rgba(var(--warning-rgb), 0.16);
    color: var(--warning);
  }
  .cost-src.src-unavailable,
  .cost-src.src-unspecified {
    background: var(--bg-subtle);
    color: var(--text-muted);
  }

  .tl-hash {
    font-size: var(--text-2xs);
    color: var(--text-muted);
    word-break: break-all;
  }
</style>
