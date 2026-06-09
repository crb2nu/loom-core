<script>
  /**
   * UnseenAboveChip — floating "↑ N new X" pill that scrolls back to the
   * top when clicked. Pairs with createStreamScroll's unseenCount,
   * isAtTop, and jumpToNewest. The caller is expected to gate the render
   * (e.g. `{#if scroll.unseenCount > 0 && !scroll.isAtTop}`); this
   * component does not render its own visibility guard so it stays trivial
   * for both lib consumers and visual testing.
   *
   * Style is `position: absolute` so it overlays the host without
   * reflowing the surrounding layout. The host MUST be `position: relative`
   * (or otherwise establish a containing block).
   *
   * @type {{
   *   count: number,
   *   onClick: () => void,
   *   singular?: string,  // default 'entry'
   *   plural?: string,    // default 'entries'
   * }}
   */
  let { count, onClick, singular = 'entry', plural = 'entries' } = $props();
</script>

<button type="button" class="unseen-indicator" onclick={onClick}>
  ↑ {count} new {count === 1 ? singular : plural}
</button>

<style>
  .unseen-indicator {
    position: absolute;
    top: var(--space-2);
    left: 50%;
    transform: translateX(-50%);
    z-index: 20;
    padding: 4px var(--space-3);
    background: color-mix(in srgb, var(--info) 22%, var(--bg-secondary));
    border: 1px solid var(--info);
    border-radius: var(--radius-full);
    color: var(--fg-primary);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 600;
    cursor: pointer;
    box-shadow: 0 2px 8px rgba(0, 0, 0, 0.4);
    transition: background var(--transition-fast), transform var(--transition-fast);
  }

  .unseen-indicator:hover {
    background: color-mix(in srgb, var(--info) 40%, var(--bg-secondary));
    transform: translateX(-50%) translateY(-1px);
  }

  .unseen-indicator:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: 2px;
  }
</style>
