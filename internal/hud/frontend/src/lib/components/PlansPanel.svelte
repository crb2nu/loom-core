<script lang="ts">
  /**
   * PlansPanel — Work → Plans view. A lifecycle board over the agent-context
   * Plan store, served by the HUD `plans` domain (/api/plans). Visible + user-
   * manageable: create plans and advance their lifecycle phase. Degrades to a
   * "deploy pending" state when the daemon predates the plan store.
   *
   * Board-polish pass: detail opens in a right-edge drawer (not a bottom strip);
   * cards carry a slice-progress bar + age + Mills/kill-test/MR meta; phase
   * stat-pills double as filters; the slice→task rollup has explicit empties.
   */
  import Badge from '../widgets/Badge.svelte';
  import FilterBar from './shared/FilterBar.svelte';
  import DetailDrawer from './shared/DetailDrawer.svelte';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { router } from '../stores/router.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { relativeTime } from '../utils/format.ts';
  import {
    type Plan,
    PLAN_ADVANCE_TARGETS,
    planPhaseVariant,
    gitlabMrUrl,
    gitlabPipelineUrl,
    gitlabBranchUrl,
    refLabel,
    groupPlansByPhase,
    groupPlansByProject,
    filterPlans,
    projectOptionsFrom,
    sliceProgress,
  } from '../utils/plansHelpers';

  let plans = $state<Plan[]>([]);
  let available = $state(true);
  let loading = $state(true);
  let error = $state('');
  let selected = $state<Plan | null>(null);

  // Filter + view state.
  let search = $state('');
  let projectFilter = $state('');
  let phaseFilter = $state('');
  let viewMode = $state<'phase' | 'project'>('phase');

  // Create form.
  let showCreate = $state(false);
  let newTitle = $state('');
  let newProject = $state('');

  async function load() {
    loading = true;
    error = '';
    try {
      const res = await fetch('/api/plans');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      available = data.available !== false;
      plans = data.plans ?? [];
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  async function openDetail(plan: Plan) {
    if (selected?.id === plan.id) { selected = null; return; }
    await openById(plan.id, plan);
    void taskStore.fetch();
  }

  async function openById(id: string, fallback?: Plan) {
    try {
      const res = await fetch(`/api/plans/${encodeURIComponent(id)}`);
      const data = await res.json();
      selected = data.plan ?? fallback ?? plans.find((p) => p.id === id) ?? null;
    } catch {
      selected = fallback ?? plans.find((p) => p.id === id) ?? null;
    }
  }

  async function createPlan() {
    const title = newTitle.trim();
    if (!title) { toastStore.error('Title is required'); return; }
    try {
      const res = await fetch('/api/plans', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title, project: newProject.trim(), agent_id: 'hud-user' }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      toastStore.success(`Created ${data.plan_id}`);
      newTitle = ''; newProject = ''; showCreate = false;
      await load();
    } catch (e) {
      toastStore.error(`Create failed: ${e instanceof Error ? e.message : e}`);
    }
  }

  async function advance(plan: Plan, toPhase: string) {
    if (!toPhase || toPhase === plan.phase) return;
    try {
      const res = await fetch(`/api/plans/${encodeURIComponent(plan.id)}/advance`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ to_phase: toPhase, agent_id: 'hud-user' }),
      });
      if (res.status === 422) { toastStore.warning(`Illegal transition ${plan.phase} → ${toPhase}`); return; }
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toastStore.info(`${plan.title}: ${plan.phase} → ${toPhase}`);
      await load();
      if (selected?.id === plan.id) selected = { ...selected, phase: toPhase };
    } catch (e) {
      toastStore.error(`Advance failed: ${e instanceof Error ? e.message : e}`);
    }
  }

  function openBacklog(backlogId: string) {
    router.navigate('mills', 'backlog', backlogId);
  }
  // Jump to the agent working a slice in the Fleet view.
  function openAgent(_agentId: string) {
    router.navigate('agents', 'fleet');
  }

  let filtered = $derived(filterPlans(plans, search, projectFilter, phaseFilter));
  let byPhase = $derived(groupPlansByPhase(filtered));
  let byProject = $derived(groupPlansByProject(filtered));
  let projectOptions = $derived(projectOptionsFrom(plans));
  // Phase stat-pills run off the unfiltered set so the totals stay stable.
  let phaseTotals = $derived(groupPlansByPhase(plans));

  let filterDefs = $derived([
    { key: 'project', label: 'All Projects', value: projectFilter, options: projectOptions },
    {
      key: 'phase',
      label: 'All Phases',
      value: phaseFilter,
      options: PLAN_ADVANCE_TARGETS.map((p) => ({ value: p, label: p.replaceAll('_', ' ') })),
    },
  ]);

  function handleFilter(key: string, val: string) {
    if (key === 'project') projectFilter = val;
    else if (key === 'phase') phaseFilter = val;
  }
  function clearFilters() { search = ''; projectFilter = ''; phaseFilter = ''; }
  // Clicking a phase stat-pill toggles a phase filter (and resets when re-clicked).
  function togglePhaseFilter(phase: string) {
    phaseFilter = phaseFilter === phase ? '' : phase;
  }

  // --- Task rollup for the selected plan (plan → slice → task) -------------
  let planTasks = $derived(
    selected ? (taskStore.tasks ?? []).filter((t) => t.plan_id === selected!.id) : [],
  );
  function tasksForSlice(sliceId: string) {
    return planTasks.filter((t) => t.slice_id === sliceId);
  }
  let unslottedTasks = $derived(planTasks.filter((t) => !t.slice_id));

  function statusVariant(status: string): string {
    switch (status) {
      case 'completed': return 'success';
      case 'in_progress': return 'info';
      case 'blocked': return 'error';
      case 'cancelled': return 'default';
      default: return 'warning';
    }
  }

  $effect(() => {
    load();
    void taskStore.fetch();
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  });

  // Honor router deep-links: #tasks/plans/<plan-id> auto-opens that plan.
  $effect(() => {
    const wantId = router.detail;
    if (!wantId || selected?.id === wantId) return;
    void openById(wantId);
  });
</script>

{#snippet sliceBar(summary: Record<string, number> | undefined)}
  {@const prog = sliceProgress(summary)}
  {#if prog}
    <div class="slice-bar" title="{prog.merged}/{prog.total} slices merged">
      {#each prog.segments as seg}
        <span class="slice-seg" style:flex={seg.count} style:background={seg.color}></span>
      {/each}
    </div>
  {/if}
{/snippet}

{#snippet cardMeta(plan: Plan, showProject: boolean)}
  <div class="card-meta dim">
    {#if showProject && plan.project}<span class="text-mono">{plan.project}</span>{/if}
    {#if plan.slice_summary}<span>{sliceProgress(plan.slice_summary)?.merged}/{sliceProgress(plan.slice_summary)?.total} slices</span>{/if}
    {#if plan.mr_refs?.length}<span>· {plan.mr_refs.length} MR</span>{/if}
    {#if plan.kill_test_status}<span class="kt">· kill-test {plan.kill_test_status}</span>{/if}
    {#if plan.mills_backlog_id}<span title="Born-linked to a Mills backlog item">· ❖</span>{/if}
    {#if plan.updated_at}<span class="age">· {relativeTime(plan.updated_at)}</span>{/if}
  </div>
{/snippet}

<div class="panel plans-panel">
  <div class="header-bar">
    <div class="header-stats">
      <span class="header-total text-mono">{plans.length} plans</span>
      {#each phaseTotals as col}
        <button
          class="pill-btn"
          class:pill-active={phaseFilter === col.phase}
          onclick={() => togglePhaseFilter(col.phase)}
          title="Filter to {col.phase.replaceAll('_', ' ')}"
        >
          <Badge text="{col.items.length} {col.phase.replaceAll('_', ' ')}" variant={planPhaseVariant(col.phase)} />
        </button>
      {/each}
    </div>
    <div class="header-actions">
      <div class="view-toggle">
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'phase'} onclick={() => viewMode = 'phase'}>By Phase</button>
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'project'} onclick={() => viewMode = 'project'}>By Project</button>
      </div>
      <button class="btn btn-success" onclick={() => showCreate = !showCreate}>+ New Plan</button>
      <button class="btn btn-ghost" onclick={load}>Refresh</button>
    </div>
  </div>

  {#if showCreate}
    <div class="create-row">
      <input class="inp" placeholder="Plan title" bind:value={newTitle} />
      <input class="inp" placeholder="project (e.g. services/loom-core)" bind:value={newProject} />
      <button class="btn btn-success" onclick={createPlan}>Create</button>
      <button class="btn btn-ghost" onclick={() => showCreate = false}>Cancel</button>
    </div>
  {/if}

  <FilterBar
    {search}
    placeholder="Search plans…"
    filters={filterDefs}
    resultCount={filtered.length}
    onSearch={(val) => search = val}
    onFilter={handleFilter}
    onClear={clearFilters}
  />

  {#if loading && plans.length === 0}
    <div class="empty">Loading plans…</div>
  {:else if !available}
    <div class="empty">
      <strong>Plan store not available on this daemon yet.</strong>
      <div class="dim">Plans appear here once the agent-context daemon ships the <code>agent_plan_*</code> tools.</div>
    </div>
  {:else if error}
    <div class="empty error-text">Failed to load plans: {error}</div>
  {:else if plans.length === 0}
    <div class="empty">No plans yet. Create one, or agents will populate this as they plan work.</div>
  {:else if filtered.length === 0}
    <div class="empty">No plans match the current filters.</div>
  {:else if viewMode === 'phase'}
    <div class="board">
      {#each byPhase as col}
        <section class="col">
          <div class="col-head"><Badge text={col.phase.replaceAll('_', ' ')} variant={planPhaseVariant(col.phase)} /> <span class="dim">{col.items.length}</span></div>
          {#each col.items as plan}
            <button class="card" class:sel={selected?.id === plan.id} onclick={() => openDetail(plan)}>
              <div class="card-title">{plan.title}</div>
              {@render sliceBar(plan.slice_summary)}
              {@render cardMeta(plan, true)}
            </button>
          {/each}
        </section>
      {/each}
    </div>
  {:else}
    <div class="project-list">
      {#each byProject as group}
        <section class="proj-group">
          <div class="proj-head">
            <span class="proj-name text-mono">{group.project}</span>
            <span class="dim">{group.items.length} plan{group.items.length !== 1 ? 's' : ''}</span>
          </div>
          <div class="proj-cards">
            {#each group.items as plan}
              <button class="card" class:sel={selected?.id === plan.id} onclick={() => openDetail(plan)}>
                <div class="card-row">
                  <Badge text={plan.phase.replaceAll('_', ' ')} variant={planPhaseVariant(plan.phase)} />
                  <span class="card-title">{plan.title}</span>
                </div>
                {@render sliceBar(plan.slice_summary)}
                {@render cardMeta(plan, false)}
              </button>
            {/each}
          </div>
        </section>
      {/each}
    </div>
  {/if}
</div>

<DetailDrawer
  open={!!selected}
  title={selected?.title ?? ''}
  subtitle={selected?.id ?? ''}
  onClose={() => selected = null}
>
  {#snippet header()}
    {#if selected}
      <div class="drawer-chips">
        <Badge text={selected.phase} variant={planPhaseVariant(selected.phase)} />
        {#if selected.project}<span class="dim text-mono">{selected.project}</span>{/if}
        {#if selected.kill_test_status}<span class="dim">· kill-test: {selected.kill_test_status}</span>{/if}
      </div>
    {/if}
  {/snippet}

  {#if selected}
    <div class="d-row">
      <label class="dim" for="adv">Advance to</label>
      <select id="adv" class="inp" onchange={(e) => advance(selected!, (e.currentTarget as HTMLSelectElement).value)}>
        <option value="">phase…</option>
        {#each PLAN_ADVANCE_TARGETS as ph}
          {#if ph !== selected.phase}<option value={ph}>{ph.replaceAll('_', ' ')}</option>{/if}
        {/each}
      </select>
    </div>

    {#if selected.mills_backlog_id}
      <div class="d-row">
        <span class="dim">Mills backlog</span>
        <button class="ref-link" onclick={() => openBacklog(selected!.mills_backlog_id!)} title="Open the born-linked Mills backlog item">
          ❖ {selected.mills_backlog_id}
        </button>
      </div>
    {/if}

    {#if selected.mr_refs?.length || selected.pipeline_refs?.length}
      <div class="d-row refs">
        {#if selected.mr_refs?.length}
          <span class="dim">MRs</span>
          {#each selected.mr_refs as ref}
            {@const url = gitlabMrUrl(ref, selected.project)}
            {#if url}<a class="ref-link" href={url} target="_blank" rel="noopener">{refLabel(ref, 'mr')}</a>{:else}<span class="ref-chip">{ref}</span>{/if}
          {/each}
        {/if}
        {#if selected.pipeline_refs?.length}
          <span class="dim">Pipelines</span>
          {#each selected.pipeline_refs as ref}
            {@const url = gitlabPipelineUrl(ref, selected.project)}
            {#if url}<a class="ref-link" href={url} target="_blank" rel="noopener">{refLabel(ref, 'pipeline')}</a>{:else}<span class="ref-chip">{ref}</span>{/if}
          {/each}
        {/if}
      </div>
    {/if}

    {#if selected.mirror_path}<div class="dim text-mono small">mirror: {selected.mirror_path}</div>{/if}

    {#if selected.slices?.length}
      <div class="d-sub">Slices &amp; tasks</div>
      <ul class="slices">
        {#each selected.slices as s}
          {@const stasks = tasksForSlice(s.id)}
          <li class="slice">
            <div class="slice-head">
              <Badge text={s.phase} variant={planPhaseVariant(s.phase)} />
              <span class="slice-name">{s.name}</span>
              {#if s.assigned_agent_id}
                <button class="slice-agent" onclick={() => openAgent(s.assigned_agent_id)} title="Open {s.assigned_agent_id} in Fleet">{s.assigned_agent_id}</button>
              {/if}
              {#if s.mr_ref}
                {@const surl = gitlabMrUrl(s.mr_ref, selected.project)}
                {#if surl}<a class="ref-link small" href={surl} target="_blank" rel="noopener">{refLabel(s.mr_ref, 'mr')}</a>{/if}
              {/if}
              <span class="slice-tcount dim small">{stasks.length ? `${stasks.length} task${stasks.length !== 1 ? 's' : ''}` : 'no tasks'}</span>
            </div>
            {#if s.branch_name}
              {@const burl = gitlabBranchUrl(s.branch_name, selected.project)}
              <div class="slice-sub">
                {#if burl}
                  <a class="branch-link" href={burl} target="_blank" rel="noopener" title="Open branch on GitLab">⎇ {s.branch_name}</a>
                {:else}
                  <span class="dim text-mono small">⎇ {s.branch_name}</span>
                {/if}
              </div>
            {/if}
            {#if s.files?.length}
              <div class="slice-files dim small text-mono" title={s.files.join('\n')}>📄 {s.files.slice(0, 4).join(', ')}{#if s.files.length > 4} +{s.files.length - 4}{/if}</div>
            {/if}
            {#if s.decisions?.length}
              <ul class="slice-decisions">
                {#each s.decisions as d}<li>{d}</li>{/each}
              </ul>
            {/if}
            {#if stasks.length}
              <ul class="task-rollup">
                {#each stasks as t}
                  <li class="task-row" title={t.title}>
                    <Badge text={t.status} variant={statusVariant(t.status)} />
                    <span class="task-title">{t.title}</span>
                  </li>
                {/each}
              </ul>
            {/if}
          </li>
        {/each}
      </ul>
    {:else}
      <div class="d-sub">Slices &amp; tasks</div>
      <p class="dim small">No slices recorded for this plan yet.</p>
    {/if}

    {#if unslottedTasks.length}
      <div class="d-sub">Plan tasks (no slice)</div>
      <ul class="task-rollup">
        {#each unslottedTasks as t}
          <li class="task-row" title={t.title}>
            <Badge text={t.status} variant={statusVariant(t.status)} />
            <span class="task-title">{t.title}</span>
          </li>
        {/each}
      </ul>
    {:else if !selected.slices?.length}
      <p class="dim small">No tasks are linked to this plan. Agents link tasks via <code>plan_id</code>/<code>slice_id</code>.</p>
    {/if}
  {/if}
</DetailDrawer>

<style>
  .plans-panel { display: flex; flex-direction: column; overflow: hidden; gap: var(--space-2); }
  .header-bar {
    display: flex; align-items: center; justify-content: space-between;
    padding: var(--space-2) 0; border-bottom: 1px solid var(--border); flex-wrap: wrap; gap: var(--space-2);
  }
  .header-stats { display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-sm); flex-wrap: wrap; }
  .header-total { font-weight: 600; color: var(--fg-primary); }
  .pill-btn { background: none; border: none; padding: 0; cursor: pointer; border-radius: var(--radius-full); }
  .pill-btn.pill-active { outline: 2px solid var(--accent, #2af); outline-offset: 1px; border-radius: var(--radius-sm); }
  .header-actions { display: flex; gap: var(--space-2); align-items: center; }
  .view-toggle { display: flex; gap: 2px; background: var(--bg-tertiary); border-radius: var(--radius-sm); padding: 2px; }
  .active-toggle {
    background: var(--bg-elevated) !important; color: var(--fg-primary) !important;
    box-shadow: 0 0 4px rgba(0, 200, 255, 0.1);
  }
  .create-row { display: flex; gap: var(--space-2); align-items: center; }
  .inp {
    background: var(--bg-tertiary); border: 1px solid var(--border); color: var(--fg-primary);
    border-radius: var(--radius-sm); padding: 4px 8px; font-size: var(--text-sm);
  }
  .create-row .inp:first-child { flex: 1; }
  .empty { padding: var(--space-4); color: var(--fg-secondary); text-align: center; }
  .error-text { color: var(--status-error, #f55); }
  .dim { color: var(--fg-secondary); }
  .small { font-size: var(--text-xs); }
  .board { flex: 1; min-height: 0; overflow: auto; display: flex; gap: var(--space-3); align-items: flex-start; }
  .col { min-width: 230px; flex: 1; display: flex; flex-direction: column; gap: var(--space-2); }
  .col-head { display: flex; align-items: center; gap: var(--space-2); position: sticky; top: 0; }
  .project-list { flex: 1; min-height: 0; overflow: auto; display: flex; flex-direction: column; gap: var(--space-3); }
  .proj-group { display: flex; flex-direction: column; gap: var(--space-2); }
  .proj-head { display: flex; align-items: baseline; gap: var(--space-2); border-bottom: 1px solid var(--border-subtle); padding-bottom: 4px; }
  .proj-name { font-weight: 600; color: var(--fg-primary); font-size: var(--text-sm); }
  .proj-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(250px, 1fr)); gap: var(--space-2); }
  .card {
    text-align: left; background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: var(--space-2); cursor: pointer; color: inherit;
    display: flex; flex-direction: column; gap: 4px;
  }
  .card:hover { border-color: var(--accent, #2af); }
  .card.sel { border-color: var(--accent, #2af); box-shadow: 0 0 6px rgba(0,170,255,0.2); }
  .card-row { display: flex; align-items: center; gap: 6px; }
  .card-title { font-size: var(--text-sm); font-weight: 600; color: var(--fg-primary); }
  .card-meta { font-size: var(--text-xs); display: flex; gap: 4px; flex-wrap: wrap; align-items: center; }
  .card-meta .kt { color: var(--warning); }
  .card-meta .age { margin-left: auto; opacity: 0.8; }
  .slice-bar {
    display: flex; height: 4px; width: 100%; border-radius: 2px; overflow: hidden;
    background: var(--bg-tertiary);
  }
  .slice-seg { min-width: 2px; }
  /* Drawer detail */
  .drawer-chips { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .d-row { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; margin-bottom: var(--space-2); }
  .d-row.refs { font-size: var(--text-xs); }
  .d-sub { font-weight: 600; color: var(--fg-primary); margin: var(--space-3) 0 var(--space-1); }
  .ref-link {
    font-family: var(--font-mono); font-size: var(--text-xs); padding: 1px 6px;
    background: color-mix(in srgb, var(--accent, #2af) 12%, var(--bg-tertiary));
    border: 1px solid color-mix(in srgb, var(--accent, #2af) 35%, var(--border));
    border-radius: var(--radius-sm); color: var(--accent, #2af); cursor: pointer; text-decoration: none;
  }
  .ref-link:hover { background: color-mix(in srgb, var(--accent, #2af) 22%, var(--bg-tertiary)); }
  .ref-chip {
    font-family: var(--font-mono); font-size: var(--text-xs); padding: 1px 6px;
    background: var(--bg-tertiary); border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm); color: var(--fg-secondary);
  }
  .slices { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: var(--space-2); }
  .slice { display: flex; flex-direction: column; gap: 2px; }
  .slice-head { display: flex; align-items: center; gap: 6px; font-size: var(--text-sm); flex-wrap: wrap; }
  .slice-name { color: var(--fg-primary); }
  .slice-tcount { margin-left: auto; }
  .slice-agent {
    background: none; border: 1px solid var(--border-subtle); color: var(--fg-secondary);
    border-radius: var(--radius-sm); padding: 0 6px; font-size: var(--text-xs);
    font-family: var(--font-mono); cursor: pointer;
  }
  .slice-agent:hover { border-color: var(--accent, #2af); color: var(--accent, #2af); }
  .slice-sub { margin-top: 1px; }
  .branch-link {
    font-family: var(--font-mono); font-size: var(--text-xs); color: var(--fg-secondary);
    text-decoration: none; border-bottom: 1px dotted var(--border);
  }
  .branch-link:hover { color: var(--accent, #2af); border-bottom-color: var(--accent, #2af); }
  .slice-files { margin-top: 1px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .slice-decisions {
    list-style: disc; margin: 2px 0 0; padding-left: var(--space-4);
    font-size: var(--text-xs); color: var(--fg-secondary);
  }
  .task-rollup { list-style: none; margin: 2px 0 0; padding: 0 0 0 var(--space-3); display: flex; flex-direction: column; gap: 2px; border-left: 1px solid var(--border-subtle); }
  .task-row { display: flex; align-items: center; gap: 6px; font-size: var(--text-xs); min-width: 0; }
  .task-title { color: var(--fg-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
