<script lang="ts">
  /**
   * PlansPanel — Work → Plans view. A lifecycle board over the agent-context
   * Plan store, served by the HUD `plans` domain (/api/plans). Visible + user-
   * manageable: create plans and advance their lifecycle phase. Degrades to a
   * "deploy pending" state when the daemon predates the plan store.
   *
   * Slice 2 (Work/Mills UX): plan → slice → task rollup, clickable MR/pipeline
   * refs, project grouping + filters, and a deep-link to the born-linked Mills
   * backlog item.
   */
  import Badge from '../widgets/Badge.svelte';
  import FilterBar from './shared/FilterBar.svelte';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { router } from '../stores/router.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import {
    type Plan,
    PLAN_ADVANCE_TARGETS,
    planPhaseVariant,
    gitlabMrUrl,
    gitlabPipelineUrl,
    refLabel,
    groupPlansByPhase,
    groupPlansByProject,
    filterPlans,
    projectOptionsFrom,
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
    // Freshen the task rollup when a plan opens.
    void taskStore.fetch();
  }

  // Open a plan by id, fetching the full record (with slices). Used by both
  // card clicks and router deep-links (e.g. a task's "Plan" chip in TasksPanel
  // navigates here with the plan id as the route detail segment).
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

  // Deep-link a born-linked plan to its Mills backlog item.
  function openBacklog(backlogId: string) {
    router.navigate('mills', 'backlog', backlogId);
  }

  let filtered = $derived(filterPlans(plans, search, projectFilter, phaseFilter));
  let byPhase = $derived(groupPlansByPhase(filtered));
  let byProject = $derived(groupPlansByProject(filtered));
  let projectOptions = $derived(projectOptionsFrom(plans));

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
    // Tasks power the rollup; fetch once up front (TasksPanel polls separately).
    void taskStore.fetch();
    const t = setInterval(load, 30000);
    return () => clearInterval(t);
  });

  // Honor router deep-links: when navigated to #tasks/plans/<plan-id> (e.g. from
  // a task's Plan chip), auto-open that plan once it resolves.
  $effect(() => {
    const wantId = router.detail;
    if (!wantId || selected?.id === wantId) return;
    void openById(wantId);
  });
</script>

<div class="panel plans-panel">
  <div class="header-bar">
    <div class="header-stats">
      <span class="header-total text-mono">{plans.length} plans</span>
      {#each byPhase as col}
        <Badge text="{col.items.length} {col.phase.replaceAll('_', ' ')}" variant={planPhaseVariant(col.phase)} />
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
              <div class="card-meta dim">
                {#if plan.project}<span>{plan.project}</span>{/if}
                {#if plan.mr_refs?.length}<span>· {plan.mr_refs.length} MR</span>{/if}
                {#if plan.slices?.length}<span>· {plan.slices.length} slices</span>{/if}
                {#if plan.mills_backlog_id}<span title="Born-linked to a Mills backlog item">· ❖</span>{/if}
              </div>
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
                <div class="card-meta dim">
                  {#if plan.mr_refs?.length}<span>{plan.mr_refs.length} MR</span>{/if}
                  {#if plan.slices?.length}<span>· {plan.slices.length} slices</span>{/if}
                  {#if plan.mills_backlog_id}<span title="Born-linked to a Mills backlog item">· ❖ Mills</span>{/if}
                </div>
              </button>
            {/each}
          </div>
        </section>
      {/each}
    </div>
  {/if}

  {#if selected}
    <aside class="detail">
      <div class="detail-head">
        <div>
          <div class="card-title">{selected.title}</div>
          <div class="dim text-mono">{selected.id}</div>
        </div>
        <button class="btn btn-ghost" onclick={() => selected = null}>✕</button>
      </div>
      <div class="detail-row">
        <Badge text={selected.phase} variant={planPhaseVariant(selected.phase)} />
        {#if selected.project}<span class="dim">{selected.project}</span>{/if}
        {#if selected.kill_test_status}<span class="dim">· kill-test: {selected.kill_test_status}</span>{/if}
      </div>
      <div class="detail-row">
        <label class="dim" for="adv">Advance to</label>
        <select id="adv" class="inp" onchange={(e) => advance(selected!, (e.currentTarget as HTMLSelectElement).value)}>
          <option value="">phase…</option>
          {#each PLAN_ADVANCE_TARGETS as ph}
            {#if ph !== selected.phase}<option value={ph}>{ph.replaceAll('_', ' ')}</option>{/if}
          {/each}
        </select>
      </div>

      {#if selected.mills_backlog_id}
        <div class="detail-row">
          <span class="dim">Mills backlog</span>
          <button class="ref-link" onclick={() => openBacklog(selected!.mills_backlog_id!)} title="Open the born-linked Mills backlog item">
            ❖ {selected.mills_backlog_id}
          </button>
        </div>
      {/if}

      {#if selected.mr_refs?.length || selected.pipeline_refs?.length}
        <div class="detail-row refs">
          {#if selected.mr_refs?.length}
            <span class="dim">MRs</span>
            {#each selected.mr_refs as ref}
              {@const url = gitlabMrUrl(ref, selected.project)}
              {#if url}
                <a class="ref-link" href={url} target="_blank" rel="noopener">{refLabel(ref, 'mr')}</a>
              {:else}
                <span class="ref-chip">{ref}</span>
              {/if}
            {/each}
          {/if}
          {#if selected.pipeline_refs?.length}
            <span class="dim">Pipelines</span>
            {#each selected.pipeline_refs as ref}
              {@const url = gitlabPipelineUrl(ref, selected.project)}
              {#if url}
                <a class="ref-link" href={url} target="_blank" rel="noopener">{refLabel(ref, 'pipeline')}</a>
              {:else}
                <span class="ref-chip">{ref}</span>
              {/if}
            {/each}
          {/if}
        </div>
      {/if}

      {#if selected.mirror_path}<div class="dim text-mono small">mirror: {selected.mirror_path}</div>{/if}

      {#if selected.slices?.length}
        <div class="detail-sub">Slices &amp; tasks</div>
        <ul class="slices">
          {#each selected.slices as s}
            <li class="slice">
              <div class="slice-head">
                <Badge text={s.phase} variant={planPhaseVariant(s.phase)} />
                <span class="slice-name">{s.name}</span>
                {#if s.assigned_agent_id}<span class="dim small"> · {s.assigned_agent_id}</span>{/if}
                {#if s.mr_ref}
                  {@const surl = gitlabMrUrl(s.mr_ref, selected.project)}
                  {#if surl}<a class="ref-link small" href={surl} target="_blank" rel="noopener">{refLabel(s.mr_ref, 'mr')}</a>{/if}
                {/if}
              </div>
              {#if s.branch_name}<div class="dim text-mono small">⎇ {s.branch_name}</div>{/if}
              {#if tasksForSlice(s.id).length}
                <ul class="task-rollup">
                  {#each tasksForSlice(s.id) as t}
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
      {/if}

      {#if unslottedTasks.length}
        <div class="detail-sub">Plan tasks (no slice)</div>
        <ul class="task-rollup">
          {#each unslottedTasks as t}
            <li class="task-row" title={t.title}>
              <Badge text={t.status} variant={statusVariant(t.status)} />
              <span class="task-title">{t.title}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </aside>
  {/if}
</div>

<style>
  .plans-panel { display: flex; flex-direction: column; overflow: hidden; gap: var(--space-2); }
  .header-bar {
    display: flex; align-items: center; justify-content: space-between;
    padding: var(--space-2) 0; border-bottom: 1px solid var(--border); flex-wrap: wrap; gap: var(--space-2);
  }
  .header-stats { display: flex; align-items: center; gap: var(--space-2); font-size: var(--text-sm); flex-wrap: wrap; }
  .header-total { font-weight: 600; color: var(--fg-primary); }
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
  .col { min-width: 220px; flex: 1; display: flex; flex-direction: column; gap: var(--space-2); }
  .col-head { display: flex; align-items: center; gap: var(--space-2); position: sticky; top: 0; }
  .project-list { flex: 1; min-height: 0; overflow: auto; display: flex; flex-direction: column; gap: var(--space-3); }
  .proj-group { display: flex; flex-direction: column; gap: var(--space-2); }
  .proj-head { display: flex; align-items: baseline; gap: var(--space-2); border-bottom: 1px solid var(--border-subtle); padding-bottom: 4px; }
  .proj-name { font-weight: 600; color: var(--fg-primary); font-size: var(--text-sm); }
  .proj-cards { display: grid; grid-template-columns: repeat(auto-fill, minmax(240px, 1fr)); gap: var(--space-2); }
  .card {
    text-align: left; background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: var(--space-2); cursor: pointer; color: inherit;
    display: flex; flex-direction: column; gap: 2px;
  }
  .card:hover { border-color: var(--accent, #2af); }
  .card.sel { border-color: var(--accent, #2af); box-shadow: 0 0 6px rgba(0,170,255,0.2); }
  .card-row { display: flex; align-items: center; gap: 6px; }
  .card-title { font-size: var(--text-sm); font-weight: 600; color: var(--fg-primary); }
  .card-meta { font-size: var(--text-xs); display: flex; gap: 4px; flex-wrap: wrap; }
  .detail {
    border-top: 1px solid var(--border); padding-top: var(--space-2);
    display: flex; flex-direction: column; gap: var(--space-2); max-height: 45%; overflow: auto;
  }
  .detail-head { display: flex; justify-content: space-between; align-items: flex-start; }
  .detail-row { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .detail-row.refs { font-size: var(--text-xs); }
  .detail-sub { font-weight: 600; color: var(--fg-primary); margin-top: var(--space-2); }
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
  .task-rollup { list-style: none; margin: 2px 0 0; padding: 0 0 0 var(--space-3); display: flex; flex-direction: column; gap: 2px; border-left: 1px solid var(--border-subtle); }
  .task-row { display: flex; align-items: center; gap: 6px; font-size: var(--text-xs); min-width: 0; }
  .task-title { color: var(--fg-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
