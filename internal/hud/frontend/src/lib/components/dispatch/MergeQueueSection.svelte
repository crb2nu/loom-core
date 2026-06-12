<script>
  import { mergeQueueStore } from '../../stores/mergeQueue.svelte.ts';
  import MetricCard from '../shared/MetricCard.svelte';
  import EmptyState from '../shared/EmptyState.svelte';

  let { collapsed = $bindable(false) } = $props();

  let summary = $derived(mergeQueueStore.summary);
  let ready = $derived(mergeQueueStore.ready);
  let blocked = $derived(mergeQueueStore.blocked);
  let totalCount = $derived(mergeQueueStore.totalCount);

  // Sort state — defaults match the backend's natural order so the first
  // render is unsurprising, but the operator can re-sort by any column.
  let readySortKey = $state('conflicts');
  let readySortDir = $state('asc');
  let blockedSortKey = $state('blockers');
  let blockedSortDir = $state('desc');

  function cmpStr(a, b) {
    return (a || '').localeCompare(b || '');
  }

  function cmpNum(a, b) {
    return (a || 0) - (b || 0);
  }

  let sortedReady = $derived.by(() => {
    const items = [...ready];
    items.sort((a, b) => {
      let cmp = 0;
      if (readySortKey === 'agent_id') cmp = cmpStr(a.agent_id, b.agent_id);
      else if (readySortKey === 'branch') cmp = cmpStr(a.branch, b.branch);
      else if (readySortKey === 'namespace') cmp = cmpStr(a.namespace, b.namespace);
      else if (readySortKey === 'tasks') cmp = cmpNum(a.task_count, b.task_count);
      else if (readySortKey === 'conflicts') cmp = cmpNum(a.conflict_files, b.conflict_files);
      if (cmp === 0) cmp = cmpStr(a.agent_id, b.agent_id);
      return readySortDir === 'asc' ? cmp : -cmp;
    });
    return items;
  });

  let sortedBlocked = $derived.by(() => {
    const items = [...blocked];
    items.sort((a, b) => {
      let cmp = 0;
      if (blockedSortKey === 'agent_id') cmp = cmpStr(a.agent_id, b.agent_id);
      else if (blockedSortKey === 'branch') cmp = cmpStr(a.branch, b.branch);
      else if (blockedSortKey === 'blockers') cmp = cmpNum(a.merge_blockers?.length ?? 0, b.merge_blockers?.length ?? 0);
      else if (blockedSortKey === 'blocked_tasks') cmp = cmpNum(a.blocked_tasks, b.blocked_tasks);
      if (cmp === 0) cmp = cmpStr(a.agent_id, b.agent_id);
      return blockedSortDir === 'asc' ? cmp : -cmp;
    });
    return items;
  });

  function sortReady(key) {
    if (readySortKey === key) {
      readySortDir = readySortDir === 'asc' ? 'desc' : 'asc';
    } else {
      readySortKey = key;
      readySortDir = key === 'agent_id' || key === 'branch' || key === 'namespace' ? 'asc' : 'desc';
    }
  }

  function sortBlocked(key) {
    if (blockedSortKey === key) {
      blockedSortDir = blockedSortDir === 'asc' ? 'desc' : 'asc';
    } else {
      blockedSortKey = key;
      blockedSortDir = key === 'agent_id' || key === 'branch' ? 'asc' : 'desc';
    }
  }

  function sortIndicator(activeKey, dir, key) {
    if (activeKey !== key) return '';
    return dir === 'asc' ? ' ▴' : ' ▾';
  }
</script>

<section class="dispatch-section">
  <div class="section-head">
    <button class="section-toggle" onclick={() => collapsed = !collapsed}>
      <span class="toggle-icon">{collapsed ? '▶' : '▼'}</span>
      <h3 class="section-title">Merge queue</h3>
      <span class="section-count">{totalCount}</span>
    </button>
    <div class="section-subtitle">
      {summary.ready_to_merge} ready · {summary.blocked} blocked · {summary.conflict_pairs} conflict pair{summary.conflict_pairs === 1 ? '' : 's'}
    </div>
  </div>

  {#if !collapsed}
    <div class="merge-metrics">
      <MetricCard label="Total" value={summary.total_branches} compact />
      <MetricCard label="Ready" value={summary.ready_to_merge} color={summary.ready_to_merge > 0 ? 'var(--success)' : 'var(--fg-primary)'} compact />
      <MetricCard label="Blocked" value={summary.blocked} color={summary.blocked > 0 ? 'var(--error)' : 'var(--fg-primary)'} compact />
      <MetricCard label="Conflicts" value={summary.conflict_pairs} color={summary.conflict_pairs > 0 ? 'var(--warning)' : 'var(--fg-primary)'} compact />
    </div>

    {#if ready.length > 0}
      <div class="subtable-label">Ready to merge</div>
      <div class="table-wrap">
        <table class="merge-table">
          <thead>
            <tr>
              <th>
                <button type="button" class="sort-th" aria-sort={readySortKey === 'agent_id' ? (readySortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortReady('agent_id')}>
                  Agent{sortIndicator(readySortKey, readySortDir, 'agent_id')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th" aria-sort={readySortKey === 'branch' ? (readySortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortReady('branch')}>
                  Branch{sortIndicator(readySortKey, readySortDir, 'branch')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th" aria-sort={readySortKey === 'namespace' ? (readySortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortReady('namespace')}>
                  Namespace{sortIndicator(readySortKey, readySortDir, 'namespace')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th sort-th-num" aria-sort={readySortKey === 'tasks' ? (readySortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortReady('tasks')}>
                  Tasks{sortIndicator(readySortKey, readySortDir, 'tasks')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th sort-th-num" aria-sort={readySortKey === 'conflicts' ? (readySortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortReady('conflicts')}>
                  Conflicts{sortIndicator(readySortKey, readySortDir, 'conflicts')}
                </button>
              </th>
              <th class="th-actions">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#each sortedReady as candidate (candidate.agent_id + candidate.branch)}
              <tr>
                <td class="cell-mono cell-agent" title={candidate.agent_id}>{candidate.agent_id}</td>
                <td class="cell-mono cell-branch" title={candidate.branch}>{candidate.branch}</td>
                <td class="cell-mono cell-ns" title={candidate.namespace || ''}>{candidate.namespace || '—'}</td>
                <td class="cell-num">{candidate.task_count}</td>
                <td class="cell-num">
                  {#if candidate.conflict_files > 0}
                    <span class="conflict-badge">{candidate.conflict_files}</span>
                  {:else}
                    —
                  {/if}
                </td>
                <td class="cell-actions">
                  {#if candidate.branch_url}
                    <a class="action-link" href={candidate.branch_url} target="_blank" rel="noopener noreferrer" title={`View ${candidate.branch} on the forge`}>
                      Branch ↗
                    </a>
                  {/if}
                  {#if candidate.merge_request_new_url}
                    <a class="action-link action-link-primary" href={candidate.merge_request_new_url} target="_blank" rel="noopener noreferrer" title={`Open a new merge request for ${candidate.branch}`}>
                      New MR ↗
                    </a>
                  {/if}
                  {#if !candidate.branch_url && !candidate.merge_request_new_url}
                    <span class="cell-empty">—</span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}

    {#if blocked.length > 0}
      <div class="subtable-label">Blocked</div>
      <div class="table-wrap">
        <table class="merge-table">
          <thead>
            <tr>
              <th>
                <button type="button" class="sort-th" aria-sort={blockedSortKey === 'agent_id' ? (blockedSortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortBlocked('agent_id')}>
                  Agent{sortIndicator(blockedSortKey, blockedSortDir, 'agent_id')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th" aria-sort={blockedSortKey === 'branch' ? (blockedSortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortBlocked('branch')}>
                  Branch{sortIndicator(blockedSortKey, blockedSortDir, 'branch')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th" aria-sort={blockedSortKey === 'blockers' ? (blockedSortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortBlocked('blockers')}>
                  Blockers{sortIndicator(blockedSortKey, blockedSortDir, 'blockers')}
                </button>
              </th>
              <th>
                <button type="button" class="sort-th sort-th-num" aria-sort={blockedSortKey === 'blocked_tasks' ? (blockedSortDir === 'asc' ? 'ascending' : 'descending') : 'none'} onclick={() => sortBlocked('blocked_tasks')}>
                  Blocked Tasks{sortIndicator(blockedSortKey, blockedSortDir, 'blocked_tasks')}
                </button>
              </th>
              <th class="th-actions">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#each sortedBlocked as candidate (candidate.agent_id + candidate.branch)}
              <tr>
                <td class="cell-mono cell-agent" title={candidate.agent_id}>{candidate.agent_id}</td>
                <td class="cell-mono cell-branch" title={candidate.branch}>{candidate.branch}</td>
                <td>
                  {#if candidate.merge_blockers?.length}
                    {#each candidate.merge_blockers as blocker}
                      <span class="blocker-badge">{blocker}</span>
                    {/each}
                  {:else}
                    —
                  {/if}
                </td>
                <td class="cell-num">{candidate.blocked_tasks}</td>
                <td class="cell-actions">
                  {#if candidate.branch_url}
                    <a class="action-link" href={candidate.branch_url} target="_blank" rel="noopener noreferrer" title={`View ${candidate.branch} on the forge`}>
                      Branch ↗
                    </a>
                  {:else}
                    <span class="cell-empty">—</span>
                  {/if}
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}

    {#if ready.length === 0 && blocked.length === 0}
      <EmptyState
        icon={'✓'}
        heading="No branches in merge queue"
        description="All branches are either merged or not yet ready for merge evaluation."
        compact
      />
    {/if}
  {/if}
</section>

<style>
  .merge-metrics {
    display: flex;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
  }

  .subtable-label {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    margin: var(--space-2) 0 var(--space-1) 0;
  }

  .table-wrap {
    overflow-x: auto;
  }

  .merge-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-sm);
  }

  .merge-table th {
    text-align: left;
    padding: 0;
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }

  .sort-th {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    width: 100%;
    padding: var(--space-1) var(--space-2);
    background: none;
    border: none;
    font: inherit;
    color: inherit;
    text-transform: inherit;
    letter-spacing: inherit;
    text-align: left;
    cursor: pointer;
    transition: color var(--transition-fast);
  }

  .sort-th:hover {
    color: var(--fg-secondary);
  }

  .sort-th[aria-sort='ascending'],
  .sort-th[aria-sort='descending'] {
    color: var(--fg-primary);
  }

  .sort-th-num {
    justify-content: center;
    text-align: center;
  }

  .merge-table td {
    padding: var(--space-1) var(--space-2);
    border-bottom: 1px solid var(--border);
    color: var(--fg-secondary);
    vertical-align: middle;
  }

  .merge-table tr:hover {
    background: var(--bg-tertiary);
  }

  .cell-mono {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .cell-agent {
    max-width: 240px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell-branch {
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell-ns {
    max-width: 160px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .cell-num {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    text-align: center;
  }

  .conflict-badge {
    display: inline-block;
    font-size: 9px;
    padding: 1px 4px;
    border-radius: var(--radius-sm);
    background: rgba(var(--error-rgb, 255, 85, 85), 0.15);
    color: var(--error);
  }

  .th-actions {
    padding: var(--space-1) var(--space-2);
    text-align: right;
  }

  .cell-actions {
    text-align: right;
    white-space: nowrap;
  }

  .cell-empty {
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .action-link {
    display: inline-block;
    font-size: 10px;
    font-weight: 600;
    padding: 1px 6px;
    margin-left: 4px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: transparent;
    color: var(--fg-secondary);
    text-decoration: none;
    transition: background var(--transition-fast), border-color var(--transition-fast), color var(--transition-fast);
  }

  .action-link:hover {
    background: var(--bg-tertiary);
    color: var(--fg-primary);
    border-color: var(--fg-muted);
  }

  .action-link-primary {
    color: var(--accent);
    border-color: rgba(var(--accent-rgb, 100, 160, 255), 0.4);
  }

  .action-link-primary:hover {
    background: rgba(var(--accent-rgb, 100, 160, 255), 0.15);
    color: var(--accent);
    border-color: var(--accent);
  }

  .blocker-badge {
    display: inline-block;
    font-size: 9px;
    padding: 1px 4px;
    border-radius: var(--radius-sm);
    background: rgba(var(--warning-rgb, 255, 170, 51), 0.15);
    color: var(--warning);
    margin-right: 4px;
    margin-bottom: 2px;
  }

  .section-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    background: none;
    border: none;
    cursor: pointer;
    padding: 0;
    color: inherit;
  }

  .toggle-icon {
    font-size: 10px;
    color: var(--fg-muted);
    width: 12px;
  }

  .section-toggle .section-title {
    margin: 0;
  }

  .section-count {
    font-family: var(--font-mono);
    font-size: 10px;
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    padding: 0 5px;
    border-radius: var(--radius-lg);
  }
</style>
