<script lang="ts">
  /**
   * DepartureBoard — the factory floor log as a split-flap airport
   * board. One row per run; a cell flap-flips only when its value
   * actually changes ({#key} remounts the cell → flip-in animation),
   * so motion on this board IS news, never ambience. Static under
   * reduced-motion.
   */
  import { squeezeSlug, type DepartureRow } from '../../utils/departureHelpers.ts';

  // onSelect turns the board from a read-only log (FactoryPanel) into a
  // clickable roster (ShuttlesPanel): when provided, each data row becomes a
  // keyboard-reachable button that opens the run's drawer. Absent → rows stay
  // presentational, so existing callers are unchanged.
  let {
    rows,
    onSelect,
    selectedKey = null,
  }: {
    rows: DepartureRow[];
    onSelect?: (key: string) => void;
    selectedKey?: string | null;
  } = $props();

  // House rule #3: normalize the array at the read boundary.
  let boardRows = $derived(rows ?? []);

  function onRowKeydown(ev: KeyboardEvent, key: string): void {
    if (!onSelect) return;
    if (ev.key === 'Enter' || ev.key === ' ') {
      ev.preventDefault();
      onSelect(key);
    }
  }

  // 1.1rem of indent per subrun level keeps the shuttle id readable while
  // making the parent→child tree legible at a glance.
  function indent(depth?: number): string {
    if (!depth || depth <= 0) return '';
    return `padding-left:${1.1 * depth}rem`;
  }
</script>

<div class="board" role="table" aria-label="Factory departures — one row per pipeline run">
  <div class="board-row board-head" role="row">
    <span role="columnheader" class="col-when">time</span>
    <span role="columnheader">shuttle</span>
    <!-- The cell below is row.destination — the run's BacklogID, i.e. the
         warp it is weaving. The bolt only exists once the run lands. -->
    <span role="columnheader">warp</span>
    <span role="columnheader">stage</span>
    <span role="columnheader" class="col-status">status</span>
  </div>
  {#snippet cells(row: DepartureRow)}
    <span class="cell col-when" role="cell">{row.when ?? ''}</span>
    <span class="cell" role="cell" style={indent(row.depth)}>
      {#if (row.depth ?? 0) > 0}<span class="tree-glyph" aria-hidden="true">└─ </span>{/if}{row.flight}
    </span>
    <span class="cell cell-dest" role="cell" title={row.destination}>{squeezeSlug(row.destination)}</span>
    <span class="cell" role="cell">
      {#key row.via}<span class="flap">{row.via}</span>{/key}
    </span>
    <span class="cell col-status st-{row.tone}" role="cell">
      <!-- Only the status word flaps. The note ticks with the clock, and a
           flap every poll would be ambience — it updates in place instead. -->
      {#key row.status}<span class="flap">{row.status}</span>{/key}{#if row.note}<span
          class="status-note"
        >{row.note}</span>{/if}
    </span>
  {/snippet}
  {#each boardRows as row (row.key)}
    {#if onSelect}
      <!-- Interactive roster (ShuttlesPanel): a real button role so the row
           is keyboard-reachable and the static tabindex passes a11y checks. -->
      <div
        class="board-row clickable"
        class:subrun={(row.depth ?? 0) > 0}
        class:selected={selectedKey === row.key}
        role="button"
        tabindex="0"
        aria-label={`Open detail for run ${row.key}`}
        onclick={() => onSelect(row.key)}
        onkeydown={(ev) => onRowKeydown(ev, row.key)}
      >
        {@render cells(row)}
      </div>
    {:else}
      <div class="board-row" role="row">
        {@render cells(row)}
      </div>
    {/if}
  {:else}
    <div class="board-empty" role="row">
      <span role="cell">floor quiet — warp strung, shuttles racked, waiting for the next dispatch</span>
    </div>
  {/each}
</div>

<style>
  .board {
    flex: 1;
    display: flex;
    flex-direction: column;
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    min-width: 0;
  }

  .board-row {
    display: grid;
    grid-template-columns: 42px minmax(90px, 1fr) minmax(120px, 2fr) minmax(120px, 2fr) minmax(84px, 1fr);
    gap: var(--space-3);
    align-items: center;
    padding: 3px var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
  }
  .board-row:last-child { border-bottom: none; }

  /* Board clock: quiet, fixed-width, right-aligned so the minutes line up
     down the column like a printed timetable. */
  .col-when {
    color: var(--fg-dim);
    text-align: right;
    font-variant-numeric: tabular-nums;
  }

  .board-head {
    color: var(--fg-dim);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    font-size: var(--text-2xs);
    padding-top: var(--space-1);
  }

  .cell {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--fg-secondary);
    /* Flap cells rotate in 3D; give them a hinge to swing from. */
    perspective: 300px;
  }
  .cell-dest { color: var(--fg-primary); }

  /* Interactive variant (ShuttlesPanel): the row is a real button, so it
     needs a pointer, hover wash, focus ring, and selected marker. Reset the
     button chrome the grid layout inherits when role="button" lands on a div
     is unnecessary (it's still a div), but keep text-aligned + full-width. */
  .board-row.clickable {
    cursor: pointer;
    text-align: left;
    transition: background 0.1s ease-out;
  }
  .board-row.clickable:hover { background: rgba(var(--mills-rgb), 0.08); }
  .board-row.clickable:focus-visible {
    outline: 2px solid var(--accent);
    outline-offset: -2px;
  }
  .board-row.selected {
    background: rgba(var(--mills-rgb), 0.14);
    box-shadow: inset 3px 0 0 var(--accent);
  }
  .board-row.subrun { background: rgba(var(--mills-rgb), 0.04); }
  .board-row.subrun.selected { background: rgba(var(--mills-rgb), 0.16); }
  .tree-glyph { color: var(--fg-dim); }
  .col-status { text-transform: uppercase; letter-spacing: var(--tracking-wide); }
  /* Lowercase duration beside the status word — it is a qualifier, not a
     second status, so it stays unshouted and inherits the row's tone. */
  .status-note {
    margin-left: 0.4em;
    text-transform: none;
    letter-spacing: normal;
    opacity: 0.75;
  }

  .st-ok  { color: var(--success); }
  .st-hot { color: var(--accent); }
  .st-wr  { color: var(--warning); }
  .st-cy  { color: var(--info); }
  /* Deliberate non-events (suppressed proposals): present but unlit. */
  .st-dm  { color: var(--fg-muted); }

  .flap {
    display: inline-block;
    transform-origin: center top;
    animation: board-flap 0.4s cubic-bezier(0.2, 0.9, 0.3, 1.15);
    backface-visibility: hidden;
  }

  .board-empty {
    padding: var(--space-2) var(--space-3);
    color: var(--fg-muted);
  }

  @keyframes board-flap {
    0% { transform: rotateX(-88deg); opacity: 0.15; }
    100% { transform: rotateX(0deg); opacity: 1; }
  }
  @media (prefers-reduced-motion: reduce) {
    .flap { animation: none; }
  }
</style>
