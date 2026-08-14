<script lang="ts">
  // RecoverySLOCard — fleet disconnect-to-recovered SLO rollup (MBL-5 slice 3).
  //
  // Reads the same-origin HUD-internal aggregate (GET /api/telemetry/recovery),
  // which the mobile companion publishes per device. Renders the fleet p95 vs
  // the SLO target, a meets/over badge, headline counts, and a per-device
  // breakdown. Self-contained fetch+poll — no store.

  import type { BadgeVariant } from '../utils/tokens.ts';
  import Badge from '../widgets/Badge.svelte';
  import EmptyState from './shared/EmptyState.svelte';

  interface RecoveryDevice {
    device_id: string;
    p95_seconds: number;
    sample_count: number;
    meets_slo: boolean;
  }

  interface RecoveryAggregate {
    device_count?: number;
    total_samples?: number;
    fleet_p95_seconds?: number;
    fleet_mean_seconds?: number;
    slo_target_seconds?: number;
    devices_meeting_slo?: number;
    meets_slo?: boolean;
    devices?: RecoveryDevice[];
  }

  let agg = $state<RecoveryAggregate | null>(null);
  let loading = $state(true);
  let error = $state('');
  let pollTimer = $state<ReturnType<typeof setInterval> | null>(null);

  async function fetchAggregate() {
    try {
      const res = await fetch('/api/telemetry/recovery');
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      agg = (await res.json()) as RecoveryAggregate;
      error = '';
    } catch (e) {
      error = e instanceof Error ? e.message : 'Failed to fetch recovery telemetry';
    } finally {
      loading = false;
    }
  }

  // 30s cadence matches the in-app recovery window / poll-fallback rhythm.
  $effect(() => {
    fetchAggregate();
    pollTimer = setInterval(fetchAggregate, 30000);
    return () => {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = null;
    };
  });

  let deviceCount = $derived(agg?.device_count ?? 0);
  let totalSamples = $derived(agg?.total_samples ?? 0);
  let fleetP95 = $derived(agg?.fleet_p95_seconds ?? 0);
  let fleetMean = $derived(agg?.fleet_mean_seconds ?? 0);
  let sloTarget = $derived(agg?.slo_target_seconds ?? 30);
  let devicesMeetingSlo = $derived(agg?.devices_meeting_slo ?? 0);
  let meetsSlo = $derived(agg?.meets_slo ?? true);
  let devices = $derived(agg?.devices ?? []);

  // p95-vs-target ratio drives the bar fill colour. >=1 is over budget.
  let p95Ratio = $derived(sloTarget > 0 ? fleetP95 / sloTarget : 0);

  function sloVariant(ratio: number): BadgeVariant {
    if (ratio >= 1) return 'error';
    if (ratio >= 0.8) return 'warning';
    return 'success';
  }

  function secs(v: number | null | undefined) {
    return `${(v ?? 0).toFixed(1)}s`;
  }

  function shortDevice(id: string | undefined) {
    if (!id) return 'unknown';
    return id.length > 10 ? `${id.slice(0, 8)}…` : id;
  }
</script>

<section class="recovery-card">
  <header class="card-header">
    <h3>Recovery SLO</h3>
    {#if !loading && !error && deviceCount > 0}
      <Badge
        text={meetsSlo ? 'Meeting SLO' : 'Over SLO'}
        variant={meetsSlo ? 'success' : 'error'}
      />
    {/if}
  </header>

  {#if loading}
    <div class="status-line">Loading recovery telemetry…</div>
  {:else if error}
    <div class="error-banner">{error}</div>
  {:else if deviceCount === 0}
    <EmptyState
      icon="📶"
      message="No devices reporting recovery telemetry yet"
      description="The companion publishes its disconnect-to-recovered window once the mobile:telemetry scope is enabled."
      compact={true}
    />
  {:else}
    <div class="hero">
      <div class="hero-metric">
        <span class="hero-value">{secs(fleetP95)}</span>
        <span class="hero-target">/ {secs(sloTarget)} p95</span>
      </div>
      <div class="slo-bar">
        <div
          class="slo-fill {sloVariant(p95Ratio)}"
          style="width: {Math.min(p95Ratio * 100, 100)}%"
        ></div>
      </div>
    </div>

    <div class="stat-row">
      <div class="stat">
        <span class="stat-value">{deviceCount}</span>
        <span class="stat-label">devices</span>
      </div>
      <div class="stat">
        <span class="stat-value">{devicesMeetingSlo}/{deviceCount}</span>
        <span class="stat-label">meeting SLO</span>
      </div>
      <div class="stat">
        <span class="stat-value">{secs(fleetMean)}</span>
        <span class="stat-label">fleet mean</span>
      </div>
      <div class="stat">
        <span class="stat-value">{totalSamples}</span>
        <span class="stat-label">samples</span>
      </div>
    </div>

    <ul class="device-list">
      {#each devices as dev (dev.device_id)}
        <li class="device-row">
          <span class="device-id" title={dev.device_id}>{shortDevice(dev.device_id)}</span>
          <span class="device-p95">{secs(dev.p95_seconds)} p95</span>
          <span class="device-samples">{dev.sample_count} smpl</span>
          <Badge
            text={dev.meets_slo ? 'ok' : 'over'}
            variant={dev.meets_slo ? 'success' : 'error'}
          />
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .recovery-card {
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    position: relative;
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .recovery-card::before {
    content: '';
    position: absolute;
    inset: 0;
    border-radius: inherit;
    background: var(--surface-highlight);
    pointer-events: none;
  }

  .card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-2);
  }

  .card-header h3 {
    margin: 0;
    font-size: var(--text-sm);
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .status-line {
    color: var(--fg-muted);
    font-size: var(--text-sm);
    padding: var(--space-2) 0;
  }

  .error-banner {
    background: var(--error-dim);
    color: var(--error);
    border: 1px solid rgba(255, 61, 113, 0.2);
    border-radius: var(--radius-md);
    padding: var(--space-2);
    font-size: var(--text-sm);
  }

  .hero {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
  }

  .hero-metric {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
  }

  .hero-value {
    font-size: var(--text-2xl);
    font-weight: 700;
    font-family: var(--font-mono);
    color: var(--fg-primary);
  }

  .hero-target {
    font-size: var(--text-sm);
    color: var(--fg-dim);
    font-family: var(--font-mono);
  }

  .slo-bar {
    height: 6px;
    background: var(--bg-elevated);
    border-radius: var(--radius-xs);
    overflow: hidden;
  }

  .slo-fill {
    height: 100%;
    border-radius: var(--radius-xs);
    transition: width var(--transition-slow);
  }

  .slo-fill.success { background: var(--success); box-shadow: 0 0 4px var(--glow-success); }
  .slo-fill.warning { background: var(--warning); box-shadow: 0 0 4px var(--glow-warning); }
  .slo-fill.error { background: var(--error); box-shadow: 0 0 4px var(--glow-error); }

  .stat-row {
    display: flex;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .stat {
    display: flex;
    flex-direction: column;
    align-items: center;
    flex: 1;
    min-width: 64px;
  }

  .stat-value {
    font-size: var(--text-base);
    font-weight: 600;
    color: var(--fg-primary);
    font-family: var(--font-mono);
  }

  .stat-label {
    font-size: var(--text-xs);
    color: var(--fg-dim);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .device-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: var(--space-1);
  }

  .device-row {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-1) 0;
    border-top: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
  }

  .device-id {
    flex: 1;
    font-family: var(--font-mono);
    color: var(--fg-secondary);
  }

  .device-p95 {
    font-family: var(--font-mono);
    color: var(--fg-primary);
  }

  .device-samples {
    font-family: var(--font-mono);
    color: var(--fg-dim);
  }
</style>
