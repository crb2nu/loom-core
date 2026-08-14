<script lang="ts">
  /**
   * InflightBoard — the Operator Deck's center column: everything currently in
   * motion, as three lanes over one row shape (see operatorHelpers.ts).
   * Purely presentational: rows arrive derived, selection + navigation are
   * callbacks the panel owns.
   */
  import type { InflightRow, InflightKind } from '../../utils/operatorHelpers.ts';

  interface Lane {
    kind: InflightKind;
    label: string;
    rows: InflightRow[];
    /** Where the lane's "all" link lands (view, subView). */
    viewTarget: [string, string];
    /** Named empty text so a quiet lane reads as healthy, not broken. */
    empty: string;
    /** Non-null when the lane's source fetch failed — must win over `empty`. */
    error?: string | null;
    /**
     * Soft caveat (reconnecting / partial refresh): rendered muted above the
     * rows, and it suppresses `empty` — an unreachable source must not claim
     * "nothing in flight".
     */
    notice?: string | null;
  }

  let {
    lanes,
    selectedKey = null,
    onSelect,
    onOpenView,
  }: {
    lanes: Lane[];
    selectedKey?: string | null;
    onSelect: (row: InflightRow) => void;
    onOpenView: (view: string, subView: string) => void;
  } = $props();

  function attention(rows: InflightRow[]): number {
    return rows.filter((r) => r.severity === 'error' || r.severity === 'warn').length;
  }
</script>

<div class="board">
  {#each lanes as lane (lane.kind)}
    <section class="lane" aria-label="{lane.label} lane">
      <header class="lane-head">
        <span class="lane-title">{lane.label}</span>
        <span class="lane-count">{lane.rows.length}</span>
        {#if attention(lane.rows) > 0}
          <span class="lane-attention" title="{attention(lane.rows)} need attention">{attention(lane.rows)}!</span>
        {/if}
        <button
          class="lane-open"
          onclick={() => onOpenView(lane.viewTarget[0], lane.viewTarget[1])}
          title="Open the full {lane.label} view"
        >all {'→'}</button>
      </header>

      {#if lane.error}
        <div class="lane-error" role="alert">Unavailable — {lane.error}</div>
      {:else if lane.notice}
        <div class="lane-notice">{lane.notice}</div>
      {/if}

      {#if lane.rows.length > 0}
        <ul class="lane-rows">
          {#each lane.rows as row (row.key)}
            <li>
              <button
                class="row"
                class:selected={selectedKey === row.key}
                data-severity={row.severity}
                onclick={() => onSelect(row)}
                title={row.title}
              >
                <span class="row-dot" aria-hidden="true"></span>
                <span class="row-main">
                  <span class="row-title">{row.title}</span>
                  <span class="row-sub">{row.subtitle}</span>
                </span>
                <span class="row-meta">
                  <span class="row-state">{row.state}</span>
                  {#if row.age}<span class="row-age">{row.age}</span>{/if}
                </span>
              </button>
            </li>
          {/each}
        </ul>
      {:else if !lane.error && !lane.notice}
        <div class="lane-empty">{lane.empty}</div>
      {/if}
    </section>
  {/each}
</div>

<style>
  .board {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    min-width: 0;
  }

  .lane {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    background: color-mix(in srgb, var(--bg-secondary) 82%, transparent);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-lg);
    padding: var(--space-3);
    min-width: 0;
  }

  .lane-head {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .lane-title {
    font-size: var(--text-xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-secondary);
  }

  .lane-count {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 1px 6px;
    border-radius: var(--radius-full);
    background: rgba(255, 255, 255, 0.04);
  }

  .lane-attention {
    font-size: 10px;
    font-family: var(--font-mono);
    font-weight: 700;
    color: var(--warning);
    padding: 1px 6px;
    border-radius: var(--radius-full);
    background: color-mix(in srgb, var(--warning) 16%, transparent);
  }

  .lane-open {
    margin-left: auto;
    font-size: var(--text-xs);
    color: var(--fg-muted);
    padding: 2px 6px;
    border-radius: var(--radius-sm);
    transition: color var(--transition-fast), background var(--transition-fast);
  }

  .lane-open:hover {
    color: var(--fg-primary);
    background: var(--bg-tertiary);
  }

  .lane-empty {
    font-size: var(--text-sm);
    color: var(--fg-dim);
    padding: var(--space-2) var(--space-1);
  }

  .lane-error {
    font-size: var(--text-sm);
    color: var(--error);
    padding: var(--space-2) var(--space-1) var(--space-2) var(--space-2);
    border-left: 2px solid var(--error);
  }

  .lane-notice {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    font-style: italic;
    padding: var(--space-1) var(--space-1) var(--space-1) var(--space-2);
    border-left: 2px solid var(--border-subtle);
  }

  .lane-rows {
    display: flex;
    flex-direction: column;
    gap: 4px;
    list-style: none;
    margin: 0;
    padding: 0;
  }

  .row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
    text-align: left;
    padding: var(--space-2);
    border-radius: var(--radius-md);
    border: 1px solid transparent;
    background: transparent;
    cursor: pointer;
    transition: background var(--transition-fast), border-color var(--transition-fast);
    min-width: 0;
  }

  .row:hover {
    background: color-mix(in srgb, var(--bg-tertiary) 85%, transparent);
  }

  .row.selected {
    background: var(--bg-tertiary);
    border-color: color-mix(in srgb, var(--info) 30%, var(--border));
  }

  .row-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    flex-shrink: 0;
    background: var(--fg-dim);
  }

  .row[data-severity='error'] .row-dot {
    background: var(--error);
    box-shadow: 0 0 6px var(--error);
  }
  .row[data-severity='warn'] .row-dot {
    background: var(--warning);
    box-shadow: 0 0 5px var(--warning);
  }
  .row[data-severity='busy'] .row-dot {
    background: var(--info);
    box-shadow: 0 0 5px var(--info);
    animation: pulse 2s infinite;
  }
  .row[data-severity='ok'] .row-dot {
    background: var(--success);
  }

  .row-main {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
    flex: 1;
  }

  .row-title {
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-primary);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .row-sub {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .row-meta {
    display: flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 1px;
    flex-shrink: 0;
  }

  .row-state {
    font-size: 10px;
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--fg-secondary);
  }

  .row[data-severity='error'] .row-state { color: var(--error); }
  .row[data-severity='warn'] .row-state { color: var(--warning); }
  .row[data-severity='busy'] .row-state { color: var(--info); }

  .row-age {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
  }
</style>
