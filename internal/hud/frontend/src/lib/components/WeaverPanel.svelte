<script lang="ts">
  import type { BadgeVariant } from '../utils/tokens.ts';
  import Badge from '../widgets/Badge.svelte';
  import EmptyState from './shared/EmptyState.svelte';
  import PanelHeader from './shared/PanelHeader.svelte';
  import ErrorBanner from './shared/ErrorBanner.svelte';
  import { createPoller } from '../utils/poller.ts';

  interface WeaverDomainObj {
    name?: string;
    description?: string;
    tools?: string[];
  }
  interface WeaverStatus {
    enabled?: boolean;
    router_model?: string;
    subagent_model?: string;
    domains?: Array<string | WeaverDomainObj>;
    degraded?: boolean;
    missing_models?: string[];
    ready_models?: string[];
    catalog_size?: number;
    catalog_error?: string;
    preflight_at?: string;
  }
  interface WeaverDomains {
    domains?: Array<string | WeaverDomainObj>;
    router_model?: string;
    subagent_model?: string;
  }
  interface WeaverHistoryEntry {
    timestamp?: string;
    query?: string;
    domains?: string[];
    latency_ms?: number;
    total_tokens?: number;
    status?: string;
  }
  interface WeaverHistory {
    entries?: WeaverHistoryEntry[];
  }
  interface WeaverMetrics {
    total_queries?: number;
    avg_latency_ms?: number;
    error_rate?: number;
    total_tokens?: number;
    error_count?: number;
  }
  interface AiModelRole {
    role?: string;
    primary?: string;
    fallbacks?: string[];
  }
  interface AiModels {
    roles?: AiModelRole[];
    override_path?: string;
  }

  let status = $state<WeaverStatus | null>(null);
  let domains = $state<WeaverDomains | null>(null);
  let history = $state<WeaverHistory | null>(null);
  let metrics = $state<WeaverMetrics | null>(null);
  let aimodels = $state<AiModels | null>(null);
  let loading = $state(true);
  let error = $state('');
  let expandedDomain = $state<string | null>(null);

  async function fetchAll(): Promise<void> {
    try {
      const [sRes, dRes, hRes, mRes, aRes] = await Promise.all([
        fetch('/api/weaver/status'),
        fetch('/api/weaver/domains'),
        fetch('/api/weaver/history'),
        fetch('/api/weaver/metrics'),
        fetch('/api/aimodels/roles'),
      ]);
      if (!sRes.ok) throw new Error(`Status: HTTP ${sRes.status}`);
      status = (await sRes.json()) as WeaverStatus;
      domains = dRes.ok ? ((await dRes.json()) as WeaverDomains) : null;
      history = hRes.ok ? ((await hRes.json()) as WeaverHistory) : null;
      metrics = mRes.ok ? ((await mRes.json()) as WeaverMetrics) : null;
      aimodels = aRes.ok ? ((await aRes.json()) as AiModels) : null;
      error = '';
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to fetch weaver data';
    } finally {
      loading = false;
    }
  }

  // Five parallel GETs per tick, so this runs on the shared poller: hidden
  // tabs pause, slow responses can't stack, and the cadence is 15s not 5s.
  $effect(() => {
    void fetchAll();
    const p = createPoller(() => fetchAll(), 15000);
    p.start();
    return () => p.stop();
  });

  // Derived
  let enabled = $derived(status?.enabled ?? false);
  let routerModel = $derived(status?.router_model ?? domains?.router_model ?? '-');
  let subagentModel = $derived(status?.subagent_model ?? domains?.subagent_model ?? '-');
  let domainList = $derived.by(() =>
    (domains?.domains ?? status?.domains ?? []).map((domain) => {
      if (typeof domain === 'string') {
        return {
          name: domain,
          description: '',
          tools: [],
        };
      }
      return {
        name: domain?.name ?? '-',
        description: domain?.description ?? '',
        tools: Array.isArray(domain?.tools) ? domain.tools : [],
      };
    })
  );
  let entries = $derived.by(() =>
    [...(history?.entries ?? [])].sort((left, right) => {
      const leftTs = new Date(left?.timestamp ?? 0).getTime();
      const rightTs = new Date(right?.timestamp ?? 0).getTime();
      return rightTs - leftTs;
    })
  );
  let totalQueries = $derived(metrics?.total_queries ?? 0);
  let avgLatency = $derived(metrics?.avg_latency_ms ?? 0);
  let errorRate = $derived(metrics?.error_rate ?? 0);
  let totalTokens = $derived(metrics?.total_tokens ?? 0);
  let errorCount = $derived(metrics?.error_count ?? 0);

  // Preflight (S4) — model catalog comparison surfaced from
  // loom/weaver/status. degraded=true when at least one configured
  // model isn't advertised by FlexInfer.
  let degraded = $derived(status?.degraded === true);
  let missingModels = $derived.by(() => {
    const m = status?.missing_models;
    return Array.isArray(m) ? m : [];
  });
  let readyModels = $derived.by(() => {
    const m = status?.ready_models;
    return Array.isArray(m) ? m : [];
  });
  let catalogSize = $derived(status?.catalog_size ?? 0);
  let catalogError = $derived(status?.catalog_error ?? '');
  let preflightAt = $derived(status?.preflight_at ?? '');

  // Defaults (S6.2) — pkg/aimodels role table.
  let roles = $derived.by(() => {
    const r = aimodels?.roles;
    return Array.isArray(r) ? r : [];
  });
  let overridePath = $derived(aimodels?.override_path ?? '');

  function toggleDomain(name: string) {
    expandedDomain = expandedDomain === name ? null : name;
  }

  // Maps weaver history/status states onto the Badge variant union. 'critical'
  // and 'neutral' (the prior return values) are not valid BadgeVariants and
  // were silently rendering as the info fallback; use error/muted instead.
  function statusVariant(s: string | undefined): BadgeVariant {
    if (s === 'ok' || s === 'success') return 'success';
    if (s === 'error') return 'error';
    return 'muted';
  }

  function fmtLatency(ms: number) {
    if (ms < 1000) return `${Math.round(ms)}ms`;
    return `${(ms / 1000).toFixed(1)}s`;
  }

  function fmtTime(ts: string | null | undefined) {
    if (!ts) return '-';
    const d = new Date(ts);
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }

  function truncate(str: string | undefined, max: number) {
    if (!str) return '-';
    return str.length > max ? str.slice(0, max) + '...' : str;
  }
</script>

<div class="weaver-panel">
  <PanelHeader title="Weaver" icon={'♥'}>
    {#snippet stats()}
      <span class="stat">
        <span class="stat-value">{totalQueries}</span>
        <span class="stat-label">queries</span>
      </span>
      <span class="stat">
        <span class="stat-value">{fmtLatency(avgLatency)}</span>
        <span class="stat-label">avg latency</span>
      </span>
      <span class="stat">
        <span class="stat-value">{domainList.length}</span>
        <span class="stat-label">domains</span>
      </span>
    {/snippet}
  </PanelHeader>

  {#if loading && !status && !metrics}
    <!-- Full-surface loading only on cold start; refreshes keep the
         last-known data on screen instead of blanking the panel. -->
    <div class="loading">Loading weaver data...</div>
  {:else}
    {#if error}
      <!-- A refresh failure used to replace every section with just this
           banner, blanking the panel on a transient 5s-poll blip. Keep the
           last-known data visible and show the error as a non-blocking banner
           above it instead. -->
      <ErrorBanner prefix="Weaver refresh failed" message={error} />
    {/if}

    <!-- Preflight degraded banner (S4) -->
    {#if degraded}
      <section class="section degraded-banner">
        <h3>Model preflight: degraded</h3>
        {#if catalogError}
          <p class="degraded-detail">
            Could not reach FlexInfer <code>/v1/models</code>:
            <code>{catalogError}</code>. Showing all configured models as missing.
          </p>
        {:else}
          <p class="degraded-detail">
            {missingModels.length} configured model{missingModels.length === 1 ? '' : 's'}
            not advertised by FlexInfer (catalog size: {catalogSize}).
            Weaver queries against {missingModels.length === 1 ? 'this model' : 'these models'} will 404.
          </p>
        {/if}
        {#if missingModels.length > 0}
          <div class="model-chips">
            {#each missingModels as m}
              <span class="model-chip missing">{m}</span>
            {/each}
          </div>
        {/if}
        {#if readyModels.length > 0}
          <p class="degraded-detail">
            Ready ({readyModels.length}):
            {#each readyModels as m, i}
              <span class="model-chip ready">{m}</span>{i < readyModels.length - 1 ? ' ' : ''}
            {/each}
          </p>
        {/if}
        {#if preflightAt}
          <p class="degraded-detail muted">Last checked: {fmtTime(preflightAt)}</p>
        {/if}
      </section>
    {/if}

    <!-- Status Card -->
    <section class="section">
      <h3>Status</h3>
      <div class="status-grid">
        <div class="status-item">
          <span class="status-label">Enabled</span>
          <Badge variant={enabled ? 'success' : 'muted'} text={enabled ? 'Active' : 'Disabled'} />
        </div>
        <div class="status-item">
          <span class="status-label">Router Model</span>
          <span class="status-value text-mono">{routerModel}</span>
        </div>
        <div class="status-item">
          <span class="status-label">Subagent Model</span>
          <span class="status-value text-mono">{subagentModel}</span>
        </div>
        <div class="status-item">
          <span class="status-label">Domains</span>
          <span class="status-value">{domainList.length}</span>
        </div>
      </div>
    </section>

    <!-- Defaults (pkg/aimodels role resolver, S6.2) -->
    {#if roles.length > 0}
      <section class="section">
        <h3>Role defaults</h3>
        <p class="muted">
          Resolved by <code>pkg/aimodels</code>. Override at
          <code>{overridePath || '~/.config/loom/aimodel-roles.yaml'}</code>.
        </p>
        <div class="roles-table">
          {#each roles as r}
            <div class="role-row">
              <span class="role-name text-mono">{r.role}</span>
              <span class="role-primary text-mono">{r.primary || '—'}</span>
              {#if r.fallbacks && r.fallbacks.length > 0}
                <span class="role-fallbacks">
                  fallbacks:
                  {#each r.fallbacks as f, i}<code>{f}</code>{i < r.fallbacks.length - 1 ? ', ' : ''}{/each}
                </span>
              {/if}
            </div>
          {/each}
        </div>
      </section>
    {/if}

    <!-- Metrics Summary -->
    <section class="section">
      <h3>Metrics</h3>
      <div class="metrics-grid">
        <div class="metric-card">
          <span class="metric-value">{totalQueries}</span>
          <span class="metric-label">Total Queries</span>
        </div>
        <div class="metric-card">
          <span class="metric-value">{fmtLatency(avgLatency)}</span>
          <span class="metric-label">Avg Latency</span>
        </div>
        <div class="metric-card">
          <span class="metric-value" class:error-text={errorRate > 0.1}>{(errorRate * 100).toFixed(1)}%</span>
          <span class="metric-label">Error Rate</span>
        </div>
        <div class="metric-card">
          <span class="metric-value">{totalTokens.toLocaleString()}</span>
          <span class="metric-label">Total Tokens</span>
        </div>
        <div class="metric-card">
          <span class="metric-value" class:error-text={errorCount > 0}>{errorCount}</span>
          <span class="metric-label">Errors</span>
        </div>
      </div>
    </section>

    <!-- Domains Table -->
    <section class="section">
      <h3>Domains</h3>
      {#if domainList.length === 0}
        <EmptyState heading="No domains configured" />
      {:else}
        <div class="domains-list">
          {#each domainList as domain}
            <div class="domain-row">
              <button class="domain-header" onclick={() => toggleDomain(domain.name)}>
                <span class="domain-expand">{expandedDomain === domain.name ? '\u25BC' : '\u25B6'}</span>
                <span class="domain-name">{domain.name}</span>
                <span class="domain-desc">{domain.description ?? ''}</span>
                <Badge variant="info" text={`${domain.tools?.length ?? 0} tools`} />
              </button>
              {#if expandedDomain === domain.name && domain.tools?.length}
                <div class="domain-tools">
                  {#each domain.tools as tool}
                    <span class="tool-tag">{tool}</span>
                  {/each}
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </section>

    <!-- Query History -->
    <section class="section">
      <h3>Recent Queries</h3>
      {#if entries.length === 0}
        <EmptyState heading="No queries recorded yet" />
      {:else}
        <div class="history-list">
          {#each entries as entry}
            <div class="history-row">
              <span class="history-time">{fmtTime(entry.timestamp)}</span>
              <span class="history-query">{truncate(entry.query, 60)}</span>
              <span class="history-domains">
                {#each (entry.domains ?? []) as d}
                  <span class="domain-chip">{d}</span>
                {/each}
              </span>
              <span class="history-latency">{fmtLatency(entry.latency_ms ?? 0)}</span>
              <span class="history-tokens">{(entry.total_tokens ?? 0).toLocaleString()} tok</span>
              <Badge variant={statusVariant(entry.status)} text={entry.status ?? ''} />
            </div>
          {/each}
        </div>
      {/if}
    </section>
  {/if}
</div>

<style>
  .weaver-panel {
    display: flex;
    flex-direction: column;
    gap: var(--space-4);
    padding: var(--space-4);
  }

  .stat {
    display: flex;
    flex-direction: column;
    align-items: center;
  }

  .stat-value {
    font-size: var(--text-lg);
    font-weight: 600;
    font-family: var(--font-mono);
  }

  .stat-label {
    font-size: var(--text-sm);
    color: var(--fg-dim);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }

  .section {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    position: relative;
  }

  .section::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .section h3 {
    margin: 0 0 var(--space-2) 0;
    font-size: var(--text-base);
    letter-spacing: var(--tracking-normal);
  }

  .loading {
    padding: var(--space-4);
    text-align: center;
    border-radius: var(--radius-sm);
  }

  /* Status Grid */
  .status-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: var(--space-2);
  }

  .status-item {
    display: flex;
    justify-content: space-between;
    align-items: center;
    padding: 6px 0;
    border-bottom: 1px solid var(--border-subtle);
  }

  .status-label {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    letter-spacing: var(--tracking-normal);
  }

  .status-value {
    font-weight: 600;
    font-size: var(--text-sm);
    color: var(--fg-primary);
  }

  .text-mono {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
  }

  /* Metrics Grid */
  .metrics-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(140px, 1fr));
    gap: var(--space-3);
  }

  .metric-card {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    padding: var(--space-3);
    text-align: center;
    background: var(--bg-secondary);
    position: relative;
    transition: border-color var(--transition-fast);
  }

  .metric-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .metric-value {
    display: block;
    font-size: var(--text-xl);
    font-weight: 700;
    font-family: var(--font-mono);
    margin-bottom: var(--space-1);
    color: var(--fg-primary);
  }

  .metric-label {
    font-size: var(--text-sm);
    color: var(--fg-dim);
    letter-spacing: var(--tracking-wide);
    text-transform: uppercase;
  }

  .error-text { color: var(--error); }

  /* Domains */
  .domains-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .domain-row {
    border: 1px solid var(--border);
    border-radius: var(--radius-sm);
    overflow: hidden;
    transition: border-color var(--transition-fast);
  }

  .domain-header {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    width: 100%;
    padding: var(--space-2) var(--space-3);
    background: transparent;
    border: none;
    color: inherit;
    cursor: pointer;
    font-size: var(--text-sm);
    text-align: left;
    transition: background var(--transition-fast);
    letter-spacing: var(--tracking-normal);
  }

  .domain-header:hover {
    background: var(--bg-elevated);
  }

  .domain-header:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }

  .domain-expand {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    width: 1rem;
    flex-shrink: 0;
  }

  .domain-name {
    font-weight: 600;
    min-width: 120px;
    color: var(--fg-primary);
  }

  .domain-desc {
    flex: 1;
    color: var(--fg-secondary);
    font-size: var(--text-sm);
  }

  .domain-tools {
    padding: var(--space-2) var(--space-3) var(--space-3) 2rem;
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-1);
    border-top: 1px solid var(--border-subtle);
  }

  .tool-tag {
    font-size: var(--text-xs);
    padding: 2px 6px;
    border-radius: var(--radius-xs);
    background: var(--info-dim);
    border: 1px solid rgba(var(--info-rgb), 0.18);
    font-family: var(--font-mono);
    color: var(--fg-secondary);
    letter-spacing: var(--tracking-normal);
  }

  /* History */
  .history-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
    max-height: 400px;
    overflow-y: auto;
  }

  .history-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 6px var(--space-2);
    border: 1px solid var(--border);
    border-radius: var(--radius-xs);
    font-size: var(--text-sm);
    transition: background var(--transition-fast);
    letter-spacing: var(--tracking-normal);
  }

  .history-row:hover {
    background: var(--bg-elevated);
  }

  .history-time {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--fg-dim);
    min-width: 70px;
    flex-shrink: 0;
  }

  .history-query {
    flex: 1;
    min-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--fg-primary);
  }

  .history-domains {
    display: flex;
    gap: var(--space-1);
    flex-shrink: 0;
  }

  .domain-chip {
    font-size: var(--text-2xs);
    padding: 1px 5px;
    border-radius: var(--radius-xs);
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
  }

  .history-latency {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    color: var(--fg-secondary);
    min-width: 50px;
    text-align: right;
    flex-shrink: 0;
  }

  .history-tokens {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    min-width: 60px;
    text-align: right;
    flex-shrink: 0;
    font-family: var(--font-mono);
  }

  /* Preflight degraded banner (S4) */
  .degraded-banner {
    border-left: 3px solid var(--color-warning);
    background: var(--bg-warning-soft);
    padding-left: 12px;
  }
  .degraded-banner h3 {
    color: var(--color-warning);
  }
  .degraded-detail {
    margin: 6px 0;
    font-size: 0.92rem;
    line-height: 1.4;
  }
  .degraded-detail.muted {
    color: var(--fg-muted);
    font-size: 0.85rem;
  }
  .model-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
    margin: 8px 0;
  }
  .model-chip {
    font-family: var(--font-mono);
    font-size: 0.82rem;
    padding: 2px 8px;
    border-radius: 4px;
    border: 1px solid var(--border-default);
  }
  .model-chip.missing {
    background: var(--bg-danger-soft);
    border-color: var(--color-danger);
    color: var(--color-danger);
  }
  .model-chip.ready {
    background: var(--bg-success-soft);
    border-color: var(--color-success);
    color: var(--color-success);
  }

  /* Role defaults subview (S6.2) */
  .roles-table {
    display: flex;
    flex-direction: column;
    gap: 4px;
    margin-top: 8px;
  }
  .role-row {
    display: grid;
    grid-template-columns: 200px 220px 1fr;
    gap: 12px;
    padding: 6px 8px;
    border-radius: 4px;
    background: var(--bg-row);
    align-items: center;
  }
  .role-row:hover {
    background: var(--bg-row-hover);
  }
  .role-name {
    color: var(--fg-secondary);
  }
  .role-primary {
    color: var(--fg-default);
    font-weight: 500;
  }
  .role-fallbacks {
    font-size: 0.85rem;
    color: var(--fg-muted);
  }
  .role-fallbacks code {
    font-family: var(--font-mono);
    margin: 0 2px;
  }

  /* Phone/tablet reflow: the 200px+220px role rails and the two-column
     status grid overflow narrow viewports — stack them. */
  @media (max-width: 720px) {
    .status-grid { grid-template-columns: 1fr; }
    .role-row {
      grid-template-columns: 1fr;
      gap: 2px;
    }
  }
</style>
