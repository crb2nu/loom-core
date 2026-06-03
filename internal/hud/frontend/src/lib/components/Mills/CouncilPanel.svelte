<script lang="ts">
  import { millsStore, type CouncilAgent } from '../../stores/mills.svelte.ts';
  import { flexinferModelsStore, type ModelStatus } from '../../stores/flexinfer_models.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import { runAdminAction } from './shared/millsActions.ts';

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

  function fmtTime(ts?: string): string {
    if (!ts) return '—';
    const d = new Date(ts);
    if (isNaN(d.getTime())) return ts;
    return d.toLocaleString();
  }
  function fmtCost(c?: number): string {
    // Distinguish unknown (—) from genuinely zero ($0.0000). A run that
    // finishes free is valid (cached, skipped, dry-run); null means we
    // never got a cost back from the operator. Conflating the two hides
    // real signal.
    if (c == null) return '—';
    return `$${c.toFixed(4)}`;
  }
  function durationMs(started?: string, ended?: string): number | null {
    if (!started || !ended) return null;
    const a = new Date(started).getTime();
    const b = new Date(ended).getTime();
    if (isNaN(a) || isNaN(b)) return null;
    return Math.max(0, b - a);
  }
  function fmtDuration(started?: string, ended?: string): string {
    const ms = durationMs(started, ended);
    if (ms == null) return '—';
    if (ms < 100) return `${ms}ms`;
    if (ms < 1000) return `${(ms / 1000).toFixed(2)}s`;
    if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
    const mins = Math.floor(ms / 60_000);
    const secs = Math.floor((ms % 60_000) / 1000);
    return `${mins}m ${secs}s`;
  }
  // Anything that "completes" in under a second almost always means
  // the run crashed before doing any work. Tag the row so the operator
  // can spot it without expanding to read the debate transcript.
  function isSuspiciouslyInstant(r: { StartedAt?: string; EndedAt?: string; Outcome?: string }): boolean {
    const ms = durationMs(r.StartedAt, r.EndedAt);
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
  title="Council"
  icon="◇"
  count={runs.length}
  loading={loading}
  empty={runs.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled ? 'Mills operator not configured' : (error ? 'Failed to load council runs' : 'No council runs yet')}
  emptyHint={disabled ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.' : (error ?? 'The council fires on cron + roadmap events.')}
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
        <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
        <tr class="row-summary" class:row-suspicious={instant || debateBroken} onclick={() => toggle(r.ID)}>
          <td class="expander">
            <span class="caret" class:open={isOpen} aria-hidden="true">▸</span>
          </td>
          <td class="mono">{r.ID}</td>
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
          <td>{fmtCost(r.CostUSD)}</td>
          <td class="mono dur" class:dur-instant={instant}>{fmtDuration(r.StartedAt, r.EndedAt)}</td>
          <td>{fmtTime(r.StartedAt)}</td>
          <td>{fmtTime(r.EndedAt)}</td>
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
                  <span>{fmtCost(totalCost)} total</span>
                </div>
                <ol class="debate-list">
                  {#each debate.rounds as row (row.ID)}
                    <li>
                      <span class="round-pill">R{row.RoundIndex}</span>
                      <span class="role">{roleLabel(row.Role)}</span>
                      <span class="cost">{fmtCost(row.CostUSD)}</span>
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
  .policy-row { display: flex; gap: 0.75rem; align-items: center; font-size: 0.85rem; }
  .kill-switch { color: var(--text-muted, #889); }
  .kill-switch.enabled { color: rgb(120, 220, 160); }
  .kill-switch strong { color: inherit; }
  .policy-version { color: var(--text-muted, #889); font-size: 0.75rem; }
  .mills-action-btn {
    font-size: 0.8rem;
    padding: 0.3rem 0.7rem;
    border-radius: 4px;
    border: 1px solid var(--border, #334);
    background: var(--bg-subtle, #1a1d26);
    color: var(--text, #cdd);
    cursor: pointer;
  }
  .mills-action-btn:hover:not(:disabled) { background: var(--bg-hover, #232733); }
  .mills-action-btn:disabled { opacity: 0.5; cursor: not-allowed; }
  .mills-action-btn.primary { border-color: rgb(90, 140, 220); color: rgb(150, 190, 250); }
  .mills-table { width: 100%; border-collapse: collapse; font-size: 0.875rem; }
  .mills-table th, .mills-table td {
    text-align: left; padding: 0.4rem 0.6rem; border-bottom: 1px solid var(--border-subtle, #233);
  }
  .mills-table th { font-weight: 600; color: var(--text-muted, #889); }
  .mono { font-family: ui-monospace, monospace; }
  .outcome { padding: 0.1rem 0.4rem; border-radius: 3px; font-size: 0.75rem; }
  .outcome-success  { background: rgba(72, 200, 128, 0.15); color: rgb(120, 220, 160); }
  .outcome-partial  { background: rgba(220, 200, 60, 0.15); color: rgb(240, 220, 120); }
  .outcome-error    { background: rgba(220, 80, 80, 0.15); color: rgb(240, 130, 130); }
  .outcome-conflict { background: rgba(220, 140, 60, 0.15); color: rgb(240, 180, 100); }

  .row-summary { cursor: pointer; }
  .row-summary:hover { background: rgba(255, 255, 255, 0.03); }
  .row-suspicious td:first-child + td + td + td {
    /* No-op: targeting handled via the badge in the outcome cell. Kept
       as a hook for future row-level tinting if we want it. */
  }
  .badge {
    margin-left: 0.4rem;
    padding: 0.05rem 0.35rem;
    border-radius: 3px;
    font-size: 0.7rem;
    font-family: ui-monospace, monospace;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    vertical-align: middle;
  }
  .badge-warn {
    background: rgba(220, 200, 60, 0.18);
    color: rgb(240, 220, 120);
    border: 1px solid rgba(220, 200, 60, 0.35);
  }
  .badge-err {
    background: rgba(220, 80, 80, 0.18);
    color: rgb(240, 130, 130);
    border: 1px solid rgba(220, 80, 80, 0.4);
  }
  .dur { color: var(--text-muted, #889); }
  .dur-instant { color: rgb(240, 200, 100); font-weight: 600; }
  .expander { width: 1.2rem; padding-right: 0; }
  .caret { display: inline-block; transition: transform 120ms ease; color: var(--text-muted, #889); }
  .caret.open { transform: rotate(90deg); }
  .row-debate td { background: rgba(255, 255, 255, 0.015); border-bottom: 1px solid var(--border-subtle, #233); }
  .debate-status { padding: 0.5rem 0.25rem; color: var(--text-muted, #889); font-size: 0.85rem; }
  .debate-status.error {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    padding: 0.6rem 0.75rem;
    background: rgba(220, 80, 80, 0.1);
    border-left: 3px solid rgb(220, 80, 80);
    border-radius: 3px;
    color: rgb(240, 200, 200);
  }
  .debate-status.error strong { color: rgb(240, 130, 130); }
  .debate-status.muted { color: var(--text-muted, #889); }
  .retry-btn {
    margin-left: auto;
    background: rgba(255, 255, 255, 0.05);
    border: 1px solid rgba(240, 130, 130, 0.4);
    color: rgb(240, 200, 200);
    cursor: pointer;
    padding: 0.2rem 0.6rem;
    border-radius: 3px;
    font-size: 0.78rem;
    font-family: ui-monospace, monospace;
  }
  .retry-btn:hover { background: rgba(240, 130, 130, 0.15); }
  .ensemble-block {
    margin: 0.4rem 0.25rem 0.5rem;
    padding: 0.5rem 0.7rem;
    background: rgba(255, 255, 255, 0.02);
    border: 1px solid var(--border-subtle, #233);
    border-radius: 4px;
  }
  .ensemble-heading {
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted, #889);
    margin-bottom: 0.4rem;
  }
  .ensemble-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 0.25rem; }
  .ensemble-row {
    display: grid;
    grid-template-columns: 11rem 5rem 1fr 4.5rem;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.8rem;
  }
  .ens-role { color: var(--text-muted, #889); font-size: 0.72rem; text-transform: uppercase; letter-spacing: 0.04em; }
  .ens-backend { color: var(--text-muted, #889); }
  .ens-model { font-family: ui-monospace, monospace; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .ens-status {
    text-align: center;
    padding: 0.05rem 0.4rem;
    border-radius: 4px;
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    font-family: ui-monospace, monospace;
    cursor: help;
    white-space: nowrap;
  }
  .ens-status-ready   { background: rgba(78, 201, 176, 0.18);  color: rgb(120, 220, 160); border: 1px solid rgba(78, 201, 176, 0.4); }
  .ens-status-idle    { background: rgba(215, 160, 58, 0.18);  color: rgb(240, 220, 120); border: 1px solid rgba(215, 160, 58, 0.4); }
  .ens-status-unknown { background: rgba(224, 108, 117, 0.15); color: rgb(240, 130, 130); border: 1px solid rgba(224, 108, 117, 0.35); }

  .debate-summary { padding: 0.4rem 0.25rem 0.25rem; font-size: 0.85rem; display: flex; gap: 0.5rem; align-items: center; }
  .debate-summary .muted { color: var(--text-muted, #889); }
  .debate-list {
    list-style: none; padding: 0 0 0.4rem 0.25rem; margin: 0;
    display: flex; flex-direction: column; gap: 0.3rem;
    font-size: 0.85rem;
  }
  .debate-list li { display: flex; gap: 0.5rem; align-items: baseline; flex-wrap: wrap; }
  .round-pill {
    display: inline-block; min-width: 1.6rem; text-align: center;
    background: rgba(120, 160, 220, 0.15); color: rgb(150, 180, 230);
    padding: 0.05rem 0.35rem; border-radius: 3px; font-size: 0.7rem; font-family: ui-monospace, monospace;
  }
  .role { color: var(--text, #ddd); font-weight: 600; }
  .cost { color: var(--text-muted, #889); font-family: ui-monospace, monospace; font-size: 0.8rem; }
  .summary { color: var(--text-muted, #ccc); flex: 1 1 100%; padding-left: 2.1rem; font-size: 0.82rem; }
  .deltas { display: flex; gap: 0.3rem; flex-wrap: wrap; flex: 1 1 100%; padding-left: 2.1rem; }
  .delta {
    background: rgba(255, 255, 255, 0.05); padding: 0.05rem 0.3rem; border-radius: 2px;
    font-size: 0.7rem; color: var(--text-muted, #aaa);
  }
</style>
