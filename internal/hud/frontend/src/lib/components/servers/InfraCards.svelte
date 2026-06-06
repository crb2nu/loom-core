<script lang="ts">
  /**
   * InfraCards — Tunnels + Cache stats card pair. Owns its own infra
   * fetch loop (was previously the panel's own $effect timer).
   */
  import { healthStore } from '../../stores/health.svelte.ts';
  import StatusDot from '../../widgets/StatusDot.svelte';
  import Badge from '../../widgets/Badge.svelte';
  import { tunnelStateVariant } from '../../utils/serversHelpers';

  // tunnels starts at null (initial / never-fetched) so we can distinguish
  // it from an empty array (fetched, none active). Same for an explicit
  // error path: previously the Tunnels card showed "none / No active
  // tunnels" identically for the cold-start error and the "infra healthy
  // but no tunnels configured" case.
  let tunnels = $state<any[] | null>(null);
  let cacheStats = $state<any>(null);
  let infraLoading = $state(false);
  let tunnelsError = $state<string | null>(null);
  let cacheError = $state<string | null>(null);

  async function fetchInfraStats() {
    infraLoading = true;
    tunnelsError = null;
    cacheError = null;
    // healthStore.fetchTunnels/fetchCacheStats swallow errors internally
    // and return [] / null on failure (writing to healthStore.error).
    // Snapshot the error around each call so we can attribute a failure
    // to the right card. The shared healthStore.error sink stays
    // populated for other consumers; we only consume the delta we caused.
    healthStore.error = null;
    const tList = await healthStore.fetchTunnels();
    if (healthStore.error) {
      tunnelsError = healthStore.error;
    } else {
      tunnels = tList ?? [];
    }
    healthStore.error = null;
    const cStats = await healthStore.fetchCacheStats();
    if (healthStore.error) {
      cacheError = healthStore.error;
    } else {
      cacheStats = cStats;
    }
    infraLoading = false;
  }

  $effect(() => {
    fetchInfraStats();
    const timer = setInterval(fetchInfraStats, 30000);
    return () => clearInterval(timer);
  });
</script>

<div class="infra-cards">
  <div class="infra-card">
    <div class="infra-card-header">
      <span class="infra-card-title">SSH Tunnels</span>
      {#if tunnelsError}
        <Badge text="error" variant="error" />
      {:else if tunnels && tunnels.length > 0}
        <Badge text="{tunnels.length} active" variant="info" />
      {:else if tunnels}
        <Badge text="none" variant="info" />
      {/if}
    </div>
    <div class="infra-card-body">
      {#if tunnelsError}
        <!-- Mirrors the Cache card's 3-state shape so an /api/tunnels
             failure is distinguishable from "no tunnels configured". -->
        <span class="text-xs infra-error" role="alert">
          <span aria-hidden="true">⚠</span> Tunnels unavailable: {tunnelsError}
        </span>
      {:else if tunnels === null && infraLoading}
        <span class="text-muted text-xs">Loading...</span>
      {:else if tunnels && tunnels.length > 0}
        <div class="tunnel-list">
          {#each tunnels as tunnel}
            <div class="tunnel-row">
              <StatusDot status={tunnel.state === 'connected' ? 'healthy' : tunnel.state === 'connecting' ? 'degraded' : 'down'} />
              <span class="text-mono tunnel-name">{tunnel.name}</span>
              <span class="text-muted text-xs">{tunnel.remote_host}</span>
              <Badge text={tunnel.state} variant={tunnelStateVariant(tunnel.state)} />
              {#if tunnel.uptime}
                <span class="text-muted text-xs">up {tunnel.uptime}</span>
              {/if}
              {#if tunnel.reconnects > 0}
                <span class="text-xs reconnect-count">↻ {tunnel.reconnects}</span>
              {/if}
            </div>
          {/each}
        </div>
      {:else}
        <span class="text-muted text-xs">No active tunnels</span>
      {/if}
    </div>
  </div>

  <div class="infra-card">
    <div class="infra-card-header">
      <span class="infra-card-title">Response Cache</span>
      {#if cacheError}
        <Badge text="error" variant="error" />
      {:else if cacheStats}
        <Badge text="{cacheStats.entries} entries" variant="info" />
      {/if}
    </div>
    <div class="infra-card-body">
      {#if cacheError}
        <span class="text-xs infra-error" role="alert">
          <span aria-hidden="true">⚠</span> Cache stats unavailable: {cacheError}
        </span>
      {:else if cacheStats}
        <div class="cache-grid">
          <div class="cache-stat">
            <span class="cache-stat-value text-mono">{cacheStats.entries}</span>
            <span class="cache-stat-label">Entries</span>
          </div>
          {#if cacheStats.size}
            <div class="cache-stat">
              <span class="cache-stat-value text-mono">{cacheStats.size}</span>
              <span class="cache-stat-label">Size</span>
            </div>
          {/if}
          <div class="cache-stat">
            <span class="cache-stat-value text-mono" style:color={cacheStats.hit_rate > 0.5 ? 'var(--success)' : 'var(--fg-secondary)'}>{(cacheStats.hit_rate * 100).toFixed(1)}%</span>
            <span class="cache-stat-label">Hit Rate</span>
          </div>
        </div>
      {:else if infraLoading}
        <span class="text-muted text-xs">Loading...</span>
      {:else}
        <span class="text-muted text-xs">Unavailable</span>
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

  /* In-card error chip — used by both Tunnels and Cache branches so the
     visual treatment for a /api/tunnels or /api/cache failure is
     identical to a non-fatal error chip elsewhere. */
  .infra-error {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    color: var(--error);
    word-break: break-word;
  }

  .tunnel-list {
    display: flex;
    flex-direction: column;
    gap: 6px;
  }

  .tunnel-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: 3px 0;
    border-bottom: 1px solid var(--border-subtle);
  }

  .tunnel-row:last-child {
    border-bottom: none;
  }

  .tunnel-name {
    font-size: var(--text-sm);
    font-weight: 500;
    color: var(--fg-primary);
  }

  .reconnect-count {
    color: var(--warning);
    font-family: var(--font-mono);
  }

  .cache-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-3);
  }

  .cache-stat {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 2px;
  }

  .cache-stat-value {
    font-size: 20px;
    font-weight: 700;
    color: var(--fg-primary);
    line-height: 1.1;
  }

  .cache-stat-label {
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
  }
</style>
