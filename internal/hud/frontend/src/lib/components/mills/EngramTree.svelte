<script lang="ts">
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import { relativeTime } from '../../utils/format.ts';
  import type { EngramGraph, EngramInfo } from '../../stores/engrams.svelte.ts';

  let { graph = null, unavailable = false, error = null }: {
    graph?: EngramGraph | null;
    unavailable?: boolean;
    error?: string | null;
  } = $props();

  let selectedId = $state<string | null>(null);
  const selected = $derived(graph?.nodes.find((node) => node.id === selectedId) ?? null);
  const tiers = [1, 2, 3] as const;

  function nodesForTier(tier: number): EngramInfo[] {
    return graph?.nodes.filter((node) => node.tier === tier) ?? [];
  }

  function dependentsOf(id: string): EngramInfo[] {
    if (!graph) return [];
    const ids = new Set(graph.edges.filter((edge) => edge.to === id).map((edge) => edge.from));
    return graph.nodes.filter((node) => ids.has(node.id));
  }

  function openNode(id: string): void {
    if (graph?.nodes.some((node) => node.id === id)) selectedId = id;
  }
</script>

<section class="engram-tree" aria-label="Engram tech tree">
  <div class="section-head">
    <span class="section-label">engram tech tree</span>
    {#if graph && !graph.degraded}<span class="count">{graph.nodes.length} engrams</span>{/if}
  </div>

  {#if unavailable || graph?.degraded}
    <div class="unavailable" role="status">bridge unavailable — engram graph is not live</div>
  {:else if error}
    <div class="unavailable" role="status">engram graph unavailable — {error}</div>
  {:else if !graph}
    <div class="empty">loading engram graph…</div>
  {:else if graph.nodes.length === 0}
    <div class="empty">no engrams yet</div>
  {:else}
    <div class="tree-canvas">
      <svg class="edges" viewBox="0 0 900 420" preserveAspectRatio="none" aria-label="Prerequisite edges">
        {#each graph.edges as edge, i (`${edge.from}-${edge.to}-${i}`)}
          {@const from = graph.nodes.find((node) => node.id === edge.from)}
          {@const to = graph.nodes.find((node) => node.id === edge.to)}
          {#if from && to}
            {@const fromRows = nodesForTier(from.tier)}
            {@const toRows = nodesForTier(to.tier)}
            {@const x1 = 130 + (to.tier - 1) * 320}
            {@const x2 = 130 + (from.tier - 1) * 320}
            {@const y1 = 64 + Math.max(0, toRows.findIndex((node) => node.id === to.id)) * 66}
            {@const y2 = 64 + Math.max(0, fromRows.findIndex((node) => node.id === from.id)) * 66}
            <path class="edge" data-from={edge.from} data-to={edge.to} d={`M ${x1} ${y1} C ${(x1+x2)/2} ${y1}, ${(x1+x2)/2} ${y2}, ${x2} ${y2}`} />
          {/if}
        {/each}
      </svg>
      <div class="tier-grid">
        {#each tiers as tier}
          <div class="tier" data-tier={tier}>
            <div class="tier-label">tier {tier} · {tier === 1 ? 'idioms' : tier === 2 ? 'composites' : 'systems'}</div>
            <div class="nodes">
              {#each nodesForTier(tier) as node (node.id)}
                <button class="node status-{node.proof_status}" data-engram-id={node.id} onclick={() => openNode(node.id)}>
                  <span class="node-name">{node.name || node.id}</span>
                  <span class="node-status">{node.proof_status}</span>
                </button>
              {/each}
            </div>
          </div>
        {/each}
      </div>
    </div>
  {/if}
</section>

<DetailDrawer open={selected !== null} title={selected?.name ?? ''} subtitle={selected?.id ?? ''} closeLabel="Close engram detail" onClose={() => selectedId = null}>
  {#if selected}
    <div class="detail">
      <p>{selected.description || 'No description.'}</p>
      <dl><dt>tier</dt><dd>{selected.tier}</dd><dt>proof</dt><dd>{selected.proof.kind || 'not specified'}</dd><dt>last verified</dt><dd>{selected.last_verified_at ? relativeTime(selected.last_verified_at) : 'never'}</dd></dl>
      {#if selected.proof.refs.length}<div class="detail-label">proof refs</div><ul>{#each selected.proof.refs as ref (ref)}<li><code>{ref}</code></li>{/each}</ul>{/if}
      <div class="detail-label">prerequisites</div>
      {#if selected.prerequisites.length}<div class="links">{#each selected.prerequisites as id (id)}<button onclick={() => openNode(id)}>{graph?.nodes.find((n) => n.id === id)?.name ?? id}</button>{/each}</div>{:else}<span class="muted">none</span>{/if}
      <div class="detail-label">dependents</div>
      {#if dependentsOf(selected.id).length}<div class="links">{#each dependentsOf(selected.id) as node (node.id)}<button onclick={() => openNode(node.id)}>{node.name || node.id}</button>{/each}</div>{:else}<span class="muted">none</span>{/if}
    </div>
  {/if}
</DetailDrawer>

<style>
  .engram-tree { border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-secondary); padding: var(--space-3); }
  .section-head { display:flex; justify-content:space-between; margin-bottom:var(--space-3); }
  .section-label { color:var(--fg-tertiary); font-size:var(--text-xs); letter-spacing:var(--tracking-wide); }
  .count,.empty,.unavailable { color:var(--fg-tertiary); font-size:var(--text-xs); }
  .unavailable { border:1px dashed var(--border); padding:var(--space-4); text-align:center; }
  .tree-canvas { position:relative; min-width:720px; min-height:420px; overflow:auto; }
  .edges { position:absolute; inset:0; width:100%; height:420px; pointer-events:none; }
  .edge { fill:none; stroke:var(--border-strong, var(--border)); stroke-width:1.5; vector-effect:non-scaling-stroke; }
  .tier-grid { position:relative; display:grid; grid-template-columns:repeat(3, minmax(180px, 1fr)); gap:70px; }
  .tier-label { color:var(--fg-muted); font-size:var(--text-xs); margin-bottom:var(--space-2); }
  .nodes { display:flex; flex-direction:column; gap:var(--space-2); }
  .node { height:56px; text-align:left; padding:var(--space-2); border-radius:var(--radius-sm); border:1px solid var(--border); border-left:3px solid var(--fg-tertiary); background:var(--bg-tertiary); color:var(--fg-primary); cursor:pointer; }
  .node:hover { border-color:var(--accent); }
  .node.status-verified { border-left-color:var(--success); }
  .node.status-stale { border-left-color:var(--warning); }
  .node.status-failing { border-left-color:var(--error); }
  .node-name,.node-status { display:block; }
  .node-name { font-size:var(--text-sm); font-weight:600; }
  .node-status { color:var(--fg-tertiary); font-size:var(--text-xs); }
  :global(.detail) { display:flex; flex-direction:column; gap:var(--space-3); color:var(--fg-secondary); font-size:var(--text-sm); }
  :global(.detail dl) { display:grid; grid-template-columns:max-content 1fr; gap:var(--space-1) var(--space-3); margin:0; }
  :global(.detail dt),:global(.detail-label) { color:var(--fg-tertiary); font-size:var(--text-xs); text-transform:lowercase; }
  :global(.detail dd) { margin:0; }
  :global(.detail ul) { margin:0; padding-left:var(--space-4); }
  :global(.links) { display:flex; flex-wrap:wrap; gap:var(--space-1); }
  :global(.links button) { border:1px solid var(--border); background:var(--bg-tertiary); color:var(--accent); border-radius:var(--radius-sm); padding:var(--space-1) var(--space-2); cursor:pointer; }
  :global(.muted) { color:var(--fg-tertiary); }
</style>
