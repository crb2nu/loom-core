<script lang="ts">
  /**
   * SessionsPanel — the unified session browser: every Claude Code and
   * Codex conversation, from this workstation AND federated Mac hosts,
   * in one repo-grouped, title-first list joined against the live fleet.
   *
   * Design (from the activity-feed + agent-observability research pass):
   *   - ONE list with vendor/host badges, not per-source tabs
   *   - title-first rows (the first prompt / conversation summary),
   *     metadata demoted to chips
   *   - background noise (codex subagent/automation threads, claude
   *     sidechains) aggregated per repo — the "N background runs" pattern
   *   - LIVE badges only from EXACT linkage (POSIX cksum of uuid/cwd
   *     reproduces the hook-minted agent-id hashes; utils/sessionsUnify.ts)
   *   - transcript search returns readable speech, grouped per session
   */
  import { fleetStore } from '../stores/fleet.svelte.ts';
  import { vendorSessionsStore } from '../stores/vendorSessions.svelte.ts';
  import { searchVendorSessions, type VendorSessionMatch } from '../clients/vendorSessions.ts';
  import {
    groupSessions,
    linkLiveAgents,
    repoFromCwd,
    sessionTitle,
    type RepoGroup,
    type SessionRow,
  } from '../utils/sessionsUnify.ts';
  import { formatBytes, relativeTime } from '../utils/format.ts';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';

  const fleetPollingOwner = Symbol('SessionsPanel');

  $effect(() => {
    vendorSessionsStore.startPolling();
    // Fleet data powers the LIVE linkage; 60s watchdog cadence, SSE covers
    // the fast path (same policy as PresencePanel).
    fleetStore.startPolling(60000, fleetPollingOwner);
    return () => {
      vendorSessionsStore.stopPolling();
      fleetStore.stopPolling(fleetPollingOwner);
    };
  });

  // ---- Filters ------------------------------------------------------------

  let vendorFilter = $state<'' | 'claude' | 'codex'>('');
  /** Expanded background sections, keyed by group repo. */
  let expandedBackground = $state<Record<string, boolean>>({});

  // ---- Unified list -------------------------------------------------------

  let filteredSessions = $derived(
    vendorFilter
      ? vendorSessionsStore.sessions.filter((s) => s.vendor === vendorFilter)
      : vendorSessionsStore.sessions,
  );
  let linkage = $derived(linkLiveAgents(fleetStore.unifiedAgents, vendorSessionsStore.sessions));
  let groups = $derived(groupSessions(filteredSessions, linkage));
  let liveCount = $derived(linkage.liveKeys.size);
  let hostCount = $derived(new Set(filteredSessions.map((s) => s.host ?? 'local')).size);

  // ---- Search -------------------------------------------------------------

  let query = $state('');
  let submitted = $state('');
  let searching = $state(false);
  let searchError = $state<string | null>(null);
  let matches = $state<VendorSessionMatch[]>([]);

  let seq = 0;
  async function runSearch(): Promise<void> {
    const q = query.trim();
    submitted = q;
    if (!q) {
      matches = [];
      return;
    }
    const mySeq = ++seq;
    searching = true;
    searchError = null;
    try {
      const res = await searchVendorSessions(q, {
        vendor: vendorFilter || undefined,
        maxResults: 40,
        maxPerSession: 4,
      });
      if (mySeq !== seq) return;
      matches = res.matches;
    } catch (e) {
      if (mySeq !== seq) return;
      searchError = e instanceof Error ? e.message : String(e);
    } finally {
      if (mySeq === seq) searching = false;
    }
  }

  function clearSearch(): void {
    query = '';
    submitted = '';
    matches = [];
    searchError = null;
  }

  /** Matches grouped per session, preserving result order. */
  let matchGroups = $derived.by(() => {
    const out: Array<{ key: string; head: VendorSessionMatch; hits: VendorSessionMatch[] }> = [];
    const index = new Map<string, number>();
    for (const m of matches) {
      const key = `${m.vendor}:${m.session_id}`;
      const at = index.get(key);
      if (at === undefined) {
        index.set(key, out.length);
        out.push({ key, head: m, hits: [m] });
      } else {
        out[at].hits.push(m);
      }
    }
    return out;
  });

  /** Known transcript titles for search-result headers. */
  let titleByKey = $derived.by(() => {
    const map = new Map<string, string>();
    for (const s of vendorSessionsStore.sessions) {
      if (s.title) map.set(`${s.vendor}:${s.id}`, s.title);
    }
    return map;
  });

  function rowTime(row: SessionRow): string {
    return relativeTime(row.session.modified_at || row.session.started_at || null);
  }

  function backgroundSummary(group: RepoGroup): string {
    const kinds = new Map<string, number>();
    for (const row of group.background) {
      const kind = row.session.kind ?? 'background';
      kinds.set(kind, (kinds.get(kind) ?? 0) + 1);
    }
    return [...kinds.entries()].map(([kind, n]) => `${n} ${kind}`).join(' · ');
  }
</script>

<div class="panel sessions-panel">
  <PanelHeader title="Sessions" icon="🧵" count={filteredSessions.length}>
    {#snippet stats()}
      <span class="hdr-stat" title="Transcripts linked to a live fleet agent">
        <span class="live-dot" data-on={liveCount > 0}></span>{liveCount} live
      </span>
      <span class="hdr-stat">{hostCount} host{hostCount === 1 ? '' : 's'}</span>
      {#if vendorSessionsStore.degraded}
        <span class="hdr-stat degraded">bridge offline</span>
      {/if}
    {/snippet}
    {#snippet actions()}
      <div class="vendor-filter" role="group" aria-label="Vendor filter">
        {#each [['', 'All'], ['claude', 'Claude'], ['codex', 'Codex']] as [value, label] (value)}
          <button
            class="chip"
            data-active={vendorFilter === value}
            data-vendor={value}
            onclick={() => (vendorFilter = value as typeof vendorFilter)}
          >
            {label}
          </button>
        {/each}
      </div>
    {/snippet}
  </PanelHeader>

  {#if vendorSessionsStore.error}
    <ErrorBanner message={vendorSessionsStore.error} />
  {/if}

  <form
    class="search-row"
    onsubmit={(e) => {
      e.preventDefault();
      void runSearch();
    }}
  >
    <input
      class="search-input"
      type="search"
      placeholder="Search every transcript — what was said, across both vendors and all hosts…"
      bind:value={query}
      aria-label="Search vendor transcripts"
    />
    {#if submitted}
      <button class="search-btn" type="button" onclick={clearSearch}>Clear</button>
    {:else}
      <button class="search-btn" type="submit">Search</button>
    {/if}
  </form>

  {#if submitted}
    <!-- Search results: matches grouped per session, readable snippets. -->
    {#if searchError}
      <ErrorBanner message={searchError} />
    {:else if searching}
      <div class="empty">Searching transcripts…</div>
    {:else if matchGroups.length === 0}
      <div class="empty">No transcript said “{submitted}”.</div>
    {:else}
      <div class="groups">
        {#each matchGroups as g (g.key)}
          <section class="repo-group">
            <header class="group-head">
              <span class="vendor-badge" data-vendor={g.head.vendor}>{g.head.vendor}</span>
              <span class="group-title">{titleByKey.get(g.key) ?? repoFromCwd(g.head.cwd).repo}</span>
              <span class="group-meta">{repoFromCwd(g.head.cwd).repo}</span>
              {#if g.head.host}<span class="host-chip">{g.head.host}</span>{/if}
              <span class="group-meta">{g.hits.length} hit{g.hits.length === 1 ? '' : 's'}</span>
            </header>
            <ul class="rows">
              {#each g.hits as m (m.line + ':' + m.snippet.slice(0, 24))}
                <li class="match-row">
                  {#if m.role}<span class="role-chip" data-role={m.role}>{m.role}</span>{/if}
                  <span class="snippet">{m.snippet}</span>
                  <span class="row-meta">
                    {#if m.timestamp}{relativeTime(m.timestamp)}{/if}
                    {#if m.line > 0}<span class="line-no">L{m.line}</span>{/if}
                  </span>
                </li>
              {/each}
            </ul>
          </section>
        {/each}
      </div>
    {/if}
  {:else if vendorSessionsStore.loading && filteredSessions.length === 0}
    <div class="empty">Loading transcripts…</div>
  {:else if vendorSessionsStore.degraded}
    <div class="empty">
      No transcript source available — the agent bridge is offline and no Mac has
      mirrored transcripts recently.
    </div>
  {:else if groups.length === 0}
    <div class="empty">No vendor CLI sessions found.</div>
  {:else}
    <div class="groups">
      {#each groups as group (group.repo)}
        <section class="repo-group">
          <header class="group-head">
            <span class="group-title">{group.repo}</span>
            {#each group.hosts as host (host)}
              <span class="host-chip">{host}</span>
            {/each}
            <span class="group-meta">
              {group.rows.length} session{group.rows.length === 1 ? '' : 's'}
              · {relativeTime(group.lastActivity)}
            </span>
          </header>

          <ul class="rows">
            {#each group.rows as row (row.key)}
              <li class="session-row" data-live={row.live} title={row.session.path}>
                <span class="vendor-badge" data-vendor={row.session.vendor}>
                  {row.session.vendor}
                </span>
                <span class="row-title">
                  {#if row.live}<span class="live-dot" data-on="true" title="Linked to a live fleet agent"></span>{/if}
                  {sessionTitle(row.session)}
                </span>
                {#if row.worktree}<span class="wt-chip" title="Linked worktree">{row.worktree}</span>{/if}
                {#if row.session.host}<span class="host-chip">{row.session.host}</span>{/if}
                <span class="row-meta">
                  {rowTime(row)} · {formatBytes(row.session.size_bytes)}
                </span>
              </li>
            {/each}
          </ul>

          {#if group.background.length > 0}
            <button
              class="bg-toggle"
              aria-expanded={expandedBackground[group.repo] ?? false}
              onclick={() =>
                (expandedBackground[group.repo] = !(expandedBackground[group.repo] ?? false))}
            >
              <span class="bg-chevron" data-open={expandedBackground[group.repo] ?? false}>▸</span>
              {group.background.length} background run{group.background.length === 1 ? '' : 's'}
              <span class="bg-detail">({backgroundSummary(group)})</span>
            </button>
            {#if expandedBackground[group.repo]}
              <ul class="rows bg-rows">
                {#each group.background as row (row.key)}
                  <li class="session-row bg-row" title={row.session.path}>
                    <span class="vendor-badge" data-vendor={row.session.vendor}>
                      {row.session.vendor}
                    </span>
                    <span class="kind-chip" data-kind={row.session.kind}>{row.session.kind}</span>
                    <span class="row-title">{sessionTitle(row.session)}</span>
                    {#if row.session.host}<span class="host-chip">{row.session.host}</span>{/if}
                    <span class="row-meta">{rowTime(row)}</span>
                  </li>
                {/each}
              </ul>
            {/if}
          {/if}
        </section>
      {/each}
    </div>
  {/if}
</div>

<style>
  .sessions-panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    min-height: 0;
  }

  .hdr-stat {
    display: inline-flex;
    align-items: center;
    gap: 5px;
    font-size: var(--text-xs);
    color: var(--fg-muted);
  }
  .hdr-stat.degraded { color: var(--warning); }

  .vendor-filter {
    display: flex;
    gap: var(--space-1);
  }

  .chip {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    padding: 2px 10px;
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    background: none;
    cursor: pointer;
  }
  .chip:hover { color: var(--fg-primary); }
  .chip[data-active='true'] {
    color: var(--fg-primary);
    border-color: var(--border-focus);
    background: rgba(255, 255, 255, 0.04);
  }
  .chip[data-active='true'][data-vendor='claude'] { color: var(--warning); }
  .chip[data-active='true'][data-vendor='codex'] { color: var(--info); }

  .search-row {
    display: flex;
    gap: var(--space-2);
  }

  .search-input {
    flex: 1;
    min-width: 0;
    font-size: var(--text-sm);
    color: var(--fg-primary);
    background: rgba(255, 255, 255, 0.03);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 6px 10px;
  }
  .search-input:focus { outline: none; border-color: var(--border-focus); }

  .search-btn {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    padding: 6px 14px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: rgba(255, 255, 255, 0.02);
    cursor: pointer;
    flex-shrink: 0;
  }
  .search-btn:hover { color: var(--fg-primary); border-color: var(--border-focus); }

  .groups {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    overflow-y: auto;
    min-height: 0;
    scrollbar-width: thin;
  }

  .repo-group {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    padding: var(--space-2) var(--space-3);
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .group-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    min-width: 0;
  }

  .group-title {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .group-meta {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    flex-shrink: 0;
    margin-left: auto;
  }
  .group-head .group-meta ~ .group-meta { margin-left: 0; }

  .rows {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
  }

  .session-row,
  .match-row {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: 4px var(--space-1);
    border-radius: var(--radius-xs);
    min-width: 0;
  }
  .session-row:hover { background: rgba(255, 255, 255, 0.03); }
  .session-row[data-live='true'] .row-title { color: var(--fg-primary); }

  .vendor-badge {
    font-size: 9px;
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    flex-shrink: 0;
    min-width: 6ch;
  }
  .vendor-badge[data-vendor='claude'] { color: var(--warning); }
  .vendor-badge[data-vendor='codex'] { color: var(--info); }

  .row-title {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    display: inline-flex;
    align-items: baseline;
    gap: 6px;
  }

  .live-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--fg-dim);
    flex-shrink: 0;
    align-self: center;
  }
  .live-dot[data-on='true'] {
    background: var(--success);
    box-shadow: 0 0 5px color-mix(in srgb, var(--success) 60%, transparent);
  }

  .wt-chip,
  .host-chip,
  .kind-chip,
  .role-chip {
    font-size: 9px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    padding: 0 6px;
    flex-shrink: 0;
    max-width: 18ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .kind-chip[data-kind='automation'] { color: var(--info); }
  .kind-chip[data-kind='sidechain'] { color: var(--fg-dim); }
  .role-chip[data-role='user'] { color: var(--warning); }
  .role-chip[data-role='assistant'] { color: var(--info); }

  .row-meta {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
    flex-shrink: 0;
  }

  .line-no { margin-left: 6px; }

  .snippet {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    flex: 1;
    min-width: 0;
    overflow-wrap: anywhere;
  }

  .bg-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    font-size: var(--text-xs);
    color: var(--fg-dim);
    padding: 2px var(--space-1);
    background: none;
    border: none;
    cursor: pointer;
    text-align: left;
  }
  .bg-toggle:hover { color: var(--fg-secondary); }

  .bg-chevron {
    display: inline-block;
    transition: transform var(--transition-fast);
  }
  .bg-chevron[data-open='true'] { transform: rotate(90deg); }

  .bg-detail { color: var(--fg-dim); }

  .bg-rows { opacity: 0.75; }

  .empty {
    font-size: var(--text-sm);
    color: var(--fg-dim);
    padding: var(--space-4) 0;
    text-align: center;
  }
</style>
