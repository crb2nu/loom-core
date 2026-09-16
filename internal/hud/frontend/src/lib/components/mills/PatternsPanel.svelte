<script lang="ts">
  // PatternsPanel — the Pattern Loom front door. Pick an approved pattern, fill
  // a materials form derived from its materials_schema, and stamp it into a Plan
  // that Mills executes. The agent_pattern_* tools go live after an
  // mcp-agent-context redeploy; until then the catalog is empty and the panel
  // shows a "no patterns yet" state.
  import { onMount, onDestroy } from 'svelte';
  import { patternsStore, type PatternInfo } from '../../stores/patterns.svelte.ts';
  import { buildMaterials, type RawMaterialValues } from '../../utils/spinningRoomHelpers.ts';
  import PatternCardPicker from '../shared/PatternCardPicker.svelte';
  import PatternMaterialsFields from '../shared/PatternMaterialsFields.svelte';
  import { engramsStore } from '../../stores/engrams.svelte.ts';
  import PanelHeader from '../shared/PanelHeader.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import EngramTree from './EngramTree.svelte';
  import ProofBadge from './shared/ProofBadge.svelte';
  import TierBadge from './shared/TierBadge.svelte';
  import { composedEngrams, refLabel } from './shared/engramLinks.ts';
  import { relativeTime } from '../../utils/format.ts';

  let selectedId = $state<string | null>(null);
  let values = $state<RawMaterialValues>({});
  let project = $state('');
  let formError = $state<string | null>(null);
  /** Selection in the engram half, lifted here so the two registers can drive
   *  each other: a pattern's composed-engram chip opens the tree's drawer, and
   *  that drawer's "composed into" links select a pattern back here. */
  let selectedEngramId = $state<string | null>(null);
  let stampFormEl = $state<HTMLElement | null>(null);

  // The panel's list rides the view-state filter; the store itself always
  // holds the full catalog (the old fetch-with-filter leaked this panel's
  // filter into the Factory shelf and the shift report).
  const patterns = $derived(patternsStore.filtered);
  const selected = $derived<PatternInfo | null>(
    patterns.find((p) => p.id === selectedId) ?? null
  );
  const schema = $derived(selected?.materials_schema ?? []);

  onMount(() => patternsStore.startPolling(30000));
  onDestroy(() => patternsStore.stopPolling());

  // Engram tech-tree rollup (GET /api/engrams/summary). Patterns compose
  // engrams — each pattern's instruction book lists the ones it pins — so the
  // proof-status of the underlying library is the honest header for this page.
  // The endpoint's `degraded` flag means the agent bridge is down and the
  // counts are placeholders; rendering those zeros as real is the exact
  // failure the flag was added to prevent.
  onMount(() => engramsStore.startPolling(60000));
  onDestroy(() => engramsStore.stopPolling());

  const engramDegraded = $derived(engramsStore.degraded || engramsStore.unavailable);
  const engramTotal = $derived(engramsStore.total);
  const engramStatuses = $derived(engramsStore.statusPairs);
  const engramTiers = $derived(engramsStore.tierPairs);

  function selectPattern(p: PatternInfo): void {
    selectedId = p.id;
    patternsStore.clearResult();
    formError = null;
    // Fresh, EMPTY form: empty fields (bools included) are omitted at build
    // time so the stamp applies the pattern's declared defaults — the same
    // semantics as the Spin dialog now that both ride buildMaterials.
    values = {};
    void patternsStore.fetchInstances(p.id);
  }

  /** Arrive at a pattern from the engram drawer: select it, close the drawer,
   *  and bring the stamp form into view so the trip ends somewhere useful. */
  function openPattern(p: PatternInfo): void {
    selectPattern(p);
    selectedEngramId = null;
    queueMicrotask(() => stampFormEl?.scrollIntoView({ behavior: 'smooth', block: 'nearest' }));
  }

  /** The engrams a selected pattern composes, resolved against the loaded
   *  catalog. Unresolvable refs are kept and rendered as inert, which is the
   *  honest reading of "this pattern names an engram the catalog lacks". */
  const selectedComposed = $derived(composedEngrams(selected, engramsStore.graph?.nodes ?? []));


  // Stamp→beam is the point of this page, so enqueue defaults ON. Unchecked,
  // the stamp writes a draft Plan only (the old behavior — which this panel
  // used to do silently while its banner implied work had started).
  let enqueueToBeam = $state(true);

  async function submit(): Promise<void> {
    if (!selected) return;
    const { materials, errors } = buildMaterials(schema, values);
    formError = errors.length > 0 ? errors.join(' · ') : null;
    if (errors.length > 0) return;
    await patternsStore.stamp(selected.id, materials, project.trim(), {
      enqueue: enqueueToBeam,
    });
  }

</script>

<div class="patterns-panel">
  <PanelHeader title="Pattern Loom" icon="◇" count={patterns.length}>
    {#snippet stats()}
      <span class="subtitle">stamp a pattern → a Plan Mills runs</span>
    {/snippet}
    {#snippet actions()}
      <div class="filter" role="group" aria-label="Status filter">
        {#each ['approved', 'candidate', 'all'] as s (s)}
          <button
            class="filter-btn"
            class:active={patternsStore.statusFilter === s}
            aria-pressed={patternsStore.statusFilter === s}
            onclick={() => patternsStore.setStatusFilter(s as 'approved' | 'candidate' | 'all')}
          >{s}</button>
        {/each}
      </div>
      <button class="btn" onclick={() => patternsStore.fetch()} disabled={patternsStore.loading}>
        {patternsStore.loading ? 'Loading…' : 'Refresh'}
      </button>
      {#if patternsStore.lastUpdated}
        <span class="updated text-mono">{relativeTime(patternsStore.lastUpdated)}</span>
      {/if}
    {/snippet}
  </PanelHeader>

  <!-- Engram library rollup. Muted "unavailable" state when the endpoint
       reports degraded (agent bridge down) or the route is absent — never
       zeros dressed up as measurements. -->
  <div class="engram-strip" class:degraded={engramDegraded}>
    <span class="engram-label">Engrams</span>
    {#if engramDegraded}
      <span class="engram-unavailable">
        unavailable — {engramsStore.unavailable
          ? 'endpoint not on this HUD build'
          : 'agent bridge not reachable'}
      </span>
    {:else if engramsStore.error}
      <span class="engram-unavailable">unavailable — {engramsStore.error}</span>
    {:else}
      <span class="engram-total text-mono">{engramTotal}</span>
      <span class="engram-groups">
        {#each engramStatuses as [status, n] (status)}
          <span class="engram-stat" class:zero={n === 0}>
            <span class="text-mono stat-n">{n}</span>
            <ProofBadge {status} />
          </span>
        {/each}
      </span>
      {#if engramTiers.length > 0}
        <span class="engram-groups tiers">
          {#each engramTiers as [tier, n] (tier)}
            <span class="engram-stat">
              <span class="text-mono stat-n">{n}</span>
              <TierBadge tier={Number(tier.split(':')[1]) || 1} />
            </span>
          {/each}
        </span>
      {/if}
    {/if}
  </div>

  <EngramTree
    graph={engramsStore.graph}
    unavailable={engramsStore.catalogUnavailable}
    error={engramsStore.catalogError ?? engramsStore.error}
    {patterns}
    bind:selectedId={selectedEngramId}
    onOpenPattern={openPattern}
  />

  {#if patternsStore.error}
    <ErrorBanner prefix="Catalog unavailable" message={patternsStore.error} />
  {/if}

  <div class="body">
    <!-- Pattern catalog -->
    <div class="catalog">
      {#if patterns.length === 0 && !patternsStore.loading}
        <EmptyState
          compact
          icon="◇"
          heading="No patterns in the catalog yet"
          description="Patterns are stampable templates that compose engrams into a Plan. The catalog seeds on the next mcp-agent-context redeploy."
        />
      {:else}
        <PatternCardPicker
          patterns={patterns}
          selectedId={selectedId}
          onPick={selectPattern}
        />
      {/if}
    </div>

    <!-- Stamp form / result -->
    <div class="stamp" bind:this={stampFormEl}>
      {#if !selected}
        <div class="stamp-placeholder">Select a pattern to supply its materials.</div>
      {:else}
        <div class="stamp-head">
          <span class="stamp-title">{selected.name}</span>
          <span class="text-mono dim">{selected.id}</span>
        </div>

        <!-- The instruction book: what the pattern pins so only materials vary. -->
        {#if selected.pins?.length || selected.gauge || selected.engrams?.length}
          <details class="book">
            <summary class="book-summary">
              Instruction book
              <span class="text-mono dim">
                {selected.pins?.length ?? 0} pins · {selected.engrams?.length ?? 0} engrams · v{selected.version}
              </span>
            </summary>
            {#if selected.provenance}
              <div class="book-provenance">
                {#if selected.provenance.approved_by}
                  <span>approved by {selected.provenance.approved_by}</span>
                {/if}
                <span>{selected.provenance.instances_shipped_green ?? 0} instances shipped green</span>
              </div>
            {/if}
            {#if selected.pins?.length}
              <div class="book-section">Pinned architecture</div>
              <dl class="book-pins">
                {#each selected.pins as pin (pin.axis)}
                  <dt class="text-mono">{pin.axis}</dt>
                  <dd>{pin.value}</dd>
                {/each}
              </dl>
            {/if}
            {#if selected.gauge}
              <div class="book-section">Gauge</div>
              {#if selected.gauge.commands?.length}
                <div class="book-commands">
                  {#each selected.gauge.commands as c (c)}<code>{c}</code>{/each}
                </div>
              {/if}
              {#if selected.gauge.assertions?.length}
                <ul class="book-assertions">
                  {#each selected.gauge.assertions as a (a)}<li>{a}</li>{/each}
                </ul>
              {/if}
            {/if}
            {#if selected.engrams?.length}
              <div class="book-section">Composed engrams</div>
              <!-- The pattern -> engram edge, made navigable. These were inert
                   spans while a working engram drawer sat on the same page. -->
              <div class="engram-links">
                {#each selectedComposed as composed (composed.ref)}
                  {#if composed.node}
                    <button
                      type="button"
                      class="engram-link"
                      title="Open {composed.node.name || composed.ref} in the engram tree"
                      onclick={() => (selectedEngramId = composed.node!.id)}
                    >
                      <ProofBadge status={composed.node.proof_status} subtle />
                      {composed.node.name || refLabel(composed.ref)}
                    </button>
                  {:else}
                    <span class="engram-link missing" title="{composed.ref} — not in the loaded catalog">
                      {refLabel(composed.ref)}
                    </span>
                  {/if}
                {/each}
              </div>
            {/if}
            {#if selected.deploy_contract}
              <div class="book-section">Deploy contract</div>
              <div class="book-deploy">{selected.deploy_contract}</div>
            {/if}
          </details>
        {/if}

        <div class="fields">
          <PatternMaterialsFields schema={schema} bind:values={values} disabled={patternsStore.stamping} />

          <div class="field">
            <label class="field-label" for="mat-project">project <span class="field-type text-mono">scope</span></label>
            <input id="mat-project" type="text" placeholder="services/loom-core" bind:value={project} />
          </div>
        </div>

        {#if formError}<div class="banner error">{formError}</div>{/if}
        {#if patternsStore.stampError}<div class="banner error">Stamp failed: {patternsStore.stampError}</div>{/if}

        <div class="actions">
          <label class="enqueue-check" title="Also queue the stamped plan as a Mills backlog item (admin-gated)">
            <input type="checkbox" bind:checked={enqueueToBeam} />
            <span>queue for Mills</span>
          </label>
          <button class="btn primary" onclick={submit} disabled={patternsStore.stamping}>
            {patternsStore.stamping ? 'Stamping…' : enqueueToBeam ? 'Stamp → beam' : 'Stamp draft'}
          </button>
        </div>

        {#if patternsStore.lastResult}
          {@const r = patternsStore.lastResult}
          <div class="result">
            <div class="result-head">✓ Stamped into a Plan</div>
            {#if r.backlog_id}
              <div class="result-row"><span class="result-key">queued</span><span class="text-mono">{r.backlog_id}</span></div>
            {:else}
              <div class="result-row">
                <span class="result-key">state</span>
                <span>draft only — advance the plan to <em>planned</em> (or restamp with "queue for Mills") to feed the beam</span>
              </div>
            {/if}
            <div class="result-row"><span class="result-key">plan</span><span class="text-mono">{r.plan_id}</span></div>
            <div class="result-row"><span class="result-key">slices</span><span class="text-mono">{r.slice_count}</span></div>
            {#if r.tools_required?.length}
              <div class="result-row">
                <span class="result-key">tools</span>
                <span class="tags">{#each r.tools_required as t (t)}<span class="tag">{t}</span>{/each}</span>
              </div>
            {/if}
            {#if r.deploy_contract}
              <div class="result-row"><span class="result-key">deploy</span><span class="result-deploy">{r.deploy_contract}</span></div>
            {/if}
            {#if r.note}<div class="result-note">{r.note}</div>{/if}
          </div>
        {/if}

        <section class="history" aria-label="Stamp provenance">
          <div class="history-label">stamp provenance</div>
          {#if patternsStore.instancesLoading}
            <div class="history-empty">loading history…</div>
          {:else if patternsStore.instancesDegraded}
            <div class="history-empty">bridge unavailable — stamp history is not live</div>
          {:else if patternsStore.instancesError}
            <div class="history-empty">history unavailable — {patternsStore.instancesError}</div>
          {:else if patternsStore.instances.length === 0}
            <div class="history-empty">never stamped</div>
          {:else}
            <div class="history-list">
              {#each patternsStore.instances as instance (`${instance.stamped_at}-${instance.plan_id}`)}
                <article class="history-item">
                  <div class="history-main"><span>{instance.target_project}</span><span class="text-mono dim">{relativeTime(instance.stamped_at)}</span></div>
                  <div class="history-meta">
                    {#if instance.plan_id}<span>plan <code>{instance.plan_id}</code></span>{/if}
                    {#if instance.run_id}<span>run <code>{instance.run_id}</code>{instance.run_outcome || instance.run_status ? ` · ${instance.run_outcome || instance.run_status}` : ''}</span>{/if}
                    {#if instance.mr_ref}<span>MR {#if instance.mr_url}<a href={instance.mr_url} target="_blank" rel="noreferrer">{instance.mr_ref}</a>{:else}<code>{instance.mr_ref}</code>{/if}{instance.mr_outcome || instance.mr_status ? ` · ${instance.mr_outcome || instance.mr_status}` : ''}</span>{/if}
                  </div>
                </article>
              {/each}
            </div>
          {/if}
        </section>
      {/if}
    </div>
  </div>
</div>

<style>
  .patterns-panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    min-height: 0;
    /* This root skips the shared `.panel` base class, so it must own the
       scroll contract itself: ViewShell's .view-content clips (overflow
       hidden), and without a scroll path here everything past the first
       viewport — the engram tree, the pattern grid tail — was simply
       unreachable (+930px clipped at 720p, 2026-08-15). */
    flex: 1;
    overflow-y: auto;
    overflow-x: hidden;
  }
  .subtitle { font-size: var(--text-xs); color: var(--fg-tertiary); }
  .filter { display: inline-flex; border: 1px solid var(--border); border-radius: var(--radius-sm); overflow: hidden; }
  .filter-btn {
    padding: var(--space-1) var(--space-2);
    background: var(--bg-tertiary);
    color: var(--fg-tertiary);
    border: none;
    cursor: pointer;
    font-size: var(--text-xs);
    text-transform: capitalize;
    transition: background var(--transition-fast), color var(--transition-fast);
  }
  .filter-btn:hover { color: var(--fg-primary); }
  .filter-btn.active { background: var(--accent); color: var(--bg-primary); }
  .updated { color: var(--fg-tertiary); font-size: var(--text-xs); }
  .btn {
    padding: var(--space-1) var(--space-3);
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--fg-primary);
    cursor: pointer;
    font-size: var(--text-sm);
    transition: background var(--transition-fast);
  }
  .btn:hover:not(:disabled) { background: var(--bg-elevated); }
  .btn:disabled { opacity: 0.5; cursor: default; }
  .btn.primary { background: var(--accent); color: var(--bg-primary); border-color: var(--accent); font-weight: 600; }

  .engram-strip {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    font-size: var(--text-xs);
  }
  .engram-strip.degraded {
    border-style: dashed;
    background: transparent;
  }
  .engram-label {
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }
  .engram-total {
    font-size: var(--text-base);
    font-weight: 700;
    color: var(--fg-primary);
  }
  .engram-unavailable { color: var(--fg-tertiary); font-style: italic; }
  .engram-groups { display: inline-flex; gap: var(--space-2); flex-wrap: wrap; }
  .engram-groups.tiers { margin-left: auto; }
  /* Count + shared badge. The status vocabulary itself now lives in
     ProofBadge/TierBadge, so the strip, the tree nodes and the pattern cards
     cannot drift apart again. */
  .engram-stat { display: inline-flex; align-items: center; gap: var(--space-1); }
  .stat-n { font-weight: 700; color: var(--fg-secondary); }
  /* A zero bucket stays in the strip so its width is stable across refreshes,
     but it must not read as a signal. */
  .engram-stat.zero { opacity: 0.45; }

  .banner {
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-sm);
    font-size: var(--text-sm);
  }
  .banner.error { background: color-mix(in srgb, var(--error) 14%, transparent); color: var(--error); border: 1px solid color-mix(in srgb, var(--error) 35%, transparent); }

  .body { display: grid; grid-template-columns: minmax(240px, 1fr) minmax(280px, 1.3fr); gap: var(--space-4); align-items: start; }
  @media (max-width: 760px) { .body { grid-template-columns: 1fr; } }

  .catalog { display: flex; flex-direction: column; gap: var(--space-2); }
  .dim { color: var(--fg-tertiary); }


  .tags { display: inline-flex; gap: 4px; flex-wrap: wrap; }
  .tag { font-size: var(--text-xs); padding: 0 var(--space-1); border-radius: var(--radius-sm); background: var(--bg-tertiary); color: var(--fg-tertiary); }

  .stamp { display: flex; flex-direction: column; gap: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-md); padding: var(--space-3); background: var(--bg-secondary); }
  .stamp-placeholder { color: var(--fg-tertiary); font-size: var(--text-sm); padding: var(--space-3); }
  .stamp-head { display: flex; align-items: baseline; justify-content: space-between; gap: var(--space-2); }
  .stamp-title { font-weight: 600; color: var(--fg-primary); }

  .book {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: var(--bg-tertiary);
    padding: var(--space-2) var(--space-3);
  }
  .book-summary {
    cursor: pointer;
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-2);
  }
  .book-summary:hover { color: var(--fg-primary); }
  .book-provenance {
    display: flex;
    gap: var(--space-3);
    font-size: var(--text-xs);
    color: var(--fg-tertiary);
    margin-top: var(--space-2);
  }
  .book-section {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-tertiary);
    margin: var(--space-3) 0 var(--space-1);
  }
  .book-pins {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 2px var(--space-3);
    margin: 0;
    font-size: var(--text-xs);
  }
  .book-pins dt { color: var(--accent); }
  .book-pins dd { margin: 0; color: var(--fg-secondary); line-height: 1.4; }
  .book-commands { display: flex; flex-wrap: wrap; gap: var(--space-2); }
  .book-commands code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    background: var(--bg-primary);
    padding: 1px var(--space-2);
    border-radius: var(--radius-sm);
    color: var(--fg-secondary);
  }
  .book-assertions {
    margin: var(--space-1) 0 0;
    padding-left: var(--space-4);
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    line-height: 1.5;
  }
  .book-deploy { font-size: var(--text-xs); color: var(--fg-secondary); }

  /* Composed-engram chips: same pill anatomy as the tree's link buttons, so
     travelling pattern -> engram -> pattern feels like one surface. */
  .engram-links { display: flex; flex-wrap: wrap; gap: var(--space-1); }
  .engram-link {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    border: 1px solid var(--border);
    background: var(--bg-primary);
    color: var(--accent);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
    transition: border-color var(--transition-fast);
  }
  .engram-link:hover { border-color: var(--accent); }
  /* A ref the catalog cannot resolve stays visible but inert — it is a real
     state (pattern names an engram the library lacks), not a rendering bug. */
  .engram-link.missing {
    color: var(--fg-tertiary);
    border-style: dashed;
    cursor: default;
    text-decoration: line-through;
  }

  .fields { display: flex; flex-direction: column; gap: var(--space-3); }
  .field { display: flex; flex-direction: column; gap: 4px; }
  .field-label { font-size: var(--text-sm); color: var(--fg-secondary); display: flex; align-items: center; gap: var(--space-2); }
  .field-type { font-size: var(--text-xs); color: var(--fg-tertiary); }
  /* Only the project input remains panel-local; the materials fields carry
     their own styles inside PatternMaterialsFields. */
  input {
    padding: var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: var(--bg-primary);
    color: var(--fg-primary);
    font-size: var(--text-sm);
    font-family: inherit;
  }
  input:focus { outline: none; border-color: var(--accent); }

  .enqueue-check {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    cursor: pointer;
  }

  .actions { display: flex; gap: var(--space-2); }

  .result { display: flex; flex-direction: column; gap: 4px; padding: var(--space-3); border-radius: var(--radius-sm); background: color-mix(in srgb, var(--success) 8%, var(--bg-tertiary)); border: 1px solid color-mix(in srgb, var(--success) 30%, transparent); }
  .result-head { font-weight: 600; color: var(--success); margin-bottom: 2px; }
  .result-row { display: flex; gap: var(--space-2); font-size: var(--text-sm); }
  .result-key { color: var(--fg-tertiary); min-width: 54px; }
  .result-deploy { color: var(--fg-secondary); font-size: var(--text-xs); }
  .result-note { font-size: var(--text-xs); color: var(--fg-tertiary); margin-top: 2px; }

  .history { border-top:1px solid var(--border); padding-top:var(--space-3); }
  .history-label { color:var(--fg-tertiary); font-size:var(--text-xs); margin-bottom:var(--space-2); }
  .history-empty { color:var(--fg-tertiary); font-size:var(--text-sm); }
  .history-list { display:flex; flex-direction:column; gap:var(--space-2); }
  .history-item { padding:var(--space-2); border:1px solid var(--border); border-radius:var(--radius-sm); background:var(--bg-tertiary); }
  .history-main { display:flex; justify-content:space-between; gap:var(--space-2); color:var(--fg-secondary); font-size:var(--text-sm); }
  .history-meta { display:flex; flex-wrap:wrap; gap:var(--space-2); margin-top:var(--space-1); color:var(--fg-tertiary); font-size:var(--text-xs); }
  .history-meta code { font-family:var(--font-mono); }
  .history-meta a { color:var(--accent); }

</style>
