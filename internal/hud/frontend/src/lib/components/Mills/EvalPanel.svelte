<script lang="ts">
  import { millsStore, loopLetterFor, type EvalScore } from '../../stores/mills.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';

  $effect(() => {
    millsStore.startPolling(15000);
    return () => { millsStore.stopPolling(); };
  });

  // Inline drill-down: expand a score to read its rubric breakdown, notes,
  // and judge — none of which fit in the dense table. Mirrors the
  // expander pattern Council/Audit/Squads already use.
  let expanded = $state<number | null>(null);
  function toggle(id: number): void {
    expanded = expanded === id ? null : id;
  }
  function onRowKeydown(ev: KeyboardEvent, id: number): void {
    if (ev.key === 'Enter' || ev.key === ' ') {
      ev.preventDefault();
      toggle(id);
    }
  }

  // breakdownEntries normalises the freeform Breakdown map into
  // [label, value] pairs for display. Numbers render with 3 decimals;
  // everything else stringifies.
  function breakdownEntries(b: Record<string, unknown> | undefined): [string, string][] {
    if (!b) return [];
    return Object.entries(b).map(([k, v]) => [
      k,
      typeof v === 'number' && Number.isFinite(v) ? v.toFixed(3) : String(v),
    ]);
  }

  // A pipeline_run subject can be opened in the run drawer (mounted
  // below). Council/cross subjects live in their own panels, so we only
  // cross-link the case we can actually render here.
  function openSubjectRun(s: EvalScore): void {
    if (s.SubjectKind === 'pipeline_run' && s.SubjectID) {
      millsStore.openRunDetail(s.SubjectID);
    }
  }

  let scores = $derived(millsStore.evalScores);
  let loading = $derived(millsStore.loading && millsStore.evalScores.length === 0);
  let disabled = $derived(millsStore.disabled);
  let error = $derived(millsStore.error);

  // Group by derived Loop letter so Loop A / B / C trends show separately.
  let byLoop = $derived.by(() => {
    const out: Record<string, { count: number; mean: number; latest: number | null }> = {};
    for (const s of scores) {
      const k = loopLetterFor(s);
      if (!out[k]) out[k] = { count: 0, mean: 0, latest: null };
      out[k].count += 1;
      out[k].mean = (out[k].mean * (out[k].count - 1) + s.Score) / out[k].count;
      out[k].latest = s.Score;
    }
    return out;
  });

  function fmtTime(ts?: string): string {
    if (!ts) return '—';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toLocaleString();
  }
  function fmtScore(s: number): string {
    return s.toFixed(3);
  }
  // subjectLabel renders the row's subject as "<kind>:<id>" so the
  // SubjectKind discriminator stays visible (a council_run id and a
  // pipeline_run id can look identical at a glance otherwise).
  function subjectLabel(s: EvalScore): string {
    if (!s.SubjectID) return s.SubjectKind || '—';
    return `${s.SubjectKind}:${s.SubjectID}`;
  }
</script>

<PanelShell
  title="Eval"
  icon="✓"
  count={scores.length}
  loading={loading}
  empty={scores.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : (error ? 'Failed to load eval scores' : 'No eval scores yet')}
  emptyHint={disabled ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.' : (error ?? 'Loop A scores artifacts; Loop B scores merges; Loop C runs weekly.')}
>
  {#snippet header()}
    <div class="loop-row">
      {#each Object.entries(byLoop) as [loop, agg]}
        <span class="loop-pill">
          Loop {loop}: <strong>{fmtScore(agg.mean)}</strong> mean ({agg.count})
        </span>
      {/each}
    </div>
  {/snippet}

  <div class="mills-table-wrap">
  <table class="mills-table">
    <thead>
      <tr>
        <th>Loop</th>
        <th>Rubric</th>
        <th>Subject</th>
        <th>Score</th>
        <th>Notes</th>
        <th>When</th>
      </tr>
    </thead>
    <tbody>
      {#each scores as s (s.ID)}
        {@const loop = loopLetterFor(s)}
        {@const isOpen = expanded === s.ID}
        {@const entries = breakdownEntries(s.Breakdown)}
        <tr
          class="clickable"
          class:selected={isOpen}
          role="button"
          tabindex="0"
          aria-expanded={isOpen}
          aria-label={`Toggle detail for eval score ${s.Rubric}`}
          onclick={() => toggle(s.ID)}
          onkeydown={(ev) => onRowKeydown(ev, s.ID)}
        >
          <td><span class="toggle">{isOpen ? '▾' : '▸'}</span><span class="loop loop-{loop}">{loop}</span></td>
          <td class="mono">{s.Rubric}</td>
          <td class="mono" title={subjectLabel(s)}><span class="cell-cap">{subjectLabel(s)}</span></td>
          <td>{fmtScore(s.Score)}</td>
          <td class="notes-cell">{s.Notes ?? ''}</td>
          <td>{fmtTime(s.EvaluatedAt)}</td>
        </tr>
        {#if isOpen}
          <tr class="detail-row">
            <td colspan="6">
              <div class="detail">
                <div class="detail-meta">
                  <span class="k">Judge</span><span class="mono">{s.JudgedBy || '—'}</span>
                  <span class="k">Subject</span><span class="mono">{subjectLabel(s)}</span>
                  {#if s.SubjectKind === 'pipeline_run' && s.SubjectID}
                    <button type="button" class="open-run" onclick={(e) => { e.stopPropagation(); openSubjectRun(s); }}>
                      ↗ open run
                    </button>
                  {/if}
                </div>
                {#if entries.length}
                  <div class="breakdown">
                    {#each entries as [k, v]}
                      <span class="bd-pair"><span class="bd-k">{k}</span><span class="bd-v">{v}</span></span>
                    {/each}
                  </div>
                {/if}
                {#if s.Notes}
                  <p class="full-notes">{s.Notes}</p>
                {/if}
                {#if !entries.length && !s.Notes}
                  <p class="muted">No breakdown recorded for this score.</p>
                {/if}
              </div>
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  </div>
</PanelShell>

<!-- Run drawer for pipeline_run subjects; driven by selectedRunID. -->
<PipelineRunDetail />

<style>
  .loop-row { display: flex; gap: 0.5rem; flex-wrap: wrap; font-size: 0.8rem; }
  .loop-pill {
    padding: 0.1rem 0.5rem; border-radius: 999px;
    background: var(--bg-subtle, #233); color: var(--text-muted, #aab);
  }
  .loop-pill strong { color: var(--text-default, #eef); }
  .mills-table-wrap { overflow-x: auto; }
  .mills-table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
  .mills-table th, .mills-table td {
    text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border-subtle, #233);
  }
  .mills-table th { font-weight: 600; color: var(--text-muted, #889); }
  .clickable { cursor: pointer; transition: background 0.1s ease-out; }
  .clickable:hover { background: rgba(120, 144, 200, 0.08); }
  .clickable:focus-visible { outline: 2px solid var(--accent, #58a); outline-offset: -2px; }
  .clickable.selected { background: rgba(120, 144, 200, 0.12); }
  .toggle { display: inline-block; width: 1rem; color: var(--text-muted, #889); }
  .notes-cell {
    max-width: 22rem; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
    color: var(--text-muted, #aab);
  }
  .detail-row > td { padding: 0; border-bottom: 1px solid var(--border-subtle, #233); }
  .detail {
    padding: 0.6rem 0.9rem; background: rgba(120, 144, 200, 0.05);
    display: flex; flex-direction: column; gap: 0.5rem;
  }
  .detail-meta { display: flex; flex-wrap: wrap; align-items: center; gap: 0.4rem 0.6rem; font-size: 0.8rem; }
  .k {
    font-size: 0.62rem; text-transform: uppercase; letter-spacing: 0.05em;
    color: var(--text-muted, #889);
  }
  .open-run {
    margin-left: auto; cursor: pointer; font-size: 0.75rem; padding: 0.2rem 0.55rem;
    border: 1px solid var(--accent, #58a); border-radius: 5px;
    background: color-mix(in srgb, var(--accent, #58a) 12%, transparent); color: var(--fg-primary, #dfe);
  }
  .open-run:hover { background: color-mix(in srgb, var(--accent, #58a) 22%, transparent); }
  .breakdown { display: flex; flex-wrap: wrap; gap: 0.35rem; }
  .bd-pair {
    display: inline-flex; align-items: baseline; gap: 0.3rem;
    padding: 0.1rem 0.45rem; border-radius: 4px; background: var(--bg-subtle, #1a2030);
    font-size: 0.74rem;
  }
  .bd-k { color: var(--text-muted, #889); font-family: ui-monospace, monospace; }
  .bd-v { color: var(--fg-primary, #dfe); font-variant-numeric: tabular-nums; }
  .full-notes { margin: 0; font-size: 0.82rem; color: var(--fg-secondary, #9ab); overflow-wrap: anywhere; }
  .muted { color: var(--text-muted, #889); }
  .mono { font-family: ui-monospace, monospace; }
  .cell-cap {
    display: inline-block;
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    vertical-align: bottom;
  }

  .loop {
    display: inline-block; min-width: 1.2rem; text-align: center;
    padding: 0.05rem 0.35rem; border-radius: 3px; font-size: 0.75rem;
    font-weight: 600;
  }
  .loop-A { background: rgba(120, 200, 240, 0.15); color: rgb(160, 220, 250); }
  .loop-B { background: rgba(180, 140, 240, 0.15); color: rgb(210, 180, 250); }
  .loop-C { background: rgba(240, 180, 100, 0.15); color: rgb(250, 210, 150); }
</style>
