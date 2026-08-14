<script lang="ts">
  /**
   * VendorTranscripts — collapsible "Vendor transcripts" affordance for the
   * InspectDock's agent lens. Lists and greps the on-disk session transcripts
   * of the vendor CLIs (Claude Code + Codex) on the workstation via
   * /api/vendor-sessions[/search] — the operator's window into what the
   * vendor desktop apps did, which presence/fleet views cannot see.
   *
   * Fetch-on-open drill-down, not a poller: transcripts change slowly and the
   * deck already polls half a dozen stores. `cwdHint` (the selected agent's
   * project) seeds a toggleable cwd_contains filter so the default view is
   * "transcripts near this agent's repo", one tap from "everything".
   */
  import {
    fetchVendorSessions,
    searchVendorSessions,
    type VendorSession,
    type VendorSessionMatch,
  } from '../../clients/vendorSessions.ts';
  import { relativeTime } from '../../utils/format.ts';

  let { cwdHint = '' }: { cwdHint?: string } = $props();

  let open = $state(false);
  let query = $state('');
  /** Last executed search; '' renders the recent-sessions list instead. */
  let submitted = $state('');
  let filterToProject = $state(true);
  let loading = $state(false);
  let degraded = $state(false);
  let error = $state<string | null>(null);
  let sessions = $state<VendorSession[]>([]);
  let matches = $state<VendorSessionMatch[]>([]);

  let effectiveCwd = $derived(filterToProject && cwdHint ? cwdHint : '');

  // Reload whenever the section is open and the executed query / cwd filter
  // change (including a new agent selection swapping cwdHint).
  $effect(() => {
    if (!open) return;
    void load(submitted, effectiveCwd);
  });

  // Monotonic sequence guards against a slow response landing after a newer
  // request (agent switch, filter toggle) already refreshed the state.
  let seq = 0;
  async function load(q: string, cwd: string): Promise<void> {
    const mySeq = ++seq;
    loading = true;
    error = null;
    try {
      if (q) {
        const res = await searchVendorSessions(q, {
          cwdContains: cwd || undefined,
          maxResults: 20,
        });
        if (mySeq !== seq) return;
        matches = res.matches;
        degraded = res.degraded;
      } else {
        const res = await fetchVendorSessions({ cwdContains: cwd || undefined, limit: 8 });
        if (mySeq !== seq) return;
        sessions = res.sessions;
        degraded = res.degraded;
      }
    } catch (e) {
      if (mySeq !== seq) return;
      error = e instanceof Error ? e.message : String(e);
    } finally {
      if (mySeq === seq) loading = false;
    }
  }

  function submitSearch(): void {
    submitted = query.trim();
  }

  function clearSearch(): void {
    query = '';
    submitted = '';
  }

  /** Last two path segments — enough to recognize a repo without the noise. */
  function cwdTail(cwd?: string): string {
    if (!cwd) return '';
    return cwd.split('/').filter(Boolean).slice(-2).join('/');
  }
</script>

<section class="vt">
  <button class="vt-toggle" onclick={() => (open = !open)} aria-expanded={open}>
    <span class="vt-chevron" data-open={open}>▸</span>
    Vendor transcripts
  </button>

  {#if open}
    <div class="vt-body">
      <form
        class="vt-search"
        onsubmit={(e) => {
          e.preventDefault();
          submitSearch();
        }}
      >
        <input
          class="vt-input"
          type="search"
          placeholder="Search claude + codex transcripts…"
          bind:value={query}
          aria-label="Search vendor transcripts"
        />
        {#if submitted}
          <button class="vt-btn" type="button" onclick={clearSearch}>Clear</button>
        {:else}
          <button class="vt-btn" type="submit">Search</button>
        {/if}
      </form>

      {#if cwdHint}
        <button
          class="vt-chip"
          data-active={filterToProject}
          onclick={() => (filterToProject = !filterToProject)}
          title="Toggle filtering transcripts to this agent's project directory"
        >
          {filterToProject ? `cwd ~ ${cwdHint} ✕` : 'filter to project'}
        </button>
      {/if}

      {#if degraded}
        <div class="vt-empty">
          Vendor transcripts unavailable — agent bridge offline (the agent-context
          server must run on this workstation).
        </div>
      {:else if error}
        <div class="vt-error">{error}</div>
      {:else if loading}
        <div class="vt-empty">Loading…</div>
      {:else if submitted}
        {#if matches.length === 0}
          <div class="vt-empty">No matches for “{submitted}”.</div>
        {:else}
          <ul class="vt-list">
            {#each matches as m (m.session_id + ':' + m.line)}
              <li class="vt-match">
                <div class="vt-row-head">
                  <span class="vt-vendor" data-vendor={m.vendor}>{m.vendor}</span>
                  {#if m.host}<span class="vt-host">{m.host}</span>{/if}
                  <span class="vt-cwd">{cwdTail(m.cwd)}</span>
                  <span class="vt-meta">
                    {#if m.timestamp}{relativeTime(m.timestamp)}{/if}
                    {#if m.timestamp && m.line > 0} · {/if}
                    {#if m.line > 0}L{m.line}{/if}
                  </span>
                </div>
                <div class="vt-snippet">{m.snippet}</div>
              </li>
            {/each}
          </ul>
        {/if}
      {:else if sessions.length === 0}
        <div class="vt-empty">No vendor CLI sessions found on this host.</div>
      {:else}
        <ul class="vt-list">
          {#each sessions as s (s.vendor + ':' + s.id)}
            <li class="vt-session" title={s.path}>
              <span class="vt-vendor" data-vendor={s.vendor}>{s.vendor}</span>
              {#if s.kind}<span class="vt-kind">{s.kind}</span>{/if}
              {#if s.host}<span class="vt-host">{s.host}</span>{/if}
              <span class="vt-cwd" title={s.title || s.cwd}>
                {s.title || cwdTail(s.cwd) || s.id.slice(0, 12)}
              </span>
              <span class="vt-meta">{relativeTime(s.modified_at)}</span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/if}
</section>

<style>
  .vt {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    margin-top: var(--space-2);
  }

  .vt-toggle {
    display: flex;
    align-items: center;
    gap: var(--space-1);
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    padding: 0;
    background: none;
    border: none;
    cursor: pointer;
    text-align: left;
  }
  .vt-toggle:hover { color: var(--fg-primary); }

  .vt-chevron {
    display: inline-block;
    transition: transform var(--transition-fast);
  }
  .vt-chevron[data-open='true'] { transform: rotate(90deg); }

  .vt-body {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .vt-search {
    display: flex;
    gap: var(--space-1);
  }

  .vt-input {
    flex: 1;
    min-width: 0;
    font-size: var(--text-xs);
    color: var(--fg-primary);
    background: rgba(255, 255, 255, 0.03);
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: 4px 8px;
  }
  .vt-input:focus { outline: none; border-color: var(--border-focus); }

  .vt-btn {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    padding: 4px 8px;
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    background: rgba(255, 255, 255, 0.02);
    cursor: pointer;
    flex-shrink: 0;
  }
  .vt-btn:hover { color: var(--fg-primary); border-color: var(--border-focus); }

  .vt-chip {
    align-self: flex-start;
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    padding: 2px 8px;
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    background: none;
    cursor: pointer;
    max-width: 100%;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .vt-chip[data-active='true'] {
    color: var(--info);
    border-color: color-mix(in srgb, var(--info) 40%, transparent);
  }
  .vt-chip:hover { color: var(--fg-primary); }

  .vt-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 2px;
    max-height: 220px;
    overflow-y: auto;
    scrollbar-width: thin;
  }

  .vt-session {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    font-size: var(--text-xs);
    padding: 3px var(--space-1);
    border-radius: var(--radius-xs);
  }

  .vt-match {
    display: flex;
    flex-direction: column;
    gap: 1px;
    padding: 4px var(--space-1);
    border-bottom: 1px solid var(--border-subtle);
  }
  .vt-match:last-child { border-bottom: none; }

  .vt-row-head {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .vt-vendor {
    font-size: 9px;
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    flex-shrink: 0;
  }
  .vt-vendor[data-vendor='claude'] { color: var(--warning); }
  .vt-vendor[data-vendor='codex'] { color: var(--info); }

  /* Background-run tag (codex subagent/automation, claude sidechain). */
  .vt-kind {
    font-size: 9px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    padding: 0 6px;
    flex-shrink: 0;
  }

  /* Source workstation for federated rows (mirror pushes). */
  .vt-host {
    font-size: 9px;
    font-family: var(--font-mono);
    color: var(--fg-muted);
    border: 1px solid var(--border-subtle);
    border-radius: 999px;
    padding: 0 6px;
    flex-shrink: 0;
    max-width: 12ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .vt-cwd {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--fg-secondary);
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .vt-meta {
    font-size: 10px;
    font-family: var(--font-mono);
    color: var(--fg-dim);
    flex-shrink: 0;
  }

  .vt-snippet {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow-wrap: anywhere;
    display: -webkit-box;
    -webkit-line-clamp: 3;
    line-clamp: 3;
    -webkit-box-orient: vertical;
    overflow: hidden;
  }

  .vt-empty {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    padding: var(--space-1) 0;
  }

  .vt-error {
    font-size: var(--text-xs);
    color: var(--error);
  }
</style>
