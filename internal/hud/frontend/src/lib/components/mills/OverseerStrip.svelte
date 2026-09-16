<script lang="ts">
  /**
   * OverseerStrip — the mill staff actually on the floor: groomer,
   * sentinel, foreman. Each chip is the agent's last tick (the only
   * heartbeat Mills has), what that tick did (inspected / acted / planned),
   * and why it is quiet when it is (paused, suppressed, errored, silent).
   */
  import type { OverseerRow } from '../../utils/shuttleBoardHelpers.ts';
  import { overseerReadiness } from '../../utils/shuttleBoardHelpers.ts';
  import { millsOverseersStore } from '../../stores/mills_overseers.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import { fmtDuration } from './shared/format.ts';

  let {
    rows,
    disabled = false,
    error = null,
    loading = false,
  }: { rows: OverseerRow[]; disabled?: boolean; error?: string | null; loading?: boolean } = $props();
  let readiness = $derived(new Map((millsOverseersStore.status?.agents ?? []).map((agent) => [
    agent.name, overseerReadiness(agent, millsOverseersStore.report, millsOverseersStore.status?.soak,
      millsOverseersStore.status?.recent_actions[agent.name]),
  ])));
</script>

<section class="overseers" aria-label="Overseers on the floor">
  <header class="ov-head">
    <span class="ov-tag">overseers</span>
    <span class="ov-hint">mill staff · last tick · what it did</span>
  </header>

  {#if disabled}
    <p class="ov-state" role="status">overseers not configured on this operator</p>
  {:else if error && rows.length === 0}
    <p class="ov-state ov-warn" role="status">overseer feed unavailable — {error}</p>
  {:else if rows.length === 0}
    <p class="ov-state" role="status">{loading ? 'reading the staff roster…' : 'no overseers registered'}</p>
  {:else}
    {#if error}<p class="ov-state ov-warn" role="status">Roster may be stale — {error}</p>{/if}
    {#if millsOverseersStore.reportError}
      <p class="ov-state ov-warn" role="status">{millsOverseersStore.report ? 'Cached evidence; report refresh failed' : 'Evidence unavailable'} — {millsOverseersStore.reportError}</p>
    {/if}
    <p class="ov-state">168h evidence · {millsOverseersStore.reportUpdated ? `updated ${millsOverseersStore.reportUpdated.toISOString()}` : millsOverseersStore.reportLoading ? 'loading…' : 'not loaded'}</p>
    <ul class="ov-rows">
      {#each rows as r (r.name)}
        {@const evidence = readiness.get(r.name)}
        <li>
          <details>
            <summary class="ov-row ov-{r.state}">
              <span class="ov-name">{r.name}</span>
              <Badge text={r.state} variant={r.tone} />
              {#if r.dryRun}
                <span class="ov-dry" title="dry run — observes, never acts">dry</span>
              {/if}
              <span class="ov-dry">{evidence?.label ?? 'no evidence'}</span>
              <span class="ov-spacer"></span>
              <span class="ov-tick mono" title="last tick">
                {r.tickAgeMs != null ? `${fmtDuration(r.tickAgeMs)} ago` : 'no tick yet'}
              </span>
              <span class="ov-result mono" title="last tick: inspected · acted · planned · errored">
                {r.inspected}<span class="k">i</span>
                {r.acted}<span class="k">a</span>
                {r.planned}<span class="k">p</span>
                {#if r.errored > 0}<span class="bad">{r.errored}<span class="k">e</span></span>{/if}
              </span>
              {#if r.note}
                <span class="ov-note" title={r.note}>{r.note}</span>
              {/if}
            </summary>
            <div class="ov-evidence">
              <p>Mode: {evidence?.mode ?? (r.dryRun ? 'dry' : 'live')} · Evidence days show observation span, not certified green days.</p>
              {#each evidence?.reasons ?? ['Promotion report unavailable; evidence is unknown.'] as reason}<p>{reason}</p>{/each}
              {#each evidence?.actions ?? [] as action (action.action)}
                <article>
                  <strong>{action.action} · {action.label}</strong>
                  <p class="mono">{action.dry} dry · {action.executed} executed · {action.subjects} subjects · {action.evidenceDays}/7 evidence days</p>
                  {#each action.reasons as reason}<p>{reason}</p>{/each}
                  {#if action.sample}
                    <p>{action.sampleNewest ? 'Newest subject' : 'Subject sample (recency unknown)'}:
                      {#if action.sample.startsWith('backlog_item/') && action.sample.slice(13)}
                        <a class="ov-link" href={`#mills/warps/${encodeURIComponent(action.sample.slice(13))}`}>{action.sample}</a>
                      {:else}{action.sample}{/if}
                    </p>
                  {/if}
                </article>
              {/each}
            </div>
          </details>
        </li>
      {/each}
    </ul>
    <p class="ov-state">no evidence: no reviewable rows · soaking (N/7d): incomplete or unmet conditions · review: human approval pending per action class · promoted: live execution observed</p>
  {/if}
</section>

<style>
  .overseers {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    overflow: hidden;
    min-width: 0;
  }
  .ov-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-bottom: 1px solid var(--border);
    background: var(--bg-primary);
  }
  .ov-tag {
    font-size: var(--text-2xs);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
    color: var(--accent);
  }
  .ov-hint { font-size: var(--text-2xs); color: var(--fg-dim); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .ov-state { margin: 0; padding: var(--space-3); color: var(--fg-muted); font-size: var(--text-xs); }
  .ov-warn { color: var(--warning); }

  .ov-rows { list-style: none; margin: 0; padding: 0; }
  .ov-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 5px var(--space-3);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
    min-width: 0;
  }
  summary { cursor: pointer; flex-wrap: wrap; }
  summary::before { content: '▸'; }
  details[open] > summary::before { content: '▾'; }
  summary:focus-visible, .ov-link:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
  .ov-evidence { padding: var(--space-3); font-size: var(--text-xs); color: var(--fg-secondary); overflow-wrap: anywhere; }
  .ov-evidence p { margin: var(--space-2) 0; }
  .ov-evidence article { border-top: 1px solid var(--border-subtle); padding-top: var(--space-2); }
  .ov-link { color: var(--accent); text-decoration: underline; cursor: pointer; background: none; border: 0; font: inherit; }
  .ov-name { font-weight: 600; color: var(--fg-primary); text-transform: capitalize; }
  .ov-dry {
    font-size: var(--text-2xs);
    padding: 0 5px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
    color: var(--fg-muted);
  }
  .ov-spacer { flex: 1 1 0; }
  .mono { font-family: var(--font-mono); }
  .ov-tick { color: var(--fg-muted); white-space: nowrap; }
  .ov-silent .ov-tick, .ov-error .ov-tick { color: var(--warning); }
  .ov-result { color: var(--fg-secondary); white-space: nowrap; }
  .k { color: var(--fg-dim); font-size: var(--text-2xs); margin-right: 3px; }
  .bad { color: var(--error); }
  .ov-note {
    flex-basis: 100%;
    color: var(--fg-muted);
    font-size: var(--text-2xs);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
</style>
