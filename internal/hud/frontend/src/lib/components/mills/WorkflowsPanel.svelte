<script lang="ts">
  // WorkflowsPanel — the list view for the durable workflow step-log
  // (plan .loom/134 §S4b). Rows are imperative workflow runs from the
  // workflow_runs journal (GET /api/mills/workflow/runs); clicking a row
  // opens the WorkflowRunDetail drawer (the S1c step timeline). Mirrors
  // PipelinesPanel's table aesthetic so the two surfaces feel like one
  // family; the workflow journal is a flat list (no subrun tree), so the
  // table is simpler than the pipeline one.

  import { millsStore } from '../../stores/mills.svelte.ts';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import PanelShell from '../shared/PanelShell.svelte';
  import WorkflowRunDetail from './WorkflowRunDetail.svelte';
  import { createPoller } from '../../utils/poller.ts';
  import { fmtCostExact, fmtRunTime, shortRunID } from './shared/format.ts';

  // The workflow journal is a separate surface from the DAG pipeline runs,
  // so this panel owns its own poll loop (the main millsStore.startPolling
  // fan-out never touches /workflow/*). 15s matches the rest of Mills.
  // createPoller replaces a raw setInterval so a backgrounded tab stops
  // pulling the journal and a slow response can't stack requests; it fires
  // no initial tick, so the explicit first load stays.
  $effect(() => {
    void millsStore.loadWorkflowRuns();
    const poller = createPoller(() => millsStore.loadWorkflowRuns(), 15000);
    poller.start();
    return () => poller.stop();
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


  function openRun(id: string): void {
    millsStore.openWorkflowRunDetail(id);
  }

  function onRowKeydown(ev: KeyboardEvent, id: string): void {
    if (ev.key === 'Enter' || ev.key === ' ') {
      ev.preventDefault();
      openRun(id);
    }
  }

  // A failed fetch goes to PanelShell's dedicated error surface, not the
  // empty card — the empty copy below only ever describes real "no rows"
  // and "not configured" states now.
  function emptyMessage(): string {
    if (disabled) return 'Mills operator not configured';
    return 'No workflow runs yet';
  }

  function emptyHint(): string {
    if (disabled) return 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.';
    return 'Imperative workflow runs appear here once the durable runtime records its first step journal.';
  }

  function emptyTone(): 'idle' | 'disabled' {
    return disabled ? 'disabled' : 'idle';
  }
</script>

<PanelShell
  title="Workflows"
  icon="↻"
  count={runs.length}
  loading={loading}
  error={!disabled && error && runs.length === 0 ? error : null}
  errorHeading="Couldn't load workflow runs"
  empty={!error && runs.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={emptyMessage()}
  emptyHint={emptyHint()}
  emptyTone={emptyTone()}
>
  {#snippet header()}
    <div class="wf-blurb">
      Durable step-log for imperative workflow runs — the S1c observation
      surface. Each run replays from a memoized journal; open a row to watch
      its step timeline, badges, and cost provenance.
    </div>
    {#if Object.keys(countsByState).length > 0}
      <!-- Plain inline stats, not pills: the panel has no state-filter
           mechanism, so pill chrome would read as clickable affordance
           that goes nowhere. -->
      <div class="counts-row">
        {#each Object.entries(countsByState) as [state, n]}
          <span class="count-stat">{n} {state}</span>
        {/each}
      </div>
    {/if}
  {/snippet}

  {#if error && runs.length > 0}
    <!-- Poll failed while stale rows are still on screen — flag it instead
         of silently presenting stale data as fresh. -->
    <ErrorBanner prefix="Workflows refresh failed" message={error} />
  {/if}

  <div class="mills-table-wrap">
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
          <td class="mono">{r.template || '—'}{#if r.template_version}<span class="tmpl-version">@{r.template_version}</span>{/if}</td>
          <td><span class="state state-{r.state}">{r.state}</span></td>
          <td class="mono">{r.step_count ?? '—'}</td>
          <td class="mono">{r.backlog_id || '—'}</td>
          <td class="mono cost">{fmtCostExact(r.cost_usd)}</td>
          <td>{fmtRunTime(r.started_at)}</td>
        </tr>
      {/each}
    </tbody>
  </table>
  </div>
</PanelShell>

<WorkflowRunDetail />

<style>
  .wf-blurb {
    font-size: var(--text-xs);
    line-height: 1.5;
    color: var(--fg-secondary);
    max-width: 60ch;
  }
  .counts-row {
    display: flex;
    gap: 0.5rem;
    flex-wrap: wrap;
    margin-top: 0.6rem;
  }
  .count-stat {
    font-size: var(--text-xs);
    color: var(--text-muted);
    font-family: var(--font-mono);
  }
  /* Horizontal-scroll escape hatch on narrow screens: the 8-column table
     can't reflow, so the wrap scrolls instead of overflowing the page. */
  .mills-table-wrap { overflow-x: auto; }
  .mills-table { width: 100%; border-collapse: collapse; font-size: var(--text-12); }
  .mills-table th, .mills-table td {
    text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border-subtle);
  }
  .mills-table th { font-weight: 600; color: var(--text-muted); }
  .mono { font-family: var(--font-mono); }
  .cost { color: var(--fg-muted); font-variant-numeric: tabular-nums; }

  .tmpl-version { color: var(--text-muted); font-size: var(--text-2xs); }
  .engine-chip {
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    border: 1px solid color-mix(in srgb, var(--accent) 32%, var(--border-subtle));
    background: color-mix(in srgb, var(--accent) 10%, transparent);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    letter-spacing: 0.02em;
    white-space: nowrap;
  }

  .state { padding: 0.1rem 0.4rem; border-radius: var(--radius-xs); font-size: var(--text-xs); }
  /* Workflow run states (store.WorkflowRun*): running / done / failed /
     quarantined / pending. Quarantined is the nondeterminism halt — shade
     it like an alert so it reads at a glance. */
  .state-pending  { background: var(--bg-subtle); color: var(--text-muted); }
  .state-running  { background: rgba(var(--info-rgb), 0.15); color: var(--info); }
  .state-done,
  .state-completed,
  .state-succeeded { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .state-failed,
  .state-error    { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .state-quarantined {
    background: rgba(var(--error-rgb), 0.18);
    color: var(--error);
    border: 1px solid rgba(var(--error-rgb), 0.45);
  }

  .clickable { cursor: pointer; transition: background 0.1s ease-out; }
  .clickable:hover { background: rgba(var(--mills-rgb), 0.08); }
  .clickable:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .clickable.selected {
    background: rgba(var(--mills-rgb), 0.14);
    box-shadow: inset 3px 0 0 var(--accent);
  }
</style>
