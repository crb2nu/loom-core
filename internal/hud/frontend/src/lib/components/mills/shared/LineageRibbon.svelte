<script lang="ts">
  /**
   * LineageRibbon — the mill-floor's connective tissue. One token-driven
   * horizontal thread diagram with two modes (spec §2.2):
   *
   *   mode="spine"  — the floor-nav ribbon at the top of every mill-floor
   *                   view: P0..P3 warps ▸ shuttles in flight ▸ bolts / sparks.
   *                   Each segment is a keyboard-reachable nav link; the current
   *                   view's segment is highlighted.
   *   mode="strand" — one entity's thread inside a DetailDrawer: warp ══ stage
   *                   nodes ══► terminal bolt / spark.
   *
   * Structure, not decoration: the thread line IS the layout and every node
   * encodes a real count/state fed by the caller from millsStore. No fetch,
   * no data logic here — segments arrive built by lineage.ts.
   */
  import type { LineageSegment } from './lineage.ts';
  import { stageLabel } from '../../../utils/factoryHelpers.ts';
  import type { BadgeVariant } from '../../../utils/tokens.ts';

  let {
    mode,
    segments,
    current = '',
  }: { mode: 'spine' | 'strand'; segments: LineageSegment[]; current?: string } = $props();

  // Null-safe at the read boundary (house rule #3).
  let nodes = $derived(segments ?? []);

  // Tokens verified defined in the theme before use (phantom-token guard).
  const TONE_VAR: Record<BadgeVariant, string> = {
    info: 'var(--info)',
    success: 'var(--success)',
    warning: 'var(--warning)',
    error: 'var(--error)',
    accent: 'var(--accent)',
    muted: 'var(--fg-muted)',
  };

  const STAGE_STATE_VAR: Record<'done' | 'active' | 'pending' | 'failed', string> = {
    done: 'var(--success)',
    active: 'var(--info)',
    pending: 'var(--fg-muted)',
    failed: 'var(--error)',
  };

  /** href for a node, or undefined for the non-navigable stage kind. */
  function segHref(seg: LineageSegment): string | undefined {
    return seg.kind === 'stage' ? undefined : seg.href;
  }

  /** The per-node accent color, by kind. */
  function segColor(seg: LineageSegment): string {
    switch (seg.kind) {
      case 'warp':
        return TONE_VAR[seg.tone ?? 'muted'];
      case 'shuttle':
        return 'var(--info)';
      case 'stage':
        return STAGE_STATE_VAR[seg.state];
      case 'bolt':
        return 'var(--success)';
      case 'spark':
        return 'var(--warning)';
    }
  }

  function isCurrent(seg: LineageSegment): boolean {
    if (!current) return false;
    const href = segHref(seg);
    return !!href && href.endsWith('/' + current);
  }

  /**
   * Hover text for one node. Only a strand's terminal spark has more to say
   * than its label: the caller passes the failing-gate reasons it already
   * fetched, and without this they had nowhere to land.
   */
  function segTitle(seg: LineageSegment): string | undefined {
    if (seg.kind !== 'spark' || !seg.reasons || seg.reasons.length === 0) return undefined;
    return seg.reasons.join('\n');
  }

  /** aria-label for one node, spoken in the loom vocabulary. */
  function nodeLabel(seg: LineageSegment): string {
    switch (seg.kind) {
      case 'warp':
        return seg.count == null ? `warp ${seg.label}` : `warp ${seg.label}: ${seg.count}`;
      case 'shuttle':
        return `${seg.count ?? 0} ${seg.label}`;
      case 'stage':
        return `${stageLabel(seg.stage)} — ${seg.state}`;
      case 'bolt':
        return seg.count == null
          ? `bolt ${seg.mriid == null ? '' : '!' + seg.mriid}`.trim()
          : `${seg.count} bolts`;
      case 'spark':
        return seg.count == null ? `spark ${seg.class}` : `${seg.count} sparks`;
    }
  }
</script>

{#snippet nodeInner(seg: LineageSegment)}
  {#if seg.kind === 'warp'}
    <span class="glyph" aria-hidden="true">▟</span>
    <span class="label">{seg.label}</span>
    {#if seg.count != null}<span class="count">{seg.count}</span>{/if}
  {:else if seg.kind === 'shuttle'}
    <span class="glyph" aria-hidden="true">⇢</span>
    {#if seg.count != null}<span class="count">{seg.count}</span>{/if}
    <span class="label">{seg.label}</span>
  {:else if seg.kind === 'stage'}
    <span class="dot" aria-hidden="true"></span>
    {#if mode === 'strand'}<span class="label">{stageLabel(seg.stage)}</span>{/if}
  {:else if seg.kind === 'bolt'}
    <span class="glyph" aria-hidden="true">▤</span>
    {#if seg.count != null}
      <span class="count">{seg.count}</span><span class="label">bolts</span>
    {:else}
      <span class="label">{seg.mriid == null ? 'bolt' : '!' + seg.mriid}</span>
    {/if}
  {:else if seg.kind === 'spark'}
    <span class="glyph" aria-hidden="true">⚡</span>
    {#if seg.count != null}
      <span class="count">{seg.count}</span><span class="label">sparks</span>
    {:else}
      <span class="label">{seg.class}</span>
    {/if}
  {/if}
{/snippet}

<svelte:element
  this={mode === 'spine' ? 'nav' : 'div'}
  class="ribbon {mode}"
  role={mode === 'spine' ? 'navigation' : 'group'}
  aria-label={mode === 'spine' ? 'mill floor' : 'run lineage'}
>
  {#each nodes as seg, i (i)}
    {#if segHref(seg)}
      <a
        class="node kind-{seg.kind}"
        class:is-current={isCurrent(seg)}
        class:is-active={seg.kind === 'shuttle' && seg.active}
        href={segHref(seg)}
        aria-current={isCurrent(seg) ? 'page' : undefined}
        aria-label={nodeLabel(seg)}
        title={segTitle(seg)}
        style:--node-color={segColor(seg)}
      >
        {@render nodeInner(seg)}
      </a>
    {:else}
      <span
        class="node kind-{seg.kind}"
        class:is-current={isCurrent(seg)}
        aria-label={seg.kind === 'stage' ? nodeLabel(seg) : undefined}
        title={segTitle(seg)}
        style:--node-color={segColor(seg)}
      >
        {@render nodeInner(seg)}
      </span>
    {/if}
    {#if i < nodes.length - 1}
      <span class="thread" aria-hidden="true"></span>
    {/if}
  {/each}
</svelte:element>

<style>
  .ribbon {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: var(--space-1, 4px);
    padding: var(--space-2) var(--space-3);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    background:
      linear-gradient(180deg, color-mix(in srgb, var(--fg-primary) 2%, transparent), transparent),
      color-mix(in srgb, var(--bg-secondary) 88%, transparent);
    font-size: var(--text-xs);
  }

  .ribbon.strand {
    background: transparent;
    border: none;
    padding: var(--space-2) 0;
  }

  .node {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    padding: 3px 8px;
    border-radius: var(--radius-full);
    border: 1px solid color-mix(in srgb, var(--node-color) 35%, var(--border));
    color: var(--fg-secondary);
    text-decoration: none;
    white-space: nowrap;
    background: color-mix(in srgb, var(--node-color) 7%, transparent);
  }

  a.node {
    transition:
      background 120ms ease,
      border-color 120ms ease;
  }

  a.node:hover {
    background: color-mix(in srgb, var(--node-color) 16%, transparent);
    border-color: color-mix(in srgb, var(--node-color) 60%, var(--border));
  }

  a.node:focus-visible {
    outline: 2px solid var(--node-color);
    outline-offset: 2px;
  }

  .node.is-current {
    background: color-mix(in srgb, var(--node-color) 20%, transparent);
    border-color: var(--node-color);
    color: var(--fg-primary);
    font-weight: 600;
  }

  .glyph {
    color: var(--node-color);
    line-height: 1;
  }

  .label {
    color: inherit;
  }

  .count {
    font-family: var(--font-mono);
    font-weight: 700;
    color: var(--node-color);
  }

  /* Stage node in strand mode: a colored pick dot + its loom-vocabulary name. */
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--node-color);
    flex-shrink: 0;
  }

  .kind-stage {
    border-color: transparent;
    background: transparent;
    padding: 3px 4px;
    color: var(--fg-muted);
  }

  /* The thread that ties nodes together — this line IS the layout, not an
     ornament. Fixed short segment so the row reads as one continuous warp. */
  .thread {
    flex: 0 0 auto;
    width: 14px;
    height: 2px;
    background: linear-gradient(
      90deg,
      color-mix(in srgb, var(--fg-muted) 45%, transparent),
      color-mix(in srgb, var(--fg-muted) 20%, transparent)
    );
  }

  .ribbon.strand .thread {
    width: 18px;
  }

  /* An active shuttle's border breathes to signal live motion. Static under
     reduced-motion (matches DepartureBoard's fallback discipline). */
  .node.is-active {
    animation: shuttle-flow 2.4s ease-in-out infinite;
  }

  @keyframes shuttle-flow {
    0%,
    100% {
      box-shadow: 0 0 0 0 color-mix(in srgb, var(--node-color) 30%, transparent);
    }
    50% {
      box-shadow: 0 0 0 4px transparent;
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .node.is-active {
      animation: none;
    }
  }
</style>
