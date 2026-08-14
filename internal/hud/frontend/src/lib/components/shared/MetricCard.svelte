<script lang="ts">
  import type { BadgeVariant } from '../../utils/tokens.ts';
  import type { Snippet } from 'svelte';
  import SparkLine from '../../widgets/SparkLine.svelte';

  // MetricCard — stat display with label, value, optional trend sparkline and badge.
  let {
    label,
    value,
    color = 'var(--fg-primary)',
    badge = '',
    badgeVariant = 'info',
    trend,
    trendColor = 'var(--info)',
    compact = false,
    proxy = false,
    proxyTitle = 'This metric is a proxy — see the Mills plan doc for the canonical definition.',
    hint = '',
    sub = '',
    element = 'div',
    children,
    onclick,
  }: {
    label: string;
    value?: string | number;
    color?: string;
    badge?: string;
    badgeVariant?: BadgeVariant;
    trend?: number[];
    trendColor?: string;
    compact?: boolean;
    proxy?: boolean;
    proxyTitle?: string;
    hint?: string;
    sub?: string;
    /**
     * Root tag. Defaults to a plain `div`; pass `article` when the card is a
     * self-contained, independently meaningful unit (an evidence report, a
     * summary block) so assistive tech keeps the implicit region boundary.
     */
    element?: 'div' | 'article' | 'section' | 'li';
    children?: Snippet;
    onclick?: () => void;
  } = $props();

  // Typed explicitly: `<svelte:element>` has a dynamic tag, so the event
  // parameter of an inline handler would otherwise be an implicit `any`.
  function handleKeydown(e: KeyboardEvent) {
    if (onclick && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      onclick();
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<svelte:element
  this={element}
  class="metric-card"
  class:compact
  class:clickable={!!onclick}
  title={hint || undefined}
  onclick={onclick}
  onkeydown={handleKeydown}
  tabindex={onclick ? 0 : undefined}
  role={onclick ? 'button' : undefined}
>
  <div class="metric-card-top">
    <span class="metric-card-label">
      {label}
      {#if proxy}
        <span class="metric-card-proxy" title={proxyTitle}>(proxy)</span>
      {/if}
    </span>
    {#if badge}
      <span class="metric-card-badge badge-{badgeVariant}">{badge}</span>
    {/if}
  </div>
  {#if value !== undefined}
    <div class="metric-card-value" style:color={color}>{value}</div>
  {/if}
  {@render children?.()}
  {#if sub}
    <div class="metric-card-sub">{sub}</div>
  {/if}
  {#if trend && trend.length > 1}
    <div class="metric-card-trend">
      <SparkLine data={trend} width={compact ? 48 : 80} height={compact ? 14 : 18} color={trendColor} />
    </div>
  {/if}
</svelte:element>

<style>
  .metric-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    padding: var(--space-2) var(--space-3);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    transition: border-color var(--transition-fast), box-shadow var(--transition-fast);
  }

  .metric-card.compact {
    padding: var(--space-1) var(--space-2);
  }

  .metric-card.clickable {
    cursor: pointer;
  }

  .metric-card.clickable:hover {
    border-color: var(--border-focus);
    box-shadow: 0 0 8px var(--glow-accent);
  }

  .metric-card.clickable:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: -2px;
  }

  .metric-card-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .metric-card-label {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .metric-card-proxy {
    margin-left: 4px;
    font-size: var(--text-2xs);
    font-weight: 500;
    text-transform: lowercase;
    letter-spacing: 0;
    color: var(--fg-dim);
    cursor: help;
  }

  .metric-card-badge {
    font-size: var(--text-2xs);
    padding: 1px 5px;
    border-radius: var(--radius-lg);
    font-weight: 500;
    /* Matches Badge.svelte's `.badge`: a status pill must never break mid-text
       when it shares a flex row with a long label. */
    white-space: nowrap;
  }

  .badge-info { background: var(--info-dim); color: var(--info); }
  .badge-success { background: var(--success-dim); color: var(--success); }
  .badge-warning { background: var(--warning-dim); color: var(--warning); }
  .badge-error { background: var(--error-dim); color: var(--error); }
  .badge-accent { background: var(--accent-dim); color: var(--accent); }
  .badge-muted { background: var(--bg-tertiary); color: var(--fg-muted); }

  .metric-card-value {
    font-size: var(--text-xl);
    font-weight: 700;
    font-family: var(--font-mono);
    line-height: 1;
    font-feature-settings: 'tnum';
  }

  .metric-card.compact .metric-card-value {
    font-size: var(--text-lg);
  }

  .metric-card-sub {
    font-size: var(--text-xs);
    color: var(--fg-muted);
  }

  .metric-card-trend {
    margin-top: var(--space-1);
  }
</style>
