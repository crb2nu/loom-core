<script lang="ts">
  /**
   * OperatorStrip — the Operator Deck's top signal row. One compact chip per
   * domain the deck composes: Mills health, in-flight runs, MR registry,
   * live agents, alerts, context pressure. Chips are buttons that jump to the
   * owning full view; the strip never blocks on any one store (each chip
   * renders whatever its store currently has).
   *
   * A chip whose store is erroring with nothing to show reads `?` /
   * "unreachable" instead of the zero its aggregate would otherwise report —
   * a dead backend must never render as "0 running" or "all healthy".
   */
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { mrwatchStore } from '../../stores/mrwatch.svelte.ts';
  import { fleetStore } from '../../stores/fleet.svelte.ts';
  import { alertsStore } from '../../stores/alerts.svelte.ts';
  import { contextHealthStore } from '../../stores/contextHealth.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import { millEfficiency } from '../../utils/operatorHelpers.ts';

  interface Chip {
    id: string;
    label: string;
    value: string;
    detail: string;
    tone: 'ok' | 'busy' | 'warn' | 'error' | 'muted';
    view: [string, string];
  }

  let chips = $derived.by((): Chip[] => {
    const out: Chip[] = [];

    const mh = millsStore.systemHealth;
    if (millsStore.disabled) {
      out.push({
        id: 'mills', label: 'Mills', value: 'off',
        detail: 'operator not configured', tone: 'muted', view: ['mills', 'mills-overview'],
      });
    } else if (millsStore.error) {
      out.push({
        id: 'mills', label: 'Mills', value: '?',
        detail: 'unreachable', tone: 'error', view: ['mills', 'mills-overview'],
      });
    } else if (millsStore.reconnecting && !millsStore.status) {
      // Routine operator-redeploy window with nothing cached yet: say so
      // quietly instead of an alarming '?' or a false '0 running'.
      out.push({
        id: 'mills', label: 'Mills', value: '…',
        detail: 'reconnecting', tone: 'warn', view: ['mills', 'mills-overview'],
      });
    } else {
      out.push({
        id: 'mills',
        label: 'Mills',
        value: `${mh.active_runs} running`,
        detail: millsStore.reconnecting
          ? 'reconnecting — last known'
          : `${mh.queued} queued · ${mh.merges_24h} merged 24h`,
        tone: mh.state === 'broken' ? 'error' : mh.state === 'in_flight' ? 'busy' : mh.state === 'idle' ? 'muted' : 'ok',
        view: ['mills', 'mills-overview'],
      });
      // Factory-model line-health dials (first-pass yield · cost per bolt)
      // from the same 1d KPI snapshot systemHealth reads. Hidden while the
      // window is idle or the KPI endpoint hasn't shipped the keys.
      const eff = millEfficiency(millsStore.kpis?.metrics);
      if (eff) {
        out.push({
          id: 'yield', label: 'Yield', value: `${eff.yieldPct}%`,
          detail: eff.detail, tone: eff.tone, view: ['mills', 'factory'],
        });
      }
      const sparks = millsStore.escalatedRuns.length;
      if (sparks > 0) {
        out.push({
          id: 'sparks', label: 'Sparks', value: `${sparks}`,
          detail: 'escalated, need review', tone: 'error', view: ['mills', 'sparks'],
        });
      }
    }

    const unhealthy = mrwatchStore.unhealthyCount;
    const liveMRs = mrwatchStore.liveMergeRequests;
    const mrsUnreachable = !!mrwatchStore.error && liveMRs.length === 0;
    out.push({
      id: 'mrs',
      label: 'MRs',
      value: mrsUnreachable ? '?' : `${liveMRs.length}`,
      detail: mrsUnreachable
        ? 'unreachable'
        : unhealthy > 0 ? `${unhealthy} unhealthy` : 'all healthy',
      tone: mrsUnreachable ? 'error' : unhealthy > 0 ? 'warn' : 'ok',
      view: ['agents', 'mrwatch'],
    });

    const summary = fleetStore.unifiedSummary;
    const fleetUnreachable = !!fleetStore.error && summary.live_agents === 0;
    out.push({
      id: 'agents',
      label: 'Agents',
      value: fleetUnreachable ? '?' : `${summary.live_agents} live`,
      detail: fleetUnreachable
        ? 'unreachable'
        : summary.orphans > 0 ? `${summary.orphans} orphaned` : `${summary.with_sessions} with sessions`,
      tone: fleetUnreachable
        ? 'error'
        : summary.orphans > 0 ? 'warn' : summary.live_agents > 0 ? 'busy' : 'muted',
      view: ['agents', 'fleet'],
    });

    if (alertsStore.activeAlerts.length > 0) {
      out.push({
        id: 'alerts',
        label: 'Alerts',
        value: `${alertsStore.activeAlerts.length}`,
        detail: alertsStore.criticalCount > 0 ? `${alertsStore.criticalCount} critical` : 'active',
        tone: alertsStore.criticalCount > 0 ? 'error' : 'warn',
        view: ['agents', 'alerts'],
      });
    }

    const compacting = contextHealthStore.needingCompaction.length;
    if (compacting > 0) {
      out.push({
        id: 'context',
        label: 'Context',
        value: `${compacting}`,
        detail: 'agents near budget',
        tone: 'warn',
        view: ['knowledge', 'context-health'],
      });
    }

    return out;
  });
</script>

<div class="strip" role="navigation" aria-label="Deck signals">
  {#each chips as chip (chip.id)}
    <button
      class="chip"
      data-tone={chip.tone}
      onclick={() => router.navigate(chip.view[0], chip.view[1])}
      title="{chip.label}: {chip.detail}"
    >
      <span class="chip-label">{chip.label}</span>
      <span class="chip-value">{chip.value}</span>
      <span class="chip-detail">{chip.detail}</span>
    </button>
  {/each}
</div>

<style>
  .strip {
    display: flex;
    gap: var(--space-2);
    overflow-x: auto;
    scrollbar-width: none;
    flex-shrink: 0;
    padding-bottom: 2px;
  }
  .strip::-webkit-scrollbar { display: none; }

  .chip {
    display: flex;
    align-items: baseline;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border-radius: var(--radius-md);
    border: 1px solid var(--border-subtle);
    background: color-mix(in srgb, var(--bg-secondary) 82%, transparent);
    cursor: pointer;
    white-space: nowrap;
    transition: border-color var(--transition-fast), background var(--transition-fast);
  }
  .chip:hover {
    border-color: var(--border-focus);
    background: var(--bg-tertiary);
  }

  .chip-label {
    font-size: 10px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .chip-value {
    font-size: var(--text-sm);
    font-weight: 600;
    font-family: var(--font-mono);
    color: var(--fg-primary);
  }

  .chip[data-tone='error'] .chip-value { color: var(--error); }
  .chip[data-tone='warn'] .chip-value { color: var(--warning); }
  .chip[data-tone='busy'] .chip-value { color: var(--info); }
  .chip[data-tone='ok'] .chip-value { color: var(--success); }
  .chip[data-tone='muted'] .chip-value { color: var(--fg-muted); }

  .chip-detail {
    font-size: var(--text-xs);
    color: var(--fg-dim);
  }

  @media (max-width: 800px) {
    .chip-detail { display: none; }
  }
</style>
