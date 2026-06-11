<script>
  /**
   * DataTable — sortable, selectable table with sticky header,
   * skeleton loading, expandable rows, and optional row pagination.
   *
   * Column priority: a column with `hideBelow: <px>` is hidden when the
   * table wrap is narrower than that many pixels, so the unsized absorber
   * column never collapses under stable-layout's table-layout: fixed.
   * The engine hides the th; consumers receive `hiddenColumns` in the row
   * snippet and must skip the matching td: {#if !hiddenColumns.has('x')}.
   * Hiding is disabled in the ≤800px stacked-card mode, where every cell
   * stacks as a labeled block and must stay visible.
   *
   * @type {{
   *   columns: Array<{ key: string, label: string, sortable?: boolean, width?: string, align?: 'left'|'center'|'right', hideBelow?: number }>,
   *   rows: any[],
   *   sortKey?: string,
   *   sortDir?: 'asc' | 'desc',
   *   loading?: boolean,
   *   skeletonRows?: number,
   *   selectable?: boolean,
   *   selectedIds?: Set<string>,
   *   expandedIds?: Set<string>,
   *   idKey?: string,
   *   maxRows?: number,
   *   rowLabel?: string,
   *   stableLayout?: boolean,
   *   onSort?: (key: string, dir: 'asc' | 'desc') => void,
   *   onSelect?: (ids: Set<string>) => void,
   *   onRowClick?: (row: any) => void,
   *   onToggleExpand?: (row: any) => void,
   *   row: import('svelte').Snippet<[{ row: any, index: number, expanded: boolean, hiddenColumns: Set<string> }]>,
   *   expandedRow?: import('svelte').Snippet<[{ row: any, index: number }]>,
   * }}
   */
  let {
    columns = [],
    rows = [],
    sortKey = '',
    sortDir = 'asc',
    loading = false,
    skeletonRows = 5,
    selectable = false,
    selectedIds = new Set(),
    expandedIds = new Set(),
    idKey = 'id',
    maxRows = undefined,
    rowLabel = 'row',
    stableLayout = false,
    keyboardNav = true,
    onSort,
    onSelect,
    onRowClick,
    onToggleExpand,
    row: rowSnippet,
    expandedRow: expandedRowSnippet,
  } = $props();

  // Keyboard navigation focus. -1 means no row is keyboard-focused; the wrap
  // itself can still hold DOM focus for shortcut delivery.
  let wrapEl = $state(null);
  let focusedIndex = $state(-1);

  // Column priority: measure the wrap and hide hideBelow columns when the
  // table is too narrow for its fixed widths. Stacked-card mode (≤800px
  // viewports) keeps every column — cells render as labeled blocks there.
  let wrapWidth = $state(Infinity);
  let stackedMode = $state(false);

  $effect(() => {
    if (!wrapEl || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver((entries) => {
      wrapWidth = entries[0]?.contentRect?.width ?? Infinity;
    });
    ro.observe(wrapEl);
    const mq = globalThis.matchMedia?.('(max-width: 800px)');
    const onMq = () => { stackedMode = !!mq?.matches; };
    onMq();
    mq?.addEventListener('change', onMq);
    return () => {
      ro.disconnect();
      mq?.removeEventListener('change', onMq);
    };
  });

  let hiddenColumns = $derived.by(() => {
    const hidden = new Set();
    if (stackedMode) return hidden;
    for (const col of columns) {
      if (typeof col.hideBelow === 'number' && wrapWidth < col.hideBelow) hidden.add(col.key);
    }
    return hidden;
  });

  let visibleColumns = $derived(columns.filter((c) => !hiddenColumns.has(c.key)));
  let colSpan = $derived((selectable ? 1 : 0) + visibleColumns.length);

  // Restore persisted sort state on mount (keyed by first column label).
  $effect(() => {
    if (!onSort || !columns.length) return;
    const storageKey = `dt-sort-${columns[0]?.label ?? 'default'}`;
    try {
      const saved = sessionStorage.getItem(storageKey);
      if (saved) {
        const { key, dir } = JSON.parse(saved);
        if (key && dir && key !== sortKey) onSort(key, dir);
      }
    } catch { /* ignore */ }
  });

  // Row pagination: show maxRows initially, expand on demand.
  // Important: do NOT reset displayCount on every `rows` update — polled
  // tables (fleet, tasks, etc.) update `rows` continuously, and resetting
  // would snap a user-expanded page back to maxRows on every poll. Initialize
  // from maxRows; only react to maxRows itself changing.
  let displayCount = $state(typeof maxRows === 'number' ? maxRows : Infinity);

  $effect(() => {
    if (typeof maxRows !== 'number') {
      displayCount = Infinity;
      return;
    }
    // Ensure the floor stays at maxRows if the prop shrank; never clobber a
    // user-expanded page on every rows update.
    if (displayCount < maxRows) displayCount = maxRows;
  });

  let displayRows = $derived(maxRows ? rows.slice(0, displayCount) : rows);
  let hasMore = $derived(maxRows ? rows.length > displayCount : false);
  let remainingCount = $derived(rows.length - displayCount);
  let showFooter = $derived(!loading && rows.length > 0 && maxRows !== undefined);
  let summaryText = $derived.by(() => {
    const visible = displayRows.length;
    const total = rows.length;
    const label = total === 1 ? rowLabel : `${rowLabel}s`;
    if (visible === total) {
      return `Showing ${visible} ${label}`;
    }
    return `Showing ${visible} of ${total} ${label}`;
  });

  function showMore() {
    displayCount = Math.min(displayCount + (maxRows ?? 50), rows.length);
  }

  function handleHeaderClick(col) {
    if (!col.sortable || !onSort) return;
    const newDir = sortKey === col.key && sortDir === 'asc' ? 'desc' : 'asc';
    // Persist sort preference.
    try {
      const storageKey = `dt-sort-${columns[0]?.label ?? 'default'}`;
      sessionStorage.setItem(storageKey, JSON.stringify({ key: col.key, dir: newDir }));
    } catch { /* ignore */ }
    onSort(col.key, newDir);
  }

  function handleSelectAll() {
    if (!onSelect) return;
    if (selectedIds.size === rows.length) {
      onSelect(new Set());
    } else {
      onSelect(new Set(rows.map(r => r[idKey])));
    }
  }

  function handleSelectRow(row) {
    if (!onSelect) return;
    const next = new Set(selectedIds);
    const id = row[idKey];
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    onSelect(next);
  }

  let allSelected = $derived(rows.length > 0 && selectedIds.size === rows.length);
  let someSelected = $derived(selectedIds.size > 0 && selectedIds.size < rows.length);

  // Keep focusedIndex in range as displayRows mutates (polling, filter, sort).
  $effect(() => {
    const max = displayRows.length;
    if (focusedIndex >= max) focusedIndex = max - 1;
    if (max === 0) focusedIndex = -1;
  });

  function scrollFocusedIntoView() {
    if (focusedIndex < 0 || !wrapEl) return;
    const row = wrapEl.querySelector(`tr[data-row-index="${focusedIndex}"]`);
    if (row && typeof row.scrollIntoView === 'function') {
      row.scrollIntoView({ block: 'nearest', behavior: 'auto' });
    }
  }

  function invokeRowAction(rowData) {
    if (onRowClick) onRowClick(rowData);
    else if (onToggleExpand) onToggleExpand(rowData);
  }

  function handleWrapKeydown(e) {
    if (!keyboardNav) return;
    if (loading || displayRows.length === 0) return;
    // Don't hijack typing inside the table (e.g. an inline editor).
    const tag = e.target?.tagName;
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || e.target?.isContentEditable) return;

    switch (e.key) {
      case 'j':
      case 'ArrowDown':
        e.preventDefault();
        focusedIndex = focusedIndex < 0 ? 0 : Math.min(focusedIndex + 1, displayRows.length - 1);
        scrollFocusedIntoView();
        return;
      case 'k':
      case 'ArrowUp':
        e.preventDefault();
        focusedIndex = focusedIndex <= 0 ? 0 : focusedIndex - 1;
        scrollFocusedIntoView();
        return;
      case 'Enter':
        if (focusedIndex >= 0 && focusedIndex < displayRows.length) {
          e.preventDefault();
          invokeRowAction(displayRows[focusedIndex]);
        }
        return;
      default:
        return;
    }
  }
</script>

<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
  class="data-table-wrap"
  bind:this={wrapEl}
  tabindex={keyboardNav ? 0 : -1}
  onkeydown={handleWrapKeydown}
>
  <table class="data-table" class:stable-layout={stableLayout} role="grid">
    <thead>
      <tr>
        {#if selectable}
          <th class="data-table-check" scope="col">
            <input
              type="checkbox"
              checked={allSelected}
              indeterminate={someSelected}
              onchange={handleSelectAll}
              aria-label="Select all rows"
            />
          </th>
        {/if}
        {#each visibleColumns as col (col.key)}
          <th
            scope="col"
            class="dt-col-{col.key}"
            class:sortable={col.sortable}
            class:sorted={sortKey === col.key}
            style:width={col.width || 'auto'}
            style:text-align={col.align || 'left'}
            aria-sort={sortKey === col.key ? (sortDir === 'asc' ? 'ascending' : 'descending') : undefined}
            onclick={() => handleHeaderClick(col)}
            onkeydown={(e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); handleHeaderClick(col); } }}
            tabindex={col.sortable ? 0 : undefined}
            role={col.sortable ? 'button' : undefined}
          >
            <span class="data-table-header-label">{col.label}</span>
            {#if col.sortable && sortKey === col.key}
              <span class="data-table-sort-icon">{sortDir === 'asc' ? '\u25B2' : '\u25BC'}</span>
            {/if}
          </th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#if loading}
        {#each Array(skeletonRows) as _, i}
          <tr class="data-table-skeleton-row">
            {#if selectable}
              <td><div class="skeleton skeleton-text" style="width: 16px;"></div></td>
            {/if}
            {#each visibleColumns as col (col.key)}
              <td><div class="skeleton skeleton-text" style="width: {60 + (i * 7) % 40}%;"></div></td>
            {/each}
          </tr>
        {/each}
      {:else}
        {#each displayRows as rowData, index (rowData[idKey] ?? index)}
          {@const isExpanded = expandedIds.has(rowData[idKey])}
          <tr
            data-row-index={index}
            class:selected={selectable && selectedIds.has(rowData[idKey])}
            class:clickable={!!onRowClick || !!onToggleExpand}
            class:expanded-row={isExpanded}
            class:keyboard-focused={focusedIndex === index}
            onclick={() => { if (onRowClick) onRowClick(rowData); else if (onToggleExpand) onToggleExpand(rowData); }}
            onkeydown={(e) => { if ((onRowClick || onToggleExpand) && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); if (onRowClick) onRowClick(rowData); else if (onToggleExpand) onToggleExpand(rowData); } }}
            tabindex={(onRowClick || onToggleExpand) ? 0 : undefined}
            role={(onRowClick || onToggleExpand) ? 'row' : undefined}
          >
            {#if selectable}
              <td class="data-table-check">
                <input
                  type="checkbox"
                  checked={selectedIds.has(rowData[idKey])}
                  onchange={() => handleSelectRow(rowData)}
                  onclick={(e) => e.stopPropagation()}
                  aria-label="Select row"
                />
              </td>
            {/if}
            {@render rowSnippet({ row: rowData, index, expanded: isExpanded, hiddenColumns })}
          </tr>
          {#if isExpanded && expandedRowSnippet}
            <tr class="data-table-expand-row">
              <td colspan={colSpan}>
                {@render expandedRowSnippet({ row: rowData, index })}
              </td>
            </tr>
          {/if}
        {/each}
      {/if}
    </tbody>
  </table>
  {#if showFooter}
    <div class="data-table-footer">
      <span class="data-table-summary">{summaryText}</span>
      {#if hasMore}
        <button class="btn btn-ghost btn-sm" onclick={showMore}>
          Show {Math.min(maxRows ?? 50, remainingCount)} more
        </button>
      {/if}
    </div>
  {/if}
</div>

<style>
  .data-table-wrap {
    display: flex;
    flex-direction: column;
    overflow-x: auto;
    flex: 1;
    min-height: 0;
    outline: none;
  }

  .data-table-wrap:focus-visible {
    box-shadow: inset 0 0 0 2px var(--info);
    border-radius: var(--radius-sm);
  }

  .data-table tbody tr.keyboard-focused :global(td) {
    background: var(--info-dim);
    color: var(--fg-primary);
    box-shadow: inset 2px 0 0 var(--info);
  }

  .data-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .data-table.stable-layout {
    table-layout: fixed;
  }

  .data-table thead th {
    text-align: left;
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    padding: var(--space-1) var(--space-2);
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
    position: sticky;
    top: 0;
    background: var(--bg-secondary);
    z-index: 1;
    user-select: none;
  }

  .data-table thead th.sortable {
    cursor: pointer;
  }

  .data-table thead th.sortable:hover {
    color: var(--fg-primary);
  }

  .data-table thead th.sorted {
    color: var(--fg-primary);
  }

  .data-table thead th.sortable:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: -2px;
    color: var(--fg-primary);
  }

  .data-table-header-label {
    display: inline;
  }

  .data-table-sort-icon {
    font-size: 8px;
    margin-left: var(--space-1);
    opacity: 0.7;
  }

  .data-table tbody :global(td) {
    padding: var(--space-1) var(--space-2);
    border-bottom: 1px solid var(--border);
    color: var(--fg-secondary);
    vertical-align: top;
  }

  .data-table tbody tr:nth-child(even) :global(td) {
    background: color-mix(in srgb, var(--bg-tertiary) 45%, transparent);
  }

  .data-table.stable-layout thead th,
  .data-table.stable-layout tbody :global(td) {
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .data-table tbody tr:hover :global(td) {
    background: var(--bg-tertiary);
    color: var(--fg-primary);
  }

  .data-table tbody tr:last-child :global(td) {
    border-bottom: none;
  }

  .data-table tbody tr.selected :global(td) {
    background: color-mix(in srgb, var(--info-dim) 80%, transparent);
  }

  .data-table tbody tr.clickable {
    cursor: pointer;
  }

  .data-table tbody tr.clickable:focus-visible {
    outline: 2px solid var(--focus-ring);
    outline-offset: -2px;
  }

  .data-table-check {
    width: 32px;
    text-align: center;
  }

  .data-table-check input[type="checkbox"] {
    accent-color: var(--info);
    cursor: pointer;
  }

  .data-table-skeleton-row td {
    padding: var(--space-1) var(--space-2);
  }

  /* Expandable row support */
  .data-table tbody tr.expanded-row :global(td) {
    border-bottom: none;
  }

  .data-table tbody tr.data-table-expand-row td {
    padding: 0 var(--space-2) var(--space-2);
    border-bottom: 1px solid var(--border);
  }

  .data-table tbody tr.data-table-expand-row:hover td {
    background: transparent;
    color: inherit;
  }

  .data-table-footer {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
    padding: var(--space-2) 0 0;
    border-top: 1px solid color-mix(in srgb, var(--border) 70%, transparent);
    margin-top: var(--space-2);
  }

  .data-table-summary {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    font-family: var(--font-mono);
  }

  @media (max-width: 640px) {
    .data-table-footer {
      flex-direction: column;
      align-items: stretch;
    }
  }

  /* ≤800px — stacked-card mode (Slice B5 of the HUD UX overhaul).
     Each row becomes a vertically-stacked card so long horizontal tables
     stay readable on phones without horizontal scrolling. Header is hidden;
     row cells stack as block elements with their own padding. The first
     cell of each row (typically the primary identifier column) gets a
     bottom border to separate cards. */
  @media (max-width: 800px) {
    .data-table-wrap {
      overflow-x: visible;
    }
    .data-table,
    .data-table.stable-layout {
      table-layout: auto;
    }
    .data-table thead {
      display: none;
    }
    .data-table tbody,
    .data-table tbody tr,
    .data-table tbody :global(td) {
      display: block;
      width: 100%;
    }
    .data-table tbody tr {
      padding: var(--space-3) var(--space-2);
      border-bottom: 1px solid var(--border);
      background: transparent;
    }
    .data-table tbody tr:nth-child(even) :global(td),
    .data-table tbody tr:hover :global(td) {
      background: transparent;
    }
    .data-table tbody :global(td) {
      padding: 4px var(--space-1);
      border-bottom: none;
      white-space: normal;
      overflow: visible;
      text-overflow: clip;
    }
    .data-table.stable-layout tbody :global(td) {
      white-space: normal;
      overflow: visible;
      text-overflow: clip;
      min-width: 0;
    }
    .data-table tbody tr:last-child {
      border-bottom: none;
    }
    .data-table-skeleton-row td {
      padding: 6px var(--space-1);
    }
    .data-table-check {
      width: auto;
      text-align: left;
    }
  }
</style>
