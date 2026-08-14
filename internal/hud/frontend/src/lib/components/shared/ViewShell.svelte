<script lang="ts">
  import type { Snippet } from 'svelte';

  /**
   * ViewShell — wrapper for a grouped view that provides sub-navigation tabs.
   * Renders a segmented control bar when the view has multiple sub-panels.
   *
   * sub-tabs may optionally carry a `count` (number) which renders as a
   * small badge next to the label. Zero/undefined counts render no
   * badge so empty tabs stay visually quiet.
   */
  interface SubTab {
    id: string;
    label: string;
    key: string;
    count?: number;
    group?: string;
  }

  let {
    subViews = [],
    activeSubView = '',
    onSwitch = () => {},
    children,
  }: {
    subViews?: SubTab[];
    activeSubView?: string;
    onSwitch?: (id: string) => void;
    children: Snippet;
  } = $props();

  let showTabs = $derived(subViews.length > 1);

  // The bar is a horizontal scroller (16 tabs on Mills) — keep the active
  // tab scrolled into view when selection arrives via hash or keyboard,
  // and on resize (the scroll offset survives layout changes).
  let tabsEl: HTMLElement | undefined = $state();
  function scrollActiveSubTabIntoView() {
    tabsEl
      ?.querySelector('.view-tab.active')
      ?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  $effect(() => {
    void activeSubView;
    scrollActiveSubTabIntoView();
  });

  /**
   * Partition the tabs into contiguous runs sharing a `group`. A view whose
   * sub-views declare no group yields exactly one unnamed section, which
   * renders identically to the old flat bar — grouping is opt-in per view
   * (today: Mills, whose 16 tabs split into mill-floor vs governance).
   */
  let groups = $derived.by(() => {
    const out: Array<{ name: string | null; tabs: SubTab[] }> = [];
    for (const sv of subViews) {
      const name = sv.group ?? null;
      const last = out[out.length - 1];
      if (last && last.name === name) last.tabs.push(sv);
      else out.push({ name, tabs: [sv] });
    }
    return out;
  });

  let grouped = $derived(groups.some((g) => g.name !== null));
</script>

<svelte:window onresize={scrollActiveSubTabIntoView} />

<div class="view-shell">
  {#if showTabs}
    <nav class="view-tabs" class:is-grouped={grouped} aria-label="Sub-navigation" bind:this={tabsEl}>
      {#each groups as group, gi (group.name ?? `_${gi}`)}
        {#if gi > 0}
          <span class="view-tab-separator" aria-hidden="true"></span>
        {/if}
        <!-- The caption names the group for sighted operators; the section's
             aria-label carries the same name for assistive tech, so the
             visual caption itself stays out of the tab reading order. -->
        <div class="view-tab-group" role="group" aria-label={group.name ?? undefined}>
          {#if group.name}
            <span class="view-tab-group-label" aria-hidden="true">{group.name}</span>
          {/if}
          <div class="view-tab-group-tabs">
            {#each group.tabs as sv (sv.id)}
              <button
                class="view-tab"
                class:active={activeSubView === sv.id}
                onclick={() => onSwitch(sv.id)}
                aria-current={activeSubView === sv.id ? 'page' : undefined}
                title="{sv.label} ({sv.key})"
              >
                <span class="view-tab-label">{sv.label}</span>
                {#if typeof sv.count === 'number' && sv.count > 0}
                  <span class="view-tab-count" aria-label={`${sv.count} items`}>{sv.count}</span>
                {/if}
                <kbd class="view-tab-key">{sv.key}</kbd>
              </button>
            {/each}
          </div>
        </div>
      {/each}
    </nav>
  {/if}

  <div class="view-content">
    {@render children()}
  </div>
</div>

<style>
  .view-shell {
    display: flex;
    flex-direction: column;
    flex: 1;
    min-width: 0;
    overflow: hidden;
  }

  .view-tabs {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    padding: var(--space-2) var(--panel-padding) var(--space-1);
    background: transparent;
    flex-shrink: 0;
    position: relative;
    overflow-x: auto;
    scrollbar-width: none;
    /* Same scroll affordance as the top nav: clipped tabs fade out. */
    mask-image: linear-gradient(90deg, transparent, black 8px, black calc(100% - 28px), transparent);
    scroll-padding-inline: 32px;
  }

  .view-tabs::-webkit-scrollbar {
    display: none;
  }

  /* A grouped bar reserves a caption line above the tabs and bottom-aligns
     the sections. The gap stays at the flat row's value: with 16 Mills tabs
     the bar already scrolls horizontally, so grouping must buy legibility
     without spending width — the separator does the visual work. */
  .view-tabs.is-grouped {
    align-items: flex-end;
    gap: 0;
  }

  /* The bar is an overflow-x scroller, so a group must never shrink below its
     tabs' natural width — a shrinking group would let its last tabs render on
     top of the next group instead of scrolling. */
  .view-tab-group {
    display: flex;
    flex: 0 0 auto;
    flex-direction: column;
    gap: 3px;
  }

  .view-tab-group-label {
    font-size: 9px;
    font-family: var(--font-mono);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
    padding-left: var(--space-1);
    white-space: nowrap;
  }

  .view-tab-group-tabs {
    display: flex;
    align-items: center;
    gap: var(--space-1);
  }

  .view-tab-group-tabs > .view-tab {
    flex: 0 0 auto;
  }

  /* Hairline between adjacent groups — the same 1px/var(--border) motif the
     top nav uses to fence the Overview tab off from the grouped views. */
  .view-tab-separator {
    width: 1px;
    align-self: stretch;
    margin: 0 var(--space-2);
    background: linear-gradient(
      180deg,
      transparent,
      var(--border) 35%,
      var(--border) 100%
    );
    flex: 0 0 auto;
  }

  .view-tab {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    padding: 8px 12px;
    border-radius: var(--radius-md);
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-muted);
    transition: background var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
    position: relative;
    cursor: pointer;
    background: transparent;
    border: 1px solid transparent;
    letter-spacing: var(--tracking-normal);
    white-space: nowrap;
  }

  .view-tab:hover {
    background: rgba(var(--fg-rgb), 0.05);
    color: var(--fg-primary);
  }

  .view-tab:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-md);
  }

  /* Active = one signifier: an info-tinted pill. Mirrors the top nav's
     accent pill one tier down — accent marks the view, info marks the
     sub-view. (Previously: filled card + underline bar + drop shadow.) */
  .view-tab.active {
    background: rgba(var(--info-rgb), var(--opacity-light));
    color: var(--fg-primary);
    font-weight: 600;
    border-color: rgba(var(--info-rgb), var(--opacity-strong));
  }

  .view-tab-label {
    white-space: nowrap;
  }

  /* Same hover-reveal contract as the top nav's shortcut chips: width is
     reserved, visibility follows hover/focus/active. */
  .view-tab-key {
    font-size: 9px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 1px 4px;
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    line-height: 1;
    opacity: 0;
    transition: opacity var(--transition-fast);
    background: rgba(255, 255, 255, 0.02);
  }

  .view-tab:hover .view-tab-key,
  .view-tab:focus-visible .view-tab-key,
  .view-tab.active .view-tab-key {
    opacity: 0.8;
  }

  .view-tab-count {
    font-size: 10px;
    font-family: var(--font-mono);
    line-height: 1;
    padding: 2px 6px;
    border-radius: 999px;
    background: color-mix(in srgb, var(--info) 18%, transparent);
    color: var(--info);
    font-weight: 600;
    min-width: 18px;
    text-align: center;
  }
  .view-tab.active .view-tab-count {
    background: color-mix(in srgb, var(--info) 28%, transparent);
  }

  .view-content {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    display: flex;
    flex-direction: column;
  }

  @media (max-width: 768px) {
    .view-tabs {
      overflow-x: auto;
      scrollbar-width: none;
      -webkit-overflow-scrolling: touch;
    }
    .view-tabs::-webkit-scrollbar {
      display: none;
    }
    .view-tab-key {
      display: none;
    }
    .view-tab {
      min-height: 44px;
      flex-shrink: 0;
    }
  }

  @media (max-width: 480px) {
    .view-tabs {
      padding: var(--space-1) var(--space-2);
    }
    /* On a phone the bar is a scroll strip, not a layout — the captions cost
       a whole line for two words, so the separator carries the grouping. */
    .view-tab-group-label {
      display: none;
    }
  }
</style>
