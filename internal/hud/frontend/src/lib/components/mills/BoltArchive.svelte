<script lang="ts">
  /**
   * BoltArchive — the "tartan of the week". The live loom's cloth
   * evaporates as rows scroll off the beam; this modal re-weaves the last
   * 7 days of terminal runs into one persistent strip (same seededPattern
   * per run as the loom, so it IS the same fabric) and exports it as a
   * self-contained SVG/PNG for standups or the office TV.
   *
   * Fetches via millsStore.fetchArchiveRuns — deliberately NOT
   * pipelineHistory, which the loom diffs into weave events.
   */
  import { millsStore, type BoltGrade, type PipelineRun } from '../../stores/mills.svelte.ts';
  import { focusTrap } from '../../actions/focusTrap';
  import { archiveDays, archiveTotals, tartanSVG } from '../../utils/tartanHelpers.ts';

  let { onclose }: { onclose: () => void } = $props();

  let loading = $state(true);
  let error = $state<string | null>(null);
  let runs = $state<PipelineRun[]>([]);
  let notes = $state<Record<string, string>>({});
  let grading = $state<Record<string, boolean>>({});
  let gradeErrors = $state<Record<string, string>>({});

  $effect(() => {
    let cancelled = false;
    millsStore
      .fetchArchiveRuns()
      .then((r) => {
        if (cancelled) return;
        runs = r;
        notes = Object.fromEntries(r.map((run) => [run.ID, run.GradeNote ?? '']));
        loading = false;
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

  let days = $derived(archiveDays(runs, 7, new Date()));
  let totals = $derived(archiveTotals(days));

  // Resolve live theme tokens to concrete colors so the exported file
  // renders identically outside the HUD. Fallbacks mirror FactoryPanel's.
  let svg = $derived.by(() => {
    const css = getComputedStyle(document.documentElement);
    const triplet = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback;
    const bolt = triplet('--success-rgb', '34, 224, 118');
    const spark = triplet('--warning-rgb', '255, 184, 48');
    const fog = triplet('--fg-rgb', '212, 238, 244');
    return tartanSVG(days, {
      title: 'mills · tartan of the week',
      colors: {
        bg: css.getPropertyValue('--bg-primary').trim() || '#0b0f14',
        bolt: `rgb(${bolt})`,
        spark: `rgb(${spark})`,
        fog: `rgb(${fog})`,
        dim: `rgba(${fog}, 0.35)`,
      },
    });
  });

  let stamp = $derived(days.length > 0 ? days[days.length - 1].date : 'week');

  function triggerDownload(url: string, name: string): void {
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.click();
    // Give the click a tick before releasing the blob.
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  function downloadSVG(): void {
    const blob = new Blob([svg], { type: 'image/svg+xml;charset=utf-8' });
    triggerDownload(URL.createObjectURL(blob), `tartan-${stamp}.svg`);
  }

  async function downloadPNG(): Promise<void> {
    const url = URL.createObjectURL(new Blob([svg], { type: 'image/svg+xml;charset=utf-8' }));
    try {
      const img = new Image();
      await new Promise<void>((resolve, reject) => {
        img.onload = () => resolve();
        img.onerror = () => reject(new Error('rasterize failed'));
        img.src = url;
      });
      const scale = 2; // crisp on office-TV pixel densities
      const canvas = document.createElement('canvas');
      canvas.width = img.width * scale;
      canvas.height = img.height * scale;
      const ctx = canvas.getContext('2d');
      if (!ctx) return;
      ctx.scale(scale, scale);
      ctx.drawImage(img, 0, 0);
      canvas.toBlob((b) => {
        if (b) triggerDownload(URL.createObjectURL(b), `tartan-${stamp}.png`);
      }, 'image/png');
    } catch {
      // SVG rasterize can fail in odd embeds; the SVG download still works.
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onclose();
  }

  async function gradeRun(runID: string, grade: BoltGrade): Promise<void> {
    if (grading[runID]) return;
    const index = runs.findIndex((run) => run.ID === runID);
    if (index < 0) return;
    const previous = runs[index];
    grading = { ...grading, [runID]: true };
    gradeErrors = { ...gradeErrors, [runID]: '' };
    runs = runs.map((run) => run.ID === runID
      ? { ...run, Grade: grade, GradeNote: notes[runID] ?? '' }
      : run);
    try {
      await millsStore.gradeRun(runID, grade, notes[runID] ?? '');
    } catch (e) {
      runs = runs.map((run) => run.ID === runID ? previous : run);
      gradeErrors = {
        ...gradeErrors,
        [runID]: e instanceof Error ? e.message : String(e),
      };
    } finally {
      grading = { ...grading, [runID]: false };
    }
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<button type="button" class="arc-scrim" onclick={onclose} aria-label="Close bolt archive"></button>
<div class="arc-modal" role="dialog" aria-modal="true" aria-label="Bolt archive — tartan of the week" use:focusTrap>
  <header class="arc-header">
    <div class="arc-title">
      <span class="arc-kicker">Bolt archive</span>
      <span class="arc-sub">the week's cloth — every row a real run, rewoven from history</span>
    </div>
    <div class="arc-actions">
      <button type="button" class="btn btn-sm" onclick={downloadSVG} disabled={loading || error != null}>↓ SVG</button>
      <button type="button" class="btn btn-sm" onclick={downloadPNG} disabled={loading || error != null}>↓ PNG</button>
      <button type="button" class="arc-close" onclick={onclose} aria-label="Close bolt archive">✕</button>
    </div>
  </header>

  {#if loading}
    <div class="arc-status" role="status" aria-live="polite">unrolling the take-up beam…</div>
  {:else if error}
    <div class="arc-status arc-error">Couldn't unroll the archive: {error}</div>
  {:else}
    <div class="arc-cloth">
      {@html svg}
    </div>
    <section class="arc-grades" aria-label="Grade terminal runs">
      {#each days.flatMap((day) => day.runs) as run (run.runID)}
        <div class="grade-row">
          <span class:grade-bolt={run.kind === 'bolt'} class:grade-spark={run.kind === 'spark'} class="grade-id">
            {run.backlogID || run.runID}
          </span>
          <div class="grade-controls" aria-label={`Grade ${run.backlogID || run.runID}`}>
            {#each ['keep', 'meh', 'regret'] as grade}
              <button
                type="button"
                class="grade-chip"
                class:selected={run.grade === grade}
                aria-pressed={run.grade === grade}
                disabled={grading[run.runID]}
                onclick={() => gradeRun(run.runID, grade as BoltGrade)}
              >{grade}</button>
            {/each}
          </div>
          <input
            class="grade-note"
            type="text"
            aria-label={`Optional grade note for ${run.backlogID || run.runID}`}
            placeholder="optional note"
            value={notes[run.runID] ?? ''}
            disabled={grading[run.runID]}
            oninput={(event) => {
              notes = { ...notes, [run.runID]: event.currentTarget.value };
            }}
          />
          {#if gradeErrors[run.runID]}
            <span class="grade-error" role="alert">{gradeErrors[run.runID]}</span>
          {/if}
        </div>
      {/each}
    </section>
    <footer class="arc-footer">
      <span>{totals.bolts} bolts · {totals.sparks} sparks this week</span>
      <span class="arc-hint">hover a row for its run · export for the standup screen</span>
    </footer>
  {/if}
</div>

<style>
  .arc-scrim {
    position: fixed;
    inset: 0;
    background: color-mix(in srgb, var(--bg-primary) 55%, transparent);
    z-index: var(--z-modal);
    animation: fadeIn 0.15s ease-out;
    border: 0;
    padding: 0;
  }

  .arc-modal {
    position: fixed;
    top: 50%;
    left: 50%;
    transform: translate(-50%, -50%);
    width: min(760px, 94vw);
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

  .arc-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border);
    flex-shrink: 0;
  }

  .arc-title {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .arc-kicker {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .arc-sub {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }

  .arc-actions {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-shrink: 0;
  }

  .arc-close {
    background: none;
    border: none;
    color: var(--fg-muted);
    font-size: var(--text-base);
    cursor: pointer;
    padding: var(--space-1) var(--space-2);
  }
  .arc-close:hover { color: var(--fg-primary); }

  .arc-status {
    padding: var(--space-6) var(--space-4);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    text-align: center;
  }
  .arc-error { color: var(--warning); }

  .arc-cloth {
    overflow: auto;
    padding: var(--space-3) var(--space-4);
  }
  .arc-cloth :global(svg) {
    display: block;
    max-width: 100%;
    height: auto;
    border-radius: var(--radius-md);
  }

  .arc-grades { overflow-y: auto; padding: 0 var(--space-4) var(--space-3); }
  .grade-row {
    display: grid;
    grid-template-columns: minmax(110px, 1fr) auto minmax(140px, 1.4fr);
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) 0;
    border-top: 1px solid color-mix(in srgb, var(--border) 60%, transparent);
  }
  .grade-id { overflow: hidden; text-overflow: ellipsis; font: var(--text-2xs) var(--font-mono); }
  .grade-bolt { color: var(--success); }
  .grade-spark { color: var(--warning); }
  .grade-controls { display: flex; gap: 3px; }
  .grade-chip {
    border: 1px solid var(--border);
    border-radius: var(--radius-full);
    background: transparent;
    color: var(--fg-muted);
    padding: 2px 7px;
    font: var(--text-2xs) var(--font-mono);
    cursor: pointer;
  }
  .grade-chip.selected { color: var(--fg-primary); border-color: var(--accent); background: color-mix(in srgb, var(--accent) 15%, transparent); }
  .grade-chip:disabled { cursor: wait; opacity: 0.55; }
  .grade-note { min-width: 0; font-size: var(--text-2xs); }
  .grade-error { grid-column: 2 / -1; color: var(--danger, var(--warning)); font-size: var(--text-2xs); }

  .arc-footer {
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
  .arc-hint { color: var(--fg-dim); }
  @media (max-width: 560px) {
    .grade-row { grid-template-columns: 1fr auto; }
    .grade-note { grid-column: 1 / -1; }
  }
</style>
