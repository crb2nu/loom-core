<script lang="ts">
  // BacklogDetail — right-edge drawer showing the full backlog item from
  // GET /api/mills/backlog/{id}. Driven entirely off
  // millsStore.selectedBacklogID + millsStore.openBacklogDetail so it stays
  // reactive to background refresh ticks. This is what makes a backlog row
  // worth clicking: the spec, the slice decomposition, the budget/policy,
  // the cost estimate, and cross-links to the runs executing the item (plus
  // a Start-pipeline action when none exist yet).
  import { millsStore } from '../../stores/mills.svelte.ts';
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { runAdminAction } from './shared/millsActions.ts';

  let load = $derived(millsStore.currentBacklogDetail);
  let open = $derived(millsStore.selectedBacklogID !== null);
  let detail = $derived(load && load.status === 'loaded' ? load.detail : null);
  let selectedID = $derived(millsStore.selectedBacklogID);

  // Cost estimate is already fetched lazily by the Backlog table; reuse it
  // here rather than refetching so the drawer opens instantly.
  let est = $derived(selectedID ? millsStore.costPreviews[selectedID] : undefined);

  // Runs spawned for this item (active + history), newest-first. The
  // load-bearing cross-link: "why is this escalated?" → open its run.
  let runs = $derived(selectedID ? millsStore.pipelineRunsForBacklog(selectedID) : []);

  // Start is offered only when the item has no run yet and isn't terminal —
  // re-starting a merged item or one already mid-flight would be confusing.
  const TERMINAL_STATES = new Set(['merged', 'done', 'closed']);
  let canStart = $derived(
    !!detail && runs.length === 0 && !TERMINAL_STATES.has(detail.State),
  );
  let starting = $state(false);
  let confirmStart = $state(false);

  function close(): void {
    millsStore.closeBacklogDetail();
  }

  function openRun(id: string): void {
    millsStore.openRunDetail(id);
  }

  async function doStart(): Promise<void> {
    confirmStart = false;
    const id = detail?.ID;
    if (!id) return;
    starting = true;
    await runAdminAction(() => millsStore.startPipeline(id), {
      success: 'Pipeline started — watch it under Pipelines',
      failurePrefix: 'Start failed',
    });
    starting = false;
  }

  function fmtUSD(n: number | undefined): string {
    if (typeof n !== 'number' || !Number.isFinite(n)) return '—';
    return `$${n.toFixed(2)}`;
  }
  function fmtTime(ts?: string): string {
    if (!ts) return '—';
    const d = new Date(ts);
    return isNaN(d.getTime()) ? ts : d.toLocaleString();
  }
  function shortRun(id: string): string {
    return id.length <= 24 ? id : `${id.slice(0, 18)}…${id.slice(-4)}`;
  }
</script>

<DetailDrawer
  {open}
  title={detail?.Title ?? selectedID ?? 'Backlog item'}
  subtitle={selectedID ?? ''}
  onClose={close}
>
  {#snippet header()}
    {#if detail}
      <div class="chips">
        <span class="state state-{detail.State}">{detail.State}</span>
        <span class="prio">P · {detail.Priority}</span>
        {#if detail.Labels?.length}
          {#each detail.Labels as label}<span class="label">{label}</span>{/each}
        {/if}
      </div>
    {/if}
  {/snippet}

  {#if load?.status === 'loading'}
    <p class="muted">Loading item…</p>
  {:else if load?.status === 'error'}
    <div class="err">
      <p>Couldn't load this item.</p>
      <code>{load.message}</code>
      <button type="button" class="retry" onclick={() => selectedID && millsStore.openBacklogDetail(selectedID)}>
        ↻ retry
      </button>
    </div>
  {:else if detail}
    <!-- Cost + linkouts -->
    <section class="grid">
      <div class="cell">
        <span class="k">Estimate</span>
        <span class="v">
          {#if est}
            {fmtUSD(est.estimate_usd)} <span class="conf conf-{est.confidence}">{est.confidence}</span>
            {#if est.capped_by_policy}<span class="capped">⚠ capped</span>{/if}
          {:else}—{/if}
        </span>
      </div>
      <div class="cell">
        <span class="k">Council run</span>
        <span class="v mono">
          {#if detail.CouncilRunID}{detail.CouncilRunID}{:else}—{/if}
        </span>
      </div>
      <div class="cell">
        <span class="k">GitLab issue</span>
        <span class="v mono">
          {#if detail.GitLabIssueIID != null}#{detail.GitLabIssueIID}{:else}—{/if}
        </span>
      </div>
      <div class="cell">
        <span class="k">Created</span>
        <span class="v">{fmtTime(detail.CreatedAt)}{#if detail.CreatedBy} · {detail.CreatedBy}{/if}</span>
      </div>
    </section>

    <!-- Linked pipeline runs (the "what happened" cross-link) -->
    <section class="block">
      <h4>Pipeline runs <span class="count">{runs.length}</span></h4>
      {#if runs.length === 0}
        <p class="muted">No runs yet for this item.</p>
      {:else}
        <ul class="runs">
          {#each runs as r (r.ID)}
            <li>
              <button type="button" class="run-link" onclick={() => openRun(r.ID)} title={r.ID}>
                <span class="mono">{shortRun(r.ID)}</span>
                <span class="state state-{r.State}">{r.State}</span>
                {#if r.CurrentStage}<span class="stage">{r.CurrentStage}</span>{/if}
                {#if r.MRIID != null}<span class="mr">!{r.MRIID}</span>{/if}
                <span class="when">{fmtTime(r.StartedAt)}</span>
              </button>
            </li>
          {/each}
        </ul>
      {/if}
    </section>

    <!-- Spec -->
    {#if detail.SpecDoc || detail.SpecAnchor}
      <section class="block">
        <h4>Spec</h4>
        <p class="mono spec">{detail.SpecDoc ?? '—'}{#if detail.SpecAnchor} <span class="muted">#{detail.SpecAnchor}</span>{/if}</p>
      </section>
    {/if}

    <!-- Slices -->
    {#if detail.Slices?.length}
      <section class="block">
        <h4>Slices <span class="count">{detail.Slices.length}</span></h4>
        <ul class="slices">
          {#each detail.Slices as s, i}
            <li>
              <span class="slice-name">{i + 1}. {s.name}</span>
              {#if s.files?.length}<span class="meta">{s.files.length} file{s.files.length === 1 ? '' : 's'}</span>{/if}
              {#if s.tests?.length}<span class="meta">{s.tests.length} test{s.tests.length === 1 ? '' : 's'}</span>{/if}
              {#if s.parallel_with?.length}<span class="meta">∥ {s.parallel_with.join(', ')}</span>{/if}
            </li>
          {/each}
        </ul>
      </section>
    {/if}

    <!-- Success criteria -->
    {#if detail.Success && (detail.Success.tests?.length || detail.Success.metrics?.length || detail.Success.manual_check)}
      <section class="block">
        <h4>Success criteria</h4>
        {#if detail.Success.tests?.length}<p><span class="k">Tests</span> <span class="mono">{detail.Success.tests.join(', ')}</span></p>{/if}
        {#if detail.Success.metrics?.length}<p><span class="k">Metrics</span> {detail.Success.metrics.join(', ')}</p>{/if}
        {#if detail.Success.manual_check}<p><span class="k">Manual</span> {detail.Success.manual_check}</p>{/if}
      </section>
    {/if}

    <!-- Budget + policy + deps -->
    <section class="grid">
      {#if detail.Budget && (detail.Budget.max_cost_usd || detail.Budget.max_turns || detail.Budget.max_pipeline_minutes)}
        <div class="cell">
          <span class="k">Budget</span>
          <span class="v">
            {#if detail.Budget.max_cost_usd}{fmtUSD(detail.Budget.max_cost_usd)}{/if}
            {#if detail.Budget.max_turns} · {detail.Budget.max_turns}t{/if}
            {#if detail.Budget.max_pipeline_minutes} · {detail.Budget.max_pipeline_minutes}m{/if}
          </span>
        </div>
      {/if}
      {#if detail.Policy}
        <div class="cell">
          <span class="k">Policy</span>
          <span class="v">
            {detail.Policy.require_human_review ? 'human review' : 'autonomous'}{detail.Policy.auto_merge ? ' · auto-merge' : ''}
          </span>
        </div>
      {/if}
      {#if detail.Dependencies?.length}
        <div class="cell">
          <span class="k">Depends on</span>
          <span class="v mono">{detail.Dependencies.join(', ')}</span>
        </div>
      {/if}
    </section>

    {#if detail.Policy?.protected_paths_touched?.length}
      <section class="block warn">
        <h4>⚠ Protected paths touched</h4>
        <p class="mono">{detail.Policy.protected_paths_touched.join(', ')}</p>
      </section>
    {/if}
  {/if}

  {#snippet footer()}
    <div class="footer-row">
      {#if canStart}
        <button type="button" class="start" disabled={starting} onclick={() => (confirmStart = true)}>
          {starting ? 'Starting…' : '▶ Start pipeline'}
        </button>
      {/if}
      <button type="button" class="ghost" onclick={close}>Close</button>
    </div>
  {/snippet}
</DetailDrawer>

<ConfirmDialog
  open={confirmStart}
  title="Start pipeline?"
  message={`Kick off a Mills pipeline run for ${detail?.ID ?? 'this item'}. This consumes budget and may open a merge request.`}
  confirmLabel="Start"
  onConfirm={doStart}
  onCancel={() => (confirmStart = false)}
/>

<style>
  .muted { color: var(--text-muted, #889); }
  .mono { font-family: ui-monospace, monospace; }
  .chips { display: flex; flex-wrap: wrap; gap: 0.35rem; align-items: center; }
  .prio { font-size: 0.72rem; color: var(--text-muted, #889); }
  .label {
    display: inline-block; padding: 0.05rem 0.4rem; border-radius: 999px;
    background: var(--bg-subtle, #1a2030); font-size: 0.68rem; color: var(--text-muted, #aab);
  }
  .grid {
    display: grid; grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.5rem 1rem; margin: 0.5rem 0 0.75rem;
  }
  .cell { display: flex; flex-direction: column; gap: 0.1rem; min-width: 0; }
  .k {
    font-size: 0.65rem; text-transform: uppercase; letter-spacing: 0.05em;
    color: var(--text-muted, #889);
  }
  .v { font-size: 0.85rem; color: var(--fg-primary, #dfe); overflow-wrap: anywhere; }
  .block { margin: 0.75rem 0; }
  .block h4 {
    margin: 0 0 0.35rem; font-size: 0.78rem; font-weight: 600;
    color: var(--fg-secondary, #9ab);
  }
  .block.warn h4 { color: rgb(240, 150, 120); }
  .count {
    padding: 0.02rem 0.35rem; border-radius: 999px; background: var(--bg-subtle, #233);
    font-size: 0.66rem; color: var(--text-muted, #aab); font-family: ui-monospace, monospace;
  }
  .spec { overflow-wrap: anywhere; font-size: 0.8rem; }
  .runs, .slices { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.3rem; }
  .run-link {
    display: flex; flex-wrap: wrap; align-items: center; gap: 0.4rem; width: 100%;
    padding: 0.35rem 0.5rem; border: 1px solid var(--border-subtle, #233);
    border-radius: 5px; background: transparent; color: var(--fg-primary, #dfe);
    cursor: pointer; text-align: left; font-size: 0.8rem;
    transition: background 0.1s ease-out, border-color 0.1s ease-out;
  }
  .run-link:hover { background: rgba(120, 144, 200, 0.08); border-color: var(--accent, #58a); }
  .run-link:focus-visible { outline: 2px solid var(--accent, #58a); outline-offset: 1px; }
  .stage {
    padding: 0.02rem 0.35rem; border-radius: 3px; font-size: 0.7rem;
    border: 1px solid color-mix(in srgb, var(--accent, #58a) 30%, var(--border-subtle, #233));
    color: var(--fg-secondary, #9ab); font-family: ui-monospace, monospace;
  }
  .mr { color: rgb(150, 190, 250); font-family: ui-monospace, monospace; font-size: 0.72rem; }
  .when { margin-left: auto; color: var(--text-muted, #889); font-size: 0.72rem; }
  .slices li {
    display: flex; flex-wrap: wrap; align-items: baseline; gap: 0.5rem;
    font-size: 0.8rem; padding: 0.15rem 0;
  }
  .slice-name { color: var(--fg-primary, #dfe); }
  .slices .meta { font-size: 0.7rem; color: var(--text-muted, #889); font-family: ui-monospace, monospace; }
  .block p { margin: 0.15rem 0; font-size: 0.8rem; }
  .err { color: rgb(240, 150, 150); display: flex; flex-direction: column; gap: 0.4rem; }
  .err code { color: var(--text-muted, #aab); font-size: 0.75rem; }
  .retry, .start, .ghost {
    cursor: pointer; border-radius: 5px; font-size: 0.8rem; padding: 0.3rem 0.7rem;
    border: 1px solid var(--border-subtle, #233); background: transparent; color: var(--fg-primary, #dfe);
  }
  .footer-row { display: flex; gap: 0.5rem; justify-content: flex-end; }
  .start {
    border-color: color-mix(in srgb, var(--success, #4c8) 45%, var(--border));
    color: var(--success, #6d9);
  }
  .start:hover:not(:disabled) { background: color-mix(in srgb, var(--success, #4c8) 14%, transparent); }
  .start:disabled { opacity: 0.5; cursor: default; }
  .ghost:hover { background: var(--bg-subtle, #233); }

  .state { padding: 0.1rem 0.4rem; border-radius: 3px; font-size: 0.72rem; }
  .state-queued, .state-ready { background: var(--bg-subtle, #233); color: var(--text-muted, #aab); }
  .state-running, .state-planning, .state-slicing, .state-implementing, .state-testing, .state-reviewing {
    background: rgba(64, 144, 240, 0.15); color: rgb(120, 180, 240);
  }
  .state-merged, .state-done { background: rgba(72, 200, 128, 0.15); color: rgb(120, 220, 160); }
  .state-failed { background: rgba(220, 80, 80, 0.15); color: rgb(240, 130, 130); }
  .state-escalated { background: rgba(220, 140, 60, 0.15); color: rgb(240, 180, 100); }
  .state-paused { background: rgba(180, 180, 60, 0.15); color: rgb(220, 220, 120); }
  .conf { padding: 0.02rem 0.3rem; border-radius: 3px; font-size: 0.62rem; text-transform: uppercase; }
  .conf-low { background: rgba(220, 80, 80, 0.15); color: rgb(240, 150, 150); }
  .conf-medium { background: rgba(220, 180, 60, 0.18); color: rgb(230, 200, 110); }
  .conf-high { background: rgba(72, 200, 128, 0.15); color: rgb(150, 220, 170); }
  .capped { font-size: 0.62rem; color: rgb(240, 140, 140); }
</style>
