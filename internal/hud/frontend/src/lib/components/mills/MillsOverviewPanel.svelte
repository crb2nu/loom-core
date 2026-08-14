<script lang="ts">
  /**
   * MillsOverviewPanel — the Mills ▸ Overview tab (router sub-view
   * `mills-overview`): autonomy readiness, operational state, the Loom wiring
   * centerpiece, and a three-tile KPI strip that deep-links into Telemetry.
   *
   * Named OverviewPanel until the rename: there is a second, unrelated
   * OverviewPanel at components/OverviewPanel.svelte (the standalone top-level
   * "Now" view), and the two sharing a component name AND a router id made
   * every id-keyed lookup ambiguous.
   */
  import type {
    BudgetWindowUsage,
    MillsCapabilityRow,
    SystemHealth,
  } from '../../stores/mills.svelte.ts';
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { router } from '../../stores/router.svelte.ts';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { createPoller } from '../../utils/poller.ts';
  import {
    backendVariant,
    isFakeBackend,
    routeChain,
    sourceVariant,
    type MillsWiring,
    type WiringModelRoute,
  } from '../../utils/millsWiringHelpers.ts';
  import Badge from '../../widgets/Badge.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import EmptyState from '../shared/EmptyState.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import PanelShell from '../shared/PanelShell.svelte';
  import SpinningRoomCard from './SpinningRoomCard.svelte';
  import { fmtCost, fmtPct, fmtRunTime } from './shared/format.ts';

  // The shared mills poll keeps status / kpis / health fresh at 15s.
  $effect(() => {
    millsStore.startPolling(15000);
    return () => {
      millsStore.stopPolling();
    };
  });

  // Model-routing wiring changes only when the operator restarts, so it gets
  // its own SLOW poller instead of riding the 15s status tick. createPoller
  // pauses on a hidden tab and fires a catch-up tick when the tab becomes
  // visible again (window focus), so a backgrounded HUD stops re-pulling
  // routing it already has. No initial tick from the poller — kick one now.
  $effect(() => {
    void millsStore.fetchWiring();
    const poller = createPoller(() => millsStore.fetchWiring(), 60_000);
    poller.start();
    return () => poller.stop();
  });

  let status = $derived(millsStore.status);
  let policy = $derived(millsStore.policy);
  let kpis = $derived(millsStore.kpis);
  let capabilities = $derived(status?.capabilities ?? []);
  let requiredCaps = $derived(capabilities.filter((cap) => cap.required_for_autonomy));
  let requiredGreen = $derived(requiredCaps.filter((cap) => cap.status === 'green').length);
  let requiredTotal = $derived(requiredCaps.length);
  // Degraded dependencies: any capability the operator flags red/yellow. This
  // is the compact replacement for the old full capability grid — the state
  // strip surfaces "what's degraded" without the wall of green rows.
  let degradedCaps = $derived(
    capabilities.filter((cap) => cap.status === 'red' || cap.status === 'yellow'),
  );
  let loading = $derived(millsStore.loading && !status);
  let disabled = $derived(millsStore.disabled);
  let error = $derived(millsStore.error);
  let blockers = $derived(millsStore.autonomyBlockers);
  let isStale = $derived(millsStore.isStale && !disabled);
  let metrics = $derived(kpis?.metrics ?? {});
  let health = $derived(millsStore.systemHealth);
  // Suppress the in-flight/queue banner only when everything is genuinely
  // fine — green health is already conveyed by the "Autonomy ready" chip and
  // the operational-state strip, so a green banner would just be noise.
  let showBanner = $derived(health.state !== 'healthy');

  // Rolling-24h fuel: the operator's pipeline budget window (spend + runs vs
  // caps). This is the closest existing signal to a "rolling-24h escalation
  // budget" — the status payload has no escalation-specific budget field, so
  // we surface the pipeline tank and let the escalation-rate KPI carry the
  // failure signal. Rendered only when the operator returned a budget block.
  let pipelineBudget = $derived<BudgetWindowUsage | undefined>(status?.budget?.pipeline);

  // --- Loom wiring (centerpiece) ------------------------------------------
  // /api/mills/wiring is live on current operator builds. A 404 therefore means
  // the connected operator predates the route, which is an operator-version
  // fact worth stating — the card used to paper over it with a committed
  // sample fixture behind a "sample data" badge, which quietly presented a
  // 2026-vintage snapshot as this deployment's real routing.
  let wiringUnavailable = $derived(millsStore.wiringUnavailable && millsStore.wiring === null);
  let wiring = $derived<MillsWiring | null>(millsStore.wiring);
  let wiringError = $derived(millsStore.wiringError);

  // The KPI snapshot omits derived ratios when their denominator is 0 — the
  // operator only sets them once there's real activity (kpi_writer.go). So a
  // missing ratio means "idle" (no gates/merges/escalations in the window),
  // NOT an error — distinguish that from "no snapshot at all" so an idle Mills
  // never reads as a broken dashboard.
  let snapshotLoaded = $derived(!!kpis?.snapshot_at);

  /** {value,color,hint} for a derived-ratio KPI tile, idle/unknown-aware. */
  function ratioTile(
    raw: number | undefined,
    fmt: (r: number) => string,
    activeColor: (r: number) => string,
    idleHint: string,
  ): { value: string; color: string; hint: string } {
    if (typeof raw === 'number' && Number.isFinite(raw)) {
      return { value: fmt(raw), color: activeColor(raw), hint: '' };
    }
    return snapshotLoaded
      ? { value: '—', color: 'var(--fg-dim)', hint: idleHint }
      : { value: '—', color: 'var(--fg-dim)', hint: 'KPI snapshot unavailable — counters will populate after the next operator tick.' };
  }

  // The overview keeps at most three KPIs — the state summary, not a wall of
  // numbers. Each deep-links to the Telemetry panel, which owns the full
  // breakdown (stage waterfall, gate health, failure Pareto, model economics).
  let costTile = $derived(
    ratioTile(
      metrics.cost_per_merged_change_usd ?? metrics.cost_per_merged_pipeline_usd,
      fmtCost,
      () => 'var(--fg-primary)',
      'No merges in the last 24h (idle, not an error).',
    ),
  );
  let gateTile = $derived(
    ratioTile(
      metrics.gate_pass_rate,
      fmtPct,
      (r) => (r < 0.85 ? 'var(--warning)' : 'var(--success)'),
      'No gate evaluations in the last 24h (idle, not an error).',
    ),
  );
  let escTile = $derived(
    ratioTile(
      metrics.escalation_rate,
      fmtPct,
      (r) => (r > 0.4 ? 'var(--error)' : r > 0.2 ? 'var(--warning)' : 'var(--success)'),
      'No escalations in the last 24h (idle, not an error).',
    ),
  );

  function goto(subView: string): void {
    router.navigate('mills', subView);
  }

  // Sparks IS the escalation view since S6, so the deep link is the plain tab
  // with no detail segment. The old `('mills', 'backlog', 'state=escalated')`
  // form redirected to Warps carrying the third segment, which Warps then read
  // as a backlog id and opened a drawer for an item that does not exist.
  function gotoSparks(): void {
    router.navigate('mills', 'sparks');
  }

  function bannerHeadline(h: SystemHealth): string {
    switch (h.state) {
      case 'broken':
        return `${h.escalations_24h} escalated · ${h.merges_24h} merged in 24h`;
      case 'in_flight':
        return `${h.active_runs} ${h.active_runs === 1 ? 'shuttle' : 'shuttles'} in flight`;
      case 'idle':
        return 'Council has never run';
      default:
        return '';
    }
  }

  function bannerDetail(h: SystemHealth): string {
    switch (h.state) {
      case 'broken':
        return h.last_successful_merge_at
          ? `Last successful merge: ${fmtRunTime(h.last_successful_merge_at)}`
          : 'No successful merge on record';
      case 'in_flight':
        return h.queued > 0 ? `${h.queued} queued behind active runs` : 'Shuttles progressing';
      case 'idle':
        return scheduleDetail();
      default:
        return '';
    }
  }

  // scheduleDetail reads the cron string out of the policy raw blob if present
  // (PolicyView keeps the parsed shape narrow; schedule_cron lives in .raw).
  function scheduleDetail(): string {
    const raw = policy?.raw as { council?: { schedule_cron?: string } } | undefined;
    const cron = raw?.council?.schedule_cron;
    if (cron) return `Next scheduled: ${cron} UTC · or fire one now`;
    return 'Fire a council run to validate the runner end-to-end.';
  }

  function bannerActionLabel(h: SystemHealth): string {
    switch (h.state) {
      case 'broken': return 'View sparks';
      case 'in_flight': return 'Open shuttles';
      case 'idle': return councilRunning ? 'Running…' : 'Run council now';
      default: return '';
    }
  }

  let councilRunning = $state(false);
  let councilError = $state<string | null>(null);

  // Global autonomy kill-switch (plan 42 Slice 1b). The flip routes through a
  // GitOps auto-PR, so clicking pause/resume opens an MR rather than mutating
  // live state — the change lands once that MR merges + Flux reconciles.
  let autonomyEnabled = $derived(status?.policy_enabled ?? policy?.enabled ?? true);
  let killSwitchBusy = $state(false);

  // The confirm runs through the shared ConfirmDialog, not globalThis.confirm():
  // Chrome's "prevent this page from creating additional dialogs" checkbox makes
  // a native confirm() return false forever, which turned the kill switch into a
  // silent no-op.
  let confirmKill = $state<'pause' | 'resume' | null>(null);

  async function runKillSwitch(): Promise<void> {
    const action = confirmKill;
    if (!action || killSwitchBusy) {
      confirmKill = null;
      return;
    }
    killSwitchBusy = true;
    try {
      const res = await millsStore.setKillSwitch(action, 'hud-overview');
      if (!res) {
        toastStore.error('Kill-switch: no response from operator');
        return;
      }
      if (!res.changed) {
        toastStore.info(res.message);
        return;
      }
      if (res.mr_url) {
        globalThis.open(res.mr_url, '_blank', 'noopener');
        toastStore.show(`Opened MR to ${action} autonomy: ${res.mr_url}`, 'success', 8000);
      } else {
        toastStore.success(res.message);
      }
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      toastStore.error(`Kill-switch failed: ${msg}`);
    } finally {
      killSwitchBusy = false;
      confirmKill = null;
    }
  }

  async function runBannerAction(h: SystemHealth): Promise<void> {
    switch (h.state) {
      case 'broken':
        gotoSparks();
        return;
      case 'in_flight':
        goto('shuttles');
        return;
      case 'idle':
        if (councilRunning) return;
        councilRunning = true;
        councilError = null;
        try {
          await millsStore.runCouncil('hud-overview-idle');
        } catch (e) {
          councilError = e instanceof Error ? e.message : String(e);
        } finally {
          councilRunning = false;
        }
        return;
    }
  }

  // --- Operational-state helpers ------------------------------------------

  function opStateLabel(h: SystemHealth): string {
    switch (h.state) {
      case 'healthy': return 'Operational';
      case 'in_flight': return 'Running';
      case 'broken': return 'Degraded';
      case 'idle': return 'Idle';
      default: return h.state;
    }
  }

  function opStateVariant(h: SystemHealth): 'success' | 'info' | 'error' | 'muted' {
    switch (h.state) {
      case 'healthy': return 'success';
      case 'in_flight': return 'info';
      case 'broken': return 'error';
      default: return 'muted';
    }
  }

  function fmtBudget(b: BudgetWindowUsage): string {
    const spent = fmtCost(b.spent_usd);
    const cap = b.cap_usd > 0 ? fmtCost(b.cap_usd) : '∞';
    const runs = b.runs_cap > 0 ? `${b.runs}/${b.runs_cap}` : `${b.runs}`;
    return `${spent} / ${cap} · ${runs} runs`;
  }

  function depSummary(rows: MillsCapabilityRow[]): string {
    const names = rows.map((r) => r.id);
    const shown = names.slice(0, 3).join(', ');
    return names.length > 3 ? `${shown} +${names.length - 3}` : shown;
  }


  // Model line for a judge/weaver route: primary model + fallbacks joined as a
  // degrade chain. Kept as an array so the template can style the arrows.
  function chainOf(route: WiringModelRoute): string[] {
    return routeChain(route);
  }
</script>

<!-- No `count`: the header count reads as a row tally, and this panel has no
     rows. The required-capability score it used to show is spelled out in
     full as "Required caps N/M" below, where it can't be misread. -->
<PanelShell
  title="Mills Overview"
  icon="❖"
  loading={loading}
  empty={disabled || (!!error && !status)}
  emptyIcon={disabled ? '◯' : '!'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'Failed to load Mills status'}
  emptyHint={disabled ? 'LOOM_MILLS_OPERATOR_URL is not available to the HUD.' : (error ?? '')}
>
  {#snippet header()}
    <!-- When the operator is not configured (disabled), every header control
         would just error — hide the chrome and let PanelShell's "not
         configured" empty state be the single source of truth. -->
    {#if !disabled}
    {#if showBanner}
      <div
        class="system-health-banner intent-{health.state}"
        role={health.state === 'broken' ? 'alert' : 'status'}
        data-testid="system-health-banner"
        data-state={health.state}
      >
        <span class="banner-dot" aria-hidden="true"></span>
        <div class="banner-text">
          <span class="banner-headline">{bannerHeadline(health)}</span>
          <span class="banner-detail">{bannerDetail(health)}</span>
        </div>
        <button
          type="button"
          class="banner-action"
          onclick={() => runBannerAction(health)}
          disabled={health.state === 'idle' && councilRunning}
        >
          {bannerActionLabel(health)} →
        </button>
      </div>
      {#if councilError}
        <div class="council-error" role="alert">Council run failed: {councilError}</div>
      {/if}
    {/if}
    {#if error && status}
      <!-- Poll failed while a (possibly stale) status snapshot is still
           rendered — flag it instead of presenting stale data as fresh. -->
      <ErrorBanner prefix="Mills refresh failed" message={error} />
    {/if}
    <div class="overview-status" role="status">
      <div
        class="readiness-chip"
        class:ready={status?.autonomy_ready === true}
        class:blocked={status?.autonomy_ready === false}
      >
        <span class="chip-dot"></span>
        <span>{status?.autonomy_ready ? 'Autonomy ready' : status?.autonomy_ready === false ? 'Autonomy paused' : 'Checking autonomy'}</span>
      </div>
      <div class="status-meta">
        Policy {status?.policy_enabled || policy?.enabled ? 'enabled' : 'disabled'}
        <span class="meta-divider"></span>
        Required caps {requiredGreen}/{requiredTotal || 0}
        <span class="meta-divider"></span>
        Updated {fmtRunTime(status?.time)}
        {#if isStale}
          <span class="meta-divider"></span>
          <span class="stale-chip" title="No fresh data in over 90s — the operator may be unreachable.">⚠ stale</span>
        {/if}
      </div>
      <button
        type="button"
        class="kill-switch"
        class:pause={autonomyEnabled}
        class:resume={!autonomyEnabled}
        onclick={() => (confirmKill = autonomyEnabled ? 'pause' : 'resume')}
        disabled={killSwitchBusy || disabled}
        title={autonomyEnabled
          ? 'Pause autonomy via a GitOps merge request'
          : 'Resume autonomy via a GitOps merge request'}
      >
        {killSwitchBusy ? 'Opening MR…' : autonomyEnabled ? 'Pause autonomy' : 'Resume autonomy'}
      </button>
    </div>
    {/if}
  {/snippet}

  <div class="overview-layout">
    <!-- Operational state: one compact line — behavioral state, degraded
         dependencies, and the rolling-24h fuel tank. Not a grid of tiles. -->
    <section class="state-strip" aria-label="Operational state">
      <div class="state-item">
        <span class="state-key">State</span>
        <Badge text={opStateLabel(health)} variant={opStateVariant(health)} />
      </div>
      <div class="state-item">
        <span class="state-key">Deps</span>
        {#if degradedCaps.length === 0}
          <span class="state-val ok">all healthy</span>
        {:else}
          <span class="state-val warn" title={degradedCaps.map((c) => `${c.id}: ${c.status}`).join(' · ')}>
            {degradedCaps.length} degraded · {depSummary(degradedCaps)}
          </span>
        {/if}
      </div>
      {#if pipelineBudget}
        <div class="state-item">
          <span class="state-key">24h budget</span>
          <button
            type="button"
            class="state-val mono link"
            onclick={() => goto('telemetry')}
            title="Rolling-24h pipeline spend + runs against the active policy caps. The status payload has no escalation-specific budget — this is the pipeline tank."
          >{fmtBudget(pipelineBudget)}</button>
        </div>
      {/if}
    </section>

    {#if blockers.length > 0}
      <section class="blocker-strip" aria-label="Autonomy blockers">
        <span class="blocker-icon" aria-hidden="true">⚠</span>
        <span class="blocker-text">Autonomy blocked: {blockers.slice(0, 3).join(' · ')}{blockers.length > 3 ? ` +${blockers.length - 3}` : ''}</span>
        <button type="button" class="text-button" onclick={() => goto('policy')}>Policy</button>
      </section>
    {/if}

    <!-- CENTERPIECE: Loom wiring — the resolved model routing for the fleet. -->
    <section class="wiring-card" aria-label="Loom wiring">
      <div class="wiring-head">
        <div class="wiring-head-text">
          <span class="wiring-title">Loom wiring</span>
          <span class="wiring-sub">resolved model routing</span>
        </div>
        {#if wiring?.generated_at}
          <span class="wiring-generated mono">resolved {fmtRunTime(wiring.generated_at)}</span>
        {/if}
      </div>

      {#if wiringError && !wiring}
        <!-- Hard error with nothing cached: the banner carries the state; no
             spinner co-renders (house rule). -->
        <ErrorBanner prefix="Wiring feed failed" message={wiringError} />
      {:else if wiringUnavailable}
        <!-- 404 with nothing cached. Not an error and not a loading state:
             the connected operator simply doesn't serve this route. Say that
             plainly instead of spinning forever or showing stand-in data. -->
        <EmptyState
          compact
          icon={'◯'}
          heading="Wiring endpoint not available"
          description="The connected Mills operator does not serve GET /api/mills/wiring. Model routing will appear once it is upgraded."
        />
      {:else if !wiring}
        <div class="wiring-loading" role="status" aria-live="polite">Loading wiring…</div>
      {:else}
        {#if wiringError}
          <ErrorBanner prefix="Wiring refresh failed" message={wiringError} />
        {/if}

        <!-- Global status chips -->
        <div class="wiring-chips">
          <Badge
            text={wiring.litellm.configured ? 'LiteLLM configured' : 'LiteLLM off'}
            variant={wiring.litellm.configured ? 'accent' : 'muted'}
          />
          <Badge
            text={wiring.gates.llm_gates_enabled ? 'LLM gates on' : 'LLM gates off'}
            variant={wiring.gates.llm_gates_enabled ? 'success' : 'muted'}
          />
          {#if wiring.gates.tiebreaker}
            <Badge text={`tiebreaker: ${wiring.gates.tiebreaker}`} variant={backendVariant(wiring.gates.tiebreaker)} />
          {/if}
          <Badge
            text={wiring.policy.autonomy_enabled ? 'autonomy enabled' : 'autonomy paused'}
            variant={wiring.policy.autonomy_enabled ? 'success' : 'warning'}
          />
        </div>

        <!-- Judge + weaver routes with degrade chains -->
        <div class="route-list">
          <div class="route-row">
            <span class="route-role">Judge</span>
            <Badge text={wiring.judge.backend || '—'} variant={backendVariant(wiring.judge.backend)} />
            <span class="route-chain mono">
              {#each chainOf(wiring.judge) as m, i (m + i)}
                {#if i > 0}<span class="route-arrow" aria-hidden="true">→</span>{/if}<span class="route-model" class:primary={i === 0}>{m}</span>
              {/each}
              {#if chainOf(wiring.judge).length === 0}<span class="route-model muted">unset</span>{/if}
            </span>
            <span class="route-meta">
              {#if wiring.judge.max_tokens}<span class="route-tok mono" title="Judge max output tokens">{wiring.judge.max_tokens} tok</span>{/if}
              {#if wiring.judge.registry_fallbacks_disabled}<Badge text="registry fallbacks off" variant="muted" />{/if}
            </span>
          </div>
          <div class="route-row">
            <span class="route-role">Weaver</span>
            <Badge text={wiring.weaver.backend || '—'} variant={backendVariant(wiring.weaver.backend)} />
            <span class="route-chain mono">
              {#each chainOf(wiring.weaver) as m, i (m + i)}
                {#if i > 0}<span class="route-arrow" aria-hidden="true">→</span>{/if}<span class="route-model" class:primary={i === 0}>{m}</span>
              {/each}
              {#if chainOf(wiring.weaver).length === 0}<span class="route-model muted">unset</span>{/if}
            </span>
            <span class="route-meta">
              {#if wiring.weaver.max_tokens}<span class="route-tok mono">{wiring.weaver.max_tokens} tok</span>{/if}
            </span>
          </div>
        </div>

        <!-- Council: judge/editor + lens chips -->
        <div class="council-block">
          <div class="council-line">
            <span class="route-role">Council</span>
            <span class="council-pair">
              <span class="council-slot">judge</span>
              <Badge text={wiring.council.judge_backend || '—'} variant={backendVariant(wiring.council.judge_backend)} />
              <span class="route-model mono">{wiring.council.judge_model || 'unset'}</span>
            </span>
            <span class="council-pair">
              <span class="council-slot">editor</span>
              <Badge text={wiring.council.editor_backend || '—'} variant={backendVariant(wiring.council.editor_backend)} />
              <span class="route-model mono">{wiring.council.editor_model || 'unset'}</span>
            </span>
          </div>
          {#if wiring.council.lenses.length > 0}
            <div class="lens-row">
              <span class="council-slot">lenses</span>
              {#each wiring.council.lenses as lens, i (lens.name + i)}
                <span class="lens-chip" title={lens.model || lens.backend}>
                  <Badge
                    text={isFakeBackend(lens.backend) ? `${lens.name}: ${lens.backend || 'fake'} ⚠` : `${lens.name}: ${lens.backend}`}
                    variant={isFakeBackend(lens.backend) ? 'warning' : backendVariant(lens.backend)}
                  />
                </span>
              {/each}
            </div>
          {/if}
        </div>

        <!-- Stage routing table -->
        <div class="stage-block">
          <div class="stage-head">Stage routing</div>
          <div class="stage-table" role="table" aria-label="Stage routing">
            <div class="stage-row stage-th" role="row">
              <span role="columnheader">Stage</span>
              <span role="columnheader">Agent</span>
              <span role="columnheader">Model</span>
              <span role="columnheader">Source</span>
            </div>
            {#each wiring.stages as st, i (st.stage + i)}
              <div class="stage-row" role="row">
                <span class="stage-name mono" role="cell">{st.stage}</span>
                <span role="cell"><Badge text={st.agent || '—'} variant={backendVariant(st.agent)} /></span>
                <span class="stage-model mono" role="cell">{st.model || '(default)'}</span>
                <span role="cell"><Badge text={st.source || 'default'} variant={sourceVariant(st.source)} /></span>
              </div>
            {/each}
            {#if wiring.stages.length === 0}
              <div class="stage-empty">No per-stage overrides — every stage runs the spawn default.</div>
            {/if}
          </div>
          <div class="spawn-line">
            <span class="council-slot">spawn default</span>
            <Badge text={wiring.spawn.default_agent || '—'} variant={backendVariant(wiring.spawn.default_agent)} />
            {#if wiring.spawn.env_agent_override}<Badge text="agent override (env)" variant="accent" />{/if}
            {#if wiring.spawn.env_model_override}<Badge text="model override (env)" variant="accent" />{/if}
          </div>
        </div>
      {/if}
    </section>

    <!-- Compact KPI strip — state summary + jump-off to Telemetry, not a wall
         of numbers. Sourced from the existing KPI store (no re-fetch). -->
    <section class="kpi-strip" aria-label="Mills KPIs">
      <MetricCard
        label="Cost / merged"
        value={costTile.value}
        color={costTile.color}
        hint={costTile.hint}
        compact
        onclick={() => goto('telemetry')}
      />
      <MetricCard
        label="Gate pass"
        value={gateTile.value}
        color={gateTile.color}
        hint={gateTile.hint}
        compact
        onclick={() => goto('telemetry')}
      />
      <MetricCard
        label="Escalation rate"
        value={escTile.value}
        color={escTile.color}
        hint={escTile.hint}
        compact
        onclick={() => goto('telemetry')}
      />
    </section>

    <SpinningRoomCard {disabled} />
  </div>
</PanelShell>

<ConfirmDialog
  open={confirmKill !== null}
  title={confirmKill === 'pause' ? 'Pause Mills autonomy?' : 'Resume Mills autonomy?'}
  message="This opens a GitOps merge request flipping the policy kill-switch. It takes effect only after that MR merges and Flux reconciles."
  confirmLabel={confirmKill === 'pause' ? 'Pause autonomy' : 'Resume autonomy'}
  variant={confirmKill === 'pause' ? 'danger' : 'default'}
  onConfirm={runKillSwitch}
  onCancel={() => (confirmKill = null)}
/>

<style>
  .system-health-banner {
    display: grid;
    grid-template-columns: 10px minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-3);
    width: 100%;
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    margin-bottom: var(--space-3);
  }

  .system-health-banner.intent-broken {
    border-color: color-mix(in srgb, var(--error) 48%, var(--border));
    background: color-mix(in srgb, var(--error) 8%, var(--bg-tertiary));
    box-shadow: var(--glow-shadow-xl) var(--glow-error);
    color: var(--fg-primary);
  }

  .system-health-banner.intent-in_flight {
    border-color: color-mix(in srgb, var(--info) 38%, var(--border));
    background: color-mix(in srgb, var(--info) 6%, var(--bg-tertiary));
    box-shadow: var(--glow-shadow-xl) var(--glow-accent);
    color: var(--fg-primary);
  }

  .system-health-banner.intent-idle {
    border-color: color-mix(in srgb, var(--warning) 42%, var(--border));
    background: color-mix(in srgb, var(--warning) 8%, var(--bg-tertiary));
    color: var(--fg-primary);
  }

  .banner-dot {
    width: 10px;
    height: 10px;
    border-radius: 50%;
    background: var(--fg-dim);
    flex: 0 0 auto;
  }

  .system-health-banner.intent-broken .banner-dot {
    background: var(--error);
    box-shadow: var(--glow-shadow-lg) var(--glow-error);
    animation: banner-pulse 1.8s ease-in-out infinite;
  }

  .system-health-banner.intent-in_flight .banner-dot {
    background: var(--info);
    box-shadow: var(--glow-shadow-lg) var(--info-glow);
    animation: banner-pulse 2.4s ease-in-out infinite;
  }

  .system-health-banner.intent-idle .banner-dot {
    background: var(--warning);
    box-shadow: var(--glow-shadow-lg) var(--glow-warning);
  }

  @keyframes banner-pulse {
    0%, 100% { transform: scale(1); opacity: 1; }
    50%      { transform: scale(1.25); opacity: 0.85; }
  }

  .banner-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }

  .banner-headline {
    font-weight: 700;
    font-size: var(--text-sm);
    color: var(--fg-primary);
    letter-spacing: var(--tracking-tight);
  }

  .system-health-banner.intent-broken .banner-headline {
    color: var(--error);
  }

  .banner-detail {
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-family: var(--font-mono);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .banner-action {
    border: 1px solid var(--border);
    background: var(--bg-secondary);
    color: var(--fg-secondary);
    border-radius: var(--radius-sm);
    padding: 4px var(--space-3);
    font: inherit;
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    cursor: pointer;
    white-space: nowrap;
  }

  .banner-action:hover,
  .banner-action:focus-visible {
    color: var(--fg-primary);
    border-color: var(--border-focus);
    outline: none;
  }

  .banner-action[disabled] {
    cursor: progress;
    opacity: 0.65;
  }

  .council-error {
    margin-top: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid color-mix(in srgb, var(--error) 50%, var(--border));
    border-radius: var(--radius-sm);
    color: var(--error);
    font-size: var(--text-xs);
  }

  /* Global autonomy kill-switch. Pause reads as destructive (error-tinted);
     resume reads as recovery (success-tinted). */
  .kill-switch {
    align-self: flex-start;
    border: 1px solid var(--border);
    background: var(--bg-secondary);
    color: var(--fg-secondary);
    border-radius: var(--radius-sm);
    padding: 4px var(--space-3);
    font: inherit;
    font-size: var(--text-xs);
    font-weight: 600;
    letter-spacing: var(--tracking-wide);
    cursor: pointer;
    white-space: nowrap;
  }

  .kill-switch.pause {
    border-color: color-mix(in srgb, var(--error) 50%, var(--border));
    color: var(--error);
  }

  .kill-switch.resume {
    border-color: color-mix(in srgb, var(--success) 50%, var(--border));
    color: var(--success);
  }

  .kill-switch:hover,
  .kill-switch:focus-visible {
    border-color: var(--border-focus);
    outline: none;
  }

  .kill-switch[disabled] {
    cursor: progress;
    opacity: 0.65;
  }

  .system-health-banner.intent-broken .banner-action {
    border-color: color-mix(in srgb, var(--error) 50%, var(--border));
    color: var(--error);
  }

  .system-health-banner.intent-in_flight .banner-action {
    border-color: color-mix(in srgb, var(--info) 42%, var(--border));
    color: var(--info);
  }

  .system-health-banner.intent-idle .banner-action {
    border-color: color-mix(in srgb, var(--warning) 46%, var(--border));
    color: var(--warning);
  }

  @media (max-width: 720px) {
    .system-health-banner {
      grid-template-columns: 10px minmax(0, 1fr);
      grid-template-rows: auto auto;
    }

    .banner-action {
      grid-column: 1 / -1;
      justify-self: stretch;
    }

    .banner-detail {
      white-space: normal;
    }
  }

  .overview-status {
    display: flex;
    align-items: flex-start;
    flex-direction: column;
    gap: var(--space-3);
    min-width: 0;
  }

  .readiness-chip {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-height: 30px;
    padding: 4px var(--space-3);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    color: var(--fg-secondary);
    background: var(--bg-tertiary);
    font-weight: 700;
    font-size: var(--text-sm);
  }

  .readiness-chip.ready {
    border-color: color-mix(in srgb, var(--success) 38%, var(--border));
    color: var(--success);
    background: var(--success-dim);
  }

  .readiness-chip.blocked {
    border-color: color-mix(in srgb, var(--warning) 42%, var(--border));
    color: var(--warning);
    background: var(--warning-dim);
  }

  .chip-dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: currentColor;
    box-shadow: var(--glow-shadow-lg) currentColor;
    flex: 0 0 auto;
  }

  .status-meta {
    display: flex;
    align-items: center;
    justify-content: flex-start;
    flex-wrap: wrap;
    gap: var(--space-2);
    min-width: 0;
    color: var(--fg-muted);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .meta-divider {
    width: 1px;
    height: 14px;
    background: var(--border);
  }

  .stale-chip {
    color: var(--warning);
    font-weight: 700;
    letter-spacing: var(--tracking-wide);
  }

  .overview-layout {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: var(--space-4);
    align-items: start;
  }

  /* Operational-state strip */
  .state-strip {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2) var(--space-4);
    padding: var(--space-3) var(--space-4);
    border: 1px solid var(--border);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
  }

  .state-item {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .state-key {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    font-weight: 700;
  }

  .state-val {
    font-size: var(--text-sm);
    color: var(--fg-secondary);
  }

  .state-val.ok { color: var(--success); }
  .state-val.warn { color: var(--warning); }
  .state-val.mono { font-family: var(--font-mono); font-size: var(--text-xs); }

  .state-val.link {
    border: none;
    background: none;
    padding: 0;
    cursor: pointer;
    color: var(--fg-secondary);
    text-decoration: underline dotted color-mix(in srgb, var(--fg-muted) 60%, transparent);
    text-underline-offset: 3px;
  }
  .state-val.link:hover,
  .state-val.link:focus-visible {
    color: var(--accent);
    outline: none;
  }

  /* Blocker strip */
  .blocker-strip {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    padding: var(--space-2) var(--space-3);
    border: 1px solid color-mix(in srgb, var(--warning) 40%, var(--border));
    border-radius: var(--radius-md);
    background: var(--warning-dim);
    color: var(--warning);
    font-size: var(--text-sm);
  }
  .blocker-icon { flex: 0 0 auto; }
  .blocker-text { min-width: 0; flex: 1; overflow: hidden; text-overflow: ellipsis; }

  /* Loom wiring card (centerpiece) */
  .wiring-card {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-4);
    border: 1px solid color-mix(in srgb, var(--accent) 20%, var(--border));
    border-radius: var(--radius-md);
    background:
      radial-gradient(ellipse 80% 60% at 0% 0%, color-mix(in srgb, var(--accent) 6%, transparent), transparent 60%),
      var(--bg-secondary);
    min-width: 0;
  }

  .wiring-head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: var(--space-3);
    flex-wrap: wrap;
  }

  .wiring-head-text {
    display: inline-flex;
    align-items: baseline;
    gap: var(--space-2);
    flex-wrap: wrap;
    min-width: 0;
  }

  .wiring-title {
    font-size: var(--text-sm);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-primary);
  }

  .wiring-sub {
    font-size: var(--text-xs);
    color: var(--fg-muted);
    font-family: var(--font-mono);
  }

  .wiring-generated {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }

  .wiring-loading {
    padding: var(--space-3);
    color: var(--fg-muted);
    font-size: var(--text-sm);
  }

  .wiring-chips {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
  }

  .route-list {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-subtle);
  }

  .route-row {
    display: grid;
    grid-template-columns: 68px auto minmax(0, 1fr) auto;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .route-role {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    font-weight: 700;
  }

  .route-chain {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 4px;
    min-width: 0;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }

  .route-arrow {
    color: var(--fg-dim);
  }

  .route-model {
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .route-model.primary { color: var(--fg-primary); font-weight: 600; }
  .route-model.muted { color: var(--fg-dim); }

  .route-meta {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    justify-self: end;
    white-space: nowrap;
  }

  .route-tok {
    font-size: var(--text-2xs);
    color: var(--fg-muted);
  }

  .council-block,
  .stage-block {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-subtle);
  }

  .council-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2) var(--space-3);
  }

  .council-pair {
    display: inline-flex;
    align-items: center;
    gap: var(--space-2);
    min-width: 0;
  }

  .council-slot {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
  }

  .lens-row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }

  .lens-chip { display: inline-flex; }

  .stage-head {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-muted);
    font-weight: 700;
  }

  .stage-table {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }

  .stage-row {
    display: grid;
    grid-template-columns: minmax(0, 1.4fr) auto minmax(0, 1.4fr) auto;
    align-items: center;
    gap: var(--space-2);
    padding: 3px var(--space-2);
    border-radius: var(--radius-sm);
    background: color-mix(in srgb, var(--bg-tertiary) 55%, transparent);
  }

  .stage-row.stage-th {
    background: transparent;
    padding-bottom: 2px;
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    color: var(--fg-dim);
  }

  .stage-name {
    font-size: var(--text-xs);
    color: var(--fg-secondary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stage-model {
    font-size: var(--text-xs);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .stage-empty {
    padding: var(--space-2);
    color: var(--fg-muted);
    font-size: var(--text-xs);
    font-style: italic;
  }

  .spawn-line {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    padding-top: var(--space-1);
  }

  .text-button {
    border: 1px solid var(--border);
    background: var(--bg-tertiary);
    color: var(--fg-secondary);
    border-radius: var(--radius-sm);
    padding: 3px var(--space-2);
    font: inherit;
    font-size: var(--text-xs);
    cursor: pointer;
    white-space: nowrap;
  }

  .text-button:hover,
  .text-button:focus-visible {
    color: var(--fg-primary);
    border-color: var(--border-focus);
    outline: none;
  }

  .kpi-strip {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: var(--space-2);
  }

  .mono {
    font-family: var(--font-mono);
  }

  @media (max-width: 720px) {
    .route-row {
      grid-template-columns: 60px auto minmax(0, 1fr);
      grid-template-rows: auto auto;
    }
    .route-meta {
      grid-column: 2 / -1;
      justify-self: start;
    }
    .stage-row {
      grid-template-columns: minmax(0, 1fr) auto;
      grid-template-rows: auto auto;
    }
    .kpi-strip {
      grid-template-columns: 1fr;
    }
  }
</style>
