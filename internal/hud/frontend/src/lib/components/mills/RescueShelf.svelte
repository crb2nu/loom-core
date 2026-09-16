<script lang="ts">
  import type { RescueRow } from '../../utils/rescueHelpers.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';
  let { rows, error = null }: { rows: RescueRow[]; error?: string | null } = $props();
  let filter = $state('');
  let copied = $state('');
  let copyError = $state('');
  let classes = $derived([...new Set(rows.map(r => r.escalationClass))].sort());
  let filtered = $derived(rows.filter(r => !filter || r.escalationClass === filter));
  async function copy(row: RescueRow) {
    if (!row.amendCommand) return;
    try {
      await navigator.clipboard.writeText(row.amendCommand);
      copied = row.id;
      copyError = '';
    } catch { copyError = 'Clipboard unavailable. Try again with clipboard access enabled.'; }
  }
</script>

{#if rows.length > 0}
  <details class="rescue-shelf">
    <summary>Rescue shelf <span class="count">{rows.length}</span></summary>
    {#if error}<p role="status">{error}</p>{/if}
    <label>Escalation class
      <select value={filter} onchange={(event) => { filter = event.currentTarget.value; }}>
        <option value="">All classes</option>
        {#each classes as value}<option {value}>{value}</option>{/each}
      </select>
    </label>
    <p>Showing {Math.min(filtered.length, 12)} of {filtered.length} · oldest first</p>
    {#each filtered.slice(0, 12) as row (row.id)}
      {@const link = mrURL(row.project, row.mrIID)}
      <details class="rescue-row">
        <summary>
          <span>{row.priority} · {row.title}</span>
          <span>{row.age === null ? 'Age unknown' : `${Math.floor(row.age / 86400000)}d old`}</span>
          <span>{row.escalationClass} / {row.failureClass}</span>
          <strong class="verdict">{row.verdict}</strong>
        </summary>
        <p class="mono">{row.id} · {row.runID ?? 'Run evidence pending'}</p>
        <p>{row.verdictText}</p>
        <p>Live branch status: unknown in HUD. MR title: {row.rescueDraft === null ? 'unknown' : row.rescueDraft ? 'Draft: [scope-escalated]' : 'not a scope rescue draft'}.</p>
        {#if link}<a href={link} target="_blank" rel="noopener noreferrer">View MR !{row.mrIID}</a>{/if}
        {#if row.violations.length > 0}
          <div class="table-scroll"><table>
            <thead><tr><th>File</th><th>Rule</th><th>Admissible</th></tr></thead>
            <tbody>{#each row.violations as v}<tr><td class="mono">{v.file}</td><td>{v.rule}</td><td>{v.admitted ? 'Yes' : 'No'}</td></tr>{/each}</tbody>
          </table></div>
        {/if}
        {#if !row.scopeKnown}<p>Scope evidence missing or malformed.</p>{:else if row.violations.length === 0}<p>No scope violations recorded.</p>{/if}
        {#if row.violations.length > 0 && !row.amendCommand}<p>Scope amendment requires complete eligible scope and plan_slice file evidence.</p>{/if}
        {#if row.amendCommand}<button onclick={() => copy(row)}>{copied === row.id ? 'Copied amend command' : 'Copy amend command'}</button>{/if}
      </details>
    {/each}
    {#if copyError}<p role="status">{copyError}</p>{/if}
  </details>
{:else if error}
  <p role="status">Rescue shelf: {error}</p>
{/if}

<style>
  .rescue-shelf { margin-top: var(--space-2); padding: var(--space-3); border: 1px solid var(--border); border-radius: var(--radius-md); background: var(--bg-secondary); font-size: var(--text-xs); }
  summary { cursor: pointer; padding: var(--space-2); }
  summary span, .verdict { margin-right: var(--space-3); }
  .count, .verdict { color: var(--warning); }
  .rescue-row { border-top: 1px solid var(--border); margin-top: var(--space-2); }
  p { color: var(--fg-muted); }
  .mono { font-family: var(--font-mono); overflow-wrap: anywhere; }
  .table-scroll { overflow-x: auto; }
  table { width: 100%; border-collapse: collapse; margin: var(--space-2) 0; }
  th, td { text-align: left; padding: var(--space-2); border-bottom: 1px solid var(--border); }
  select, button { background: var(--bg-primary); color: var(--fg-primary); border: 1px solid var(--border); padding: var(--space-2); }
  a { color: var(--accent); }
</style>
