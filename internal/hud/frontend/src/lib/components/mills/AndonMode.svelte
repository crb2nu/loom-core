<script lang="ts">
  /**
   * AndonMode — the Factory panel's fullscreen glance board, for the
   * office TV. An andon board is not interacted with; it is read from
   * 3–10 m. One lamp (weaving / idle / paused / escalation storm, with
   * FEED STALE outranking everything — the board never glows green on
   * a dead feed), one giant north-star odometer (autonomous merges,
   * 24 h), four supporting numbers, and an honest freshness line.
   *
   * Mounted inside FactoryPanel, so the panel's existing 15 s poll is
   * the only data source — no pollers of its own. Deep-linkable: the
   * parent maps #mills/factory/andon onto this overlay.
   */
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { focusTrap } from '../../actions/focusTrap';
  import { andonState, freshnessLabel, odometerDigits } from '../../utils/andonHelpers.ts';
  import { fuelReading } from '../../utils/factoryHelpers.ts';

  let { onclose }: { onclose: () => void } = $props();

  let activeRuns = $derived(millsStore.pipelineRuns ?? []);
  let metrics = $derived(millsStore.kpis?.metrics ?? null);
  let backlogActive = $derived.by(() => {
    const byState = millsStore.backlogByState;
    return (byState['queued'] ?? 0) + (byState['ready'] ?? 0) +
      (byState['running'] ?? 0) + (byState['escalated'] ?? 0) + (byState['paused'] ?? 0);
  });

  let reading = $derived(
    andonState({
      stale: millsStore.isStale || millsStore.error != null,
      // snake_case on the wire (`policy_enabled`) — see FactoryPanel's
      // millsPaused note; a PascalCase read here would never trip the lamp.
      paused: !(millsStore.status?.policy_enabled ?? millsStore.policy?.enabled ?? true),
      activeRuns: activeRuns.length,
      escalated24h: metrics?.pipeline_escalated_runs ?? 0,
      merged24h: metrics?.pipeline_merged_runs ?? 0,
    }),
  );

  let digits = $derived(odometerDigits(metrics?.pipeline_merged_runs, 3));
  // Leading zeros stay on the reel (it is an odometer) but recede: at
  // 3–10 m "013" reads as a three-digit number, and the glance answer
  // is 13. First significant digit onward carries the light.
  let firstSignificant = $derived.by(() => {
    const i = digits.findIndex((d) => d !== 0);
    return i === -1 ? digits.length - 1 : i;
  });

  // The overnight question a TV board must answer is not just "is it
  // healthy" but "does it have fuel" — the 24 h pipeline budget is a real
  // stop condition for a lights-out shift. Same honest reading the
  // factory rail uses.
  let fuel = $derived(fuelReading(millsStore.status?.budget?.pipeline));

  // 1 s wall-clock tick drives the freshness line and the corner clock —
  // the age readout is the honesty affordance, so it must visibly move.
  let now = $state(new Date());
  $effect(() => {
    const t = setInterval(() => (now = new Date()), 1000);
    return () => clearInterval(t);
  });
  let freshness = $derived(freshnessLabel(millsStore.lastUpdated, now));
  // Three missed poll ticks: the line itself turns urgent before the
  // staleness threshold flips the whole lamp.
  let freshnessAging = $derived.by(() => {
    const last = millsStore.lastUpdated;
    return !!last && now.getTime() - last.getTime() > 45_000;
  });
  let clock = $derived(
    `${String(now.getHours()).padStart(2, '0')}:${String(now.getMinutes()).padStart(2, '0')}`,
  );

  function pct(v: number | undefined): string {
    return typeof v === 'number' ? `${Math.round(v * 100)}%` : '—';
  }

  /* Browser fullscreen is optional garnish on top of the fixed overlay —
     the mode works without it, but a TV wants the chrome gone. */
  let isFullscreen = $state(false);
  $effect(() => {
    const sync = () => (isFullscreen = document.fullscreenElement != null);
    sync();
    document.addEventListener('fullscreenchange', sync);
    return () => document.removeEventListener('fullscreenchange', sync);
  });
  async function toggleFullscreen(): Promise<void> {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await document.documentElement.requestFullscreen();
    } catch {
      /* fullscreen can be denied in embeds — the overlay still stands */
    }
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') onclose();
  }
</script>

<svelte:window onkeydown={handleKeydown} />

<div
  class="andon andon-{reading.state}"
  role="dialog"
  aria-modal="true"
  aria-label="Factory andon board: {reading.word} — {reading.caption}"
  use:focusTrap
>
  <header class="andon-bar">
    <span class="andon-brand">❖ mills · factory floor</span>
    <div class="andon-controls">
      <button type="button" class="andon-btn" onclick={toggleFullscreen}>
        {isFullscreen ? '⛶ exit fullscreen' : '⛶ fullscreen'}
      </button>
      <button type="button" class="andon-btn" onclick={onclose} aria-label="Close andon board">✕ close</button>
    </div>
  </header>

  <div class="lamp" role="status" aria-live="polite">
    <span class="lamp-word">{reading.word}</span>
    <span class="lamp-caption">{reading.caption}</span>
  </div>

  <div class="north">
    <div class="odometer" aria-label="{metrics?.pipeline_merged_runs ?? 0} bolts merged in 24 hours">
      {#each digits as d, i (i)}
        <span class="odo-digit" class:leading={i < firstSignificant} aria-hidden="true">
          <span class="odo-reel" style="--d: {d}">
            {#each [0, 1, 2, 3, 4, 5, 6, 7, 8, 9] as n (n)}
              <span class="odo-n">{n}</span>
            {/each}
          </span>
        </span>
      {/each}
    </div>
    <div class="north-label">bolts merged · 24h · autonomous</div>
  </div>

  <!-- Same vocabulary as the factory instrument rail — the TV and the
       desk must not teach two names for one thing. -->
  <div class="stats">
    <div class="stat"><span class="stat-num or">{activeRuns.length}</span><span class="stat-lbl">shuttles</span></div>
    <div class="stat"><span class="stat-num wr">{metrics?.pipeline_escalated_runs ?? 0}</span><span class="stat-lbl">sparks · 24h</span></div>
    <div class="stat"><span class="stat-num cy">{backlogActive}</span><span class="stat-lbl">on the beam</span></div>
    <div class="stat"><span class="stat-num">{pct(metrics?.gate_pass_rate)}</span><span class="stat-lbl">inspection</span></div>
    {#if fuel.frac !== null}
      <div class="stat">
        <span class="stat-num stat-fuel fuel-{fuel.tone}">{fuel.label}</span>
        <span class="stat-lbl">fuel · 24h</span>
      </div>
    {/if}
  </div>

  <footer class="andon-foot">
    <span class="foot-fresh" class:aging={freshnessAging || reading.state === 'stale'}>{freshness}</span>
    <span class="foot-clock">{clock}</span>
  </footer>
</div>

<style>
  .andon {
    position: fixed;
    inset: 0;
    z-index: var(--z-takeover);
    display: flex;
    flex-direction: column;
    background: var(--bg-primary);
    /* Per-state lamp color; every glowing element reads from these. */
    --lamp: var(--info);
    --lamp-rgb: var(--info-rgb);
  }
  .andon-weaving { --lamp: var(--success); --lamp-rgb: var(--success-rgb); }
  .andon-idle    { --lamp: var(--info);    --lamp-rgb: var(--info-rgb); }
  .andon-paused  { --lamp: var(--warning); --lamp-rgb: var(--warning-rgb); }
  .andon-storm,
  .andon-stale   { --lamp: var(--error);   --lamp-rgb: var(--error-rgb); }

  .andon-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-3) var(--space-5);
    flex-shrink: 0;
  }
  .andon-brand {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }
  .andon-controls { display: flex; gap: var(--space-2); }
  .andon-btn {
    background: none;
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    padding: var(--space-1) var(--space-3);
    cursor: pointer;
  }
  .andon-btn:hover { color: var(--fg-primary); border-color: var(--fg-muted); }

  .lamp {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-4) var(--space-5);
    background: rgba(var(--lamp-rgb), 0.1);
    border-block: 2px solid rgba(var(--lamp-rgb), 0.55);
    flex-shrink: 0;
  }
  /* A stale board must be unmistakable even peripherally: hazard stripes,
     not just a different hue. */
  .andon-stale .lamp {
    background: repeating-linear-gradient(
      -45deg,
      rgba(var(--lamp-rgb), 0.16) 0 24px,
      rgba(var(--lamp-rgb), 0.05) 24px 48px
    );
  }
  .lamp-word {
    font-size: clamp(2.4rem, 9vw, 7rem);
    font-weight: 800;
    line-height: 1;
    text-transform: uppercase;
    letter-spacing: 0.06em;
    color: var(--lamp);
    text-shadow: 0 0 32px rgba(var(--lamp-rgb), 0.55);
  }
  .andon-weaving .lamp-word { animation: andon-breathe 3.2s ease-in-out infinite; }
  .andon-storm .lamp-word,
  .andon-stale .lamp-word { animation: andon-urgent 1.6s ease-in-out infinite; }
  .lamp-caption {
    font-family: var(--font-mono);
    font-size: clamp(0.8rem, 1.8vw, 1.2rem);
    color: var(--fg-secondary);
  }

  .north {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: var(--space-3);
    min-height: 0;
  }
  .odometer {
    display: flex;
    gap: clamp(4px, 1vw, 12px);
  }
  .odo-digit {
    display: block;
    height: 1em;
    overflow: hidden;
    font-size: clamp(4rem, 16vw, 13rem);
    font-weight: 800;
    font-variant-numeric: tabular-nums;
    line-height: 1;
    font-family: var(--font-mono);
    color: var(--fg-primary);
    background: var(--bg-secondary);
    border: 1px solid var(--border);
    border-radius: clamp(4px, 0.8vw, 12px);
    padding-inline: clamp(6px, 1.2vw, 20px);
    text-shadow: 0 0 24px rgba(var(--lamp-rgb), 0.35);
  }
  .odo-reel {
    display: flex;
    flex-direction: column;
    transform: translateY(calc(var(--d) * -1em));
    transition: transform 0.7s cubic-bezier(0.22, 1, 0.36, 1);
  }
  .odo-n { display: block; height: 1em; line-height: 1; text-align: center; }
  /* Leading zeros hold their reel position but surrender the light — the
     eye lands on the significant digits from across the room. */
  .odo-digit.leading { color: var(--fg-dim); text-shadow: none; }
  .north-label {
    font-family: var(--font-mono);
    font-size: clamp(0.85rem, 2vw, 1.4rem);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .stats {
    display: flex;
    flex-wrap: wrap;
    justify-content: center;
    gap: clamp(var(--space-4), 5vw, var(--space-8));
    padding: var(--space-4) var(--space-5);
    flex-shrink: 0;
  }
  .stat { display: flex; flex-direction: column; align-items: center; gap: var(--space-1); }
  .stat-num {
    font-size: clamp(1.8rem, 5vw, 3.6rem);
    font-weight: 700;
    font-variant-numeric: tabular-nums;
    line-height: 1;
    color: var(--fg-primary);
  }
  .stat-num.cy { color: var(--info); text-shadow: var(--glow-shadow-lg) var(--glow-info); }
  .stat-num.or { color: var(--accent); text-shadow: var(--glow-shadow-lg) var(--glow-accent); }
  .stat-num.wr { color: var(--warning); text-shadow: var(--glow-shadow-lg) var(--glow-warning); }
  /* Fuel is a spent/ceiling pair, not a single count — mono keeps the
     slash pair legible at stat size, and the tone tracks the tank. */
  .stat-num.stat-fuel { font-family: var(--font-mono); font-size: clamp(1.3rem, 3.4vw, 2.6rem); }
  .stat-num.fuel-ok { color: var(--success); text-shadow: var(--glow-shadow-lg) var(--glow-success); }
  .stat-num.fuel-wr { color: var(--warning); text-shadow: var(--glow-shadow-lg) var(--glow-warning); }
  .stat-num.fuel-er { color: var(--error); }
  .stat-num.fuel-cy { color: var(--fg-secondary); }
  .stat-lbl {
    font-size: clamp(0.65rem, 1.3vw, 0.9rem);
    font-family: var(--font-mono);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
  }

  .andon-foot {
    display: flex;
    align-items: center;
    justify-content: space-between;
    padding: var(--space-3) var(--space-5);
    font-family: var(--font-mono);
    font-size: clamp(0.75rem, 1.5vw, 1rem);
    color: var(--fg-muted);
    border-top: 1px solid var(--border-subtle);
    flex-shrink: 0;
  }
  .foot-fresh.aging { color: var(--error); font-weight: 700; }

  @keyframes andon-breathe {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.72; }
  }
  @keyframes andon-urgent {
    0%, 100% { opacity: 1; }
    50% { opacity: 0.45; }
  }
  @media (prefers-reduced-motion: reduce) {
    .lamp-word { animation: none !important; }
    .odo-reel { transition: none; }
  }
</style>
