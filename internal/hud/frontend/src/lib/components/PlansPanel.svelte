<script lang="ts">
  /**
   * PlansPanel — Work → Plans view. A lifecycle board over the agent-context
   * Plan store, served by the HUD `plans` domain (/api/plans). Visible + user-
   * manageable: create plans and advance their lifecycle phase. Degrades to a
   * "deploy pending" state when the daemon predates the plan store.
   */
  import Badge from '../widgets/Badge.svelte';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { router } from '../stores/router.svelte.ts';

  type Slice = { id: string; name: string; phase: string; assigned_agent_id?: string; mr_ref?: string };
  type Plan = {
    id: string; title: string; slug?: string; project?: string; namespace?: string;
    phase: string; created_by?: string; mr_refs?: string[]; pipeline_refs?: string[];
    mirror_path?: string; slices?: Slice[]; updated_at?: string;
  };

  // Lifecycle order for the board columns.
  const PHASES = ['draft', 'planned', 'in_progress', 'in_review', 'merging', 'merged', 'deployed', 'done'];
  const ADVANCE_TARGETS = [...PHASES, 'abandoned'];

  let plans = $state<Plan[]>([]);
  let available = $state(true);
  let loading = $state(true);
  let error = $state('');
  let selected = $state<Plan | null>(null);

  // Create form.
  let showCreate = $state(false);
  let newTitle = $state('');
  let newProject = $state('');

  function phaseVariant(phase: string): string {
    switch (phase) {
      case 'in_review': case 'merging': return 'warning';
      case 'merged': case 'deployed': case 'done': return 'success';
      case 'in_progress': case 'planned': return 'info';
      case 'abandoned': return 'error';
      default: return 'default';
    }
  }

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

  let byPhase = $derived(
    PHASES.map((ph) => ({ phase: ph, items: plans.filter((p) => p.phase === ph) }))
      .filter((col) => col.items.length > 0)
  );
  let abandoned = $derived(plans.filter((p) => p.phase === 'abandoned'));

  $effect(() => {
    load();
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
        <Badge text="{col.items.length} {col.phase.replaceAll('_', ' ')}" variant={phaseVariant(col.phase)} />
      {/each}
    </div>
    <div class="header-actions">
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
  {:else}
    <div class="board">
      {#each byPhase as col}
        <section class="col">
          <div class="col-head"><Badge text={col.phase.replaceAll('_', ' ')} variant={phaseVariant(col.phase)} /> <span class="dim">{col.items.length}</span></div>
          {#each col.items as plan}
            <button class="card" class:sel={selected?.id === plan.id} onclick={() => openDetail(plan)}>
              <div class="card-title">{plan.title}</div>
              <div class="card-meta dim">
                {#if plan.project}<span>{plan.project}</span>{/if}
                {#if plan.mr_refs?.length}<span>· {plan.mr_refs.length} MR</span>{/if}
                {#if plan.slices?.length}<span>· {plan.slices.length} slices</span>{/if}
              </div>
            </button>
          {/each}
        </section>
      {/each}
      {#if abandoned.length > 0}
        <section class="col">
          <div class="col-head"><Badge text="abandoned" variant="error" /> <span class="dim">{abandoned.length}</span></div>
          {#each abandoned as plan}
            <button class="card" onclick={() => openDetail(plan)}><div class="card-title">{plan.title}</div></button>
          {/each}
        </section>
      {/if}
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
        <Badge text={selected.phase} variant={phaseVariant(selected.phase)} />
        {#if selected.project}<span class="dim">{selected.project}</span>{/if}
      </div>
      <div class="detail-row">
        <label class="dim" for="adv">Advance to</label>
        <select id="adv" class="inp" onchange={(e) => advance(selected!, (e.currentTarget as HTMLSelectElement).value)}>
          <option value="">phase…</option>
          {#each ADVANCE_TARGETS as ph}
            {#if ph !== selected.phase}<option value={ph}>{ph.replaceAll('_', ' ')}</option>{/if}
          {/each}
        </select>
      </div>
      {#if selected.mirror_path}<div class="dim text-mono small">mirror: {selected.mirror_path}</div>{/if}
      {#if selected.slices?.length}
        <div class="detail-sub">Slices</div>
        <ul class="slices">
          {#each selected.slices as s}
            <li><Badge text={s.phase} variant={phaseVariant(s.phase)} /> <span>{s.name}</span>{#if s.assigned_agent_id}<span class="dim"> · {s.assigned_agent_id}</span>{/if}</li>
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
  .header-actions { display: flex; gap: var(--space-2); }
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
  .card {
    text-align: left; background: var(--bg-secondary); border: 1px solid var(--border);
    border-radius: var(--radius-md); padding: var(--space-2); cursor: pointer; color: inherit;
    display: flex; flex-direction: column; gap: 2px;
  }
  .card:hover { border-color: var(--accent, #2af); }
  .card.sel { border-color: var(--accent, #2af); box-shadow: 0 0 6px rgba(0,170,255,0.2); }
  .card-title { font-size: var(--text-sm); font-weight: 600; color: var(--fg-primary); }
  .card-meta { font-size: var(--text-xs); display: flex; gap: 4px; flex-wrap: wrap; }
  .detail {
    border-top: 1px solid var(--border); padding-top: var(--space-2);
    display: flex; flex-direction: column; gap: var(--space-2);
  }
  .detail-head { display: flex; justify-content: space-between; align-items: flex-start; }
  .detail-row { display: flex; align-items: center; gap: var(--space-2); }
  .detail-sub { font-weight: 600; color: var(--fg-primary); margin-top: var(--space-2); }
  .slices { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .slices li { display: flex; align-items: center; gap: 6px; font-size: var(--text-sm); }
</style>
