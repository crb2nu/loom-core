<script lang="ts">
  /**
   * ShuttleBoard — one lane per active pipeline run, the facts beside the
   * loom's rhythm. Each lane carries the warp (priority, title, repo), the
   * nine-pick stage track with the current pick lit, how long the shuttle
   * has sat in that pick, who is weaving it (model + agent from the stage
   * record), the gate tally so far, spend, the MR once one exists, and
   * the last line of the in-flight stage's log — what it is doing now.
   *
   * Pure renderer: lanes arrive built by utils/shuttleBoardHelpers.ts.
   * A lane with `detail: 'pending'` shows "…" for weaver/pass until its
   * stage records land; never a fabricated value.
   */
  import type { ShuttleLane } from '../../utils/shuttleBoardHelpers.ts';
  import Badge from '../../widgets/Badge.svelte';
  import { fmtCost, fmtDuration } from './shared/format.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';

  let {
    lanes,
    onSelect,
    lastCouncilAt = null,
    lastMergeAt = null,
    now = Date.now(),
  }: {
    lanes: ShuttleLane[];
    onSelect?: (runID: string) => void;
    /** Operator status stamps: when the drawing office last sat, when the
     * last bolt was pressed. Both polled; neither was drawn before. */
    lastCouncilAt?: string | null;
    lastMergeAt?: string | null;
    now?: number;
  } = $props();

  function ago(iso: string | null | undefined): string | null {
    if (!iso) return null;
    const t = Date.parse(iso);
    if (!Number.isFinite(t)) return null;
    return `${fmtDuration(Math.max(0, now - t))} ago`;
  }

  function open(lane: ShuttleLane): void {
    onSelect?.(lane.runID);
  }

  function onKey(e: KeyboardEvent, lane: ShuttleLane): void {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      open(lane);
    }
  }

  function weaver(lane: ShuttleLane): string {
    if (lane.model) return lane.model;
    if (lane.agent) return lane.agent;
    return lane.detail === 'pending' ? '…' : '—';
  }

  // Where the weaver answer came from, when it is not the finished record.
  function weaverNote(lane: ShuttleLane): { text: string; title: string } | null {
    switch (lane.weaverSource) {
      case 'routed':
        return { text: 'routed', title: 'From the run’s provenance routing; the stage record fills in its model when the pick ends' };
      case 'spawn':
        return { text: 'model pending', title: 'A spawn is running this pick; its model is recorded when the pick ends' };
      case 'operator':
        return { text: 'no model', title: 'The operator runs this pick itself (tests, MR, CI watch, merge, cleanup)' };
      default:
        return null;
    }
  }
</script>

<section class="shuttle-board" aria-label="Shuttles in flight">
  <header class="board-head">
    <span class="board-tag">shuttles in flight</span>
    <span class="board-count">{lanes.length}</span>
    <span class="board-hint">one lane per active run · stage track · weaver · what it is doing now</span>
    <span class="board-spacer"></span>
    {#if ago(lastCouncilAt)}
      <span class="board-stamp" title="When the drawing office (council) last sat">council {ago(lastCouncilAt)}</span>
    {/if}
    {#if ago(lastMergeAt)}
      <span class="board-stamp" title="When the last bolt was pressed (merged)">last bolt {ago(lastMergeAt)}</span>
    {/if}
  </header>

  {#if lanes.length === 0}
    <p class="board-empty" role="status">◌ no shuttles in flight — the beam is idle</p>
  {:else}
    <ol class="lanes">
      {#each lanes as lane (lane.runID)}
        <li class="lane-item" style="--depth: {lane.depth}">
          <div
            class="lane"
            class:delayed={lane.delayed}
            class:escalated={lane.state === 'escalated' || lane.state === 'paused'}
            role="button"
            tabindex="0"
            aria-label={`Open run ${lane.runID}`}
            onclick={() => open(lane)}
            onkeydown={(e) => onKey(e, lane)}
          >
            <div class="lane-head">
              {#if lane.priority}
                <Badge text={lane.priority} variant={lane.priorityTone} />
              {/if}
              <span class="lane-title" title={lane.backlogID}>{lane.title}</span>
              <span class="lane-flight mono" title={lane.runID}>{lane.flight}</span>
              {#if lane.project}
                <span class="lane-project mono">{lane.project}</span>
              {/if}
              {#if lane.depth > 0}
                <span class="lane-depth" title="sub-run depth">↳ {lane.depth}</span>
              {/if}
              <span class="lane-spacer"></span>
              <span class="lane-stage" class:is-delayed={lane.delayed}>
                {lane.stageLabel}
                {#if lane.stageAgeMs != null}
                  <!-- "seen": the age is observed from this browser's polls,
                       not the operator's stage clock; a run already in its
                       stage when the page opened reads younger than it is. -->
                  <span class="lane-age" title="observed in this stage since this page started watching">· {lane.stageAgeMs < 15_000 ? 'just seen' : `seen ${fmtDuration(lane.stageAgeMs)}`}{lane.delayed ? ' · delayed' : ''}</span>
                {/if}
              </span>
            </div>

            <ol class="track" aria-label="pipeline stages">
              {#each lane.nodes as n (n.stage)}
                <li class="node node-{n.state}" title={`${n.label} (${n.stage})`}>
                  <span class="sr-only">{n.label}: {n.state}</span>
                </li>
              {/each}
            </ol>

            <div class="lane-meta">
              <span class="meta">
                <span class="k">weaver</span>
                <span class="v">{weaver(lane)}</span>
                {#if lane.agent && lane.model && lane.agent !== lane.model}
                  <span class="dim">via {lane.agent}</span>
                {/if}
                {#if weaverNote(lane)}
                  {@const note = weaverNote(lane)}
                  <span class="weaver-note" title={note?.title}>{note?.text}</span>
                {/if}
              </span>
              <span class="meta">
                <span class="k">pass</span>
                <span class="v">{lane.stageAttempt ?? (lane.detail === 'pending' ? '…' : '—')}</span>
                {#if lane.attempts > 1}
                  <span class="dim">· run attempt {lane.attempts}</span>
                {/if}
              </span>
              <span class="meta">
                <span class="k">gates</span>
                <span class="v ok">{lane.gatesPassed}</span>
                {#if lane.gatesFailed > 0}
                  <span class="bad">· {lane.gatesFailed} failed{lane.lastFailedGate ? ` (${lane.lastFailedGate})` : ''}</span>
                {/if}
              </span>
              <span class="meta">
                <span class="k">spend</span>
                <span class="v">{fmtCost(lane.costUSD)}</span>
              </span>
              {#if lane.mrIID != null}
                <a
                  class="mr-chip mono"
                  href={mrURL(lane.project || undefined, lane.mrIID)}
                  target="_blank"
                  rel="noreferrer noopener"
                  title={`Open merge request !${lane.mrIID}`}
                  onclick={(e) => e.stopPropagation()}
                >!{lane.mrIID}</a>
              {/if}
            </div>

            {#if lane.now}
              <!-- Labelled with its stage when it is the last finished pick
                   rather than the in-flight one, so a line from plan_slice
                   never reads as research output. -->
              <div class="lane-now mono" title={lane.now}>
                {#if lane.nowStage && lane.nowStage !== lane.stage}
                  <span class="now-stage">last pick · {lane.nowStage}</span>
                {/if}
                › {lane.now}
              </div>
            {/if}
          </div>
        </li>
      {/each}
    </ol>
  {/if}
</section>

<style>
  .shuttle-board {
    margin-top: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
    flex-shrink: 0;
  }
  .board-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    background: var(--bg-primary);
  }
  .board-tag {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--accent);
    white-space: nowrap;
  }
  .board-count {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 700;
    color: var(--accent);
  }
  .board-hint {
    font-size: var(--text-2xs);
    color: var(--fg-dim);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .board-spacer { flex: 1 1 0; }
  .board-stamp {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    white-space: nowrap;
  }
  .board-empty {
    margin: 0;
    padding: var(--space-4);
    text-align: center;
    color: var(--fg-muted);
    font-size: var(--text-sm);
  }

  .lanes { list-style: none; margin: 0; padding: 0; }
  .lane-item { border-bottom: 1px solid var(--border-subtle); }
  .lane-item:last-child { border-bottom: 0; }
  .lane {
    display: grid;
    gap: 5px;
    padding: var(--space-2) var(--space-3);
    padding-left: calc(var(--space-3) + var(--depth, 0) * 1.1rem);
    border-left: 3px solid transparent;
    cursor: pointer;
    outline: none;
  }
  .lane:hover { background: var(--bg-tertiary); }
  .lane:focus-visible { box-shadow: inset 0 0 0 2px var(--focus-ring); }
  .lane.delayed { border-left-color: var(--warning); }
  .lane.escalated { border-left-color: var(--error); }

  .lane-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }
  .lane-title {
    font-weight: 600;
    color: var(--fg-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
    flex: 0 1 auto;
  }
  .mono { font-family: var(--font-mono); }
  .lane-flight { font-size: var(--text-2xs); color: var(--fg-dim); white-space: nowrap; }
  .lane-project { font-size: var(--text-2xs); color: var(--fg-muted); white-space: nowrap; }
  .lane-depth { font-size: var(--text-2xs); color: var(--fg-muted); white-space: nowrap; }
  .lane-spacer { flex: 1 1 0; }
  .lane-stage {
    font-size: var(--text-xs);
    color: var(--info);
    white-space: nowrap;
  }
  .lane-stage.is-delayed { color: var(--warning); }
  .lane-age { color: var(--fg-muted); font-family: var(--font-mono); }
  .lane-stage.is-delayed .lane-age { color: var(--warning); }

  /* Nine-pick stage track: the same order LineageRibbon's strand draws,
     compressed to a bar so a lane stays one row tall. */
  .track {
    display: grid;
    grid-template-columns: repeat(9, 1fr);
    gap: 3px;
    list-style: none;
    margin: 0;
    padding: 0;
  }
  .node {
    height: 5px;
    border-radius: var(--radius-xs);
    background: var(--border);
  }
  .node-done { background: var(--success); opacity: 0.75; }
  .node-active {
    background: var(--info);
    box-shadow: var(--glow-shadow-md) var(--glow-info);
    animation: node-pulse 2.4s ease-in-out infinite;
  }
  .node-failed { background: var(--error); box-shadow: var(--glow-shadow-md) var(--glow-error); }
  @keyframes node-pulse {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.55; }
  }
  @media (prefers-reduced-motion: reduce) {
    .node-active { animation: none; }
  }

  .lane-meta {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-3);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }
  .meta { display: inline-flex; align-items: baseline; gap: 4px; white-space: nowrap; }
  .k {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--fg-dim);
  }
  .v { font-family: var(--font-mono); color: var(--fg-primary); }
  .dim { color: var(--fg-muted); }
  .weaver-note {
    padding: 0 5px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    font-size: var(--text-2xs);
    color: var(--fg-dim);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }
  .ok { color: var(--success); }
  .bad { color: var(--error); font-family: var(--font-mono); }
  .mr-chip {
    padding: 0 6px;
    border-radius: var(--radius-sm);
    border: 1px solid color-mix(in srgb, var(--mills) 32%, transparent);
    background: color-mix(in srgb, var(--mills) 14%, transparent);
    color: var(--mills);
    font-size: var(--text-xs);
    text-decoration: none;
  }
  .mr-chip:hover { background: color-mix(in srgb, var(--mills) 24%, transparent); }
  .mr-chip:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 1px; }

  .lane-now {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .now-stage {
    margin-right: var(--space-1);
    padding: 0 5px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    color: var(--fg-dim);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }

  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    overflow: hidden;
    clip: rect(0 0 0 0);
    white-space: nowrap;
  }

  @media (max-width: 720px) {
    .board-hint { display: none; }
    .lane-head { flex-wrap: wrap; }
    .lane-spacer { display: none; }
    .lane-stage { flex-basis: 100%; }
  }
</style>
