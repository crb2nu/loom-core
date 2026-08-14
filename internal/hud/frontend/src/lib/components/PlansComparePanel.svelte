<script lang="ts">
  /**
   * PlansComparePanel — side-by-side compare of 2–3 competing DRAFT plans spun
   * from the same brief, plus a merge editor that composes ONE new plan from
   * cherry-picked slices. Opened from any "Compare all N" affordance via
   * router.navigateCompare([...ids]) (#tasks/plans/id1+id2); App.svelte routes a
   * `+`-bearing plans detail here instead of the board drawer.
   *
   * Color language (load-bearing, from the approved mockup): a slice reads
   * SHARED (teal --success) when the same theme appears across the competing
   * frames, UNIQUE (accent-orange) when only one frame proposed it. Accent is
   * otherwise reserved for the single merge action. Two (or three) columns +
   * a merge compose rail; columns stack under ~900px.
   *
   * Presentation-only: all classification lives in the pure, unit-tested
   * planCompareHelpers.ts.
   */
  import Badge from '../widgets/Badge.svelte';
  import { router } from '../stores/router.svelte.ts';
  import { toastStore } from '../stores/toasts.svelte.ts';
  import { spinRunsStore } from '../stores/spinRuns.svelte.ts';
  import {
    type Plan,
    type PlanSlice,
    planPhaseVariant,
    planPriorityVariant,
    normalizePlan,
  } from '../utils/plansHelpers.ts';
  import {
    alignSlices,
    frameForPlan,
    sliceKey,
    type AlignedSlice,
    type SliceKind,
  } from '../utils/planCompareHelpers.ts';
  import { briefHeadline } from '../utils/spinRunsHelpers.ts';

  // --- Target plan ids from the route --------------------------------------
  // detail is "id1+id2" (2–3 ids). Parse defensively; a malformed/short list
  // falls through to the "need 2+" empty state.
  let ids = $derived(
    (router.detail ?? '')
      .split('+')
      .map((s) => s.trim())
      .filter(Boolean)
      .slice(0, 3),
  );

  let plans = $state<Plan[]>([]);
  let loading = $state(true);
  let error = $state('');

  // Cherry-pick selection: sliceKey(planId, sliceId) → the picked slice + its
  // provenance. A Map preserves insertion order so the merge rail reads in the
  // order the operator picked.
  interface PickedSlice {
    planId: string;
    frame: string;
    kind: SliceKind;
    themeKey?: string;
    slice: PlanSlice;
  }
  let picked = $state<Map<string, PickedSlice>>(new Map());
  let composing = $state(false);
  let composeError = $state('');
  // Operator-editable title for the merged plan; empty falls back to the
  // brief-derived default (shown as the input's placeholder).
  let titleDraft = $state('');
  // Theme under the cursor/focus — counterpart slices in the OTHER columns
  // light up so the operator can see what a shared theme matched against.
  let hoverTheme = $state<string | null>(null);
  let mergeEl = $state<HTMLElement | null>(null);

  // Spin group resolution: the compared drafts belong to a competitive spin, so
  // we can recover the shared brief + frame labels. Poll the durable spin log
  // while mounted (frames + brief; cheap, degrades to unlabeled columns).
  let spinGroups = $derived(spinRunsStore.competitiveGroups);

  // The spin run that authored these drafts (any compared id appears in its
  // plan_ids) — its brief is the shared brief shown in the top bar.
  let sharedBrief = $derived.by(() => {
    for (const run of spinRunsStore.runs) {
      if ((run.plan_ids ?? []).some((pid) => ids.includes(pid))) {
        return run.brief ?? '';
      }
    }
    return '';
  });

  function frameLabel(planId: string): string {
    return frameForPlan(planId, spinGroups);
  }

  // The id set the current selection belongs to — navigating to a DIFFERENT
  // comparison must drop the picks (they'd silently leak into the next
  // compose otherwise).
  let pickedIdsKey = '';

  async function load() {
    const idsKey = ids.join('+');
    if (idsKey !== pickedIdsKey) {
      pickedIdsKey = idsKey;
      picked = new Map();
      composeError = '';
      titleDraft = '';
    }
    if (ids.length < 2) {
      loading = false;
      return;
    }
    loading = true;
    error = '';
    try {
      const results = await Promise.all(
        ids.map(async (id) => {
          const res = await fetch(`/api/plans/${encodeURIComponent(id)}`);
          if (!res.ok) throw new Error(`HTTP ${res.status} for ${id}`);
          const data = await res.json();
          if (data.available === false) throw new Error('plan store unavailable');
          return normalizePlan(data.plan) ?? undefined;
        }),
      );
      const loaded = results.filter((p): p is Plan => !!p && !!p.id);
      if (loaded.length < 2) throw new Error('could not load at least two plans');
      plans = loaded;
      // Prune picks whose slice no longer exists in the freshly loaded plans
      // (a reload can drop a plan or a slice; a stale pick would 400 or write
      // a phantom slice into the merged plan).
      const valid = new Set<string>();
      for (const p of loaded) for (const s of p.slices ?? []) valid.add(sliceKey(p.id, s.id));
      if ([...picked.keys()].some((k) => !valid.has(k))) {
        picked = new Map([...picked].filter(([k]) => valid.has(k)));
      }
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    } finally {
      loading = false;
    }
  }

  // Alignment (shared/unique per plan + diff summary). Recomputes when plans
  // change. Pure.
  let alignment = $derived(alignSlices(plans));
  let diff = $derived(alignment.diffSummary);

  // Per-plan unique count, keyed by frame label (or plan title fallback) for
  // the diff-summary strip.
  interface UniqueTally {
    planId: string;
    label: string;
    count: number;
  }
  let uniqueTallies = $derived.by<UniqueTally[]>(() =>
    plans.map((p) => ({
      planId: p.id,
      label: frameLabel(p.id) || p.title,
      count: diff.uniquePerPlan[p.id] ?? 0,
    })),
  );

  function isPicked(planId: string, sliceId: string): boolean {
    return picked.has(sliceKey(planId, sliceId));
  }

  function toggle(planId: string, aligned: AlignedSlice): void {
    const key = sliceKey(planId, aligned.slice.id);
    const next = new Map(picked);
    if (next.has(key)) {
      next.delete(key);
    } else {
      next.set(key, {
        planId,
        frame: frameLabel(planId),
        kind: aligned.kind,
        themeKey: aligned.themeKey,
        slice: aligned.slice,
      });
    }
    picked = next;
  }

  // themeKey → planIds that carry a slice of that theme, for the shared-slice
  // "also in <frame>" tooltip and the cross-column hover highlight.
  let themeMembers = $derived.by(() => {
    const m = new Map<string, Set<string>>();
    for (const ap of alignment.plans) {
      for (const as of ap.slices) {
        if (!as.themeKey) continue;
        let set = m.get(as.themeKey);
        if (!set) m.set(as.themeKey, (set = new Set()));
        set.add(ap.planId);
      }
    }
    return m;
  });

  /** Frame labels of the OTHER plans sharing this slice's theme. */
  function sharedWith(aligned: AlignedSlice, ownPlanId: string): string {
    if (!aligned.themeKey) return '';
    const members = themeMembers.get(aligned.themeKey);
    if (!members) return '';
    return [...members]
      .filter((pid) => pid !== ownPlanId)
      .map((pid) => frameLabel(pid) || pid)
      .join(', ');
  }

  // Themes the operator picked from MORE THAN ONE frame — the merged plan
  // would carry near-duplicate slices, so the rail warns.
  let dupThemeCount = $derived.by(() => {
    const counts = new Map<string, number>();
    for (const p of picked.values()) {
      if (p.themeKey) counts.set(p.themeKey, (counts.get(p.themeKey) ?? 0) + 1);
    }
    let dups = 0;
    for (const c of counts.values()) if (c > 1) dups++;
    return dups;
  });

  function removePicked(key: string): void {
    const next = new Map(picked);
    next.delete(key);
    picked = next;
  }

  function clearPicks(): void {
    picked = new Map();
    composeError = '';
  }

  let pickedList = $derived([...picked.entries()].map(([key, p]) => ({ key, ...p })));
  let pickedSharedCount = $derived(pickedList.filter((p) => p.kind === 'shared').length);
  let pickedUniqueCount = $derived(pickedList.length - pickedSharedCount);

  function closeCompare(): void {
    router.navigate('tasks', 'plans');
  }

  // Derive a title for the merged plan from the shared brief's first line, or a
  // generic fallback.
  function mergedTitle(): string {
    const head = briefHeadline(sharedBrief).trim();
    if (head) return `Merged: ${head.slice(0, 80)}`;
    return `Merged plan (${ids.length} drafts)`;
  }

  async function advanceMerged(): Promise<void> {
    if (composing || picked.size === 0) return;
    composing = true;
    composeError = '';
    // Carry project/namespace/priority from the first compared plan (the drafts
    // share a spin, so these agree).
    const base = plans[0];
    const body = {
      title: titleDraft.trim() || mergedTitle(),
      source_plan_ids: ids,
      project: base?.project ?? '',
      namespace: base?.namespace ?? '',
      priority: base?.priority ?? '',
      slices: pickedList.map((p) => ({
        name: p.slice.name,
        goal: p.slice.goal ?? '',
        files: p.slice.files ?? [],
        source: p.frame || p.planId,
      })),
    };
    try {
      const res = await fetch('/api/plans/compose', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        let detail = `HTTP ${res.status}`;
        try {
          const err = await res.json();
          if (err?.error) detail = err.error;
        } catch {
          /* non-JSON error body */
        }
        throw new Error(detail);
      }
      const data = await res.json();
      toastStore.success('Merged plan created');
      // Keep the selection intact until we've navigated away.
      if (data.plan_id) {
        router.navigate('tasks', 'plans', data.plan_id);
      } else {
        router.navigate('tasks', 'plans');
      }
    } catch (e) {
      composeError = e instanceof Error ? e.message : String(e);
    } finally {
      composing = false;
    }
  }

  $effect(() => {
    // Re-load whenever the compared id set changes.
    void ids;
    void load();
    spinRunsStore.start();
    return () => spinRunsStore.stop();
  });

  // Grid column template: N plan columns + the fixed merge rail. Falls back to
  // a single column under 900px via the CSS media query.
  let gridCols = $derived(`repeat(${Math.max(plans.length, 1)}, minmax(0, 1fr)) 320px`);
  // NOTE: Escape-to-close is handled globally (App.svelte keydown → router.back
  // when a detail is open), and it now yields to focused inputs so Escape in
  // the title field doesn't discard the comparison.
</script>

<div class="compare-wrap">
  <h2 class="sr-only">
    Side-by-side comparison of {ids.length} competing draft plans spun from the same
    brief, with a merge-editor lane for cherry-picking slices into one plan.
    {#if plans.length}
      {diff.shared} shared themes;
      {#each uniqueTallies as t}{t.count} unique to {t.label};{/each}
    {/if}
  </h2>

  <!-- Top bar -->
  <div class="topbar">
    <div class="topbar-text">
      <h1>Compare competing drafts</h1>
      {#if sharedBrief}
        <div class="brief">{briefHeadline(sharedBrief)}</div>
      {:else if plans.length}
        <div class="brief dim">{plans.length} drafts — {plans.map((p) => p.title).join(' · ')}</div>
      {/if}
    </div>
    {#if plans.length}
      <span class="badge-compete" aria-label="{plans.length} competing frames">⚔ {plans.length} competing frames</span>
    {/if}
    <button class="iconbtn" aria-label="Close compare" title="Back to the plans board" onclick={closeCompare}>✕</button>
  </div>

  {#if ids.length < 2}
    <div class="state-msg">Comparing needs at least two draft plans. Open a competitive spin from the Plans board and choose “Compare all”.</div>
  {:else if loading}
    <div class="state-msg" aria-live="polite">Loading {ids.length} drafts…</div>
  {:else if error}
    <div class="state-msg error" role="alert">
      Failed to load the compared drafts: {error}
      <div><button class="ghost-btn" onclick={load}>Retry</button></div>
    </div>
  {:else if plans.length < 2}
    <div class="state-msg">Could not load at least two of the requested drafts.</div>
  {:else}
    <!-- Diff summary strip -->
    <div class="diffbar" aria-label="Where the frames landed">
      <span class="diff-lbl">Where the frames landed</span>
      <span class="chip"><span class="dot shared"></span><b class="count shared-fg">{diff.shared}</b>&nbsp;shared themes</span>
      {#each uniqueTallies as t}
        <span class="chip"><span class="dot unique"></span><b class="count unique-fg">{t.count}</b>&nbsp;only in {t.label}</span>
      {/each}
    </div>

    <!-- Columns + merge rail -->
    <div class="grid" style:grid-template-columns={gridCols}>
      {#each alignment.plans as ap (ap.planId)}
        {@const frame = frameLabel(ap.planId)}
        <section class="col" aria-label="Draft {frame || ap.plan.title}">
          <div class="colhead">
            <div class="frame">
              <span class="frame-name">{frame || ap.plan.title}</span>
              {#if frame && ap.plan.title !== frame}<span class="frame-model" title={ap.plan.title}>{ap.plan.title}</span>{/if}
            </div>
            <div class="ptitle">{ap.plan.title}</div>
            {#if ap.plan.objective}
              <div class="pobjective" title="This draft's objective — the end-state its slices add up to">{ap.plan.objective}</div>
            {/if}
            <div class="metarow">
              <Badge text={ap.plan.phase} variant={planPhaseVariant(ap.plan.phase)} />
              {#if ap.plan.priority}<Badge text={ap.plan.priority} variant={planPriorityVariant(ap.plan.priority)} />{/if}
              <span class="pill">{ap.slices.length} slice{ap.slices.length === 1 ? '' : 's'}</span>
            </div>
          </div>

          <div class="slices">
            {#if ap.slices.length === 0}
              <div class="slices-empty">This draft has no slices.</div>
            {/if}
            {#each ap.slices as aligned (aligned.slice.id)}
              {@const on = isPicked(ap.planId, aligned.slice.id)}
              {@const counterparts = aligned.kind === 'shared' ? sharedWith(aligned, ap.planId) : ''}
              <div
                class="slice {aligned.kind}"
                class:picked={on}
                class:theme-hl={!!aligned.themeKey && hoverTheme === aligned.themeKey}
                onmouseenter={() => (hoverTheme = aligned.themeKey ?? null)}
                onmouseleave={() => (hoverTheme = null)}
                onfocusin={() => (hoverTheme = aligned.themeKey ?? null)}
                onfocusout={() => (hoverTheme = null)}
              >
                <span
                  class="stag {aligned.kind}"
                  title={aligned.kind === 'shared' && counterparts ? `Also proposed by ${counterparts} — hover to see the matching slice` : undefined}
                >
                  {#if aligned.kind === 'shared'}● shared{#if counterparts}&nbsp;· also in {counterparts}{:else}&nbsp;theme{/if}{:else}◆ only in {frame || ap.plan.title}{/if}
                </span>
                <div class="sname">{aligned.slice.name}</div>
                {#if aligned.slice.goal}<div class="sgoal">{aligned.slice.goal}</div>{/if}
                {#if aligned.slice.files?.length}
                  <div class="files">
                    {#each aligned.slice.files as f}<span class="file">{f}</span>{/each}
                  </div>
                {/if}
                <button
                  class="addbtn"
                  class:on
                  aria-pressed={on}
                  onclick={() => toggle(ap.planId, aligned)}
                  title={on ? 'Remove from the merged plan' : 'Add this slice to the merged plan'}
                >
                  {on ? '✓ in merged plan' : '+ add to merge'}
                </button>
              </div>
            {/each}
          </div>
        </section>
      {/each}

      <!-- Merge editor rail -->
      <aside class="merge" aria-label="Merged plan editor" bind:this={mergeEl}>
        <div class="mergehead">
          <h2>⚙ Merged plan</h2>
          <div class="merge-sub">Cherry-pick the best slice from each frame. The drafts stay as provenance.</div>
        </div>
        <div class="mergebody">
          {#if pickedList.length === 0}
            <div class="mempty">Pick slices from either column to compose one plan to advance.</div>
          {:else}
            {#each pickedList as p (p.key)}
              <div class="mslice">
                <span class="prov" title="From draft {p.frame || p.planId}">{p.frame || p.planId}</span>
                <span class="mn">{p.slice.name}</span>
                <button class="rm" aria-label="Remove {p.slice.name} from the merge" title="Remove" onclick={() => removePicked(p.key)}>✕</button>
              </div>
            {/each}
          {/if}
        </div>
        <div class="mergefoot">
          {#if pickedList.length > 0}
            <label class="mtitle-lbl" for="merged-plan-title">Merged plan title</label>
            <input
              id="merged-plan-title"
              class="mtitle"
              type="text"
              bind:value={titleDraft}
              placeholder={mergedTitle()}
              maxlength="120"
              disabled={composing}
            />
            <div class="mcoverlap">
              Composed from <b>{pickedList.length}</b> slice{pickedList.length === 1 ? '' : 's'} —
              {pickedSharedCount} shared core + {pickedUniqueCount} distinctive. Composing authors
              one new plan (phase <b>draft</b>) for review; the {plans.length} drafts are kept as
              provenance.
            </div>
          {/if}
          {#if dupThemeCount > 0}
            <div class="dup-warn" role="status">
              ⚠ {dupThemeCount} shared theme{dupThemeCount === 1 ? '' : 's'} picked from more than
              one frame — the merged plan will carry both variants.
            </div>
          {/if}
          {#if composeError}
            <div class="compose-error" role="alert">{composeError}</div>
          {/if}
          <button class="cta" disabled={pickedList.length === 0 || composing} onclick={advanceMerged}>
            {composing ? 'Composing…' : 'Compose merged plan'} · <span class="count">{pickedList.length}</span> slice{pickedList.length === 1 ? '' : 's'}
          </button>
          {#if pickedList.length > 0}
            <button class="cta ghost" onclick={clearPicks} disabled={composing}>Clear selection</button>
          {/if}
        </div>
      </aside>
    </div>

    <!-- Stacked-layout affordance: under 900px the merge rail sits below all
         columns, so picking a slice is otherwise invisible. This chip mirrors
         the count and jumps to the rail. Hidden on wide layouts via CSS. -->
    {#if pickedList.length > 0}
      <button
        class="pick-tracker"
        onclick={() => mergeEl?.scrollIntoView({ behavior: 'smooth', block: 'start' })}
        aria-label="Jump to the merged plan editor — {pickedList.length} slice{pickedList.length === 1 ? '' : 's'} picked"
      >
        ⚙ {pickedList.length} picked ↓
      </button>
    {/if}
  {/if}
</div>

<style>
  .compare-wrap {
    display: flex;
    flex-direction: column;
    min-height: 0;
    flex: 1;
    background: var(--bg-primary);
    color: var(--fg-primary);
    border: 1px solid var(--border);
    border-radius: var(--radius-lg);
    overflow: hidden;
    position: relative; /* anchors the stacked-layout pick tracker */
  }
  .sr-only {
    position: absolute;
    width: 1px;
    height: 1px;
    padding: 0;
    margin: -1px;
    overflow: hidden;
    clip: rect(0, 0, 0, 0);
    border: 0;
  }

  /* Top bar */
  .topbar {
    display: flex;
    align-items: flex-start;
    gap: var(--space-4);
    padding: var(--space-4) var(--space-4);
    border-bottom: 1px solid var(--border);
    background: var(--bg-secondary);
    flex-shrink: 0;
  }
  .topbar-text { min-width: 0; }
  .topbar h1 {
    margin: 0;
    font-size: var(--text-base);
    font-weight: 500;
    letter-spacing: 0.2px;
    color: var(--fg-primary);
  }
  .brief {
    color: var(--fg-muted);
    font-size: var(--text-12);
    margin-top: 3px;
    max-width: 62ch;
  }
  .brief.dim { color: var(--fg-secondary); }
  .badge-compete {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--text-xs);
    padding: 3px 8px;
    border-radius: var(--radius-md);
    white-space: nowrap;
    color: var(--accent);
    border: 1px solid color-mix(in srgb, var(--accent) 40%, var(--border));
    background: var(--accent-dim);
    margin-left: auto;
    flex: none;
  }
  .iconbtn {
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--fg-secondary);
    width: 30px;
    height: 30px;
    border-radius: var(--radius-md);
    cursor: pointer;
    font-size: var(--text-base);
    line-height: 1;
    flex: none;
  }
  .iconbtn:hover { background: var(--bg-elevated); color: var(--fg-primary); }

  /* States */
  .state-msg {
    padding: var(--space-8) var(--space-4);
    color: var(--fg-secondary);
    text-align: center;
    font-size: var(--text-sm);
  }
  .state-msg.error { color: var(--error); }
  .ghost-btn {
    margin-top: var(--space-3);
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--fg-secondary);
    border-radius: var(--radius-sm);
    padding: 4px 12px;
    cursor: pointer;
    font-size: var(--text-sm);
  }
  .ghost-btn:hover { border-color: var(--accent); color: var(--accent); }

  /* Diff summary strip */
  .diffbar {
    display: flex;
    align-items: center;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-4);
    border-bottom: 1px solid var(--border);
    background: var(--bg-primary);
    flex-wrap: wrap;
    flex-shrink: 0;
  }
  .diff-lbl {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: var(--text-12);
    padding: 4px 10px;
    border-radius: var(--radius-full);
    border: 1px solid var(--border-focus);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
  }
  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .dot.shared { background: var(--success); }
  .dot.unique { background: var(--accent); }
  .count { font-variant-numeric: tabular-nums; font-weight: 600; }
  .shared-fg { color: var(--success); }
  .unique-fg { color: var(--accent); }

  /* Grid */
  .grid {
    display: grid;
    gap: 0;
    flex: 1;
    min-height: 0;
    overflow: auto;
  }
  .col {
    border-right: 1px solid var(--border);
    min-width: 0;
    display: flex;
    flex-direction: column;
  }
  .colhead {
    padding: var(--space-3) var(--space-4) var(--space-3);
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    background: var(--bg-secondary);
    z-index: 1;
  }
  .frame {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    margin-bottom: var(--space-2);
    flex-wrap: wrap;
  }
  .frame-name {
    font-family: var(--font-mono);
    font-size: var(--text-12);
    font-weight: 500;
    letter-spacing: 0.3px;
    color: var(--fg-primary);
  }
  .frame-model {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    background: var(--bg-elevated);
    padding: 2px 7px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-focus);
    max-width: 20ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ptitle {
    font-size: var(--text-sm);
    color: var(--fg-primary);
    margin: 2px 0 var(--space-2);
    font-weight: 500;
  }
  .pobjective {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    line-height: 1.4;
    margin: 0 0 var(--space-2);
    padding-left: var(--space-2);
    border-left: 2px solid color-mix(in srgb, var(--accent) 40%, var(--border));
  }
  .metarow {
    display: flex;
    gap: 6px;
    align-items: center;
    flex-wrap: wrap;
  }
  .pill {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    padding: 2px 7px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-focus);
    color: var(--fg-muted);
    background: var(--bg-tertiary);
  }

  .slices {
    padding: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }
  .slices-empty {
    color: var(--fg-muted);
    font-size: var(--text-sm);
    text-align: center;
    padding: var(--space-4);
    border: 1px dashed var(--border);
    border-radius: var(--radius-md);
  }
  .slice {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    padding: 11px var(--space-3);
    position: relative;
  }
  .slice.shared { border-left: 3px solid var(--success); }
  .slice.unique { border-left: 3px solid var(--accent); }
  .stag {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    margin-bottom: 6px;
  }
  .stag.shared { color: var(--success); }
  .stag.unique { color: var(--accent); }
  .sname {
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-primary);
    margin-bottom: 4px;
    line-height: 1.35;
  }
  .sgoal {
    font-size: var(--text-12);
    color: var(--fg-secondary);
    margin-bottom: 9px;
  }
  .files {
    display: flex;
    gap: 5px;
    flex-wrap: wrap;
    margin-bottom: var(--space-2);
  }
  .file {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    background: var(--bg-tertiary);
    padding: 2px 7px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
  }
  .addbtn {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 6px;
    font-size: var(--text-12);
    padding: 7px;
    border-radius: var(--radius-md);
    cursor: pointer;
    background: transparent;
    border: 1px dashed var(--border-focus);
    color: var(--fg-secondary);
    transition: background var(--transition-fast), border-color var(--transition-fast), color var(--transition-fast);
  }
  .addbtn:hover { border-color: var(--accent); color: var(--accent); }
  .slice.picked {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-secondary));
  }
  /* Cross-column theme link: hovering/focusing a shared slice lights up its
     counterpart(s) in the other columns. */
  .slice.theme-hl {
    border-color: var(--success);
    box-shadow: 0 0 0 1px color-mix(in srgb, var(--success) 55%, transparent);
    background: color-mix(in srgb, var(--success) 6%, var(--bg-secondary));
  }
  .slice.theme-hl.picked {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-secondary));
  }
  .addbtn.on {
    background: var(--accent);
    border-style: solid;
    border-color: var(--accent);
    color: var(--bg-primary);
    font-weight: 500;
  }

  /* Merge rail */
  .merge {
    background: var(--bg-secondary);
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .mergehead {
    padding: var(--space-3) var(--space-4) var(--space-3);
    border-bottom: 1px solid var(--border);
    position: sticky;
    top: 0;
    background: var(--bg-secondary);
    z-index: 1;
  }
  .mergehead h2 {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 500;
    display: flex;
    align-items: center;
    gap: var(--space-2);
    color: var(--fg-primary);
  }
  .merge-sub {
    font-size: var(--text-12);
    color: var(--fg-muted);
    margin-top: 4px;
  }
  .mergebody {
    padding: var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    flex: 1;
    min-height: 180px;
    overflow-y: auto;
  }
  .mslice {
    display: flex;
    align-items: flex-start;
    gap: 9px;
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-tertiary);
    padding: 9px 10px;
  }
  .prov {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    padding: 2px 6px;
    border-radius: var(--radius-xs);
    flex: none;
    margin-top: 1px;
    color: var(--info);
    border: 1px solid color-mix(in srgb, var(--info) 40%, var(--border));
    background: var(--info-dim);
    max-width: 12ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .mn { font-size: var(--text-12); color: var(--fg-primary); line-height: 1.35; }
  .rm {
    margin-left: auto;
    background: transparent;
    border: none;
    color: var(--fg-muted);
    cursor: pointer;
    font-size: var(--text-14);
    flex: none;
    line-height: 1;
  }
  .rm:hover { color: var(--error); }
  .mempty {
    color: var(--fg-muted);
    font-size: var(--text-12);
    text-align: center;
    padding: 26px var(--space-3);
    border: 1px dashed var(--border);
    border-radius: var(--radius-md);
  }
  .mergefoot {
    border-top: 1px solid var(--border);
    padding: var(--space-3);
  }
  .mtitle-lbl {
    display: block;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    margin-bottom: 4px;
  }
  .mtitle {
    width: 100%;
    box-sizing: border-box;
    font-size: var(--text-sm);
    font-family: inherit;
    color: var(--fg-primary);
    background: var(--bg-primary);
    border: 1px solid var(--border-focus);
    border-radius: var(--radius-md);
    padding: 7px 10px;
    margin-bottom: var(--space-2);
  }
  .mtitle::placeholder { color: var(--fg-muted); }
  .mtitle:focus {
    outline: none;
    border-color: var(--accent);
  }
  .mtitle:disabled { opacity: 0.6; }
  .dup-warn {
    font-size: var(--text-xs);
    color: var(--warning);
    margin-bottom: var(--space-2);
    padding: var(--space-2) 10px;
    background: var(--warning-dim);
    border: 1px solid color-mix(in srgb, var(--warning) 30%, var(--border));
    border-radius: var(--radius-md);
    line-height: 1.5;
  }
  .mcoverlap {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    margin-bottom: var(--space-2);
    padding: var(--space-2) 10px;
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    line-height: 1.5;
  }
  .mcoverlap b { color: var(--success); font-weight: 500; }
  .compose-error {
    font-size: var(--text-xs);
    color: var(--error);
    margin-bottom: var(--space-2);
    padding: var(--space-2) 10px;
    background: var(--error-dim);
    border: 1px solid color-mix(in srgb, var(--error) 30%, var(--border));
    border-radius: var(--radius-md);
  }
  .cta {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 7px;
    font-size: var(--text-sm);
    font-weight: 500;
    padding: 9px;
    border-radius: var(--radius-md);
    border: none;
    cursor: pointer;
    background: var(--accent);
    color: var(--bg-primary);
    transition: opacity var(--transition-fast);
  }
  .cta:hover:not(:disabled) { opacity: 0.9; }
  .cta:disabled { background: var(--bg-elevated); color: var(--fg-muted); cursor: not-allowed; }
  .cta.ghost {
    background: transparent;
    border: 1px solid var(--border-focus);
    color: var(--fg-secondary);
    margin-top: var(--space-2);
    font-weight: 400;
  }
  .cta.ghost:hover:not(:disabled) { background: var(--bg-tertiary); color: var(--fg-primary); opacity: 1; }
  .count { font-variant-numeric: tabular-nums; }

  /* Stacked-layout pick tracker — desktop keeps the rail in view, so the chip
     only exists under the 900px breakpoint. */
  .pick-tracker { display: none; }

  /* Stack under ~900px: single column, merge rail flows to the bottom. */
  @media (max-width: 900px) {
    .grid { grid-template-columns: 1fr !important; }
    .col { border-right: none; border-bottom: 1px solid var(--border); }
    .colhead, .mergehead { position: static; }
    .pick-tracker {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      position: absolute;
      bottom: 14px;
      right: 14px;
      z-index: 5;
      font-size: var(--text-12);
      font-weight: 500;
      padding: 8px 14px;
      border: none;
      border-radius: var(--radius-full);
      background: var(--accent);
      color: var(--bg-primary);
      cursor: pointer;
      box-shadow: var(--shadow-md);
    }
  }
</style>
