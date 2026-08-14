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
  import PanelHeader from './shared/PanelHeader.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import DetailDrawer from './shared/DetailDrawer.svelte';
  import ConfirmDialog from './shared/ConfirmDialog.svelte';
  import PlanDispatchDialog from './shared/PlanDispatchDialog.svelte';
  import SpinPlanDialog from './shared/SpinPlanDialog.svelte';
  import SpinningRoomTray from './shared/SpinningRoomTray.svelte';
  import BootstrapRepoDialog from './shared/BootstrapRepoDialog.svelte';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { router } from '../stores/router.svelte.ts';
  import { taskStore } from '../stores/tasks.svelte.ts';
  import { millsStore } from '../stores/mills.svelte.ts';
  import { spawnStore } from '../stores/spawn.svelte.ts';
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { navigateToAgentSessionOrTraces } from '../utils/drilldown.ts';
  import { spinRunsStore } from '../stores/spinRuns.svelte.ts';
  import { clockStore } from '../stores/staleness.svelte.ts';
  import { createHandoff } from '../clients/presenceActions.ts';
  import { relativeTime, statusVariant } from '../utils/format.ts';
  import type { SpinRun } from '../utils/spinRunsHelpers.ts';
  import {
    type Plan,
    type PlanSlice,
    PLAN_ADVANCE_TARGETS,
    PLAN_PRIORITIES,
    planPhaseVariant,
    planPriorityVariant,
    gitlabMrUrl,
    gitlabPipelineUrl,
    gitlabBranchUrl,
    refLabel,
    groupPlansByPhase,
    groupPlansByProject,
    filterPlans,
    projectOptionsFrom,
    sliceProgress,
    sliceDependsLabels,
    normalizePlan,
    dispatchInstructions,
    buildMillsBacklogItem,
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
  let newPriority = $state('');

  // Spinning Room dialog (Live Beam slice 3): spin a draft plan on a chosen
  // model frame. The dialog fires an ASYNC spin (202 + spin_id) and hands the id
  // here via onQueued; we poll each in-flight spin until it lands a draft (plan
  // .loom/166) and reload the board on success. A frontier frame runs minutes,
  // past the client-facing proxy timeout, so we never hold the request open.
  let showSpin = $state(false);

  // Respin: redo an existing plan or slice on a model frame. Non-destructive —
  // we seed the Spin dialog's brief + scope from the plan/slice and let the
  // normal async spin author a FRESH draft to compare and advance (older,
  // pre-spinner plans are the main use). null seed = a plain "Spin a plan".
  interface SpinSeed {
    brief: string;
    project: string;
    namespace: string;
    priority: string;
    label: string;
    respunFrom: string;
    frames?: string[];
  }
  let respinSeed = $state<SpinSeed | null>(null);

  // Plan→repo bootstrap: mint a GitLab project for a plan that has no home yet
  // (project=""), seed it, and re-scope the plan onto the new path so the
  // beam's plan-slice emitter can source it. Only meaningful for project-less
  // plans in a pre-implementation phase.
  let showBootstrap = $state(false);
  let bootstrapTarget = $state<Plan | null>(null);
  function openBootstrap(plan: Plan): void {
    bootstrapTarget = plan;
    showBootstrap = true;
  }
  // A plan is bootstrappable when it has no project yet and hasn't started
  // implementing — mirrors the operator's bootstrappablePhases gate.
  function canBootstrap(plan: Plan | null): boolean {
    if (!plan) return false;
    const phase = (plan.phase ?? '').toLowerCase();
    return !(plan.project ?? '').trim() && (phase === 'draft' || phase === 'planned');
  }

  function openFreshSpin(): void {
    respinSeed = null;
    showSpin = true;
  }

  function respinPlan(plan: Plan): void {
    respinSeed = {
      brief: buildPlanRespinBrief(plan),
      project: plan.project ?? '',
      namespace: plan.namespace ?? '',
      priority: plan.priority ?? '',
      label: `Respin plan: ${plan.title}`,
      respunFrom: plan.id,
    };
    showSpin = true;
  }

  function respinSlice(plan: Plan, slice: PlanSlice): void {
    respinSeed = {
      brief: buildSliceRespinBrief(plan, slice),
      project: plan.project ?? '',
      namespace: plan.namespace ?? '',
      priority: plan.priority ?? '',
      label: `Respin slice: ${slice.name}`,
      respunFrom: plan.id,
    };
    showSpin = true;
  }

  function buildPlanRespinBrief(plan: Plan): string {
    const lines = [
      'Redo and expand this existing plan into a richer, fully-decomposed draft. ' +
        'Preserve the intent; where the original is sparse, add the missing slices, ' +
        'concrete file scopes, and acceptance criteria.',
      '',
      `Plan: ${plan.title}`,
    ];
    if (plan.priority) lines.push(`Priority: ${plan.priority}`);
    if (plan.project) lines.push(`Project: ${plan.project}`);
    const slices = plan.slices ?? [];
    lines.push('');
    if (slices.length) {
      lines.push('Existing slices:');
      for (const s of slices) {
        const files = s.files?.length ? ` (files: ${s.files.join(', ')})` : '';
        lines.push(`- ${s.name}${files}`);
      }
    } else {
      lines.push('(The original plan has no slices — decompose it from the title/intent above.)');
    }
    return lines.join('\n');
  }

  function buildSliceRespinBrief(plan: Plan, slice: PlanSlice): string {
    const lines = [
      'Expand this single slice from an existing plan into a fuller, self-contained ' +
        'draft plan with concrete, independently-shippable implementation slices.',
      '',
      `From plan: ${plan.title}`,
      `Slice: ${slice.name}`,
    ];
    if (slice.files?.length) lines.push(`Files: ${slice.files.join(', ')}`);
    if (slice.decisions?.length) {
      lines.push('Notes:');
      for (const d of slice.decisions) lines.push(`- ${d}`);
    }
    return lines.join('\n');
  }

  // Spinning Room status surface. spinRunsStore polls the operator's DURABLE
  // spin-runs log (survives refresh, shows spins from any session); the tray
  // reads it and the header indicator summarises it. onQueued registers the id
  // the user just started so its completion toasts (via the store), and pops
  // the tray so the operator immediately sees it tracking.
  let showSpinTray = $state(false);
  let now = $derived(clockStore.now);
  let spinVisible = $derived(spinRunsStore.visible(now));
  let spinLive = $derived(spinRunsStore.liveCount);
  let spinStuck = $derived(spinRunsStore.stuckCount(now));
  // plan id → competitive spin it belongs to, so sibling drafts group on the
  // board instead of appearing as two disconnected cards.
  let spinGroups = $derived(spinRunsStore.competitiveGroups);

  function onSpinQueued(spin: { spinId: string; frames: string[] }): void {
    spinRunsStore.track(spin.spinId);
    showSpinTray = true; // surface the tracker immediately
  }

  // Retry a failed/timed-out/stuck spin from the tray: seed a fresh Spin dialog
  // with the original brief + frames + scope (a plain spin, not a plan respin).
  function respinFromRun(run: SpinRun): void {
    respinSeed = {
      brief: run.brief,
      project: run.project ?? '',
      namespace: run.namespace ?? '',
      priority: run.priority ?? '',
      label: 'Retry spin',
      respunFrom: '',
      frames: run.frames,
    };
    showSpin = true;
  }

  // A spin the user started reaching `succeeded` bumps landedTick — refresh the
  // board so its draft appears now, not on the 30s poll. Guarded so the initial
  // (tick=0) run doesn't double-load on mount.
  let lastLanded = 0;
  $effect(() => {
    const t = spinRunsStore.landedTick;
    if (t !== lastLanded) {
      lastLanded = t;
      if (t > 0) void load();
    }
  });

  async function load() {
    loading = true;
    error = '';
    try {
      const res = await fetch('/api/plans');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      available = data.available !== false;
      // Normalize at the fetch boundary so objective/slice-tissue fields are
      // never `undefined` downstream (older plans coerce to empty, not "undefined").
      plans = ((data.plans ?? []) as Plan[]).map((p) => normalizePlan(p)!).filter(Boolean);
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  // Selection is route-driven: open/close mutate router.detail and the $effect
  // below loads/clears `selected` to match. This keeps a single source of truth
  // so closing the drawer can't be undone by the deep-link effect re-opening it
  // (the bug: onClose cleared `selected` but router.detail stayed set, so the
  // effect immediately re-opened — an infinite popup).
  function openDetail(plan: Plan) {
    if (selected?.id === plan.id) { closeDetail(); return; }
    router.navigateDetail(plan.id);
  }

  function closeDetail() {
    if (selected) selected = null;
    if (router.detail) router.navigateDetail(null);
  }

  async function openById(id: string, fallback?: Plan) {
    // Show the cached row immediately, then enrich with the full record (slices).
    if (fallback && selected?.id !== id) selected = fallback;
    try {
      const res = await fetch(`/api/plans/${encodeURIComponent(id)}`);
      const data = await res.json();
      selected = normalizePlan(data.plan) ?? fallback ?? plans.find((p) => p.id === id) ?? null;
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
        body: JSON.stringify({ title, project: newProject.trim(), priority: newPriority, agent_id: 'hud-user' }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      toastStore.success(`Created ${data.plan_id}`);
      newTitle = ''; newProject = ''; newPriority = ''; showCreate = false;
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

  // Set/clear a plan's warp-beam priority. The plan-slice emitter resyncs
  // still-queued Mills items to the new bucket on its next tick, so this is
  // the live dispatch-reorder knob, not just a display label.
  async function setPriority(plan: Plan, priority: string) {
    if ((plan.priority ?? '') === priority) return;
    try {
      const res = await fetch(`/api/plans/${encodeURIComponent(plan.id)}/priority`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ priority }),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      toastStore.info(`${plan.title}: priority ${priority || 'cleared'}`);
      await load();
      if (selected?.id === plan.id) selected = { ...selected, priority };
    } catch (e) {
      toastStore.error(`Set priority failed: ${e instanceof Error ? e.message : e}`);
    }
  }

  // After a repo is minted for a plan, the plan was re-scoped onto the new
  // path server-side — reload the board so the card moves under its project,
  // and refresh the open drawer to reflect the new project + link.
  async function onRepoBootstrapped(res: { project: string; plan_id: string }): Promise<void> {
    await load();
    if (selected?.id === res.plan_id) {
      selected = { ...selected, project: res.project };
      await openById(res.plan_id, selected);
    }
  }

  function openBacklog(backlogId: string) {
    router.navigate('mills', 'warps', backlogId);
  }
  // Jump to the agent working a slice: its session detail in Fleet when
  // one resolves, otherwise its filtered traces (same drilldown Lifecycle
  // uses). Previously this dropped the agent id and landed on the bare
  // Fleet roster.
  function openAgent(agentId: string) {
    navigateToAgentSessionOrTraces(router, { agent_id: agentId }, (id) => fleetStore.sessionForAgent(id));
  }

  // --- Hand a plan/slice off to a session, an agent, or Mills --------------
  // One "Hand off" affordance per plan and per slice opens a chooser dialog;
  // every destination routes through here. `dispatchTarget` is the plan (and
  // optional slice) being dispatched; `confirmMillsTarget` defers the autonomous
  // Mills path behind a second confirm because it spends budget + may merge.
  let busy = $state(false);
  let dispatchOpen = $state(false);
  let dispatchTarget = $state<{ plan: Plan; slice?: PlanSlice } | null>(null);
  let confirmMillsTarget = $state<{ plan: Plan; slice?: PlanSlice } | null>(null);

  function openDispatch(plan: Plan, slice?: PlanSlice) {
    dispatchTarget = { plan, slice };
    dispatchOpen = true;
    // Refresh the live-agent list so the "hand to existing agent" picker is
    // current even when the Fleet panel was never mounted this session.
    void fleetStore.fetch();
  }
  function closeDispatch() {
    dispatchOpen = false;
    dispatchTarget = null;
  }

  // Spawn an agent session to work a plan (or a specific slice). Runs on the
  // connected daemon's spawn substrate; the plan/slice ids ride in metadata so
  // the session is traceable and the spawned agent can fetch the live spec.
  async function spawnSession(plan: Plan, slice?: PlanSlice) {
    if (busy) return;
    busy = true;
    const meta: Record<string, string> = { plan_id: plan.id, source: 'hud-plans' };
    if (slice) meta.slice_id = slice.id;
    const branch = slice?.branch_name || plan.slices?.[0]?.branch_name;
    const res = await spawnStore.spawn({
      agent_type: 'claude-code',
      project: plan.project || 'loom-core',
      task_description: slice ? `Work plan "${plan.title}" — slice: ${slice.name}` : `Work on plan: ${plan.title}`,
      ...(branch ? { branch } : {}),
      metadata: meta,
    });
    busy = false;
    if (res?.spawn_id) {
      toastStore.success(`Spawned ${res.spawn_id}`);
      router.navigate('sandbox', 'spawn', res.spawn_id);
    } else {
      toastStore.error('Spawn failed (admin token required?)');
    }
  }

  // Dispatch-dialog → spawn a fresh session for the chosen target.
  async function dispatchSpawn() {
    const t = dispatchTarget;
    if (!t) return;
    closeDispatch();
    await spawnSession(t.plan, t.slice);
  }

  // Dispatch-dialog → hand the target off to an existing agent's inbox. The
  // backend auto-provisions a source dispatch session, so we only need the
  // target agent + a self-contained brief.
  async function dispatchHandoff(targetAgentId: string) {
    const t = dispatchTarget;
    if (!t || busy) return;
    busy = true;
    try {
      await createHandoff({
        target_agent_id: targetAgentId,
        instructions: dispatchInstructions(t.plan, t.slice),
        handoff_type: 'summary_only',
      });
      toastStore.success(`Handed off to ${targetAgentId}`);
      closeDispatch();
      router.navigate('agents', 'fleet');
    } catch (e) {
      toastStore.error(`Handoff failed: ${e instanceof Error ? e.message : e}`);
    } finally {
      busy = false;
    }
  }

  // Dispatch-dialog → Mills. Hand off to the confirm gate (autonomous + spendy).
  function dispatchMills() {
    if (!dispatchTarget) return;
    confirmMillsTarget = dispatchTarget;
    closeDispatch();
  }

  // Run a plan (or a single slice) in Mills: for a whole plan, start its
  // born-linked backlog item or create one from the plan; for a slice, always
  // create a slice-scoped backlog item. Deterministic ids keep both idempotent.
  async function runInMills() {
    const t = confirmMillsTarget;
    confirmMillsTarget = null;
    if (!t || busy) return;
    const { plan, slice } = t;
    busy = true;
    try {
      // A slice runs as its own backlog item; only a whole-plan run can reuse
      // the plan's born-linked backlog id.
      let backlogId = slice ? '' : (plan.mills_backlog_id ?? '');
      if (!backlogId) {
        // PascalCase wire shape + TargetProject routing live in the helper so
        // the operator's strict decoder accepts the body and a minted non-home
        // repo (e.g. services/procmodel) runs cross-repo. See buildMillsBacklogItem.
        const item = buildMillsBacklogItem(plan, slice);
        const created = await millsStore.createBacklog(item);
        backlogId = created?.id || (item.ID as string);
      }
      // requeue: the confirm gate above IS the human review an escalation
      // asks for — a born-linked item left escalated by a prior run flips
      // back to queued instead of 409ing the whole hand-off.
      await millsStore.startPipeline(backlogId, { requeue: true });
      toastStore.success(`Running in Mills: ${backlogId}`);
      router.navigate('mills', 'shuttles');
    } catch (e) {
      toastStore.error(`Mills run failed: ${e instanceof Error ? e.message : e}`);
    } finally {
      busy = false;
    }
  }

  // Hide abandoned (incl. superseded-by-respin) plans by default so respinning
  // a batch of old sparse plans doesn't bury the board. The toggle (and an
  // explicit phase=abandoned filter) brings them back.
  let showArchived = $state(false);
  let archivedCount = $derived(plans.filter((p) => p.phase === 'abandoned').length);
  let filtered = $derived(
    filterPlans(plans, search, projectFilter, phaseFilter).filter(
      (p) => showArchived || phaseFilter === 'abandoned' || p.phase !== 'abandoned'
    )
  );
  let byPhase = $derived(groupPlansByPhase(filtered));
  let byProject = $derived(groupPlansByProject(filtered));
  let projectOptions = $derived(projectOptionsFrom(plans));

  // Respun children of the open plan (drafts whose respun_from points at it),
  // so the drawer can link them + offer a one-click supersede once one lands.
  let respinChildren = $derived(
    selected ? plans.filter((p) => p.respun_from === selected!.id) : []
  );

  // supersede advances a plan to `abandoned` after the operator has compared its
  // respun draft — the "review, then 1-click" deprecation. Reversible.
  async function supersede(plan: Plan): Promise<void> {
    await advance(plan, 'abandoned');
  }
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

  $effect(() => {
    load();
    void taskStore.fetch();
    spinRunsStore.start(); // durable Spinning Room status (survives refresh)
    const t = setInterval(load, 30000);
    return () => {
      clearInterval(t);
      spinRunsStore.stop(); // don't leak the spin-runs poller on unmount
    };
  });

  // Single source of truth: keep `selected` in sync with router.detail. Drives
  // card clicks, deep-links (#tasks/plans/<id>), and close — all just change
  // router.detail. Clears selection when the detail segment is gone so the
  // drawer stays closed (no reopen loop).
  $effect(() => {
    const wantId = router.detail;
    if (!wantId) { if (selected) selected = null; return; }
    if (selected?.id === wantId) return;
    void openById(wantId, plans.find((p) => p.id === wantId));
    void taskStore.fetch();
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
    {#if plan.priority}<Badge text={plan.priority} variant={planPriorityVariant(plan.priority)} />{/if}
    {#if showProject && plan.project}<span class="text-mono">{plan.project}</span>{/if}
    {#if plan.slice_summary}<span>{sliceProgress(plan.slice_summary)?.merged}/{sliceProgress(plan.slice_summary)?.total} slices</span>{/if}
    {#if plan.mr_refs?.length}<span>· {plan.mr_refs.length} MR</span>{/if}
    {#if plan.kill_test_status}<span class="kt">· kill-test {plan.kill_test_status}</span>{/if}
    {#if plan.mills_backlog_id}<span title="Born-linked to a Mills backlog item">· ❖</span>{/if}
    {#if spinGroups.has(plan.id)}<span class="competing" title="Competing spin: {spinGroups.get(plan.id)?.frames.join(' vs ')} — {spinGroups.get(plan.id)?.planIds.length} sibling drafts">· ⚔ competing {spinGroups.get(plan.id)?.planIds.length}</span>{/if}
    {#if plan.updated_at}<span class="age">· {relativeTime(plan.updated_at)}</span>{/if}
  </div>
{/snippet}

<div class="panel plans-panel">
  <PanelHeader title="Plans" icon={'▤'} count={plans.length}>
    {#snippet stats()}
      {#each phaseTotals as col}
        <button
          class="pill-btn"
          class:pill-active={phaseFilter === col.phase}
          aria-pressed={phaseFilter === col.phase}
          onclick={() => togglePhaseFilter(col.phase)}
          title="Filter to {col.phase.replaceAll('_', ' ')}"
        >
          <Badge text="{col.items.length} {col.phase.replaceAll('_', ' ')}" variant={planPhaseVariant(col.phase)} />
        </button>
      {/each}
    {/snippet}
    {#snippet actions()}
      <div class="view-toggle" role="group" aria-label="Plan grouping">
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'phase'} aria-pressed={viewMode === 'phase'} onclick={() => viewMode = 'phase'}>By Phase</button>
        <button class="btn btn-ghost" class:active-toggle={viewMode === 'project'} aria-pressed={viewMode === 'project'} onclick={() => viewMode = 'project'}>By Project</button>
      </div>
      <button class="btn btn-success" onclick={() => showCreate = !showCreate}>+ New Plan</button>
      <button class="btn btn-ghost" onclick={openFreshSpin} title="Spin a draft plan from a brief on a chosen model frame">⟳ Spin a plan</button>
      {#if spinVisible.length > 0}
        <div class="spin-indicator-wrap">
          <button
            class="spin-indicator"
            class:has-live={spinLive > 0}
            class:has-stuck={spinStuck > 0}
            onclick={() => (showSpinTray = !showSpinTray)}
            title="Spinning Room — live + recent spins"
          >
            {#if spinLive > 0}
              <span class="spin-indicator-dot" class:stuck={spinStuck > 0}></span>
              {spinLive} spinning{#if spinStuck > 0} · ⚠ {spinStuck} stuck{/if}
            {:else}
              ⟳ Spins
            {/if}
          </button>
          <SpinningRoomTray
            open={showSpinTray}
            onClose={() => (showSpinTray = false)}
            onOpenPlan={(pid) => router.navigateDetail(pid)}
            onRetry={respinFromRun}
            onCompare={(planIds) => router.navigateCompare(planIds)}
          />
        </div>
      {/if}
      {#if archivedCount > 0}
        <button
          class="btn btn-ghost"
          class:active-toggle={showArchived}
          onclick={() => (showArchived = !showArchived)}
          title="Abandoned plans (incl. respin-superseded) are hidden by default"
        >
          {showArchived ? 'Hide' : 'Show'} archived ({archivedCount})
        </button>
      {/if}
      <button class="btn btn-ghost" onclick={load}>Refresh</button>
    {/snippet}
  </PanelHeader>

  {#if showCreate}
    <div class="create-row">
      <input class="inp" placeholder="Plan title" aria-label="Plan title" bind:value={newTitle} />
      <input class="inp" placeholder="project (e.g. services/loom-core)" aria-label="Project" bind:value={newProject} />
      <select class="inp" bind:value={newPriority} aria-label="Priority" title="Warp-beam priority (P0 dispatches first)">
        <option value="">priority…</option>
        {#each PLAN_PRIORITIES as pr}<option value={pr}>{pr}</option>{/each}
      </select>
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
    <EmptyState icon={'◯'} heading="Loading plans…" compact />
  {:else if !available}
    <EmptyState
      icon={'▤'}
      heading="Plan store not available on this daemon yet"
      description="Plans appear here once the agent-context daemon ships the agent_plan_* tools."
    />
  {:else if error}
    <EmptyState icon={'⚠'} heading="Failed to load plans" description={error} />
  {:else if plans.length === 0}
    <EmptyState icon={'▤'} heading="No plans yet" description="Create one, or agents will populate this as they plan work.">
      {#snippet action()}
        <button class="btn btn-success" onclick={() => showCreate = true}>+ New Plan</button>
      {/snippet}
    </EmptyState>
  {:else if filtered.length === 0}
    <EmptyState icon={'▤'} heading="No plans match the current filters" compact>
      {#snippet action()}
        <button class="btn btn-ghost" onclick={clearFilters}>Clear filters</button>
      {/snippet}
    </EmptyState>
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
  onClose={closeDetail}
>
  {#snippet header()}
    {#if selected}
      <div class="drawer-chips">
        <Badge text={selected.phase} variant={planPhaseVariant(selected.phase)} />
        {#if selected.priority}<Badge text={selected.priority} variant={planPriorityVariant(selected.priority)} />{/if}
        {#if selected.project}<span class="dim text-mono">{selected.project}</span>{/if}
        {#if selected.kill_test_status}<span class="dim">· kill-test: {selected.kill_test_status}</span>{/if}
      </div>
    {/if}
  {/snippet}

  {#snippet footer()}
    {#if selected}
      <div class="drawer-actions">
        <button class="btn btn-success" disabled={busy} onclick={() => openDispatch(selected!)} title="Hand this plan off to a session, an existing agent, or Mills">
          ⤳ Hand off…
        </button>
        <button class="btn btn-ghost" disabled={busy} onclick={() => respinPlan(selected!)} title="Respin this plan on a model frame into a fresh draft to compare + advance (good for older, sparse plans)">
          ⟳ Respin…
        </button>
        {#if canBootstrap(selected)}
          <button class="btn btn-ghost" disabled={busy} onclick={() => openBootstrap(selected!)} title="Mint a new GitLab repo for this project-less plan, seed it, and re-scope the plan onto it so Mills can source its slices">
            🧵 Spin up repo…
          </button>
        {/if}
      </div>
    {/if}
  {/snippet}

  {#if selected}
    {#if selected.objective}
      <div class="objective" title="The plan's synthesized end-state + through-line connecting its slices">
        <span class="objective-label">Objective</span>
        <p class="objective-body">{selected.objective}</p>
      </div>
    {/if}

    <div class="d-row">
      <label class="dim" for="adv">Advance to</label>
      <select id="adv" class="inp" onchange={(e) => advance(selected!, (e.currentTarget as HTMLSelectElement).value)}>
        <option value="">phase…</option>
        {#each PLAN_ADVANCE_TARGETS as ph}
          {#if ph !== selected.phase}<option value={ph}>{ph.replaceAll('_', ' ')}</option>{/if}
        {/each}
      </select>
      <label class="dim" for="beam-pri">Priority</label>
      <select
        id="beam-pri"
        class="inp"
        value={selected.priority ?? ''}
        onchange={(e) => setPriority(selected!, (e.currentTarget as HTMLSelectElement).value)}
        title="Warp-beam priority: P0 dispatches first; still-queued Mills items follow on the emitter's next tick"
      >
        <option value="">unset</option>
        {#each PLAN_PRIORITIES as pr}<option value={pr}>{pr}</option>{/each}
      </select>
    </div>

    {#if selected.respun_from}
      <div class="d-row">
        <span class="dim">⟳ respun from</span>
        <button class="ref-link" onclick={() => router.navigateDetail(selected!.respun_from!)} title="Open the plan this draft was respun from">
          {selected.respun_from}
        </button>
      </div>
    {/if}

    {#if respinChildren.length > 0}
      <div class="d-row respun-into">
        <span class="dim">⟳ respun into</span>
        {#each respinChildren as child}
          <button class="ref-link" onclick={() => router.navigateDetail(child.id)} title="Open the respun draft ({child.phase})">
            {child.title} <span class="dim small">· {child.phase}</span>
          </button>
        {/each}
        {#if selected.phase !== 'abandoned'}
          <button class="btn btn-ghost" disabled={busy} onclick={() => supersede(selected!)} title="Abandon this plan now that it's been respun (reversible — un-abandon via Advance to)">
            Supersede this plan
          </button>
        {/if}
      </div>
    {/if}

    {#if spinGroups.has(selected.id) && (spinGroups.get(selected.id)?.planIds.length ?? 0) > 1}
      {@const g = spinGroups.get(selected.id)!}
      <div class="d-row respun-into">
        <span class="dim">⚔ competing spin ({g.frames.join(' vs ')})</span>
        {#each g.planIds.filter((id) => id !== selected!.id) as sib}
          <button class="ref-link" onclick={() => router.navigateDetail(sib)} title="Open the competing sibling draft — compare and advance the winner">
            {sib}
          </button>
        {/each}
        <button class="btn btn-ghost" onclick={() => router.navigateCompare(g.planIds)} title="Open the side-by-side compare + merge editor for all {g.planIds.length} competing drafts">
          ⚖ Compare all {g.planIds.length}
        </button>
      </div>
    {/if}

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
          {@const deps = sliceDependsLabels(s.depends_on, selected.slices)}
          <li class="slice">
            <div class="slice-head">
              <Badge text={s.phase} variant={planPhaseVariant(s.phase)} />
              <span class="slice-name">{s.name}</span>
              {#if s.assigned_agent_id}
                <button class="slice-agent" onclick={() => s.assigned_agent_id && openAgent(s.assigned_agent_id)} title="Open {s.assigned_agent_id} in Fleet">{s.assigned_agent_id}</button>
              {/if}
              {#if s.mr_ref}
                {@const surl = gitlabMrUrl(s.mr_ref, selected.project)}
                {#if surl}<a class="ref-link small" href={surl} target="_blank" rel="noopener">{refLabel(s.mr_ref, 'mr')}</a>{/if}
              {/if}
              <span class="slice-tcount dim small">{stasks.length ? `${stasks.length} task${stasks.length !== 1 ? 's' : ''}` : 'no tasks'}</span>
              <button class="slice-spawn" disabled={busy} onclick={() => openDispatch(selected!, s)} title="Hand this slice off to a session, an agent, or Mills">⤳ hand off</button>
              <button class="slice-spawn" disabled={busy} onclick={() => respinSlice(selected!, s)} title="Respin this slice into a fresh draft plan on a model frame">⟳ respin</button>
            </div>
            {#if s.goal}
              <div class="slice-goal small">{s.goal}</div>
            {/if}
            {#if deps.length || s.interface_contracts}
              <div class="slice-tissue small">
                {#if deps.length}
                  <span class="tissue-dep" title="Slices that must merge before this one">↳ depends on {deps.join(', ')}</span>
                {/if}
                {#if s.interface_contracts}
                  <span class="tissue-contract" title={s.interface_contracts}>⇄ provides: {s.interface_contracts}</span>
                {/if}
              </div>
            {/if}
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

<PlanDispatchDialog
  open={dispatchOpen}
  target={dispatchTarget}
  agents={fleetStore.liveAgents}
  {busy}
  onSpawn={dispatchSpawn}
  onHandoff={dispatchHandoff}
  onMills={dispatchMills}
  onClose={closeDispatch}
/>

<SpinPlanDialog
  open={showSpin}
  onClose={() => { showSpin = false; respinSeed = null; }}
  onQueued={onSpinQueued}
  seed={respinSeed}
/>

<BootstrapRepoDialog
  open={showBootstrap}
  planId={bootstrapTarget?.id ?? ''}
  planTitle={bootstrapTarget?.title ?? ''}
  onClose={() => { showBootstrap = false; bootstrapTarget = null; }}
  onBootstrapped={onRepoBootstrapped}
/>

<ConfirmDialog
  open={!!confirmMillsTarget}
  title="Run in Mills?"
  message={`Create/queue a Mills backlog item for "${confirmMillsTarget?.slice?.name ?? confirmMillsTarget?.plan.title ?? ''}" and start an autonomous pipeline. This spends budget and may open and merge a merge request.`}
  confirmLabel="Run in Mills"
  onConfirm={runInMills}
  onCancel={() => (confirmMillsTarget = null)}
/>

<style>
  .plans-panel { display: flex; flex-direction: column; overflow: hidden; gap: var(--space-2); }
  .pill-btn { background: none; border: none; padding: 0; cursor: pointer; border-radius: var(--radius-full); transition: filter var(--transition-fast); }
  .pill-btn:hover { filter: brightness(1.25); }
  .pill-btn:focus-visible { outline: 2px solid color-mix(in srgb, var(--info) 55%, transparent); outline-offset: 2px; }
  .pill-btn.pill-active { outline: 2px solid var(--accent); outline-offset: 1px; border-radius: var(--radius-sm); }
  .view-toggle { display: flex; gap: 2px; background: var(--bg-tertiary); border-radius: var(--radius-sm); padding: 2px; }
  .spin-indicator-wrap { position: relative; display: inline-flex; }
  .spin-indicator {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    background: transparent;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 2px 8px;
    cursor: pointer;
    transition: border-color var(--transition-fast), color var(--transition-fast);
  }
  .spin-indicator:hover { border-color: var(--accent); color: var(--fg-primary); }
  .spin-indicator.has-live { color: var(--fg-primary); border-color: color-mix(in srgb, var(--accent) 45%, var(--border)); }
  .spin-indicator.has-stuck { color: var(--status-error); border-color: color-mix(in srgb, var(--status-error) 50%, var(--border)); }
  .spin-indicator-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--accent);
    animation: spin-inflight-pulse 1.2s ease-in-out infinite;
  }
  .spin-indicator-dot.stuck { background: var(--status-error); }
  @keyframes spin-inflight-pulse {
    0%, 100% { opacity: 0.35; }
    50% { opacity: 1; }
  }
  .competing { color: var(--accent); font-family: var(--font-mono); }
  .active-toggle {
    background: var(--bg-elevated) !important; color: var(--fg-primary) !important;
    box-shadow: var(--glow-shadow-sm) rgba(var(--info-rgb), 0.1);
  }
  .create-row { display: flex; gap: var(--space-2); align-items: center; }
  .inp {
    background: var(--bg-tertiary); border: 1px solid var(--border); color: var(--fg-primary);
    border-radius: var(--radius-sm); padding: 4px 8px; font-size: var(--text-sm);
  }
  .create-row .inp:first-child { flex: 1; }
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
  .card:hover { border-color: var(--accent); }
  .card.sel { border-color: var(--accent); box-shadow: var(--glow-shadow-md) var(--glow-accent); }
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
    background: color-mix(in srgb, var(--accent) 12%, var(--bg-tertiary));
    border: 1px solid color-mix(in srgb, var(--accent) 35%, var(--border));
    border-radius: var(--radius-sm); color: var(--accent); cursor: pointer; text-decoration: none;
  }
  .ref-link:hover { background: color-mix(in srgb, var(--accent) 22%, var(--bg-tertiary)); }
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
  .slice-spawn {
    background: none; border: 1px solid var(--border-subtle); color: var(--fg-secondary);
    border-radius: var(--radius-sm); padding: 0 6px; font-size: var(--text-xs); cursor: pointer;
  }
  .slice-spawn:hover:not(:disabled) { border-color: var(--accent); color: var(--accent); }
  .slice-spawn:disabled { opacity: 0.5; cursor: default; }
  .drawer-actions { display: flex; gap: var(--space-2); justify-content: flex-end; width: 100%; }
  .slice-agent {
    background: none; border: 1px solid var(--border-subtle); color: var(--fg-secondary);
    border-radius: var(--radius-sm); padding: 0 6px; font-size: var(--text-xs);
    font-family: var(--font-mono); cursor: pointer;
  }
  .slice-agent:hover { border-color: var(--accent); color: var(--accent); }
  .slice-sub { margin-top: 1px; }
  .branch-link {
    font-family: var(--font-mono); font-size: var(--text-xs); color: var(--fg-secondary);
    text-decoration: none; border-bottom: 1px dotted var(--border);
  }
  .branch-link:hover { color: var(--accent); border-bottom-color: var(--accent); }
  .slice-files { margin-top: 1px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .objective {
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-secondary));
    border: 1px solid color-mix(in srgb, var(--accent) 25%, var(--border));
    border-left: 3px solid var(--accent);
    border-radius: var(--radius-md); padding: var(--space-2) var(--space-3);
    margin-bottom: var(--space-3);
  }
  .objective-label {
    display: block; font-size: var(--text-xs); font-weight: 600;
    text-transform: uppercase; letter-spacing: 0.04em; color: var(--accent);
    margin-bottom: 2px;
  }
  .objective-body { margin: 0; font-size: var(--text-sm); color: var(--fg-primary); line-height: 1.45; }
  .slice-goal { color: var(--fg-secondary); margin-top: 1px; }
  .slice-tissue { display: flex; flex-wrap: wrap; gap: var(--space-2); margin-top: 2px; }
  .tissue-dep { color: var(--info); }
  .tissue-contract {
    color: var(--fg-secondary); overflow: hidden; text-overflow: ellipsis;
    white-space: nowrap; max-width: 100%;
  }
  .slice-decisions {
    list-style: disc; margin: 2px 0 0; padding-left: var(--space-4);
    font-size: var(--text-xs); color: var(--fg-secondary);
  }
  .task-rollup { list-style: none; margin: 2px 0 0; padding: 0 0 0 var(--space-3); display: flex; flex-direction: column; gap: 2px; border-left: 1px solid var(--border-subtle); }
  .task-row { display: flex; align-items: center; gap: 6px; font-size: var(--text-xs); min-width: 0; }
  .task-title { color: var(--fg-secondary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
</style>
