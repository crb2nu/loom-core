<script lang="ts" generics="T">
  import type { Snippet } from 'svelte';

  /**
   * VirtualList — renders only visible items + buffer for large lists.
   *
   * Props:
   *  - items: T[]           — full data array
   *  - itemHeight: number   — fixed height per row (px)
   *  - buffer: number       — extra rows to render above/below viewport
   *  - containerEl: bindable handle to the scroll viewport so parents can
   *                 imperatively read/write scrollTop (e.g. snap-to-top on
   *                 new entries). Defaults to null; only meaningful when the
   *                 parent binds it.
   *  - label: accessible name for the scroll region. The viewport is
   *           tabbable so arrow/PageUp/PageDown/Home/End reach the data
   *           without a pointer — rows hold no focusable elements, so
   *           without this the whole log was unreachable by keyboard.
   *
   * Slots:
   *  - default: receives { item, index } for each visible item
   */
  let {
    items = [],
    itemHeight = 32,
    buffer = 10,
    label = 'Scrollable list',
    children,
    containerEl = $bindable(null),
  }: {
    items?: T[];
    itemHeight?: number;
    buffer?: number;
    label?: string;
    containerEl?: HTMLElement | null;
    children: Snippet<[{ item: T; index: number }]>;
  } = $props();

  let scrollTop = $state(0);
  let containerHeight = $state(400);

  function handleScroll() {
    if (containerEl) {
      scrollTop = containerEl.scrollTop;
    }
  }

  let totalHeight = $derived(items.length * itemHeight);

  let startIdx = $derived(Math.max(0, Math.floor(scrollTop / itemHeight) - buffer));
  let endIdx = $derived(Math.min(items.length, Math.ceil((scrollTop + containerHeight) / itemHeight) + buffer));

  let visibleItems = $derived(
    items.slice(startIdx, endIdx).map((item, i) => ({
      item,
      index: startIdx + i,
      offsetY: (startIdx + i) * itemHeight,
    }))
  );

  $effect(() => {
    if (containerEl) {
      containerHeight = containerEl.clientHeight;
      const ro = new ResizeObserver((entries) => {
        containerHeight = entries[0].contentRect.height;
      });
      ro.observe(containerEl);
      return () => ro.disconnect();
    }
  });
</script>

<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div
  class="virtual-list"
  bind:this={containerEl}
  onscroll={handleScroll}
  tabindex="0"
  role="region"
  aria-label={label}
>
  <div class="virtual-list-spacer" style:height="{totalHeight}px">
    {#each visibleItems as { item, index, offsetY } (index)}
      <div class="virtual-list-item" style:transform="translateY({offsetY}px)" style:height="{itemHeight}px">
        {@render children({ item, index })}
      </div>
    {/each}
  </div>
</div>

<style>
  .virtual-list {
    overflow-y: auto;
    height: 100%;
    position: relative;
  }

  .virtual-list:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: -2px;
  }

  .virtual-list-spacer {
    position: relative;
  }

  .virtual-list-item {
    position: absolute;
    top: 0;
    left: 0;
    right: 0;
  }
</style>
