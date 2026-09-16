<script lang="ts">
  /**
   * PatternCardPicker — THE pattern catalog picker, shared by the Pattern
   * Loom page and the Spin-a-plan dialog (which used to carry two diverging
   * card lists). Ordering is status-grouped (approved → candidate →
   * deprecated), name-ascending within each group, for every consumer.
   *
   * approvedOnly preserves the dialog's conservative policy: candidate and
   * deprecated cards render (so the operator sees what exists and what still
   * needs a kill-test/promote) but are not selectable. The page leaves it off
   * — selecting a candidate there opens its instruction book for inspection.
   */
  import type { PatternInfo } from '../../stores/patterns.svelte.ts';
  import { greenCount } from '../../utils/spinningRoomHelpers.ts';
  import ProofBadge from '../mills/shared/ProofBadge.svelte';

  let {
    patterns,
    selectedId,
    onPick,
    approvedOnly = false,
    compact = false,
    disabled = false,
  }: {
    patterns: PatternInfo[];
    selectedId: string | null;
    onPick: (p: PatternInfo) => void;
    approvedOnly?: boolean;
    compact?: boolean;
    disabled?: boolean;
  } = $props();

  const STATUS_ORDER: Record<string, number> = { approved: 0, candidate: 1, deprecated: 2 };
  let ordered = $derived(
    [...patterns].sort(
      (a, b) =>
        (STATUS_ORDER[a.status] ?? 3) - (STATUS_ORDER[b.status] ?? 3) ||
        a.name.localeCompare(b.name),
    ),
  );

  function pickable(p: PatternInfo): boolean {
    return !approvedOnly || p.status === 'approved';
  }
</script>

<div class="pattern-picks" class:compact role="group" aria-label="Pattern cards">
  {#each ordered as p (p.id)}
    <button
      type="button"
      class="pick"
      class:picked={selectedId === p.id}
      class:unpickable={!pickable(p)}
      disabled={disabled || !pickable(p)}
      title={pickable(p)
        ? `${p.makes} · v${p.version}${greenCount(p) > 0 ? ` · ${greenCount(p)} shipped green` : ''}`
        : `${p.status} — needs a kill-test or a promote before this surface stamps it`}
      onclick={() => onPick(p)}
    >
      <span class="pick-head">
        <span class="pick-name">{p.name}</span>
        <ProofBadge status={p.status} subtle={compact} />
      </span>
      <span class="pick-makes">{p.makes}</span>
      {#if !compact}
        {#if p.description}<span class="pick-desc">{p.description}</span>{/if}
        <span class="pick-foot">
          <span class="text-mono dim">v{p.version}</span>
          {#if greenCount(p) > 0}<span class="pick-green" title="Instances shipped green">✓{greenCount(p)}</span>{/if}
          {#if p.engrams?.length}
            <span class="pick-engrams" title="Composes {p.engrams.length} engram(s)">◇ {p.engrams.length}</span>
          {/if}
          {#if p.tags?.length}
            <span class="pick-tags">{#each p.tags as t (t)}<span class="pick-tag">{t}</span>{/each}</span>
          {/if}
        </span>
      {:else if greenCount(p) > 0}
        <span class="pick-green" title="Instances shipped green">✓{greenCount(p)}</span>
      {/if}
    </button>
  {/each}
</div>

<style>
  .pattern-picks {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    min-width: 0;
  }
  .pattern-picks.compact {
    flex-direction: row;
    flex-wrap: wrap;
  }
  .pick {
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
    text-align: left;
    background: var(--bg-subtle);
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: 0.5rem 0.65rem;
    cursor: pointer;
    min-width: 0;
  }
  .compact .pick { flex: 0 1 14rem; }
  .pick:hover:not(:disabled) { border-color: color-mix(in srgb, var(--accent) 45%, var(--border-default)); }
  .pick.picked {
    border-color: var(--accent);
    background: color-mix(in srgb, var(--accent) 8%, var(--bg-subtle));
  }
  .pick.unpickable { opacity: 0.55; cursor: not-allowed; }
  .pick:disabled:not(.unpickable) { opacity: 0.6; }
  .pick-head { display: flex; align-items: center; justify-content: space-between; gap: 0.5rem; }
  .pick-name { font-size: var(--text-xs); font-weight: 600; color: var(--fg-primary); }
  .pick-makes { font-size: var(--text-2xs); color: var(--fg-secondary); }
  .pick-desc { font-size: var(--text-2xs); color: var(--text-muted); }
  .pick-foot { display: flex; align-items: center; gap: 0.5rem; flex-wrap: wrap; }
  .dim { color: var(--text-muted); font-size: var(--text-2xs); }
  .pick-green { color: var(--success); font-size: var(--text-2xs); }
  .pick-engrams { color: var(--mills); font-size: var(--text-2xs); }
  .pick-tags { display: flex; gap: 0.25rem; flex-wrap: wrap; }
  .pick-tag {
    padding: 0 0.35rem;
    border-radius: var(--radius-full);
    background: var(--bg-elevated);
    font-size: var(--text-2xs);
    color: var(--text-muted);
  }
</style>
