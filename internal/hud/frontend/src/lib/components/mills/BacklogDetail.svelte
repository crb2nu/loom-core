<script lang="ts">
  // BacklogDetail — right-edge drawer showing the full backlog item from
  // GET /api/mills/backlog/{id}. Driven entirely off
  // millsStore.selectedBacklogID + millsStore.openBacklogDetail so it stays
  // reactive to background refresh ticks. This is what makes a backlog row
  // worth clicking: the spec, the slice decomposition, the budget/policy,
  // the cost estimate, and cross-links to the runs executing the item (plus
  // a Start-pipeline action when none exist yet).
  import {
    millsStore,
    TERMINAL_WORKFLOW_STATES,
    workflowRunBranch,
  } from '../../stores/mills.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { runAdminAction } from './shared/millsActions.ts';
  import { fmtCost, fmtRunTime, shortRunID } from './shared/format.ts';

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

  // Imperative-lane runs (S7): items routed through ClaimWorkflowStart have
  // NO pipeline run — their "what happened" lives in workflow runs. Lazily
  // populate the list when the drawer opens so the cross-link works from any
  // subview, not just when the Workflows panel's poller has run.
  $effect(() => {
    if (open) millsStore.ensureWorkflowRunsLoaded();
  });
  let wfRuns = $derived(selectedID ? millsStore.workflowRunsForBacklog(selectedID) : []);

  // The item's explicit template selection (authored on ItemPolicy). Shown
  // even before any run exists so an operator can see the routing intent.
  let wfSelection = $derived(
    detail?.Policy?.workflow_template
      ? `${detail.Policy.workflow_template}@${detail.Policy.workflow_template_version || '?'}`
      : null,
  );

  // Terminal-settle context, derived from the run row itself (the settle
  // that escalated this item released the run's reservation in the same
  // transaction): outcome + the deterministic work-product branch.
  let settledWfRun = $derived(
    wfRuns.find((r) => TERMINAL_WORKFLOW_STATES.has((r.state ?? '').toLowerCase())) ?? null,
  );

  let branchCopied = $state(false);
  async function copyWfBranch(runID: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(workflowRunBranch(runID));
      branchCopied = true;
      setTimeout(() => (branchCopied = false), 1500);
    } catch {
      // Clipboard unavailable (permissions/HTTP) — the branch name is
      // rendered as text either way, so failing silently is fine.
    }
  }

  function openWorkflowRun(id: string): void {
    millsStore.closeBacklogDetail();
    router.navigate('mills', 'mills-workflows');
    millsStore.openWorkflowRunDetail(id);
  }

  // Escalation attention: when the item (or its newest run) is escalated/failed,
  // surface the most-likely culprit run + stage at the top so an operator
  // triaging at scale sees "where to look" without drilling run → stage → log.
  const ATTENTION_STATES = new Set(['escalated', 'failed']);
  let attentionRun = $derived(
    runs.find((r) => ATTENTION_STATES.has((r.State ?? '').toLowerCase())) ?? null,
  );
  let needsAttention = $derived(
    !!detail && (ATTENTION_STATES.has((detail.State ?? '').toLowerCase()) || !!attentionRun),
  );

  // Start is offered when the item has no run yet and isn't terminal —
  // re-starting a merged item or one already mid-flight would be confusing.
  // Exception: an escalated item is parked awaiting exactly this human
  // action, so it stays startable even with prior runs; the start then
  // carries requeue=1 so the operator flips it back to queued first.
  const TERMINAL_STATES = new Set(['merged', 'done', 'closed']);
  let isEscalated = $derived((detail?.State ?? '').toLowerCase() === 'escalated');
  let canStart = $derived(
    !!detail && !TERMINAL_STATES.has(detail.State) && (runs.length === 0 || isEscalated),
  );
  let starting = $state(false);
  let confirmStart = $state(false);

  function close(): void {
    millsStore.closeBacklogDetail();
  }

  function openRun(id: string): void {
    millsStore.openRunDetail(id);
  }

  // Deep-link the born-linked plan into Work → Plans. PlanID is authoritative
  // (set when the council/import born-links the item or the boot backfill runs).
  function openPlan(id: string): void {
    millsStore.closeBacklogDetail();
    router.navigate('tasks', 'plans', id);
  }

  async function doStart(): Promise<void> {
    confirmStart = false;
    const id = detail?.ID;
    if (!id) return;
    starting = true;
    await runAdminAction(() => millsStore.startPipeline(id, { requeue: isEscalated }), {
      success: 'Pipeline started — watch it under Pipelines',
      failurePrefix: 'Start failed',
    });
    starting = false;
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
    {#if needsAttention}
      <section class="attention" role="alert">
        <span class="attention-icon" aria-hidden="true">⚠</span>
        <div class="attention-body">
          <strong>Needs attention — {detail.State}</strong>
          {#if attentionRun}
            <button type="button" class="attention-link" onclick={() => openRun(attentionRun.ID)}>
              {attentionRun.State}{#if attentionRun.CurrentStage} at <span class="mono">{attentionRun.CurrentStage}</span>{/if} · open run {shortRunID(attentionRun.ID)} →
            </button>
          {:else if settledWfRun}
            <!-- S7 terminal settle: the imperative run that escalated this
                 item, and the branch holding its work product. -->
            <button type="button" class="attention-link" onclick={() => settledWfRun && openWorkflowRun(settledWfRun.id)}>
              imperative run ended <strong>{settledWfRun.state}</strong> · open run {shortRunID(settledWfRun.id)} →
            </button>
            <span class="wf-branch">
              work product: <span class="mono">{workflowRunBranch(settledWfRun.id)}</span>
              <button type="button" class="copy-btn" onclick={() => settledWfRun && copyWfBranch(settledWfRun.id)} title="Copy branch name">
                {branchCopied ? '✓ copied' : '⧉ copy'}
              </button>
              <span class="muted">(pre-merge template — nothing was merged)</span>
            </span>
          {:else}
            <span class="muted">No run linked yet — check the council decision and dependencies.</span>
          {/if}
        </div>
      </section>
    {/if}
    <!-- Cost + linkouts -->
    <section class="grid">
      <div class="cell">
        <span class="k">Estimate</span>
        <span class="v">
          {#if est}
            {fmtCost(est.estimate_usd)} <span class="conf conf-{est.confidence}">{est.confidence}</span>
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
        <span class="k">Plan</span>
        <span class="v mono">
          {#if detail.PlanID}
            <button type="button" class="plan-link" onclick={() => detail.PlanID && openPlan(detail.PlanID)} title="Open the born-linked plan in Work → Plans">
              ◈ {detail.PlanID}
            </button>
          {:else}—{/if}
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
        <span class="v">{fmtRunTime(detail.CreatedAt)}{#if detail.CreatedBy} · {detail.CreatedBy}{/if}</span>
      </div>
    </section>

    {#if wfSelection || wfRuns.length > 0}
      <!-- S7 imperative lane: the item's template selection and the workflow
           runs claimed for it (these items get NO pipeline runs). -->
      <section class="block">
        <h4>Imperative workflow <span class="count">{wfRuns.length}</span></h4>
        {#if wfSelection}
          <p class="wf-selection">
            template <span class="mono">{wfSelection}</span>
            <span class="muted">— routed through the workflow lane; terminal outcomes escalate for review</span>
          </p>
        {/if}
        {#if wfRuns.length === 0}
          <p class="muted">No workflow runs yet for this item.</p>
        {:else}
          <ul class="runs">
            {#each wfRuns as r (r.id)}
              <li>
                <button type="button" class="run-link" onclick={() => openWorkflowRun(r.id)} title={r.id}>
                  <span class="mono">{shortRunID(r.id)}</span>
                  <span class="state state-{r.state}">{r.state}</span>
                  <span class="mono wf-template">{r.template}{r.template_version ? `@${r.template_version}` : ''}</span>
                  <span class="when">{fmtRunTime(r.started_at)}</span>
                  <span class="cost">{fmtCost(r.cost_usd)}</span>
                </button>
              </li>
            {/each}
          </ul>
        {/if}
      </section>
    {/if}

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
                <span class="mono">{shortRunID(r.ID)}</span>
                <span class="state state-{r.State}">{r.State}</span>
                {#if r.CurrentStage}<span class="stage">{r.CurrentStage}</span>{/if}
                {#if r.MRIID != null}<span class="mr">!{r.MRIID}</span>{/if}
                <span class="when">{fmtRunTime(r.StartedAt)}</span>
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
            {#if detail.Budget.max_cost_usd}{fmtCost(detail.Budget.max_cost_usd)}{/if}
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
          {starting ? 'Starting…' : isEscalated ? '▶ Re-run (requeue)' : '▶ Start pipeline'}
        </button>
      {/if}
      <button type="button" class="ghost" onclick={close}>Close</button>
    </div>
  {/snippet}
</DetailDrawer>

<ConfirmDialog
  open={confirmStart}
  title={isEscalated ? 'Re-run escalated item?' : 'Start pipeline?'}
  message={isEscalated
    ? `Requeue ${detail?.ID ?? 'this item'} (currently escalated) and kick off a fresh Mills pipeline run. This consumes budget and may open a merge request.`
    : `Kick off a Mills pipeline run for ${detail?.ID ?? 'this item'}. This consumes budget and may open a merge request.`}
  confirmLabel={isEscalated ? 'Re-run' : 'Start'}
  onConfirm={doStart}
  onCancel={() => (confirmStart = false)}
/>

<style>
  .muted { color: var(--text-muted); }
  .mono { font-family: var(--font-mono); }
  .attention {
    display: flex; gap: 0.6rem; align-items: flex-start;
    margin: 0 0 0.75rem; padding: 0.6rem 0.7rem; border-radius: var(--radius-sm);
    border: 1px solid color-mix(in srgb, var(--error) 45%, var(--border-subtle));
    background: color-mix(in srgb, var(--error) 9%, transparent);
  }
  .attention-icon { color: var(--warning); font-size: var(--text-sm); line-height: 1.2; }
  .attention-body { display: flex; flex-direction: column; gap: 0.2rem; min-width: 0; }
  .attention-body strong { color: var(--warning); font-size: var(--text-xs); }
  .attention-link {
    background: none; border: none; padding: 0; text-align: left; cursor: pointer;
    color: var(--mills); font-size: var(--text-xs);
  }
  .attention-link:hover { text-decoration: underline; }
  .wf-branch {
    display: flex; flex-wrap: wrap; gap: 0.35rem; align-items: center;
    font-size: var(--text-2xs); color: var(--text-muted); margin-top: 0.25rem;
  }
  .copy-btn {
    background: var(--bg-subtle); border: 1px solid var(--border-default);
    border-radius: var(--radius-sm); padding: 0 0.35rem; cursor: pointer;
    font-size: var(--text-2xs); color: var(--fg-secondary);
  }
  .copy-btn:hover { color: var(--fg-primary); }
  .wf-selection {
    font-size: var(--text-xs); color: var(--fg-secondary);
    margin: 0 0 0.4rem;
  }
  .wf-template { color: var(--text-muted); font-size: var(--text-2xs); }
  .chips { display: flex; flex-wrap: wrap; gap: 0.35rem; align-items: center; }
  .prio { font-size: var(--text-2xs); color: var(--text-muted); }
  .label {
    display: inline-block; padding: 0.05rem 0.4rem; border-radius: var(--radius-full);
    background: var(--bg-subtle); font-size: var(--text-2xs); color: var(--text-muted);
  }
  .grid {
    display: grid; grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.5rem 1rem; margin: 0.5rem 0 0.75rem;
  }
  .cell { display: flex; flex-direction: column; gap: 0.1rem; min-width: 0; }
  .k {
    font-size: var(--text-2xs); text-transform: uppercase; letter-spacing: 0.05em;
    color: var(--text-muted);
  }
  .v { font-size: var(--text-12); color: var(--fg-primary); overflow-wrap: anywhere; }
  .block { margin: 0.75rem 0; }
  .block h4 {
    margin: 0 0 0.35rem; font-size: var(--text-xs); font-weight: 600;
    color: var(--fg-secondary);
  }
  .block.warn h4 { color: var(--warning); }
  .count {
    padding: 0.02rem 0.35rem; border-radius: var(--radius-full); background: var(--bg-subtle);
    font-size: var(--text-2xs); color: var(--text-muted); font-family: var(--font-mono);
  }
  .spec { overflow-wrap: anywhere; font-size: var(--text-xs); }
  .runs, .slices { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.3rem; }
  .run-link {
    display: flex; flex-wrap: wrap; align-items: center; gap: 0.4rem; width: 100%;
    padding: 0.35rem 0.5rem; border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm); background: transparent; color: var(--fg-primary);
    cursor: pointer; text-align: left; font-size: var(--text-xs);
    transition: background 0.1s ease-out, border-color 0.1s ease-out;
  }
  .run-link:hover { background: rgba(var(--mills-rgb), 0.08); border-color: var(--accent); }
  .run-link:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .stage {
    padding: 0.02rem 0.35rem; border-radius: var(--radius-xs); font-size: var(--text-2xs);
    border: 1px solid color-mix(in srgb, var(--accent) 30%, var(--border-subtle));
    color: var(--fg-secondary); font-family: var(--font-mono);
  }
  .mr { color: var(--mills); font-family: var(--font-mono); font-size: var(--text-2xs); }
  .plan-link {
    display: inline-block; padding: 0.02rem 0.4rem; border-radius: var(--radius-sm); cursor: pointer;
    font-family: var(--font-mono); font-size: var(--text-xs);
    background: color-mix(in srgb, var(--accent) 14%, transparent);
    border: 1px solid color-mix(in srgb, var(--accent) 40%, var(--border-subtle));
    color: var(--mills);
  }
  .plan-link:hover { background: color-mix(in srgb, var(--accent) 26%, transparent); }
  .when { margin-left: auto; color: var(--text-muted); font-size: var(--text-2xs); }
  .slices li {
    display: flex; flex-wrap: wrap; align-items: baseline; gap: 0.5rem;
    font-size: var(--text-xs); padding: 0.15rem 0;
  }
  .slice-name { color: var(--fg-primary); }
  .slices .meta { font-size: var(--text-2xs); color: var(--text-muted); font-family: var(--font-mono); }
  .block p { margin: 0.15rem 0; font-size: var(--text-xs); }
  .err { color: var(--error); display: flex; flex-direction: column; gap: 0.4rem; }
  .err code { color: var(--text-muted); font-size: var(--text-xs); }
  .retry, .start, .ghost {
    cursor: pointer; border-radius: var(--radius-sm); font-size: var(--text-xs); padding: 0.3rem 0.7rem;
    border: 1px solid var(--border-subtle); background: transparent; color: var(--fg-primary);
  }
  .footer-row { display: flex; gap: 0.5rem; justify-content: flex-end; }
  .start {
    border-color: color-mix(in srgb, var(--success) 45%, var(--border));
    color: var(--success);
  }
  .start:hover:not(:disabled) { background: color-mix(in srgb, var(--success) 14%, transparent); }
  .start:disabled { opacity: 0.5; cursor: default; }
  .ghost:hover { background: var(--bg-subtle); }

  .state { padding: 0.1rem 0.4rem; border-radius: var(--radius-xs); font-size: var(--text-2xs); }
  .state-queued, .state-ready { background: var(--bg-subtle); color: var(--text-muted); }
  .state-running, .state-planning, .state-slicing, .state-implementing, .state-testing, .state-reviewing {
    background: rgba(var(--info-rgb), 0.15); color: var(--info);
  }
  .state-merged, .state-done { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .state-failed { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .state-escalated { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .state-paused { background: rgba(var(--warning-rgb), 0.1); color: var(--fg-secondary); }
  .conf { padding: 0.02rem 0.3rem; border-radius: var(--radius-xs); font-size: var(--text-2xs); text-transform: uppercase; }
  .conf-low { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .conf-medium { background: rgba(var(--warning-rgb), 0.18); color: var(--warning); }
  .conf-high { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .capped { font-size: var(--text-2xs); color: var(--error); }

  /* Phone reflow: the two-column meta grid gets too cramped to read. */
  @media (max-width: 480px) {
    .grid { grid-template-columns: 1fr; }
  }
</style>
