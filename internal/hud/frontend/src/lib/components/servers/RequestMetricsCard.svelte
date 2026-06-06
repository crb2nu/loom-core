<script lang="ts">
  /**
   * RequestMetricsCard — request-count + latency-percentiles table from
   * the daemon prometheus metrics. Reads daemonMetricsStore directly.
   *
   * Previously this card rendered *only* when `servers.length > 0`, so a
   * failed /metrics fetch (loomd down or proxy 5xx) and a cold start
   * both showed as a missing card with no signal. Now the card shell
   * always renders and the body branches: error → in-card error chip,
   * loading-with-no-rows → skeleton, !loading-no-rows → "No request
   * metrics" EmptyState, otherwise → the table.
   */
  import { daemonMetricsStore } from '../../stores/daemonMetrics.svelte.ts';
  import Badge from '../../widgets/Badge.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import { formatLatency } from '../../utils/serversHelpers';

  const SKELETON_ROWS = 3;
</script>

<div class="infra-cards">
  <div class="infra-card infra-card-wide">
    <div class="infra-card-header">
      <span class="infra-card-title">Request Metrics</span>
      {#if daemonMetricsStore.error}
        <Badge text="error" variant="error" />
      {:else if daemonMetricsStore.servers.length > 0}
        <Badge text="{daemonMetricsStore.totalRequests} reqs" variant="info" />
        {#if daemonMetricsStore.overallErrorRate > 0.01}
          <Badge text="{(daemonMetricsStore.overallErrorRate * 100).toFixed(1)}% errors" variant="error" />
        {/if}
      {:else if daemonMetricsStore.loading}
        <Badge text="loading" variant="neutral" />
      {/if}
    </div>
    <div class="infra-card-body">
      {#if daemonMetricsStore.error}
        <div class="metrics-error" role="alert" aria-live="polite">
          <span aria-hidden="true">⚠</span>
          <span>Request metrics unavailable: {daemonMetricsStore.error}</span>
        </div>
      {:else if !daemonMetricsStore.loading && daemonMetricsStore.servers.length === 0}
        <EmptyState icon={'□'} heading="No request metrics" description="Metrics will appear once the daemon serves traffic." compact />
      {:else if daemonMetricsStore.loading && daemonMetricsStore.servers.length === 0}
        <!-- Skeleton: a 3-row placeholder so the card has weight while
             the first /metrics call is in flight. Prevents the panel
             from reflowing once data lands. -->
        <div class="metrics-table-wrap">
          <table class="metrics-table">
            <thead>
              <tr>
                <th>Server</th>
                <th>Requests</th>
                <th>Errors</th>
                <th>p50</th>
                <th>p95</th>
                <th>p99</th>
                <th>In-Flight</th>
              </tr>
            </thead>
            <tbody>
              {#each Array(SKELETON_ROWS) as _, i}
                <tr class="skeleton-row">
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                  <td class="text-mono"><span class="skeleton-cell"></span></td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <div class="metrics-table-wrap">
          <table class="metrics-table">
            <thead>
              <tr>
                <th>Server</th>
                <th>Requests</th>
                <th>Errors</th>
                <th>p50</th>
                <th>p95</th>
                <th>p99</th>
                <th>In-Flight</th>
              </tr>
            </thead>
            <tbody>
              {#each daemonMetricsStore.servers as m (m.name)}
                <tr class:has-errors={m.error_count > 0}>
                  <td class="text-mono">{m.name}</td>
                  <td class="text-mono">{m.request_count}</td>
                  <td class="text-mono" class:text-error={m.error_count > 0}>{m.error_count || '—'}</td>
                  <td class="text-mono">{formatLatency(m.p50_ms)}</td>
                  <td class="text-mono">{formatLatency(m.p95_ms)}</td>
                  <td class="text-mono" class:text-warning={m.p99_ms > 5000}>{formatLatency(m.p99_ms)}</td>
                  <td class="text-mono">{m.in_flight || '—'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </div>
  </div>
</div>

<style>
  .infra-cards {
    display: grid;
    grid-template-columns: 1fr;
    gap: var(--space-3);
  }

  .infra-card {
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3) var(--space-4);
    position: relative;
  }

  .infra-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .infra-card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: var(--space-2);
  }

  .infra-card-title {
    font-size: var(--text-xs);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .infra-card-body {
    font-size: var(--text-sm);
  }

  .infra-card-wide {
    width: 100%;
  }

  .metrics-table-wrap {
    overflow-x: auto;
  }

  .metrics-table {
    width: 100%;
    border-collapse: collapse;
    font-size: var(--text-xs);
  }

  .metrics-table th {
    text-align: left;
    padding: 4px var(--space-2);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    border-bottom: 1px solid var(--border);
    white-space: nowrap;
  }

  .metrics-table td {
    padding: 4px var(--space-2);
    border-bottom: 1px solid var(--border-subtle);
    color: var(--fg-secondary);
    white-space: nowrap;
  }

  .metrics-table tr:hover {
    background: var(--bg-tertiary);
  }

  .metrics-table tr.has-errors {
    border-left: 2px solid var(--error);
  }

  .text-error { color: var(--error); }
  .text-warning { color: var(--warning); }

  .metrics-error {
    display: flex;
    gap: var(--space-2);
    align-items: flex-start;
    padding: 8px var(--space-3);
    background: color-mix(in srgb, var(--error) 18%, var(--bg-secondary));
    border: 1px solid var(--error);
    border-radius: var(--radius-md);
    color: var(--fg-primary);
    font-size: var(--text-sm);
    word-break: break-word;
  }

  .metrics-error :global(span:first-child) {
    color: var(--error);
    font-weight: 700;
    flex-shrink: 0;
  }

  /* Skeleton placeholder cells — keep the row height ~aligned with real
     rows so the card has no reflow when data lands. */
  .skeleton-row td {
    padding: 6px var(--space-2);
  }

  .skeleton-cell {
    display: inline-block;
    width: 60%;
    height: 10px;
    background: linear-gradient(
      90deg,
      var(--bg-tertiary) 0%,
      color-mix(in srgb, var(--bg-tertiary) 50%, var(--bg-elevated)) 50%,
      var(--bg-tertiary) 100%
    );
    background-size: 200% 100%;
    border-radius: var(--radius-sm);
    animation: skeleton-shimmer 1.6s ease-in-out infinite;
  }

  @keyframes skeleton-shimmer {
    0% { background-position: 200% 0; }
    100% { background-position: -200% 0; }
  }
</style>
