<script lang="ts">
  /**
   * ShiftReport — the end-of-shift story. Re-reads the last 24 hours of
   * terminal runs (same fetchArchiveRuns the bolt archive uses — NOT
   * pipelineHistory, which the loom diffs into weave events) and tells
   * them as deterministic prose: bolts woven, sparks struck, patterns
   * stamped, retries, spend. No summarizer — same runs in, same words
   * out. Exports standup-pasteable markdown.
   *
   * Sparks are enriched in the background with their failing gate names
   * (bounded per-run detail fetches); the report renders immediately and
   * the gate reasons fill in when they land.
   */
  import { millsStore, type BoltGrade } from '../../stores/mills.svelte.ts';
  import { patternsStore } from '../../stores/patterns.svelte.ts';
  import { focusTrap } from '../../actions/focusTrap';
  import {
    shiftMarkdown,
    shiftNarrative,
    shiftStats,
    shiftWindow,
    type SparkGateSummary,
    type ShiftRun,
  } from '../../utils/shiftHelpers.ts';
  import { fmtCost, fmtRunTime } from './shared/format.ts';

  let { onclose }: { onclose: () => void } = $props();

  // The report is a snapshot: the window anchors to the moment it opened.
  const openedAt = new Date();
  // Resolve gate detail for at most this many sparks — a spark storm
  // should not fan out into dozens of detail fetches.
  const GATE_FETCH_MAX = 8;

  let loading = $state(true);
  let error = $state<string | null>(null);
  let gateSummaries = $state<SparkGateSummary[]>([]);
  let copied = $state(false);
  let shiftRuns = $state<ShiftRun[]>([]);
  let notes = $state<Record<string, string>>({});
  let grading = $state<Record<string, boolean>>({});
  let gradeErrors = $state<Record<string, string>>({});
  let stats = $derived(shiftStats(shiftRuns, patternsStore.patterns, millsStore.backlog));

  $effect(() => {
    let cancelled = false;
    millsStore
      .fetchArchiveRuns()
      .then(async (runs) => {
        if (cancelled) return;
        const shift = shiftWindow(runs, openedAt);
        shiftRuns = shift;
        notes = Object.fromEntries(shift.map((run) => [run.runID, run.gradeNote ?? '']));
        loading = false;
        // Background gate enrichment: sequential keeps it gentle on the
        // operator; each landed summary re-renders its spark row.
        for (const spark of stats.sparks.slice(0, GATE_FETCH_MAX)) {
          const detail = await millsStore.fetchArchiveRunDetail(spark.runID);
          if (cancelled) return;
          if (!detail) continue;
          const failed = (detail.gates ?? [])
            .filter((g) => g.Outcome === 'fail')
            .map((g) => g.GateName);
          gateSummaries = [...gateSummaries, { runID: spark.runID, failedGates: failed }];
        }
      })
      .catch((e) => {
        if (cancelled) return;
        error = e instanceof Error ? e.message : String(e);
        loading = false;
      });
    return () => {
      cancelled = true;
    };
  });

  let narrative = $derived(shiftNarrative(stats));
  let gatesByRun = $derived(new Map(gateSummaries.map((g) => [g.runID, g.failedGates])));
  let markdown = $derived(shiftMarkdown(stats, narrative, openedAt, gateSummaries));

  async function copyMarkdown(): Promise<void> {
    try {
      await navigator.clipboard.writeText(markdown);
      copied = true;
      setTimeout(() => (copied = false), 1600);
    } catch {
      // Clipboard can be unavailable in odd embeds; download still works.
    }
  }

  function downloadMarkdown(): void {
    const stamp = openedAt.toISOString().slice(0, 10);
    const url = URL.createObjectURL(new Blob([markdown], { type: 'text/markdown;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = `shift-report-${stamp}.md`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onclose();
  }

  async function gradeRun(runID: string, grade: BoltGrade): Promise<void> {
    if (grading[runID]) return;
    const previous = shiftRuns.find((run) => run.runID === runID);
    if (!previous) return;
    grading = { ...grading, [runID]: true };
    gradeErrors = { ...gradeErrors, [runID]: '' };
    shiftRuns = shiftRuns.map((run) => run.runID === runID
      ? { ...run, grade, gradeNote: notes[runID] ?? '' }
      : run);
    try {
      await millsStore.gradeRun(runID, grade, notes[runID] ?? '');
    } catch (e) {
      shiftRuns = shiftRuns.map((run) => run.runID === runID ? previous : run);
      gradeErrors = { ...gradeErrors, [runID]: e instanceof Error ? e.message : String(e) };
    } finally {
      grading = { ...grading, [runID]: false };
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<button type="button" class="shr-scrim" onclick={onclose} aria-label="Close shift report"></button>
<div class="shr-modal" role="dialog" aria-modal="true" aria-label="Shift report — the last 24 hours" use:focusTrap>
  <header class="shr-header">
    <div class="shr-title">
      <span class="shr-kicker">Shift report</span>
      <span class="shr-sub">the last 24 hours, told straight — every line a real run</span>
    </div>
    <div class="shr-actions">
      <button type="button" class="btn btn-sm" onclick={copyMarkdown} disabled={loading || error != null}>
        {copied ? '✓ copied' : '⧉ copy md'}
      </button>
      <button type="button" class="btn btn-sm" onclick={downloadMarkdown} disabled={loading || error != null}>↓ .md</button>
      <button type="button" class="shr-close" onclick={onclose} aria-label="Close shift report">✕</button>
    </div>
  </header>

  {#if loading}
    <div class="shr-status" role="status" aria-live="polite">tallying the shift…</div>
  {:else if error}
    <div class="shr-status shr-error">Couldn't tally the shift: {error}</div>
  {:else}
    <div class="shr-body">
      <div class="shr-narrative">
        {#each narrative as line, i (i)}
          <p class:shr-headline={i === 0}>{line}</p>
        {/each}
      </div>

      <div class="shr-chips" aria-label="Shift totals">
        <span class="chip chip-bolt">{stats.bolts.length} bolts</span>
        <span class="chip chip-spark">{stats.sparks.length} sparks</span>
        {#if stats.retried.length > 0}
          <span class="chip">{stats.retried.length} retried</span>
        {/if}
        {#if stats.costUSD > 0}
          <span class="chip">{fmtCost(stats.costUSD)}</span>
        {/if}
      </div>

      {#if stats.sparks.length > 0}
        <section class="shr-sparks" aria-label="Escalated runs this shift">
          <h3>sparks on the floor</h3>
          <ul>
            {#each stats.sparks as spark (spark.runID)}
              {@const failed = gatesByRun.get(spark.runID)}
              <li>
                <span class="spark-when">{fmtRunTime(spark.endedAt)}</span>
                <span class="spark-id">{spark.backlogID || spark.runID}</span>
                <span class="spark-meta">
                  {spark.template || 'pipeline'} · {spark.attempts} attempt{spark.attempts === 1 ? '' : 's'}
                  {#if failed && failed.length > 0}
                    · failed <strong>{failed.join(', ')}</strong>
                  {:else if failed}
                    · no failing gate recorded
                  {/if}
                </span>
              </li>
            {/each}
          </ul>
        </section>
      {/if}

      {#if stats.bolts.length + stats.sparks.length > 0}
        <section class="shr-departures" aria-label="Grade shift departures">
          <h3>departures</h3>
          {#each [...stats.bolts, ...stats.sparks].sort((a, b) => a.endedAt - b.endedAt) as run (run.runID)}
            <div class="grade-row">
              <span class:grade-bolt={run.kind === 'bolt'} class:grade-spark={run.kind === 'spark'} class="grade-id">
                {fmtRunTime(run.endedAt)} · {run.backlogID || run.runID}
              </span>
              <div class="grade-controls" aria-label={`Grade ${run.backlogID || run.runID}`}>
                {#each ['keep', 'meh', 'regret'] as grade}
                  <button type="button" class="grade-chip" class:selected={run.grade === grade}
                    aria-pressed={run.grade === grade} disabled={grading[run.runID]}
                    onclick={() => gradeRun(run.runID, grade as BoltGrade)}>{grade}</button>
                {/each}
              </div>
              <input class="grade-note" type="text" placeholder="optional note"
                aria-label={`Optional grade note for ${run.backlogID || run.runID}`}
                value={notes[run.runID] ?? ''} disabled={grading[run.runID]}
                oninput={(event) => { notes = { ...notes, [run.runID]: event.currentTarget.value }; }} />
              {#if gradeErrors[run.runID]}
                <span class="grade-error" role="alert">{gradeErrors[run.runID]}</span>
              {/if}
            </div>
          {/each}
        </section>
      {/if}
    </div>
    <footer class="shr-footer">
      <span>{stats.bolts.length + stats.sparks.length} departures this shift</span>
      <span class="shr-hint">copy the markdown for the standup thread</span>
    </footer>
  {/if}
</div>

<style>
  .shr-scrim {
    position: fixed;
    inset: 0;
    background: color-mix(in srgb, var(--bg-primary) 55%, transparent);
    z-index: var(--z-modal);
    animation: fadeIn 0.15s ease-out;
    border: 0;
    padding: 0;
  }

  .shr-modal {
    position: fixed;
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    width: min(600px, 94vw);
    max-height: 88vh;
    display: flex;
    flex-direction: column;
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    box-shadow: 0 24px 48px rgba(0, 0, 0, 0.45);
    z-index: calc(var(--z-modal) + 1);
    overflow: hidden;
  }

  .shr-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }

  .shr-title {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .shr-kicker {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .shr-sub {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }

  .shr-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .shr-close {
    background: none;
    border: none;
    color: var(--fg-muted);
    font-size: var(--text-base);
    cursor: pointer;
    padding: var(--space-1) var(--space-2);
  }
  .shr-close:hover { color: var(--fg-primary); }

  .shr-status {
    padding: var(--space-6) var(--space-4);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    text-align: center;
  }
  .shr-error { color: var(--warning); }

  .shr-body {
    overflow-y: auto;
    padding: var(--space-4);
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
  }

  .shr-narrative p {
    margin: 0 0 var(--space-2);
    font-size: var(--text-sm);
    line-height: 1.5;
    color: var(--fg-secondary);
  }
  .shr-narrative p:last-child { margin-bottom: 0; }
  .shr-headline {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
  }

  .shr-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .chip {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-full);
    padding: 2px var(--space-2);
    white-space: nowrap;
  }
  .chip-bolt { color: var(--success); border-color: color-mix(in srgb, var(--success) 45%, transparent); }
  .chip-spark { color: var(--warning); border-color: color-mix(in srgb, var(--warning) 45%, transparent); }

  .shr-sparks h3, .shr-departures h3 {
    margin: 0 0 var(--space-2);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .shr-sparks ul {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .shr-sparks li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: var(--space-1) var(--space-2);
    border-left: 2px solid var(--warning);
    background: color-mix(in srgb, var(--warning) 6%, transparent);
    border-radius: 0 var(--radius-sm) var(--radius-sm) 0;
  }

  .spark-when {
    font-family: var(--font-mono);
    color: var(--fg-dim);
    flex-shrink: 0;
  }

  .spark-id {
    font-family: var(--font-mono);
    font-weight: 600;
    color: var(--fg-primary);
    word-break: break-all;
  }

  .spark-meta { color: var(--fg-muted); }
  .spark-meta strong { color: var(--warning); font-weight: 600; }

  .shr-departures { display: flex; flex-direction: column; gap: var(--space-1); }
  .grade-row { display: grid; grid-template-columns: minmax(130px, 1fr) auto; gap: var(--space-1) var(--space-2); align-items: center; }
  .grade-id { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font: var(--text-2xs) var(--font-mono); }
  .grade-bolt { color: var(--success); }
  .grade-spark { color: var(--warning); }
  .grade-controls { display: flex; gap: 3px; }
  .grade-chip { border: 1px solid var(--border); border-radius: var(--radius-full); background: transparent; color: var(--fg-muted); padding: 2px 7px; font: var(--text-2xs) var(--font-mono); cursor: pointer; }
  .grade-chip.selected { color: var(--fg-primary); border-color: var(--accent); background: color-mix(in srgb, var(--accent) 15%, transparent); }
  .grade-chip:disabled { cursor: wait; opacity: 0.55; }
  .grade-note { grid-column: 1 / -1; min-width: 0; font-size: var(--text-2xs); }
  .grade-error { grid-column: 1 / -1; color: var(--danger, var(--warning)); font-size: var(--text-2xs); }

  .shr-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-4);
    border-top: 1px solid var(--border);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    flex-shrink: 0;
  }
  .shr-hint { color: var(--fg-dim); }

  /* Phones: full-width sheet, hint drops to keep the footer one line. */
  @media (max-width: 480px) {
    .shr-modal {
      width: 100vw;
      max-height: 92vh;
      top: auto;
      bottom: 0;
      left: 0;
      transform: none;
      border-radius: var(--radius-lg) var(--radius-lg) 0 0;
      border-left: none;
      border-right: none;
      border-bottom: none;
    }
    .shr-hint { display: none; }
  }
</style>
