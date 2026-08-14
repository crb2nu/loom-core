<script lang="ts">
  import { millsStore, type CouncilAgent } from '../../stores/mills.svelte.ts';
  import { flexinferModelsStore, type ModelStatus } from '../../stores/flexinfer_models.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import ErrorBanner from '../shared/ErrorBanner.svelte';
  import { runAdminAction } from './shared/millsActions.ts';
  import { elapsedMs, fmtCostExact, fmtDuration, fmtRunTime } from './shared/format.ts';

  $effect(() => {
    millsStore.startPolling(15000);
    flexinferModelsStore.startPolling();
    return () => {
      millsStore.stopPolling();
      flexinferModelsStore.stopPolling();
    };
  });

  let runs = $derived(millsStore.councilRuns);
  let policy = $derived(millsStore.policy);
  let loading = $derived(millsStore.loading && millsStore.councilRuns.length === 0);
  let disabled = $derived(millsStore.disabled);
  let error = $derived(millsStore.error);

  // Per-run expansion state. Toggling a row triggers a lazy
  // loadDebate() that caches in millsStore.debateByRun, so subsequent
  // expansions on the same row are instant.
  let expanded = $state<Record<string, boolean>>({});

  function toggle(runID: string): void {
    expanded = { ...expanded, [runID]: !expanded[runID] };
    if (expanded[runID]) {
      void millsStore.loadDebate(runID);
    }
  }

  // Day-2 admin actions (plan 42 Slice 1): replay a real council pass or
  // a safe dryrun without dropping to `loom mills council run/dryrun`.
  let replaying = $state(false);
  let dryrunning = $state(false);
  let confirmReplay = $state(false);

  async function doReplay(): Promise<void> {
    confirmReplay = false;
    replaying = true;
    await runAdminAction(() => millsStore.runCouncil('hud-council-panel'), {
      success: 'Council run triggered',
      failurePrefix: 'Council run failed',
    });
    replaying = false;
  }

  async function doDryrun(): Promise<void> {
    dryrunning = true;
    await runAdminAction(() => millsStore.dryrunCouncil('hud-council-panel'), {
      success: 'Council dryrun complete (scratch DB, no commits)',
      failurePrefix: 'Council dryrun failed',
    });
    dryrunning = false;
  }


  // The live /api/mills/council/runs payload splits spend by tier —
  // CostFrontierUSD + CostLocalUSD — while the store's CouncilRun type still
  // only declares the pre-split CostUSD. Reading CostUSD alone rendered '—'
  // on every row. Widen locally (the store type is owned elsewhere) and sum
  // the tiers; fall back to CostUSD for rows written before the split.
  type RunCost = { CostUSD?: number; CostFrontierUSD?: number; CostLocalUSD?: number };

  function totalCostOf(r: RunCost): number | undefined {
    const tiers = [r.CostFrontierUSD, r.CostLocalUSD].filter(
      (v): v is number => typeof v === 'number' && Number.isFinite(v),
    );
    if (tiers.length > 0) return tiers.reduce((a, b) => a + b, 0);
    // No tier fields at all → an old row. Absent stays '—'; a real $0 that
    // the operator did report still renders as $0.0000.
    return Number.isFinite(r.CostUSD as number) ? r.CostUSD : undefined;
  }
  // Anything that "completes" in under a second almost always means
  // the run crashed before doing any work. Tag the row so the operator
  // can spot it without expanding to read the debate transcript.
  function isSuspiciouslyInstant(r: { StartedAt?: string; EndedAt?: string; Outcome?: string }): boolean {
    const ms = elapsedMs(r.StartedAt, r.EndedAt);
    return ms != null && ms < 1000;
  }

  // The /api/mills/policy endpoint returns the full Policy struct; we
  // extract just the council ensemble lazily so unrelated panels don't
  // pay the deserialization cost. Returns null when the operator hasn't
  // wired the council section yet.
  let ensemble = $derived.by(() => {
    const raw = policy?.raw as { council?: { ensemble?: { editor?: CouncilAgent; reviewers?: CouncilAgent[]; judge?: CouncilAgent } } } | undefined;
    return raw?.council?.ensemble ?? null;
  });

  function modelStatusTitle(status: ModelStatus, model: string): string {
    switch (status) {
      case 'ready':   return `${model}: Ready in flexinfer-system — accepting requests.`;
      case 'idle':    return `${model}: Idle / scaled-to-zero — first request will cold-start (or fail if the probe is offline).`;
      case 'unknown': return `${model}: not present in the flexinfer-system /v1/models registry, OR the registry hasn't loaded yet. If it's the former, every dispatch to this agent will 404.`;
    }
  }
  function roleLabel(role: string): string {
    switch (role) {
      case 'editor_proposes': return 'Editor proposes';
      case 'reviewer_critiques': return 'Reviewer critiques';
      case 'moderator_decision': return 'Moderator decision';
      case 'editor_revises': return 'Editor revises';
      default: return role;
    }
  }
</script>

<PanelShell
  title="Drawing Office — Council"
  icon="◇"
  count={runs.length}
  loading={loading}
  error={!disabled && error && runs.length === 0 ? error : null}
  errorHeading="Couldn't load council runs"
  empty={!error && runs.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : 'No council runs yet'}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : 'The council fires on cron + roadmap events.'}
  emptyTone={disabled ? 'disabled' : 'idle'}
>
  {#snippet header()}
    <div class="policy-row">
      <span class="kill-switch" class:enabled={policy?.enabled}>
        kill switch: <strong>{policy?.enabled ? 'enabled' : 'disabled'}</strong>
      </span>
      {#if policy?.version != null}
        <span class="policy-version">policy v{policy.version}</span>
      {/if}
    </div>
  {/snippet}

  {#snippet actions()}
    <button
      type="button"
      class="mills-action-btn secondary"
      disabled={disabled || dryrunning}
      onclick={doDryrun}
      title="Run the current ensemble against a scratch DB — no commits, no backlog writes"
    >
      {dryrunning ? 'Dryrun…' : 'Dryrun'}
    </button>
    <button
      type="button"
      class="mills-action-btn primary"
      disabled={disabled || replaying}
      onclick={() => (confirmReplay = true)}
      title="Fire a real council pass now (may create backlog items)"
    >
      {replaying ? 'Running…' : 'Replay council'}
    </button>
  {/snippet}

  {#if error && runs.length > 0}
    <ErrorBanner prefix="Council refresh failed" message={error} />
  {/if}

  <div class="mills-table-wrap">
  <table class="mills-table">
    <thead>
      <tr>
        <th aria-label="expand"></th>
        <th>Run ID</th>
        <th>Trigger</th>
        <th>Outcome</th>
        <th>Cost</th>
        <th>Duration</th>
        <th>Started</th>
        <th>Ended</th>
      </tr>
    </thead>
    <tbody>
      {#each runs as r (r.ID)}
        {@const isOpen = !!expanded[r.ID]}
        {@const debate = millsStore.debateByRun[r.ID]}
        {@const instant = isSuspiciouslyInstant(r)}
        {@const debateBroken = debate && debate.status === 'error'}
        <tr
          class="row-summary"
          class:row-suspicious={instant || debateBroken}
          role="button"
          tabindex="0"
          aria-expanded={isOpen}
          aria-label={`Toggle run ${r.ID}`}
          onclick={() => toggle(r.ID)}
          onkeydown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              toggle(r.ID);
            }
          }}
        >
          <td class="expander">
            <span class="caret" class:open={isOpen} aria-hidden="true">▸</span>
          </td>
          <td class="mono" title={r.ID}><span class="cell-cap">{r.ID}</span></td>
          <td>{r.Trigger}</td>
          <td>
            <span class="outcome outcome-{r.Outcome}">{r.Outcome}</span>
            {#if instant}
              <span class="badge badge-warn" title="Started and ended within 1 second — usually means the run crashed before doing any work">instant</span>
            {/if}
            {#if debateBroken}
              <span class="badge badge-err" title="Debate transcript failed to load — expand for details">debate ✕</span>
            {/if}
          </td>
          <td title="frontier + local model spend">{fmtCostExact(totalCostOf(r))}</td>
          <td class="mono dur" class:dur-instant={instant}>{fmtDuration(elapsedMs(r.StartedAt, r.EndedAt))}</td>
          <td>{fmtRunTime(r.StartedAt)}</td>
          <td>{fmtRunTime(r.EndedAt)}</td>
        </tr>
        {#if isOpen}
          <tr class="row-debate">
            <td></td>
            <td colspan="7">
              {#if ensemble}
                {@const editorBackend = ensemble.editor?.backend ?? ''}
                {@const editorModel = ensemble.editor?.model ?? '—'}
                {@const editorStatus = editorBackend === 'flexinfer' ? flexinferModelsStore.statusFor(editorModel) : 'unknown'}
                <div class="ensemble-block">
                  <div class="ensemble-heading">Ensemble (policy v{policy?.version ?? '?'})</div>
                  <ul class="ensemble-list">
                    <li class="ensemble-row">
                      <span class="ens-role">editor</span>
                      <span class="ens-backend">{editorBackend || '—'}</span>
                      <span class="ens-model">{editorModel}</span>
                      {#if editorBackend === 'flexinfer'}
                        <span class="ens-status ens-status-{editorStatus}" title={modelStatusTitle(editorStatus, editorModel)}>{editorStatus}</span>
                      {:else}
                        <span class="ens-status ens-status-unknown" title="Status checks only available for flexinfer-backed agents today.">—</span>
                      {/if}
                    </li>
                    {#each ensemble.reviewers ?? [] as rev (rev.name ?? rev.model)}
                      {@const revBackend = rev.backend ?? ''}
                      {@const revModel = rev.model ?? '—'}
                      {@const revStatus = revBackend === 'flexinfer' ? flexinferModelsStore.statusFor(revModel) : 'unknown'}
                      <li class="ensemble-row">
                        <span class="ens-role">reviewer · {rev.name ?? '(unnamed)'}</span>
                        <span class="ens-backend">{revBackend || '—'}</span>
                        <span class="ens-model">{revModel}</span>
                        {#if revBackend === 'flexinfer'}
                          <span class="ens-status ens-status-{revStatus}" title={modelStatusTitle(revStatus, revModel)}>{revStatus}</span>
                        {:else}
                          <span class="ens-status ens-status-unknown" title="Status checks only available for flexinfer-backed agents today.">—</span>
                        {/if}
                      </li>
                    {/each}
                  </ul>
                </div>
              {/if}
              {#if !debate || debate.status === 'idle' || debate.status === 'loading'}
                <div class="debate-status">Loading debate transcript…</div>
              {:else if debate.status === 'error'}
                <div class="debate-status error">
                  <strong>Failed to load debate:</strong> {debate.message}
                  <button type="button" class="retry-btn" onclick={(e) => { e.stopPropagation(); void millsStore.loadDebate(r.ID); }}>↻ retry</button>
                </div>
              {:else if debate.rounds.length === 0}
                <div class="debate-status muted">No debate ran for this council run (single-pass).</div>
              {:else}
                {@const totalCost = debate.rounds.reduce((s, x) => s + (x.CostUSD ?? 0), 0)}
                <div class="debate-summary">
                  <strong>Debate Rounds</strong>
                  <span class="muted">·</span>
                  <span>{debate.rounds.length} entries</span>
                  <span class="muted">·</span>
                  <span>{fmtCostExact(totalCost)} total</span>
                </div>
                <ol class="debate-list">
                  {#each debate.rounds as row (row.ID)}
                    <li>
                      <span class="round-pill">R{row.RoundIndex}</span>
                      <span class="role">{roleLabel(row.Role)}</span>
                      <span class="cost">{fmtCostExact(row.CostUSD)}</span>
                      {#if row.Summary}
                        <span class="summary">{row.Summary}</span>
                      {/if}
                      {#if row.ArtifactDeltas && row.ArtifactDeltas.length > 0}
                        <span class="deltas">
                          {#each row.ArtifactDeltas as d}
                            <code class="delta">
                              {d.action ?? 'edit'} {d.path ?? '?'}{d.line_range ? `:${d.line_range}` : ''}
                            </code>
                          {/each}
                        </span>
                      {/if}
                    </li>
                  {/each}
                </ol>
              {/if}
            </td>
          </tr>
        {/if}
      {/each}
    </tbody>
  </table>
  </div>
</PanelShell>

<ConfirmDialog
  open={confirmReplay}
  title="Replay council now?"
  message="Fires a real council pass with the current ensemble. This may create new backlog items and incur model cost."
  confirmLabel="Replay"
  variant="warn"
  onConfirm={doReplay}
  onCancel={() => (confirmReplay = false)}
/>

<style>
  .policy-row { display: flex; gap: 0.75rem; align-items: center; font-size: var(--text-12); }
  .kill-switch { color: var(--text-muted); }
  .kill-switch.enabled { color: var(--success); }
  .kill-switch strong { color: inherit; }
  .policy-version { color: var(--text-muted); font-size: var(--text-xs); }
  .mills-action-btn {
    font-size: var(--text-xs);
    padding: 0.3rem 0.7rem;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    background: var(--bg-subtle);
    color: var(--text);
    cursor: pointer;
  }
  .mills-action-btn:hover:not(:disabled) { background: var(--bg-hover); }
  .mills-action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
  .mills-action-btn.primary { border-color: rgba(var(--mills-rgb), 0.55); color: var(--mills); }
  .mills-table-wrap { overflow-x: auto; }
  .mills-table { width: 100%; border-collapse: collapse; font-size: var(--text-12); }
  .mills-table th, .mills-table td {
    text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border-subtle);
  }
  .mills-table th { font-weight: 600; color: var(--text-muted); }
  .mono { font-family: ui-monospace, monospace; }
  .cell-cap {
    display: inline-block;
    max-width: 220px;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    vertical-align: bottom;
  }

  .outcome { padding: 0.1rem 0.4rem; border-radius: var(--radius-xs); font-size: var(--text-xs); }
  .outcome-success  { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .outcome-partial  { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .outcome-error    { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .outcome-conflict { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }

  .row-summary { cursor: pointer; }
  .row-summary:hover { background: color-mix(in srgb, var(--fg-primary) 3%, transparent); }
  .row-summary:focus-visible {
    outline: 2px solid var(--info);
    outline-offset: 2px;
    border-radius: var(--radius-sm);
  }
  .row-suspicious td:first-child + td + td + td {
    /* No-op: targeting handled via the badge in the outcome cell. Kept
       as a hook for future row-level tinting if we want it. */
  }
  .badge {
    margin-left: 0.4rem;
    padding: 0.05rem 0.35rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: ui-monospace, monospace;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    vertical-align: middle;
  }
  .badge-warn {
    background: rgba(var(--warning-rgb), 0.18);
    color: var(--warning);
    border: 1px solid rgba(var(--warning-rgb), 0.35);
  }
  .badge-err {
    background: rgba(var(--error-rgb), 0.18);
    color: var(--error);
    border: 1px solid rgba(var(--error-rgb), 0.4);
  }
  .dur { color: var(--text-muted); }
  .dur-instant { color: var(--warning); font-weight: 600; }
  .expander { width: 1.2rem; padding-right: 0; }
  .caret { display: inline-block; transition: transform 120ms ease; color: var(--text-muted); }
  .caret.open { transform: rotate(90deg); }
  .row-debate td { background: color-mix(in srgb, var(--fg-primary) 1.5%, transparent); border-bottom: 1px solid var(--border-subtle); }
  .debate-status { padding: 0.5rem 0.25rem; color: var(--text-muted); font-size: var(--text-12); }
  .debate-status.error {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.6rem 0.75rem;
    background: rgba(var(--error-rgb), 0.1);
    border-left: 3px solid var(--error);
    border-radius: var(--radius-xs);
    color: var(--error);
  }
  .debate-status.error strong { color: var(--error); }
  .debate-status.muted { color: var(--text-muted); }
  .retry-btn {
    margin-left: auto;
    background: color-mix(in srgb, var(--fg-primary) 5%, transparent);
    border: 1px solid rgba(var(--error-rgb), 0.4);
    color: var(--error);
    cursor: pointer;
    padding: 0.2rem 0.6rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    font-family: ui-monospace, monospace;
  }
  .retry-btn:hover { background: rgba(var(--error-rgb), 0.15); }
  .ensemble-block {
    margin: 0.4rem 0.25rem 0.5rem;
    padding: 0.5rem 0.7rem;
    background: color-mix(in srgb, var(--fg-primary) 2%, transparent);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-sm);
  }
  .ensemble-heading {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    margin-bottom: 0.4rem;
  }
  .ensemble-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.25rem; }
  .ensemble-row {
    display: grid;
    grid-template-columns: 11rem 5rem 1fr 4.5rem;
    align-items: center;
    gap: 0.5rem;
    font-size: var(--text-xs);
  }
  /* Phone reflow: the fixed 11rem+5rem rails overflow narrow viewports, so
     fold each row to two lines — role·backend, then model·status. */
  @media (max-width: 720px) {
    .ensemble-row {
      grid-template-columns: minmax(0, 1fr) auto;
      row-gap: 0.15rem;
    }
    .ens-role { grid-row: 1; grid-column: 1; }
    .ens-backend { grid-row: 1; grid-column: 2; text-align: right; }
    .ens-model { grid-row: 2; grid-column: 1; }
    .ens-status { grid-row: 2; grid-column: 2; }
  }
  .ens-role { color: var(--text-muted); font-size: var(--text-2xs); text-transform: uppercase; letter-spacing: 0.04em; }
  .ens-backend { color: var(--text-muted); }
  .ens-model { font-family: ui-monospace, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ens-status {
    text-align: center;
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-sm);
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-family: ui-monospace, monospace;
    cursor: help;
    white-space: nowrap;
  }
  .ens-status-ready   { background: rgba(var(--success-rgb), 0.18);  color: var(--success); border: 1px solid rgba(var(--success-rgb), 0.4); }
  .ens-status-idle    { background: rgba(var(--warning-rgb), 0.18);  color: var(--warning); border: 1px solid rgba(var(--warning-rgb), 0.4); }
  .ens-status-unknown { background: rgba(var(--error-rgb), 0.15); color: var(--error); border: 1px solid rgba(var(--error-rgb), 0.35); }

  .debate-summary { padding: 0.4rem 0.25rem 0.25rem; font-size: var(--text-12); display: flex; gap: 0.5rem; align-items: center; }
  .debate-summary .muted { color: var(--text-muted); }
  .debate-list {
    list-style: none; padding: 0 0 0.4rem 0.25rem; margin: 0;
    display: flex; flex-direction: column; gap: 0.3rem;
    font-size: var(--text-12);
  }
  .debate-list li { display: flex; gap: 0.5rem; align-items: baseline; flex-wrap: wrap; }
  .round-pill {
    display: inline-block; min-width: 1.6rem; text-align: center;
    background: rgba(var(--info-rgb), 0.15); color: var(--info);
    padding: 0.05rem 0.35rem; border-radius: var(--radius-xs); font-size: var(--text-2xs); font-family: ui-monospace, monospace;
  }
  .role { color: var(--text); font-weight: 600; }
  .cost { color: var(--text-muted); font-family: ui-monospace, monospace; font-size: var(--text-xs); }
  .summary { color: var(--text-muted); flex: 1 1 100%; padding-left: 2.1rem; font-size: var(--text-xs); }
  .deltas { display: flex; gap: 0.3rem; flex-wrap: wrap; flex: 1 1 100%; padding-left: 2.1rem; }
  .delta {
    background: color-mix(in srgb, var(--fg-primary) 5%, transparent); padding: 0.05rem 0.3rem; border-radius: var(--radius-xs);
    font-size: var(--text-2xs); color: var(--text-muted);
  }
</style>
