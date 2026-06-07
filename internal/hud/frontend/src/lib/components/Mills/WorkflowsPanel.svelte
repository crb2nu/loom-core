<script lang="ts">
  // WorkflowsPanel — the list view for the durable workflow step-log
  // (plan .loom/134 §S4b). Rows are imperative workflow runs from the
  // workflow_runs journal (GET /api/mills/workflow/runs); clicking a row
  // opens the WorkflowRunDetail drawer (the S1c step timeline). Mirrors
  // PipelinesPanel's table aesthetic so the two surfaces feel like one
  // family; the workflow journal is a flat list (no subrun tree), so the
  // table is simpler than the pipeline one.

  import { millsStore } from '../../stores/mills.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import WorkflowRunDetail from './WorkflowRunDetail.svelte';

  // The workflow journal is a separate surface from the DAG pipeline runs,
  // so this panel owns its own poll loop (the main millsStore.startPolling
  // fan-out never touches /workflow/*). 15s matches the rest of Mills.
  $effect(() => {
    void millsStore.loadWorkflowRuns();
    const t = setInterval(() => void millsStore.loadWorkflowRuns(), 15000);
    return () => clearInterval(t);
  });

  let runs = $derived(millsStore.workflowRuns);
  let loading = $derived(millsStore.workflowLoading && millsStore.workflowRuns.length === 0);
  let error = $derived(millsStore.workflowError);
  // The operator-not-configured signal is owned by the shared store flag.
  let disabled = $derived(millsStore.disabled);
  let selectedID = $derived(millsStore.selectedWorkflowID);

  // countsByState drives the at-a-glance pill row, same as Pipelines.
  let countsByState = $derived.by(() => {
    const out: Record<string, number> = {};
    for (const r of runs) out[r.state] = (out[r.state] ?? 0) + 1;
    return out;
  });

  function fmtCost(c?: number): string {
    if (c == null || !Number.isFinite(c)) return '—';
    if (c === 0) return '$0';
    if (c < 0.0001) return '<$0.0001';
    return `$${c.toFixed(4)}`;
  }

  function fmtTime(ts?: string): string {
    if (!ts) return '—';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toLocaleString();
  }

  // shortRunID keeps the id glanceable in a dense table; the full id is in
  // the row title attribute and click-to-copy lives in the drawer.
  function shortRunID(id: string): string {
    if (id.length <= 28) return id;
    return `${id.slice(0, 20)}…${id.slice(-5)}`;
  }

  function openRun(id: string): void {
    millsStore.openWorkflowRunDetail(id);
  }

  function onRowKeydown(ev: KeyboardEvent, id: string): void {
    if (ev.key === 'Enter' || ev.key === ' ') {
      ev.preventDefault();
      openRun(id);
    }
  }

  function emptyMessage(): string {
    if (disabled) return 'Mills operator not configured';
    if (error) return 'Failed to load workflow runs';
    return 'No workflow runs yet';
  }

  function emptyHint(): string {
    if (disabled) return 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.';
    if (error) return error ?? '';
    return 'Imperative workflow runs appear here once the durable runtime records its first step journal.';
  }
</script>

<PanelShell
  title="Workflows"
  icon="↻"
  count={runs.length}
  loading={loading}
  empty={runs.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={emptyMessage()}
  emptyHint={emptyHint()}
>
  {#snippet header()}
    <div class="wf-blurb">
      Durable step-log for imperative workflow runs — the S1c observation
      surface. Each run replays from a memoized journal; open a row to watch
      its step timeline, badges, and cost provenance.
    </div>
    {#if Object.keys(countsByState).length > 0}
      <div class="counts-row">
        {#each Object.entries(countsByState) as [state, n]}
          <span class="count-pill state-{state}">{state}: {n}</span>
        {/each}
      </div>
    {/if}
  {/snippet}

  <table class="mills-table">
    <thead>
      <tr>
        <th>Run ID</th>
        <th>Engine</th>
        <th>Template</th>
        <th>State</th>
        <th>Steps</th>
        <th>Backlog</th>
        <th>Cost</th>
        <th>Started</th>
      </tr>
    </thead>
    <tbody>
      {#each runs as r (r.id)}
        <tr
          class:selected={selectedID === r.id}
          class="clickable"
          role="button"
          tabindex="0"
          aria-label={`Open step timeline for workflow run ${r.id}`}
          onclick={() => openRun(r.id)}
          onkeydown={(ev) => onRowKeydown(ev, r.id)}
        >
          <td class="mono" title={r.id}>{shortRunID(r.id)}</td>
          <td><span class="engine-chip" title="workflow engine">{r.engine || '—'}</span></td>
          <td class="mono">{r.template || '—'}</td>
          <td><span class="state state-{r.state}">{r.state}</span></td>
          <td class="mono">{r.step_count ?? '—'}</td>
          <td class="mono">{r.backlog_id || '—'}</td>
          <td class="mono cost">{fmtCost(r.cost_usd)}</td>
          <td>{fmtTime(r.started_at)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
</PanelShell>

<WorkflowRunDetail />

<style>
  .wf-blurb {
    font-size: 0.82rem;
    line-height: 1.5;
    color: var(--fg-secondary, #9ab);
    max-width: 60ch;
  }
  .counts-row {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    margin-top: 0.6rem;
  }
  .count-pill {
    padding: 0.1rem 0.5rem;
    border-radius: 999px;
    font-size: 0.75rem;
    background: var(--bg-subtle, #233);
    color: var(--text-muted, #aab);
  }
  .mills-table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
  .mills-table th, .mills-table td {
    text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border-subtle, #233);
  }
  .mills-table th { font-weight: 600; color: var(--text-muted, #889); }
  .mono { font-family: ui-monospace, monospace; }
  .cost { color: rgb(220, 200, 140); font-variant-numeric: tabular-nums; }

  .engine-chip {
    padding: 0.05rem 0.4rem;
    border-radius: 3px;
    border: 1px solid color-mix(in srgb, var(--accent, #58a) 32%, var(--border-subtle, #233));
    background: color-mix(in srgb, var(--accent, #58a) 10%, transparent);
    color: var(--fg-secondary, #9ab);
    font-family: ui-monospace, monospace;
    font-size: 0.72rem;
    letter-spacing: 0.02em;
    white-space: nowrap;
  }

  .state { padding: 0.1rem 0.4rem; border-radius: 3px; font-size: 0.75rem; }
  /* Workflow run states (store.WorkflowRun*): running / done / failed /
     quarantined / pending. Quarantined is the nondeterminism halt — shade
     it like an alert so it reads at a glance. */
  .state-pending  { background: var(--bg-subtle, #233); color: var(--text-muted, #aab); }
  .state-running  { background: rgba(64, 144, 240, 0.15); color: rgb(120, 180, 240); }
  .state-done,
  .state-completed,
  .state-succeeded { background: rgba(72, 200, 128, 0.15); color: rgb(120, 220, 160); }
  .state-failed,
  .state-error    { background: rgba(220, 80, 80, 0.15); color: rgb(240, 130, 130); }
  .state-quarantined {
    background: rgba(220, 70, 70, 0.18);
    color: rgb(245, 140, 140);
    border: 1px solid rgba(220, 80, 80, 0.45);
  }

  .clickable { cursor: pointer; transition: background 0.1s ease-out; }
  .clickable:hover { background: rgba(120, 144, 200, 0.08); }
  .clickable:focus-visible {
    outline: 2px solid var(--accent, #58a);
    outline-offset: -2px;
  }
  .clickable.selected {
    background: rgba(120, 144, 200, 0.14);
    box-shadow: inset 3px 0 0 var(--accent, #58a);
  }
</style>
