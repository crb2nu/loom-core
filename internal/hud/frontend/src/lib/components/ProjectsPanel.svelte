<script lang="ts">
  /**
   * ProjectsPanel — Projects lens (Slice 4 of the Work/Mills UX work). A
   * first-class per-project rollup that federates plans, tasks, and active
   * sessions (all stamped with a GitLab path_with_namespace "project") into a
   * single pane. Cards deep-link out to the Plans, Tasks, and Fleet views so a
   * project becomes a navigable hub rather than a filter buried in three panels.
   */
  import Badge from '../widgets/Badge.svelte';
  import FilterBar from './shared/FilterBar.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import { router } from '../stores/router.svelte.ts';
  import { projectsStore, type ProjectRollup } from '../stores/projects.svelte.ts';
  import { planPhaseVariant } from '../utils/plansHelpers';
  import { relativeTime, statusVariant } from '../utils/format.ts';

  let search = $state('');
  let selectedId = $state<string | null>(null);

  let all = $derived(projectsStore.projects);
  let filtered = $derived(
    search.trim()
      ? all.filter((p) => p.project.toLowerCase().includes(search.trim().toLowerCase()))
      : all,
  );
  let selected = $derived(selectedId ? all.find((p) => p.project === selectedId) ?? null : null);

  let totalPlans = $derived(all.reduce((n, p) => n + p.plans.length, 0));
  let totalOpenTasks = $derived(all.reduce((n, p) => n + p.openTasks, 0));

  function millsLinked(p: ProjectRollup): number {
    return p.plans.filter((pl) => pl.mills_backlog_id).length;
  }

  function selectProject(id: string) {
    selectedId = selectedId === id ? null : id;
  }
  function openPlan(id: string) {
    router.navigate('tasks', 'plans', id);
  }
  function openPlans() {
    router.navigate('tasks', 'plans');
  }
  function openTasks() {
    router.navigate('tasks', 'tasks');
  }
  function openBacklog(id: string) {
    router.navigate('mills', 'warps', id);
  }

  $effect(() => {
    projectsStore.startPolling(30000);
    return () => projectsStore.stopPolling();
  });
</script>

<div class="panel projects-panel">
  <PanelHeader title="Projects" icon={'▦'} count={all.length}>
    {#snippet stats()}
      <!-- Header stat pills deep-link into the Work views, matching the
           TasksPanel/PlansPanel clickable-pill pattern. -->
      <button class="pill-btn" onclick={openPlans} title="Open Plans">
        <Badge text="{totalPlans} plans" variant="info" />
      </button>
      <button class="pill-btn" onclick={openTasks} title="Open Tasks">
        <Badge text="{totalOpenTasks} open tasks" variant="warning" />
      </button>
    {/snippet}
    {#snippet actions()}
      <button class="btn btn-ghost" onclick={() => projectsStore.fetch()}>Refresh</button>
    {/snippet}
  </PanelHeader>

  <FilterBar
    {search}
    placeholder="Search projects…"
    resultCount={filtered.length}
    onSearch={(val) => search = val}
  />

  {#if projectsStore.error && projectsStore.lastUpdated}
    <!-- Refresh failure with data already on screen: keep the cards visible
         but flag that they may be stale. Cold-start failures (no lastUpdated)
         are handled by the dedicated empty state below instead. -->
    <div class="error-row">
      <ErrorBanner prefix="Projects refresh failed" message={projectsStore.error} />
      <button class="btn btn-ghost" onclick={() => projectsStore.fetch()}>Retry</button>
    </div>
  {/if}

  {#if projectsStore.loading && all.length === 0}
    <div class="empty">Loading projects…</div>
  {:else if projectsStore.error && !projectsStore.lastUpdated}
    <div class="empty error-text">Couldn’t load projects: {projectsStore.error}. Use Refresh to retry, or check the daemon.</div>
  {:else if all.length === 0}
    <div class="empty">No projects yet. Projects appear here as agents create plans, tasks, and sessions scoped to a repo.</div>
  {:else if filtered.length === 0}
    <div class="empty">No projects match “{search}”.</div>
  {:else}
    <div class="grid">
      {#each filtered as p (p.project)}
        <button class="card" class:sel={selectedId === p.project} onclick={() => selectProject(p.project)}>
          <div class="card-head">
            <span class="card-title text-mono">{p.project}</span>
            {#if p.lastActivity > 0}<span class="dim small">{relativeTime(new Date(p.lastActivity).toISOString())}</span>{/if}
          </div>
          <div class="metrics">
            <span class="metric"><strong>{p.plans.length}</strong> plans</span>
            <span class="metric" class:warn={p.openTasks > 0}><strong>{p.openTasks}</strong> open</span>
            {#if p.blockedTasks > 0}<span class="metric err"><strong>{p.blockedTasks}</strong> blocked</span>{/if}
            {#if p.activeSessions > 0}<span class="metric ok"><strong>{p.activeSessions}</strong> active</span>{/if}
            {#if millsLinked(p) > 0}<span class="metric mills" title="Plans born-linked to Mills">❖ {millsLinked(p)}</span>{/if}
          </div>
          {#if p.plansByPhase.length}
            <div class="phase-row">
              {#each p.plansByPhase as ph}
                <Badge text="{ph.count} {ph.phase.replaceAll('_', ' ')}" variant={ph.variant} />
              {/each}
            </div>
          {/if}
        </button>
      {/each}
    </div>
  {/if}

  {#if selected}
    <aside class="detail">
      <div class="detail-head">
        <div class="card-title text-mono">{selected.project}</div>
        <button class="btn btn-ghost" aria-label="Close detail" onclick={() => selectedId = null}>✕</button>
      </div>

      <div class="detail-cols">
        <!-- Plans -->
        <section class="col">
          <div class="col-head">
            <h4>Plans <span class="count">{selected.plans.length}</span></h4>
            <button class="link-btn" onclick={openPlans}>open Plans →</button>
          </div>
          {#if selected.plans.length === 0}
            <p class="dim small">No plans.</p>
          {:else}
            <ul class="list">
              {#each selected.plans.slice(0, 12) as pl}
                <li>
                  <button class="row-link" onclick={() => openPlan(pl.id)} title={pl.title}>
                    <Badge text={pl.phase.replaceAll('_', ' ')} variant={planPhaseVariant(pl.phase)} />
                    <span class="row-title">{pl.title}</span>
                    {#if pl.mills_backlog_id}<span class="mills-dot" title="Born-linked to Mills">❖</span>{/if}
                  </button>
                </li>
              {/each}
            </ul>
            {#if selected.plans.length > 12}<p class="dim small">+{selected.plans.length - 12} more…</p>{/if}
          {/if}
        </section>

        <!-- Tasks -->
        <section class="col">
          <div class="col-head">
            <h4>Tasks <span class="count">{selected.tasks.length}</span></h4>
            <button class="link-btn" onclick={openTasks}>open Tasks →</button>
          </div>
          <div class="task-summary">
            <span class="metric" class:warn={selected.openTasks > 0}><strong>{selected.openTasks}</strong> open</span>
            <span class="metric"><strong>{selected.inProgressTasks}</strong> in-progress</span>
            {#if selected.blockedTasks > 0}<span class="metric err"><strong>{selected.blockedTasks}</strong> blocked</span>{/if}
          </div>
          {#if selected.tasks.length}
            <ul class="list">
              {#each selected.tasks.slice(0, 8) as t}
                <li class="task-row" title={t.title}>
                  <Badge text={t.status} variant={statusVariant(t.status)} />
                  <span class="row-title">{t.title}</span>
                </li>
              {/each}
            </ul>
          {/if}
        </section>

        <!-- Sessions / agents -->
        <section class="col">
          <div class="col-head">
            <h4>Active sessions <span class="count">{selected.activeSessions}</span></h4>
            <button class="link-btn" onclick={() => router.navigate('agents', 'fleet')}>open Fleet →</button>
          </div>
          {#if selected.agents.length}
            <div class="agent-chips">
              {#each selected.agents as a}<span class="agent-chip">{a}</span>{/each}
            </div>
          {:else}
            <p class="dim small">No active sessions.</p>
          {/if}
          {#if millsLinked(selected) > 0}
            {@const bl = selected.plans.find((pl) => pl.mills_backlog_id)?.mills_backlog_id}
            {#if bl}
              <button class="link-btn mills" onclick={() => openBacklog(bl)}>❖ open in Mills backlog →</button>
            {/if}
          {/if}
        </section>
      </div>
    </aside>
  {/if}
</div>

<style>
  .projects-panel { display: flex; flex-direction: column; overflow: hidden; gap: var(--space-2); }
  /* Clickable header stat pills — same treatment as TasksPanel. */
  .pill-btn { background: none; border: none; padding: 0; cursor: pointer; border-radius: var(--radius-full); transition: filter var(--transition-fast); }
  .pill-btn:hover { filter: brightness(1.25); }
  .pill-btn:focus-visible { outline: 2px solid color-mix(in srgb, var(--info) 55%, transparent); outline-offset: 2px; }
  .empty { padding: var(--space-4); color: var(--fg-secondary); text-align: center; }
  .error-text { color: var(--error); }
  /* Wraps the shared ErrorBanner so the Retry action sits beside it. */
  .error-row { display: flex; align-items: center; gap: var(--space-2); }
  .error-row > :global(.error-banner) { flex: 1; min-width: 0; margin-bottom: 0; }
  .dim { color: var(--fg-secondary); }
  .small { font-size: var(--text-xs); }
  .grid {
    flex: 1; min-height: 0; overflow: auto;
    display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
    gap: var(--space-3); align-content: start;
  }
  .card {
    text-align: left; background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: var(--space-3); cursor: pointer; color: inherit;
    display: flex; flex-direction: column; gap: var(--space-2);
  }
  .card:hover { border-color: var(--accent); }
  .card.sel { border-color: var(--accent); box-shadow: var(--glow-shadow-md) var(--glow-accent); }
  .card-head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-2); }
  .card-title { font-size: var(--text-sm); font-weight: 600; color: var(--fg-primary); overflow-wrap: anywhere; }
  .metrics { display: flex; flex-wrap: wrap; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .metric strong { color: var(--fg-primary); }
  .metric.warn strong { color: var(--warning); }
  .metric.err strong, .metric.err { color: var(--error); }
  .metric.ok strong { color: var(--success); }
  .metric.mills { color: var(--mills); }
  .phase-row { display: flex; flex-wrap: wrap; gap: 4px; }
  .detail {
    border-top: 1px solid var(--border); padding-top: var(--space-2);
    display: flex; flex-direction: column; gap: var(--space-2); max-height: 48%; overflow: auto;
  }
  .detail-head { display: flex; justify-content: space-between; align-items: center; }
  .detail-cols { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: var(--space-3); }
  .col { display: flex; flex-direction: column; gap: var(--space-1); min-width: 0; }
  .col-head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-2); }
  .col-head h4 { margin: 0; font-size: var(--text-sm); color: var(--fg-secondary); font-weight: 600; }
  .count {
    padding: 0.02rem 0.35rem; border-radius: 999px; background: var(--bg-tertiary);
    font-size: 0.66rem; color: var(--fg-secondary); font-family: var(--font-mono);
  }
  .link-btn {
    background: none; border: none; color: var(--accent); cursor: pointer;
    font-size: var(--text-xs); padding: 0;
  }
  .link-btn:hover { text-decoration: underline; }
  .link-btn.mills { color: var(--mills); margin-top: var(--space-1); }
  .list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 2px; }
  .row-link {
    display: flex; align-items: center; gap: 6px; width: 100%; text-align: left;
    background: none; border: none; cursor: pointer; color: inherit; padding: 2px 0; min-width: 0;
  }
  .row-link:hover .row-title { color: var(--accent); }
  .row-title { font-size: var(--text-xs); color: var(--fg-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mills-dot { color: var(--mills); font-size: var(--text-xs); }
  .task-row { display: flex; align-items: center; gap: 6px; min-width: 0; }
  .task-summary { display: flex; gap: var(--space-2); font-size: var(--text-xs); color: var(--fg-secondary); }
  .agent-chips { display: flex; flex-wrap: wrap; gap: 4px; }
  .agent-chip {
    font-family: var(--font-mono); font-size: var(--text-xs); padding: 1px 6px;
    background: var(--bg-tertiary); border: 1px solid var(--border-subtle); border-radius: var(--radius-sm);
    color: var(--fg-secondary);
  }
</style>
