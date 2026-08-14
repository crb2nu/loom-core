<script lang="ts">
  /**
   * QueueColumn — the Operator Deck's left column: the work waiting on a
   * decision. Plans (the dispatchable unit) split into "ready" vs "in motion",
   * the task pressure rollup, and the busiest projects. Owns the /api/plans
   * fetch (plans have no shared store — PlansPanel precedent) and the
   * Spinning Room indicator/tray; dispatch + spin dialogs live in the panel
   * so the dock and board can share them.
   */
  import Badge from '../../widgets/Badge.svelte';
  import SpinningRoomTray from '../shared/SpinningRoomTray.svelte';
  import { router } from '../../stores/router.svelte.ts';
  import { taskStore } from '../../stores/tasks.svelte.ts';
  import { projectsStore } from '../../stores/projects.svelte.ts';
  import { spinRunsStore } from '../../stores/spinRuns.svelte.ts';
  import { clockStore } from '../../stores/staleness.svelte.ts';
  import { createPoller } from '../../utils/poller.ts';
  import type { SpinRun } from '../../utils/spinRunsHelpers.ts';
  import {
    type Plan,
    normalizePlan,
    planPhaseVariant,
    planPriorityVariant,
    sliceProgress,
  } from '../../utils/plansHelpers';

  let {
    onDispatch,
    onSpin,
    onRetrySpin,
  }: {
    onDispatch: (plan: Plan) => void;
    onSpin: () => void;
    onRetrySpin: (run: SpinRun) => void;
  } = $props();

  let plans = $state<Plan[]>([]);
  let plansAvailable = $state(true);
  let plansError = $state('');
  let loading = $state(true);

  async function load(): Promise<void> {
    try {
      const res = await fetch('/api/plans');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      plansAvailable = data.available !== false;
      plans = ((data.plans ?? []) as Plan[])
        .map((p) => normalizePlan(p))
        .filter((p): p is Plan => p !== null);
      plansError = '';
    } catch (e) {
      plansError = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  const poller = createPoller(() => load(), 30000);
  $effect(() => {
    void load();
    poller.start(30000);
    spinRunsStore.start();
    return () => {
      poller.stop();
      spinRunsStore.stop();
    };
  });

  // A spin the operator started landing a draft should show up now, not on
  // the 30s poll — same landedTick contract PlansPanel uses.
  let lastLanded = 0;
  $effect(() => {
    const t = spinRunsStore.landedTick;
    if (t !== lastLanded) {
      lastLanded = t;
      if (t > 0) void load();
    }
  });

  const READY_PHASES = new Set(['draft', 'planned']);
  const MOVING_PHASES = new Set(['implementing', 'review']);

  // P0 first, unset priority last, then freshest first — the queue is a
  // "what should run next" list, not an archive.
  function byUrgency(a: Plan, b: Plan): number {
    const pa = a.priority || 'P9';
    const pb = b.priority || 'P9';
    if (pa !== pb) return pa < pb ? -1 : 1;
    return (b.updated_at ?? '').localeCompare(a.updated_at ?? '');
  }

  const SHOW_MAX = 6;
  let ready = $derived(plans.filter((p) => READY_PHASES.has(p.phase)).sort(byUrgency));
  let moving = $derived(plans.filter((p) => MOVING_PHASES.has(p.phase)).sort(byUrgency));

  // Spinning Room indicator (shared durable store; see spinRuns.svelte.ts).
  let showSpinTray = $state(false);
  let now = $derived(clockStore.now);
  let spinLive = $derived(spinRunsStore.liveCount);
  let spinStuck = $derived(spinRunsStore.stuckCount(now));

  let topProjects = $derived(projectsStore.projects.slice(0, 5));

  function openPlan(plan: Plan): void {
    router.navigate('tasks', 'plans', plan.id);
  }
</script>

<div class="queue">
  <section class="q-section">
    <header class="q-head">
      <span class="q-title">Queue</span>
      <div class="spin-wrap">
        <button
          class="spin-chip"
          class:live={spinLive > 0}
          class:stuck={spinStuck > 0}
          onclick={() => (showSpinTray = !showSpinTray)}
          title="Spinning Room — live + recent spins"
        >
          {#if spinLive > 0}⟳ {spinLive}{#if spinStuck > 0} · ⚠{spinStuck}{/if}{:else}⟳{/if}
        </button>
        <SpinningRoomTray
          open={showSpinTray}
          onClose={() => (showSpinTray = false)}
          onOpenPlan={(pid) => router.navigate('tasks', 'plans', pid)}
          onRetry={(run) => { showSpinTray = false; onRetrySpin(run); }}
          onCompare={(planIds) => router.navigateCompare(planIds)}
        />
      </div>
      <button class="q-cta" onclick={onSpin} title="Spin a new plan on a model frame">+ Spin plan</button>
    </header>

    {#if loading}
      <div class="q-empty">Loading plans…</div>
    {:else if !plansAvailable}
      <div class="q-empty">Plan store not available on this daemon yet.</div>
    {:else if plansError}
      <div class="q-error">{plansError}</div>
    {:else}
      <div class="q-group">
        <div class="q-group-label">Ready <span class="q-count">{ready.length}</span></div>
        {#if ready.length === 0}
          <div class="q-empty">Nothing waiting — spin a plan to feed the mills.</div>
        {/if}
        {#each ready.slice(0, SHOW_MAX) as plan (plan.id)}
          <div class="plan-card">
            <button class="plan-main" onclick={() => openPlan(plan)} title={plan.objective || plan.title}>
              <span class="plan-title">{plan.title}</span>
              <span class="plan-meta">
                {#if plan.priority}<Badge text={plan.priority} variant={planPriorityVariant(plan.priority)} />{/if}
                <Badge text={plan.phase} variant={planPhaseVariant(plan.phase)} />
                {#if plan.project}<span class="plan-project">{plan.project}</span>{/if}
              </span>
            </button>
            <button class="plan-act" onclick={() => onDispatch(plan)} title="Dispatch: spawn an agent, hand to a live agent, or run in Mills">
              Dispatch
            </button>
          </div>
        {/each}
        {#if ready.length > SHOW_MAX}
          <button class="q-more" onclick={() => router.navigate('tasks', 'plans')}>
            +{ready.length - SHOW_MAX} more →
          </button>
        {/if}
      </div>

      <div class="q-group">
        <div class="q-group-label">In motion <span class="q-count">{moving.length}</span></div>
        {#each moving.slice(0, SHOW_MAX) as plan (plan.id)}
          {@const prog = sliceProgress(plan.slice_summary)}
          <div class="plan-card">
            <button class="plan-main" onclick={() => openPlan(plan)} title={plan.objective || plan.title}>
              <span class="plan-title">{plan.title}</span>
              <span class="plan-meta">
                <Badge text={plan.phase} variant={planPhaseVariant(plan.phase)} />
                {#if prog}
                  <span class="plan-project">{prog.merged}/{prog.total} slices merged</span>
                {/if}
              </span>
            </button>
          </div>
        {/each}
        {#if moving.length === 0}
          <div class="q-empty">No plans mid-flight.</div>
        {/if}
      </div>
    {/if}
  </section>

  <section class="q-section">
    <header class="q-head">
      <span class="q-title">Tasks</span>
      <button class="q-cta" onclick={() => router.navigate('tasks', 'tasks')}>all →</button>
    </header>
    <div class="task-chips">
      <span class="task-chip">{taskStore.pendingCount} pending</span>
      <span class="task-chip busy">{taskStore.inProgressCount} active</span>
      {#if taskStore.blockedCount > 0}
        <span class="task-chip warn">{taskStore.blockedCount} blocked</span>
      {/if}
    </div>
  </section>

  <section class="q-section">
    <header class="q-head">
      <span class="q-title">Projects</span>
      <button class="q-cta" onclick={() => router.navigate('tasks', 'projects')}>all →</button>
    </header>
    {#if topProjects.length === 0}
      <div class="q-empty">No project activity yet.</div>
    {:else}
      <ul class="proj-list">
        {#each topProjects as p (p.project)}
          <li>
            <button class="proj-row" onclick={() => router.navigate('tasks', 'projects', p.project)}>
              <span class="proj-name">{p.project}</span>
              <span class="proj-meta">
                {#if p.activeSessions > 0}<span class="proj-live">{p.activeSessions} live</span>{/if}
                {#if p.openTasks > 0}<span>{p.openTasks} open</span>{/if}
                {#if p.plans.length > 0}<span>{p.plans.length} plans</span>{/if}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </section>
</div>

<style>
  .queue {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .q-section {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: color-mix(in srgb, var(--bg-secondary) 82%, transparent);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    padding: var(--space-3);
  }

  .q-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .q-title {
    font-size: var(--text-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-secondary);
    flex: 1;
  }

  .q-cta {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    border: 1px solid transparent;
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .q-cta:hover { color: var(--fg-primary); border-color: var(--border-focus); }

  .spin-wrap { position: relative; display: inline-flex; }

  .spin-chip {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 2px 8px;
    border-radius: var(--radius-full);
    border: 1px solid var(--border-subtle);
    transition: color var(--transition-fast), border-color var(--transition-fast);
  }
  .spin-chip:hover { color: var(--fg-primary); border-color: var(--border-focus); }
  .spin-chip.live { color: var(--info); border-color: var(--info-dim); }
  .spin-chip.stuck { color: var(--warning); border-color: var(--warning-dim); }

  .q-group {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .q-group-label {
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    margin-top: var(--space-1);
  }

  .q-count {
    font-family: var(--font-mono);
    color: var(--fg-dim);
    font-weight: 500;
  }

  .plan-card {
    display: flex;
    align-items: stretch;
    gap: var(--space-1);
    border-radius: var(--radius-md);
    min-width: 0;
  }

  .plan-main {
    display: flex;
    flex-direction: column;
    gap: 3px;
    flex: 1;
    min-width: 0;
    text-align: left;
    padding: var(--space-2);
    border-radius: var(--radius-md);
    border: 1px solid transparent;
    cursor: pointer;
    transition: background var(--transition-fast);
  }
  .plan-main:hover { background: color-mix(in srgb, var(--bg-tertiary) 85%, transparent); }

  .plan-title {
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .plan-meta {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    flex-wrap: wrap;
  }

  .plan-project {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
  }

  /* Quiet at rest, accent on engagement. Six ready plans used to render six
     identical orange blocks — the loudest thing on the Deck, none of them an
     alert. The action tints accent when the operator engages the row (hover
     or keyboard focus), so the affordance is loud exactly when it's the
     likely next click. */
  .plan-act {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    padding: 0 var(--space-2);
    border-radius: var(--radius-md);
    border: 1px solid var(--border-subtle);
    background: transparent;
    cursor: pointer;
    flex-shrink: 0;
    transition: color var(--transition-fast),
                background var(--transition-fast),
                border-color var(--transition-fast);
  }
  .plan-card:hover .plan-act,
  .plan-card:focus-within .plan-act {
    color: var(--accent);
    border-color: color-mix(in srgb, var(--accent) 28%, transparent);
  }
  .plan-act:hover,
  .plan-act:focus-visible {
    color: var(--accent);
    background: var(--accent-dim);
    border-color: color-mix(in srgb, var(--accent) 40%, transparent);
  }

  .q-more {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    text-align: left;
    padding: var(--space-1) var(--space-2);
  }
  .q-more:hover { color: var(--fg-primary); }

  .q-empty {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    padding: var(--space-1) var(--space-1);
  }

  .q-error {
    font-size: var(--text-xs);
    color: var(--error);
  }

  .task-chips {
    display: flex;
    gap: var(--space-2);
    flex-wrap: wrap;
  }

  .task-chip {
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    padding: 3px 8px;
    border-radius: var(--radius-full);
    background: rgba(255, 255, 255, 0.04);
  }
  .task-chip.busy { color: var(--info); background: color-mix(in srgb, var(--info) 12%, transparent); }
  .task-chip.warn { color: var(--warning); background: color-mix(in srgb, var(--warning) 12%, transparent); }

  .proj-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .proj-row {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
    width: 100%;
    text-align: left;
    padding: 4px var(--space-2);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: background var(--transition-fast);
    min-width: 0;
  }
  .proj-row:hover { background: color-mix(in srgb, var(--bg-tertiary) 85%, transparent); }

  .proj-name {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    min-width: 0;
  }

  .proj-meta {
    display: flex;
    gap: var(--space-2);
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
    flex-shrink: 0;
  }

  .proj-live { color: var(--info); }
</style>
