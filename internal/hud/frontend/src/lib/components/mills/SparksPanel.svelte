<script lang="ts">
  /**
   * SparksPanel — the broken picks that flew off the loom (spec §3.3).
   *
   * Surfaces every escalated run as a spark with *why* it flew (its failing
   * gate names + reasons, resolved lazily in the background) and a one-click
   * requeue. Active escalations come from the live run poll; today's struck
   * sparks come from the terminal archive (millsStore.fetchArchiveRuns — NEVER
   * pipelineHistory, which the loom diffs into weave events, house rule #9).
   *
   * Requeue reflects the full RequeueOutcome per row: a 409 ghost-spark
   * (already merged/done) and a 403 policy/token refusal render as their own
   * states, never a generic failure — mirroring PipelineRunDetail's inline
   * requeue banner (requeuePipelineRun is contractually no-throw).
   *
   * The "why" cells fill in asynchronously without ever flipping the panel to
   * loading: the table renders immediately, each cell shows "—" then fills
   * when its bounded gate fetch lands (mirrors ShiftReport, GATE_FETCH_MAX).
   */
  import { untrack } from 'svelte';
  import type { PipelineRun, RequeueOutcome } from '../../stores/mills.svelte.ts';
  import { millsStore } from '../../stores/mills.svelte.ts';
  import { priorityTone } from './shared/lineage.ts';
  import { runAdminAction } from './shared/millsActions.ts';
  import { toastStore } from '../../stores/toasts.svelte.ts';
  import { createPoller } from '../../utils/poller.ts';
  import Badge from '../../widgets/Badge.svelte';
  import DataTable from '../shared/DataTable.svelte';
  import FilterBar from '../shared/FilterBar.svelte';
  import MetricCard from '../shared/MetricCard.svelte';
  import PanelShell from '../shared/PanelShell.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import PipelineRunDetail from './PipelineRunDetail.svelte';
  import { fmtPct, shortRunID } from './shared/format.ts';
  import { relativeTime } from '../../utils/format.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';

  // Resolve failing gates for at most this many sparks per pass — a spark
  // storm must not fan out into dozens of detail fetches (mirrors ShiftReport).
  const GATE_FETCH_MAX = 8;

  // "why" enrichment: runID → its failing gate names + verbatim reasons. Filled
  // lazily in the background; a run absent from the map has not been resolved
  // yet (cell shows "—"), a run present with empty gates resolved clean.
  interface SparkWhy {
    gates: string[];
    reasons: string[];
  }
  let whyByRun = $state<Record<string, SparkWhy>>({});

  // Per-row requeue state, keyed by run ID so a late outcome can never render
  // under an unrelated row (the table re-orders as runs land/clear).
  interface RequeueState {
    busy: boolean;
    outcome?: RequeueOutcome;
  }
  let requeueByRun = $state<Record<string, RequeueState>>({});
  let requeueByCandidate = $state<Record<string, RequeueState>>({});

  // Filters.
  let search = $state('');
  let classFilter = $state('');
  let retryFilter = $state('');

  // --- Polling: shared 15s run poll + a slower archive refresh -------------
  // The archive (today's terminal sparks) changes slowly; refresh it on its
  // own 30s cadence. No initial tick from the poller (house rule #5) — the
  // explicit refresh below primes it before start.
  const archivePoller = createPoller(() => millsStore.refreshArchiveRuns(), 30000);
  const relaunchPoller = createPoller(() => millsStore.fetchRelaunchCandidates(), 60000);

  $effect(() => {
    millsStore.startPolling(15000);
    void millsStore.refreshArchiveRuns();
    void millsStore.fetchRelaunchCandidates();
    archivePoller.start(30000);
    relaunchPoller.start(60000);
    return () => {
      millsStore.stopPolling();
      archivePoller.stop();
      relaunchPoller.stop();
    };
  });

  // --- Async "why" enrichment ----------------------------------------------
  // Reacts to the set of sparks changing. For each not-yet-resolved spark (up
  // to the fetch budget) pull its stages+gates WITHOUT touching the drawer
  // cache and record its failing gates. All writes to whyByRun go through
  // untrack so this effect never re-triggers on its own writes (house rule #4).
  $effect(() => {
    const sparks = millsStore.escalatedRuns;
    let cancelled = false;
    void (async () => {
      let budget = GATE_FETCH_MAX;
      for (const run of sparks) {
        if (cancelled) return;
        if (budget <= 0) break;
        const already = untrack(() => run.ID in whyByRun);
        if (already) continue;
        budget--;
        const detail = await millsStore.fetchArchiveRunDetail(run.ID);
        if (cancelled) return;
        if (!detail) continue;
        const failed = (detail.gates ?? []).filter((g) => g.Outcome === 'fail');
        const gates = failed.map((g) => g.GateName);
        const reasons = failed.flatMap((g) => g.Reasons ?? []);
        untrack(() => {
          whyByRun = { ...whyByRun, [run.ID]: { gates, reasons } };
        });
      }
    })();
    return () => {
      cancelled = true;
    };
  });

  // --- Derivations ---------------------------------------------------------
  let sparks = $derived(millsStore.escalatedRuns);
  let disabled = $derived(millsStore.disabled);
  // A failed run poll is the panel's error; the archive refresh swallows its
  // own failures (it keeps last-good data), so it never red-flags the panel.
  let error = $derived(millsStore.error);
  let loading = $derived(millsStore.loading && millsStore.pipelineRuns.length === 0);
  let metrics = $derived(millsStore.kpis?.metrics);

  // Distinct escalation classes present, for the class filter dropdown. Built
  // from the runs themselves so the filter only offers classes that exist.
  let classOptions = $derived.by(() => {
    const seen = new Set<string>();
    for (const r of sparks) {
      const c = escalationClass(r);
      if (c) seen.add(c);
    }
    return [...seen].sort().map((c) => ({ value: c, label: c }));
  });

  let filtered = $derived.by(() => {
    const q = search.trim().toLowerCase();
    return sparks.filter((r) => {
      if (classFilter && escalationClass(r) !== classFilter) return false;
      if (retryFilter && retryBucket(r) !== retryFilter) return false;
      if (q) {
        const hay = `${r.ID} ${r.BacklogID} ${escalationClass(r)}`.toLowerCase();
        if (!hay.includes(q)) return false;
      }
      return true;
    });
  });

  // Header tally: N on the floor now (active escalated/paused) + today's
  // archived strikes, split infra-vs-real from the escalation class.
  let activeCount = $derived(
    (millsStore.pipelineRuns ?? []).filter((r) => isSparkLive(r.State)).length,
  );
  let struckToday = $derived(
    (millsStore.archiveRuns ?? []).filter((r) => (r.State ?? '').toLowerCase() === 'escalated'),
  );
  let infraToday = $derived(struckToday.filter((r) => isInfraClass(escalationClass(r))).length);
  let realToday = $derived(struckToday.length - infraToday);

  const columns = [
    { key: 'run', label: 'Run' },
    { key: 'warp', label: 'Warp' },
    { key: 'class', label: 'Class' },
    { key: 'retry', label: 'Retry?', align: 'center' as const },
    { key: 'why', label: 'Why (failing gate)', hideBelow: 720 },
    { key: 'mr', label: 'MR' },
    { key: 'act', label: '', align: 'right' as const },
  ];

  // --- Helpers -------------------------------------------------------------
  function isSparkLive(state: string | undefined): boolean {
    const s = (state ?? '').toLowerCase();
    return s === 'escalated' || s === 'paused';
  }

  // The runner's error-class spelling wins; fall back to the policy-facing
  // taxonomy, then a stable sentinel so a classless spark still filters.
  function escalationClass(r: PipelineRun): string {
    return r.EscalationClass || r.FailureClass || 'unclassified';
  }

  // infra-vs-real split (memory: escalation de-noise) — infra/transient faults
  // are noise the operator can requeue through; the rest are real work.
  function isInfraClass(c: string): boolean {
    const k = c.toLowerCase();
    return (
      k === 'infra' ||
      k === 'transient' ||
      k === 'transient_quota' ||
      k === 'external' ||
      k === 'quota'
    );
  }

  // EscalationRetryable is *bool on the wire: null/absent is a genuine third
  // state (the run predates the column / carried no marker), not "no".
  function retryBucket(r: PipelineRun): 'yes' | 'no' | 'unknown' {
    if (r.EscalationRetryable == null) return 'unknown';
    return r.EscalationRetryable ? 'yes' : 'no';
  }

  function warpItem(backlogID: string) {
    if (!backlogID) return undefined;
    return (millsStore.backlog ?? []).find((b) => b.ID === backlogID);
  }

  function openRun(id: string): void {
    millsStore.openRunDetail(id);
  }

  async function copyMR(iid: number): Promise<void> {
    try {
      await navigator.clipboard.writeText(`!${iid}`);
      toastStore.info(`Copied !${iid}`);
    } catch {
      // Clipboard can be unavailable in odd embeds — silently no-op.
    }
  }

  // Requeue one spark. requeuePipelineRun is no-throw and returns a 4-way
  // RequeueOutcome; we reflect it inline per row AND toast it at the matching
  // severity, so a 409 ghost-spark reads as "already completed" (info) and a
  // 403 policy/token refusal reads as forbidden — never a generic failure.
  // runAdminAction wraps only the genuinely-failed kinds (forbidden/error) so
  // its toast + error styling fire there; started/conflict are handled inline.
  async function performRequeue(backlogID: string): Promise<RequeueOutcome | undefined> {
    let outcome: RequeueOutcome | undefined;
    await runAdminAction(
      async () => {
        outcome = await millsStore.requeuePipelineRun(backlogID);
        if (outcome.kind === 'started') {
          toastStore.success(outcome.message);
          return;
        }
        if (outcome.kind === 'conflict') {
          // Ghost spark (already merged/done) is not a failure to chase.
          (outcome.alreadyCompleted ? toastStore.info : toastStore.warning).call(
            toastStore,
            outcome.message,
          );
          return;
        }
        // forbidden / error: throw so runAdminAction emits the error toast.
        throw new Error(outcome.message);
      },
      { success: '', failurePrefix: 'Requeue' },
    );
    return outcome;
  }

  async function requeue(run: PipelineRun): Promise<void> {
    const runID = run.ID;
    requeueByRun = { ...requeueByRun, [runID]: { busy: true } };
    const outcome = await performRequeue(run.BacklogID);
    requeueByRun = { ...requeueByRun, [runID]: { busy: false, outcome } };
  }

  async function requeueCandidate(backlogID: string): Promise<void> {
    requeueByCandidate = { ...requeueByCandidate, [backlogID]: { busy: true } };
    const outcome = await performRequeue(backlogID);
    requeueByCandidate = { ...requeueByCandidate, [backlogID]: { busy: false, outcome } };
  }

  function fmtCount(v: number | undefined): string {
    if (v == null || !Number.isFinite(v)) return '—';
    return String(v);
  }

  let filters = $derived([
    {
      key: 'class',
      label: 'Class',
      value: classFilter,
      options: classOptions,
    },
    {
      key: 'retry',
      label: 'Retryable',
      value: retryFilter,
      options: [
        { value: 'yes', label: 'retryable' },
        { value: 'no', label: 'not retryable' },
        { value: 'unknown', label: 'unmarked' },
      ],
    },
  ]);

  function onFilter(key: string, value: string): void {
    if (key === 'class') classFilter = value;
    else if (key === 'retry') retryFilter = value;
  }

  function clearFilters(): void {
    search = '';
    classFilter = '';
    retryFilter = '';
  }
</script>

<PanelShell
  title="Sparks"
  icon="⚡"
  count={sparks.length}
  {loading}
  error={error && sparks.length === 0 ? error : null}
  errorHeading="Couldn't read the floor"
  empty={!error
    && sparks.length === 0
    && !millsStore.relaunchCandidatesLoading
    && !millsStore.relaunchCandidatesError
    && millsStore.relaunchCandidates.length === 0}
  emptyIcon={disabled ? '◯' : '✨'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'no sparks — every pick landed clean'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'Escalated runs land here the instant a pick breaks. The floor is clean.'}
  emptyTone={disabled ? 'disabled' : 'ready'}
>
  {#snippet actions()}
    <!-- Today's strike split rides in the header rather than as small body
         text: the infra-vs-real ratio is how an operator decides whether the
         floor needs a human or a requeue. Rendered here ONLY — the body tally
         below carries the live floor count, so neither number is repeated. -->
    <span class="struck-today" role="status" aria-live="polite">
      <span class="struck-count">{struckToday.length}</span>
      <span class="struck-label">struck today</span>
      {#if struckToday.length > 0}
        <span class="struck-split">{infraToday} infra · {realToday} real</span>
      {/if}
    </span>
  {/snippet}

  <LineageRibbon mode="spine" segments={millsStore.millFloorSpine} current="sparks" />

  <div class="spark-tally" role="status" aria-live="polite">
    <span class="tally-lead">⚡ {activeCount} spark{activeCount === 1 ? '' : 's'} on the floor</span>
  </div>

  <div class="spark-kpis">
    <MetricCard
      label="escalated (24h)"
      value={fmtCount(metrics?.pipeline_escalated_runs)}
      color="var(--warning)"
    />
    <MetricCard
      label="escalation rate"
      value={fmtPct(metrics?.escalation_rate)}
      color="var(--warning)"
    />
    <MetricCard
      label="merged (24h)"
      value={fmtCount(metrics?.pipeline_merged_runs)}
      color="var(--success)"
    />
  </div>

  <section class="relaunch-queue" aria-labelledby="relaunch-queue-title">
    <h3 id="relaunch-queue-title">relaunch queue</h3>
    {#if millsStore.relaunchCandidatesLoading && millsStore.relaunchCandidates.length === 0}
      <p class="queue-state">Loading relaunch candidates…</p>
    {:else if millsStore.relaunchCandidatesError}
      <p class="queue-state queue-unavailable" role="status">Relaunch queue unavailable.</p>
    {:else if millsStore.relaunchCandidates.length === 0}
      <p class="queue-state">no relaunch candidates</p>
    {:else}
      <ul class="queue-list">
        {#each millsStore.relaunchCandidates as candidate (candidate.backlogId)}
          {@const rq = requeueByCandidate[candidate.backlogId]}
          <li>
            <span class="mono queue-id">{candidate.backlogId}</span>
            <Badge text={candidate.escalationClass || candidate.failureClass || 'unclassified'} variant="warning" />
            <span class="queue-age" title={candidate.latestRunEndedAt ?? ''}>{relativeTime(candidate.latestRunEndedAt)}</span>
            <div class="act-wrap">
              <button
                type="button"
                class="requeue-btn"
                disabled={rq?.busy || rq?.outcome?.kind === 'started'}
                onclick={() => void requeueCandidate(candidate.backlogId)}
                title={`Requeue ${candidate.backlogId}`}
              >{rq?.busy ? 'Requeuing…' : '↻ Requeue'}</button>
              {#if rq?.outcome}
                <span
                  class="requeue-result requeue-{rq.outcome.kind}"
                  class:already={rq.outcome.kind === 'conflict' && rq.outcome.alreadyCompleted}
                  role="status"
                  aria-live="polite"
                  title={rq.outcome.message}
                >{rq.outcome.message}</span>
              {/if}
            </div>
          </li>
        {/each}
      </ul>
    {/if}
  </section>

  <div class="spark-toolbar">
    <FilterBar
      {search}
      placeholder="Search sparks by run, backlog, or class…"
      {filters}
      resultCount={filtered.length}
      onSearch={(v) => (search = v)}
      {onFilter}
      onClear={clearFilters}
    />
  </div>

  {#if filtered.length === 0}
    <p class="spark-nomatch">No sparks match this filter.</p>
  {:else}
    <DataTable
      {columns}
      rows={filtered}
      idKey="ID"
      rowLabel="spark"
      onRowClick={(r) => openRun(r.ID)}
    >
      {#snippet row({ row: r, hiddenColumns })}
        {@const item = warpItem(r.BacklogID)}
        {@const why = whyByRun[r.ID]}
        {@const rq = requeueByRun[r.ID]}
        {@const retry = retryBucket(r)}
        <td class="mono run-cell" title={r.ID}>
          <span class="run-id">{shortRunID(r.ID)}</span>
          {#if (r.State ?? '').toLowerCase() === 'paused'}
            <span class="state-pill">paused</span>
          {/if}
        </td>
        <td class="warp-cell">
          {#if item?.Priority}
            <Badge text={item.Priority} variant={priorityTone(item.Priority)} />
          {/if}
          <span class="mono warp-id" title={r.BacklogID}>{r.BacklogID || '—'}</span>
        </td>
        <td class="class-cell">
          <Badge text={escalationClass(r)} variant={isInfraClass(escalationClass(r)) ? 'info' : 'error'} />
          {#if r.ExternalDependency}
            <span class="ext-dep" title="known upstream incident">{r.ExternalDependency}</span>
          {/if}
        </td>
        <td class="retry-cell" style="text-align:center">
          {#if retry === 'yes'}
            <span class="retry retry-yes" title="operator marked this retryable">yes</span>
          {:else if retry === 'no'}
            <span class="retry retry-no" title="operator marked this NOT retryable">no</span>
          {:else}
            <span class="retry retry-unknown" title="no retryable marker on this run">—</span>
          {/if}
        </td>
        {#if !hiddenColumns.has('why')}
          <td class="why-cell">
            {#if !why}
              <span class="why-pending" aria-label="resolving failing gate">—</span>
            {:else if why.gates.length === 0}
              <span class="why-clean">no failing gate recorded</span>
            {:else}
              <span class="why-gate">{why.gates.join(', ')}</span>
              {#if why.reasons.length > 0}
                <span class="why-reason" title={why.reasons.join(' · ')}>{why.reasons[0]}</span>
              {/if}
            {/if}
          </td>
        {/if}
        <td class="mr-cell mono">
          {#if r.MRIID != null}
            <span class="mr-affordances">
              <a
                class="mr-chip"
                href={mrURL(item?.TargetProject, r.MRIID)}
                target="_blank"
                rel="noreferrer noopener"
                title={`Open merge request !${r.MRIID}`}
                onclick={(e) => e.stopPropagation()}
              >!{r.MRIID}</a>
              <button
                type="button"
                class="mr-copy"
                aria-label={`Copy merge request !${r.MRIID}`}
                title="Copy MR reference"
                onclick={(e) => { e.stopPropagation(); void copyMR(r.MRIID as number); }}
              >⎘</button>
            </span>
          {:else}
            <span class="mr-empty">—</span>
          {/if}
        </td>
        <td class="act-cell" style="text-align:right">
          <div class="act-wrap">
            <button
              type="button"
              class="requeue-btn"
              disabled={rq?.busy || rq?.outcome?.kind === 'started'}
              onclick={(e) => { e.stopPropagation(); void requeue(r); }}
              title="Requeue this escalated item — flips it back to queued and starts a fresh run"
            >
              {rq?.busy ? 'Requeuing…' : '↻ Requeue'}
            </button>
            {#if rq?.outcome}
              <span
                class="requeue-result requeue-{rq.outcome.kind}"
                class:already={rq.outcome.kind === 'conflict' && rq.outcome.alreadyCompleted}
                role="status"
                aria-live="polite"
                title={rq.outcome.message}
              >{rq.outcome.message}</span>
            {/if}
          </div>
        </td>
      {/snippet}
    </DataTable>
  {/if}
</PanelShell>

<PipelineRunDetail />

<style>
  .spark-tally {
    display: flex;
    align-items: baseline;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin: var(--space-3) 0 var(--space-2);
    font-size: var(--text-sm);
    color: var(--fg-secondary);
  }
  .tally-lead { font-weight: 600; color: var(--warning); }

  /* Header strike tally. Escalation tone (warning) matches the spark
     vocabulary everywhere else on the floor. */
  .struck-today {
    display: inline-flex;
    align-items: baseline;
    gap: var(--space-2);
    font-size: var(--text-xs);
    color: var(--fg-muted);
    white-space: nowrap;
  }
  .struck-count {
    font-family: var(--font-mono);
    font-size: var(--text-sm);
    font-weight: 700;
    color: var(--warning);
    font-variant-numeric: tabular-nums;
  }
  .struck-label { color: var(--fg-secondary); }
  .struck-split {
    padding: 1px 6px;
    border-radius: var(--radius-full);
    border: 1px solid var(--border-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
  }

  .spark-kpis {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin-bottom: var(--space-3);
  }
  .spark-kpis :global(.metric-card) {
    flex: 1 1 140px;
    min-width: 120px;
  }

  .spark-toolbar { margin-bottom: var(--space-2); }

  .relaunch-queue {
    margin: var(--space-3) 0;
    padding: var(--space-3);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-subtle);
  }
  .relaunch-queue h3 {
    margin: 0 0 var(--space-2);
    color: var(--fg-muted);
    font-size: var(--text-2xs);
    font-weight: 600;
    letter-spacing: 0.08em;
    text-transform: uppercase;
  }
  .queue-state { margin: 0; color: var(--fg-muted); font-size: var(--text-sm); }
  .queue-unavailable { color: var(--warning); }
  .queue-list { display: grid; gap: var(--space-2); margin: 0; padding: 0; list-style: none; }
  .queue-list li { display: flex; align-items: center; gap: var(--space-2); }
  .queue-id { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; }
  .queue-age { color: var(--fg-muted); font-size: var(--text-xs); white-space: nowrap; }

  .spark-nomatch {
    padding: var(--space-4);
    text-align: center;
    color: var(--fg-muted);
    font-size: var(--text-sm);
  }

  .mono { font-family: var(--font-mono); }

  .run-cell { white-space: nowrap; }
  .run-id { color: var(--fg-primary); }
  .state-pill {
    margin-left: var(--space-1);
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    font-size: var(--text-2xs);
    background: color-mix(in srgb, var(--warning) 12%, transparent);
    color: var(--warning);
  }

  .warp-cell {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    white-space: nowrap;
  }
  .warp-id { color: var(--fg-muted); font-size: var(--text-xs); }

  .class-cell {
    display: flex;
    align-items: center;
    gap: var(--space-2);
    flex-wrap: wrap;
  }
  .ext-dep {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    color: var(--fg-muted);
    padding: 1px 5px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border-subtle);
  }

  .retry {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    padding: 1px 6px;
    border-radius: var(--radius-full);
  }
  .retry-yes { color: var(--success); background: color-mix(in srgb, var(--success) 12%, transparent); }
  .retry-no { color: var(--error); background: color-mix(in srgb, var(--error) 12%, transparent); }
  .retry-unknown { color: var(--fg-muted); }

  .why-cell { max-width: 34ch; }
  .why-pending { color: var(--fg-dim); }
  .why-clean { color: var(--fg-muted); font-size: var(--text-xs); }
  .why-gate {
    color: var(--warning);
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    font-weight: 600;
  }
  .why-reason {
    display: block;
    color: var(--fg-muted);
    font-size: var(--text-2xs);
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .mr-affordances { display: inline-flex; align-items: center; gap: 3px; }
  .mr-chip {
    background: color-mix(in srgb, var(--mills) 14%, transparent);
    color: var(--mills);
    border: 1px solid color-mix(in srgb, var(--mills) 32%, transparent);
    border-radius: var(--radius-sm);
    padding: 1px 6px;
    font-family: var(--font-mono);
    font-size: var(--text-xs);
    cursor: pointer;
    text-decoration: none;
  }
  .mr-chip:hover { background: color-mix(in srgb, var(--mills) 24%, transparent); }
  .mr-chip:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 1px; }
  .mr-copy {
    padding: 1px 3px;
    color: var(--fg-muted);
    background: transparent;
    border: 0;
    cursor: pointer;
  }
  .mr-copy:hover { color: var(--mills); }
  .mr-empty { color: var(--fg-muted); }

  .act-cell { white-space: nowrap; }
  .act-wrap {
    display: inline-flex;
    flex-direction: column;
    align-items: flex-end;
    gap: 3px;
  }
  .requeue-btn {
    background: color-mix(in srgb, var(--info) 12%, transparent);
    color: var(--info);
    border: 1px solid color-mix(in srgb, var(--info) 32%, transparent);
    border-radius: var(--radius-sm);
    padding: 2px 8px;
    font-size: var(--text-xs);
    cursor: pointer;
    white-space: nowrap;
  }
  .requeue-btn:hover:not(:disabled) { background: color-mix(in srgb, var(--info) 22%, transparent); }
  .requeue-btn:disabled { opacity: 0.5; cursor: not-allowed; }
  .requeue-btn:focus-visible { outline: 2px solid var(--focus-ring); outline-offset: 1px; }

  .requeue-result {
    max-width: 26ch;
    font-size: var(--text-2xs);
    line-height: 1.3;
    text-align: right;
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .requeue-started { color: var(--success); }
  /* A ghost-spark 409 (already merged/done) is benign — muted, not alarming.
     A non-ghost conflict still reads as a warning. */
  .requeue-conflict { color: var(--warning); }
  .requeue-conflict.already { color: var(--fg-muted); }
  .requeue-forbidden,
  .requeue-error { color: var(--error); }
</style>
