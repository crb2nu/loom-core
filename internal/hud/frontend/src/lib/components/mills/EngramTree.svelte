<script lang="ts">
  /**
   * EngramTree — the proven-capability half of the reusable-knowledge surface.
   *
   * Engrams are tier-gated capability capsules with a prerequisite DAG; each
   * node here shares its card anatomy and badge vocabulary with the pattern
   * catalog beside it, because they are two levels of assembly of one library,
   * not two products. The drawer closes the loop the API cannot: an engram
   * lists the patterns that compose it (derived by inverting the pattern list —
   * see engramLinks.ts), so the operator can travel capability -> template ->
   * stamp without leaving the panel.
   */
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import ProofBadge from './shared/ProofBadge.svelte';
  import TierBadge from './shared/TierBadge.svelte';
  import { patternsComposing, refLabel } from './shared/engramLinks.ts';
  import { relativeTime } from '../../utils/format.ts';
  import type { EngramGraph, EngramInfo } from '../../stores/engrams.svelte.ts';
  import type { PatternInfo } from '../../stores/patterns.svelte.ts';

  let {
    graph = null,
    unavailable = false,
    error = null,
    /** Loaded patterns, used only to derive the engram -> pattern reverse edge. */
    patterns = [],
    /** Selection is owned here but drivable from outside so a pattern's
     *  "composed engrams" chip can open this drawer. */
    selectedId = $bindable<string | null>(null),
    /** Travel the reverse edge: select a pattern in the catalog. */
    onOpenPattern,
  }: {
    graph?: EngramGraph | null;
    unavailable?: boolean;
    error?: string | null;
    patterns?: PatternInfo[];
    selectedId?: string | null;
    onOpenPattern?: (pattern: PatternInfo) => void;
  } = $props();

  const selected = $derived(graph?.nodes.find((node) => node.id === selectedId) ?? null);
  const tiers = [1, 2, 3] as const;

  // Layout constants. These were previously duplicated as magic numbers in the
  // SVG path math (`64 + i * 66`) while the nodes were positioned by CSS, so
  // the edges drifted from the nodes as soon as a tier held more than a handful
  // and detached entirely past the frozen 420px canvas. One source of truth
  // now, consumed by both the geometry below and the CSS via custom properties.
  const NODE_H = 64;
  const ROW_GAP = 12;
  const ROW_PITCH = NODE_H + ROW_GAP;
  const HEAD_H = 34; // tier label strip above the first node
  const COL_W = 300;
  const COL_GAP = 56;
  const COL_PITCH = COL_W + COL_GAP;

  const byTier = $derived.by(() => {
    const map = new Map<number, EngramInfo[]>();
    for (const tier of tiers) map.set(tier, []);
    for (const node of graph?.nodes ?? []) {
      // Anything outside the 1-3 contract still has to render somewhere; park
      // it in tier 1 rather than dropping it silently.
      const tier = map.has(node.tier) ? node.tier : 1;
      map.get(tier)!.push(node);
    }
    return map;
  });

  function nodesForTier(tier: number): EngramInfo[] {
    return byTier.get(tier) ?? [];
  }

  /** Canvas height follows the tallest tier instead of a frozen 420px. */
  const rowCount = $derived(Math.max(1, ...tiers.map((t) => nodesForTier(t).length)));
  const canvasH = $derived(HEAD_H + rowCount * ROW_PITCH);
  const canvasW = COL_PITCH * 3;

  /** Centre of a node's card in canvas coordinates. */
  function centreOf(node: EngramInfo): { x: number; y: number } | null {
    const tier = byTier.has(node.tier) ? node.tier : 1;
    const idx = nodesForTier(tier).findIndex((n) => n.id === node.id);
    if (idx < 0) return null;
    return {
      x: (tier - 1) * COL_PITCH + COL_W / 2,
      y: HEAD_H + idx * ROW_PITCH + NODE_H / 2,
    };
  }

  interface EdgePath { key: string; d: string; from: string; to: string }

  const edgePaths = $derived.by<EdgePath[]>(() => {
    const g = graph;
    if (!g) return [];
    const paths: EdgePath[] = [];
    g.edges.forEach((edge, i) => {
      const from = g.nodes.find((n) => n.id === edge.from);
      const to = g.nodes.find((n) => n.id === edge.to);
      if (!from || !to) return;
      const a = centreOf(to); // prerequisite, drawn on the left
      const b = centreOf(from); // dependent
      if (!a || !b) return;
      const mid = (a.x + b.x) / 2;
      paths.push({
        key: `${edge.from}-${edge.to}-${i}`,
        from: edge.from,
        to: edge.to,
        d: `M ${a.x} ${a.y} C ${mid} ${a.y}, ${mid} ${b.y}, ${b.x} ${b.y}`,
      });
    });
    return paths;
  });

  function dependentsOf(id: string): EngramInfo[] {
    if (!graph) return [];
    const ids = new Set(graph.edges.filter((edge) => edge.to === id).map((edge) => edge.from));
    return graph.nodes.filter((node) => ids.has(node.id));
  }

  function openNode(id: string): void {
    if (graph?.nodes.some((node) => node.id === id)) selectedId = id;
  }

  const composedInto = $derived(patternsComposing(selected, patterns));

  /** True when an edge touches the selected node, so the path can highlight. */
  function edgeActive(edge: EdgePath): boolean {
    return selectedId !== null && (edge.from === selectedId || edge.to === selectedId);
  }
</script>

<section
  class="engram-tree"
  aria-label="Engram tech tree"
  style:--node-h="{NODE_H}px"
  style:--row-gap="{ROW_GAP}px"
  style:--col-w="{COL_W}px"
  style:--col-gap="{COL_GAP}px"
>
  <div class="section-head">
    <div class="section-heading">
      <span class="section-label">engrams</span>
      <span class="section-sub">proven capabilities · prerequisites flow left to right</span>
    </div>
    <!-- Labelled "shown": the graph endpoint caps nodes at 500 server-side,
         so past that this and the summary strip's total diverge — a bare
         number here read as a contradicting second total. -->
    {#if graph && !graph.degraded}<span class="count" title="Nodes in this tree (server caps the graph at 500)">{graph.nodes.length} shown</span>{/if}
  </div>

  <!-- State copy is deliberately verbatim from the pre-unification component.
       Each string is asserted by a regression test guarding the production bug
       where a 502 on /api/engrams/graph left the tree spinning forever
       (e0f012a6); the visual treatment is unified onto EmptyState, the wording
       that distinguishes the four states is not up for redecoration. -->
  {#if unavailable || graph?.degraded}
    <EmptyState
      compact
      icon="⚠"
      heading="bridge unavailable"
      description="the engram graph is not live — the agent bridge is unreachable, so the catalog cannot be read."
    />
  {:else if error}
    <EmptyState compact icon="⚠" heading="engram graph unavailable" description={error} />
  {:else if !graph}
    <EmptyState compact icon="◴" heading="loading engram graph…" />
  {:else if graph.nodes.length === 0}
    <EmptyState
      compact
      icon="◇"
      heading="no engrams yet"
      description="engrams are recorded as proof-gated capsules via agent_engram_add. once the library has entries they appear here, tiered by depth of assembly."
    />
  {:else}
    <div class="tree-scroll">
      <div class="tree-canvas" style:min-height="{canvasH}px">
        <svg
          class="edges"
          viewBox="0 0 {canvasW} {canvasH}"
          width={canvasW}
          height={canvasH}
          aria-hidden="true"
        >
          {#each edgePaths as edge (edge.key)}
            <path class="edge" class:active={edgeActive(edge)} data-from={edge.from} data-to={edge.to} d={edge.d} />
          {/each}
        </svg>
        <div class="tier-grid">
          {#each tiers as tier (tier)}
            <div class="tier" data-tier={tier}>
              <div class="tier-head">
                <TierBadge {tier} />
                <span class="tier-count">{nodesForTier(tier).length}</span>
              </div>
              <div class="nodes">
                {#each nodesForTier(tier) as node (node.id)}
                  <button
                    class="node"
                    class:selected={node.id === selectedId}
                    data-engram-id={node.id}
                    data-status={node.proof_status}
                    onclick={() => openNode(node.id)}
                  >
                    <span class="node-name">{node.name || refLabel(node.id)}</span>
                    <span class="node-meta">
                      <ProofBadge status={node.proof_status} subtle />
                      {#if node.prerequisites.length}
                        <span class="node-prereq">{node.prerequisites.length} prereq</span>
                      {/if}
                    </span>
                  </button>
                {:else}
                  <div class="tier-empty">none</div>
                {/each}
              </div>
            </div>
          {/each}
        </div>
      </div>
    </div>
  {/if}
</section>

<DetailDrawer
  open={selected !== null}
  title={selected?.name ?? ''}
  subtitle={selected?.id ?? ''}
  closeLabel="Close engram detail"
  onClose={() => (selectedId = null)}
>
  {#if selected}
    <div class="engram-detail">
      <div class="detail-badges">
        <TierBadge tier={selected.tier} />
        <ProofBadge status={selected.proof_status} />
      </div>

      <p class="detail-desc">{selected.description || 'No description.'}</p>

      <dl class="detail-facts">
        <dt>proof</dt>
        <dd>{selected.proof.kind || 'not specified'}</dd>
        <dt>last verified</dt>
        <dd>{selected.last_verified_at ? relativeTime(selected.last_verified_at) : 'never'}</dd>
      </dl>

      {#if selected.proof.refs.length}
        <div class="detail-label">proof refs</div>
        <ul class="detail-refs">
          {#each selected.proof.refs as ref (ref)}<li><code>{ref}</code></li>{/each}
        </ul>
      {/if}

      <div class="detail-label">prerequisites</div>
      {#if selected.prerequisites.length}
        <div class="links">
          {#each selected.prerequisites as id (id)}
            <button onclick={() => openNode(id)}>
              {graph?.nodes.find((n) => n.id === id)?.name ?? refLabel(id)}
            </button>
          {/each}
        </div>
      {:else}
        <span class="muted">none</span>
      {/if}

      <div class="detail-label">dependents</div>
      {#if dependentsOf(selected.id).length}
        <div class="links">
          {#each dependentsOf(selected.id) as node (node.id)}
            <button onclick={() => openNode(node.id)}>{node.name || refLabel(node.id)}</button>
          {/each}
        </div>
      {:else}
        <span class="muted">none</span>
      {/if}

      <!-- The reverse edge the API cannot serve: patterns that compose this
           engram, each a way back into the stamp form. -->
      <div class="detail-label">composed into</div>
      {#if composedInto.length}
        <div class="links">
          {#each composedInto as pattern (pattern.id)}
            <button class="pattern-link" onclick={() => onOpenPattern?.(pattern)}>
              ◇ {pattern.name}
            </button>
          {/each}
        </div>
      {:else}
        <span class="muted">no pattern in the catalog composes this engram</span>
      {/if}
    </div>
  {/if}
</DetailDrawer>

<style>
  .engram-tree {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    padding: var(--space-3);
  }

  .section-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
    margin-bottom: var(--space-3);
  }
  .section-heading { display: flex; align-items: baseline; gap: var(--space-2); flex-wrap: wrap; }
  .section-label {
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }
  .section-sub { color: var(--fg-tertiary); font-size: var(--text-2xs); }
  .count {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    border: 1px solid var(--border);
    border-radius: var(--radius-full);
    padding: 1px var(--space-2);
  }

  /* The canvas scrolls horizontally inside its own container so the panel body
     never gains a horizontal scrollbar. */
  .tree-scroll { overflow-x: auto; overflow-y: hidden; }
  .tree-canvas { position: relative; width: max-content; min-width: 100%; }

  .edges { position: absolute; inset: 0; pointer-events: none; overflow: visible; }
  .edge {
    fill: none;
    stroke: var(--border-strong, var(--border));
    stroke-width: 1.5;
    vector-effect: non-scaling-stroke;
    transition: stroke var(--transition-fast);
  }
  .edge.active { stroke: var(--accent); stroke-width: 2; }

  .tier-grid {
    position: relative;
    display: grid;
    grid-template-columns: repeat(3, var(--col-w));
    gap: var(--col-gap);
  }

  .tier-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    height: 34px; /* HEAD_H */
  }
  .tier-count { color: var(--fg-tertiary); font-size: var(--text-2xs); font-family: var(--font-mono); }

  .nodes { display: flex; flex-direction: column; gap: var(--row-gap); }
  .tier-empty {
    height: var(--node-h);
    display: flex;
    align-items: center;
    justify-content: center;
    border: 1px dashed var(--border-subtle);
    border-radius: var(--radius-sm);
    color: var(--fg-dim);
    font-size: var(--text-2xs);
  }

  /* Card anatomy shared with the pattern catalog: name line, meta row, hairline
     border, accent on hover/selection. */
  .node {
    height: var(--node-h);
    display: flex;
    flex-direction: column;
    justify-content: center;
    gap: 4px;
    text-align: left;
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-md);
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--fg-primary);
    cursor: pointer;
    transition: border-color var(--transition-fast), background var(--transition-fast);
  }
  .node:hover { border-color: var(--accent); }
  .node.selected {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-tertiary));
  }
  .node-name {
    font-size: var(--text-sm);
    font-weight: 600;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .node-meta { display: flex; align-items: center; gap: var(--space-2); }
  .node-prereq { color: var(--fg-tertiary); font-size: var(--text-2xs); }

  /* Drawer body. Previously these selectors were :global() — generic names like
     .detail, .links and .muted leaked into every other panel's stylesheet. */
  .engram-detail {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    color: var(--fg-secondary);
    font-size: var(--text-sm);
  }
  .detail-badges { display: flex; gap: var(--space-2); flex-wrap: wrap; }
  .detail-desc { margin: 0; line-height: 1.5; }
  .detail-facts {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: var(--space-1) var(--space-3);
    margin: 0;
  }
  .detail-facts dt,
  .detail-label {
    color: var(--fg-tertiary);
    font-size: var(--text-xs);
    text-transform: lowercase;
  }
  .detail-facts dd { margin: 0; }
  .detail-refs { margin: 0; padding-left: var(--space-4); }
  .detail-refs code { font-family: var(--font-mono); font-size: var(--text-xs); }

  .links { display: flex; flex-wrap: wrap; gap: var(--space-1); }
  .links button {
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--accent);
    border-radius: var(--radius-sm);
    padding: var(--space-1) var(--space-2);
    font-size: var(--text-xs);
    cursor: pointer;
    transition: border-color var(--transition-fast);
  }
  .links button:hover { border-color: var(--accent); }
  .links button.pattern-link { color: var(--info); }
  .muted { color: var(--fg-tertiary); font-size: var(--text-xs); }
</style>
