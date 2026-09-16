<script lang="ts">
  /**
   * OriginChip — who authored this piece of work.
   *
   * Same anatomy as ProofBadge (one tint formula, tone by meaning, the meaning
   * itself in the tooltip) so the floor keeps one chip vocabulary rather than
   * growing a second. The classification is pure and lives in provenance.ts;
   * this file only renders.
   *
   * The tooltip carries the evidence — `created_by=…` or `label=…` — because a
   * chip that asserts "this came from the sensor" over free-text CreatedBy
   * heuristics owes the reader its reasoning.
   */
  import { originOf, originTone, type Origin } from './provenance.ts';
  import type { BacklogItem } from '../../../stores/mills.svelte.ts';

  let {
    item,
    /** Drop the fill inside surfaces that already carry their own. */
    subtle = false,
  }: {
    item: Pick<BacklogItem, 'Labels' | 'CreatedBy'> | undefined | null;
    subtle?: boolean;
  } = $props();

  const origin = $derived<Origin>(originOf(item));
  const variant = $derived(originTone(origin.kind));
</script>

<span
  class="origin-chip tone-{variant}"
  class:subtle
  title={origin.detail}
  data-origin={origin.kind}
>
  <span class="dot" aria-hidden="true"></span>{origin.label}
</span>

<style>
  .origin-chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-1);
    padding: 1px var(--space-2);
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    letter-spacing: var(--tracking-wide);
    white-space: nowrap;
    background: color-mix(in srgb, var(--tone) 16%, transparent);
    color: var(--tone);
    border: 1px solid color-mix(in srgb, var(--tone) 34%, transparent);
    --tone: var(--fg-tertiary);
  }

  .tone-success { --tone: var(--success); }
  .tone-accent { --tone: var(--accent); }
  .tone-info { --tone: var(--info); }
  .tone-warning { --tone: var(--warning); }
  .tone-error { --tone: var(--error); }
  .tone-muted { --tone: var(--fg-tertiary); }

  .origin-chip.subtle {
    background: transparent;
    border-color: transparent;
    color: var(--fg-tertiary);
    padding-left: 0;
  }

  .dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--tone);
    flex-shrink: 0;
  }
</style>
