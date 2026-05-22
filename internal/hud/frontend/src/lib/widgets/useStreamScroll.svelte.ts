// useStreamScroll — behavioral composable for audit-stream-style panels
// (Stream, Timeline, Traces) backed by widgets/VirtualList. Provides:
//
//   - scroll-aware "at top" detection on the VirtualList container
//   - snap-to-top on prepend when the user is near the top
//   - scroll-position anchoring on prepend when the user is scrolled down
//     (so prepends don't silently shift the items they're reading)
//   - unseen-count accumulator while scrolled down, reset when they return
//     to the top
//   - jumpToNewest() action for the "N new above" chip click handler
//
// Usage in a Svelte 5 panel:
//
//   <script>
//     import { createStreamScroll } from '../widgets/useStreamScroll.svelte.ts';
//     import UnseenAboveChip from '../widgets/UnseenAboveChip.svelte';
//
//     let entries = $derived(store.entries ?? []);
//     let filtered = $derived.by(() => /* filter entries */);
//
//     const scroll = createStreamScroll({
//       rowHeight: ROW_HEIGHT,
//       source: () => entries.length,
//       visible: () => filtered.length,
//       paused: () => store.paused,  // optional
//     });
//   </script>
//
//   <div class="panel-area">
//     {#if scroll.unseenCount > 0 && !scroll.isAtTop}
//       <UnseenAboveChip count={scroll.unseenCount} onClick={scroll.jumpToNewest} />
//     {/if}
//     <VirtualList items={filtered} itemHeight={ROW_HEIGHT} bind:containerEl={scroll.containerEl}>
//       ...
//     </VirtualList>
//   </div>
//
// Real prepends are detected by growth of source() (the raw store count),
// not visible() (the post-filter count). That way a typeFilter / agentFilter
// change doesn't get misread as a prepend — only actual new data shifts
// scroll. Compensation distance uses visible() since the user scrolls
// through the filtered list.

export interface StreamScrollOptions {
  /** Pixel height per row, used to compute scroll compensation on prepend. */
  rowHeight: number;
  /** Reactive getter for the raw source count (e.g. () => store.entries.length).
   *  Real prepends are detected by growth of this number. */
  source: () => number;
  /** Reactive getter for the visible (filtered/sorted) count.
   *  Compensation distance and unseen delta come from this. */
  visible: () => number;
  /** Reactive getter for paused state — when true, both snap-to-top and
   *  anchoring are gated off (the prior StreamPanel behavior). Optional;
   *  defaults to () => false for panels without a pause concept. */
  paused?: () => boolean;
  /** When scrollTop is within this many pixels of 0, treat as "at top".
   *  Defaults to rowHeight / 2 so brief overshoot still counts. */
  scrollTopTolerancePx?: number;
}

export interface StreamScroll {
  /** Bindable container element — assign via bind:containerEl on VirtualList. */
  containerEl: HTMLElement | null;
  /** True when the user is within tolerance of scrollTop=0. */
  readonly isAtTop: boolean;
  /** Count of prepended visible rows accumulated since the user left the top. */
  readonly unseenCount: number;
  /** Scrolls the container back to the top (clears unseenCount via isAtTop). */
  jumpToNewest: () => void;
}

export function createStreamScroll(opts: StreamScrollOptions): StreamScroll {
  const tolerance = opts.scrollTopTolerancePx ?? opts.rowHeight / 2;
  const isPaused = opts.paused ?? (() => false);

  // Plain-let trackers for the previous source/visible counts — writes to
  // these don't re-trigger the prepend effect, so they're stored outside
  // the $state proxy intentionally.
  let prevSourceLen = 0;
  let prevVisibleLen = 0;

  const state = $state<{
    containerEl: HTMLElement | null;
    isAtTop: boolean;
    unseenCount: number;
    jumpToNewest: () => void;
  }>({
    containerEl: null,
    isAtTop: true,
    unseenCount: 0,
    jumpToNewest() {
      if (state.containerEl) state.containerEl.scrollTop = 0;
    },
  });

  // Track whether the user is near the top by listening on the container's
  // own scroll events. Call the handler once on attach so a restored
  // scrollTop > 0 is reflected immediately, rather than waiting for the
  // first user interaction.
  $effect(() => {
    const el = state.containerEl;
    if (!el) return;
    const handler = () => {
      state.isAtTop = el.scrollTop < tolerance;
    };
    handler();
    el.addEventListener('scroll', handler, { passive: true });
    return () => el.removeEventListener('scroll', handler);
  });

  // Clear the unseen accumulator whenever the user returns to the top.
  $effect(() => {
    if (state.isAtTop) state.unseenCount = 0;
  });

  // React to prepends. Gate on source() growth so filter-only changes don't
  // count. When source actually grew:
  //   - isAtTop & !paused → snap to top so the newest row stays visible;
  //   - scrolled down → compensate scrollTop by visibleDelta * rowHeight so
  //     the items currently in the user's viewport stay anchored, and
  //     accumulate the unseen count for the chip.
  $effect(() => {
    const sourceLen = opts.source();
    const visibleLen = opts.visible();
    const sourceDelta = sourceLen - prevSourceLen;
    const visibleDelta = visibleLen - prevVisibleLen;
    prevSourceLen = sourceLen;
    prevVisibleLen = visibleLen;

    if (sourceDelta <= 0 || isPaused() || !state.containerEl) return;

    if (state.isAtTop) {
      state.containerEl.scrollTop = 0;
    } else if (visibleDelta > 0) {
      state.containerEl.scrollTop += visibleDelta * opts.rowHeight;
      state.unseenCount += visibleDelta;
    }
  });

  return state;
}
