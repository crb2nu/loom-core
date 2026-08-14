<script lang="ts">
  // PipelineRunDetail — right-edge drawer showing the full
  // {run, stages, gates} payload from GET /api/mills/pipeline/runs/{id}
  // for the currently selected pipeline run. Driven entirely off
  // millsStore.selectedRunID + millsStore.openPipelineDetail so the
  // drawer is reactive to background refresh ticks without re-wiring.
  //
  // The drawer chrome (scrim, panel, focus trap, Escape, ✕) belongs to the
  // shared DetailDrawer; this file owns only the run-specific body. Escape /
  // backdrop / ✕ all funnel through the one `onClose={close}` path.

  import { untrack } from 'svelte';
  import {
    millsStore,
    type GateOutcome,
    type RequeueOutcome,
    type StageResult,
  } from '../../stores/mills.svelte.ts';
  import ConfirmDialog from '../shared/ConfirmDialog.svelte';
  import DetailDrawer from '../shared/DetailDrawer.svelte';
  import LineageRibbon from './shared/LineageRibbon.svelte';
  import { runAdminAction } from './shared/millsActions.ts';
  import { elapsedMs, fmtCost, fmtDuration, fmtRunTime } from './shared/format.ts';
  import {
    showRunVerdict as shouldShowRunVerdict,
    verdictCorrected as isVerdictCorrected,
  } from '../../utils/runVerdict.ts';
  import { mrURL } from '../../utils/gitlabLinks.ts';

  let load = $derived(millsStore.openPipelineDetail);
  let open = $derived(millsStore.selectedRunID !== null);
  let detail = $derived(load && load.status === 'loaded' ? load.detail : null);

  // A 502 (proxy couldn't reach the operator) or a client-side timeout
  // almost always means the operator pod is mid-rollout (Recreate strategy
  // — a short gap every deploy). Name that instead of showing a bare
  // status code; the store re-fetches the open run on its background poll,
  // so the drawer heals itself once the operator is back.
  let transientError = $derived(
    load?.status === 'error' &&
      !!load.message &&
      (load.message.includes('502') ||
        load.message.includes('unreachable') ||
        load.message.includes('timed out')),
  );

  // A stalled or verbose agent can emit megabytes of log; rendering that as a
  // single <pre> blocks the main thread and can make the whole drawer feel
  // frozen (including the close button). Render only the trailing window —
  // the tail is what matters for triage — and flag the elision.
  const MAX_LOGTAIL_CHARS = 20_000;
  function clampLog(s: string): { text: string; clamped: boolean } {
    if (s.length <= MAX_LOGTAIL_CHARS) return { text: s, clamped: false };
    return { text: s.slice(-MAX_LOGTAIL_CHARS), clamped: true };
  }

  // Force-escalate (plan 42 Slice 1): only offered while the run is
  // still in-flight. Terminal runs (merged/done/failed/escalated) have
  // nothing to escalate. The reconciler does not auto-retry escalated
  // items, so this hands ownership back to the operator.
  const TERMINAL_STATES = new Set(['merged', 'done', 'failed', 'escalated', 'paused']);
  let canEscalate = $derived(!!detail && !TERMINAL_STATES.has(detail.run.State));
  let escalating = $state(false);
  let confirmEscalate = $state(false);

  async function doEscalate(): Promise<void> {
    confirmEscalate = false;
    const id = detail?.run.ID;
    if (!id) return;
    escalating = true;
    await runAdminAction(() => millsStore.escalateRun(id, 'manual escalation from HUD'), {
      success: 'Run escalated — operator owns the next move',
      failurePrefix: 'Escalate failed',
    });
    escalating = false;
  }

  let pausing = $state(false);
  let confirmPause = $state(false);
  let pauseReason = $state('Stopped from HUD');
  let canPause = $derived(!!detail && !TERMINAL_STATES.has(detail.run.State));
  async function doPause(): Promise<void> {
    confirmPause = false;
    const id = detail?.run.ID;
    if (!id) return;
    pausing = true;
    await runAdminAction(() => millsStore.pauseRun(id, pauseReason), { success: 'Run paused', failurePrefix: 'Stop failed' });
    pausing = false;
  }

  // Requeue (plan wave-2 W3): recover an escalated run without an admin curl.
  // Only escalated runs are parked awaiting exactly this human action; the
  // requeue flips the backlog item back to queued and starts a fresh run.
  // Feedback is inline (not just a toast) so the ghost-spark 409 ("state is
  // merged" ⇒ already-done) and the 403 admin-token hint read in context.
  let requeuing = $state(false);
  let confirmRequeue = $state(false);
  let requeueOutcome = $state<RequeueOutcome | null>(null);

  // After a successful requeue THIS run stays escalated (the fresh run is a
  // new row), so drop the button once started to avoid a confusing re-click —
  // the success banner stays.
  let canRequeue = $derived(
    !!detail && detail.run.State === 'escalated' && requeueOutcome?.kind !== 'started',
  );

  // Clear a stale requeue banner when the drawer switches to a different run.
  // Reads selectedRunID (tracked) but mutates the tracking + result state
  // inside untrack() so the effect never depends on the state it writes —
  // avoids the self-referential read+write that tears down the effect tree.
  let requeueOutcomeRun = $state<string | null>(null);
  $effect(() => {
    const id = millsStore.selectedRunID;
    untrack(() => {
      if (id !== requeueOutcomeRun) {
        requeueOutcomeRun = id;
        requeueOutcome = null;
      }
    });
  });

  async function doRequeue(): Promise<void> {
    confirmRequeue = false;
    const backlogID = detail?.run.BacklogID;
    if (!backlogID) return;
    // Capture which run this request belongs to: the drawer can be closed and
    // reopened on a different run while the request is in flight (the started
    // path even awaits a fetchAll), and a late outcome must not render under —
    // or hide the Requeue button of — an unrelated run.
    const dispatchedRun = millsStore.selectedRunID;
    requeuing = true;
    requeueOutcome = null;
    try {
      const outcome = await millsStore.requeuePipelineRun(backlogID);
      if (millsStore.selectedRunID === dispatchedRun) {
        requeueOutcome = outcome;
      }
    } finally {
      // finally: requeuePipelineRun is contractually no-throw, but a defect
      // there must degrade to a re-enabled button, not a drawer stuck on
      // "Requeuing…" until unmount.
      requeuing = false;
    }
  }

  // expandedStages tracks which stage rows have the log+artifacts
  // panel open. Stored locally (not on the store) because expansion
  // is per-view UX state, not data.
  let expandedStages = $state<Set<number>>(new Set());

  function close(): void {
    millsStore.closeRunDetail();
    expandedStages = new Set();
  }

  function toggleStage(id: number): void {
    const next = new Set(expandedStages);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    expandedStages = next;
  }


  function stageOutcomeLabel(s: StageResult): string {
    if (!s.EndedAt) return 'in flight';
    if (!s.Outcome) return 'pending';
    return s.Outcome;
  }

  function targetProject(backlogID: string): string | undefined {
    return millsStore.backlog.find((item) => item.ID === backlogID)?.TargetProject;
  }

  function stageArtifactEntries(s: StageResult): Array<[string, string]> {
    if (!s.Artifacts) return [];
    return Object.entries(s.Artifacts).map(([k, v]) => [
      k,
      typeof v === 'string' ? v : JSON.stringify(v),
    ]);
  }

  function gateOutcomeRank(o: GateOutcome['Outcome']): number {
    // Sort fails first, then skips, then passes — fails are what the
    // user opened the drawer for.
    if (o === 'fail') return 0;
    if (o === 'skip') return 1;
    return 2;
  }

  let sortedGates = $derived.by(() => {
    if (!detail) return [] as GateOutcome[];
    return [...detail.gates].sort(
      (a, b) => gateOutcomeRank(a.Outcome) - gateOutcomeRank(b.Outcome),
    );
  });

  // Ground-truth evidence (the answerable floor): judge verdicts, the
  // provenance stamp, and any regression attribution off the run's MR.
  // Older operators omit the block entirely — every read is null-guarded so
  // the drawer renders identically against them.
  let verdicts = $derived(detail?.evidence?.verdicts ?? []);
  let provenance = $derived(detail?.evidence?.provenance ?? null);
  let regression = $derived(detail?.evidence?.regression ?? null);
  let stageModelEntries = $derived.by(() => {
    const models = provenance?.stage_models;
    return models ? Object.entries(models) : [];
  });
  function fmtScore(v: number | undefined): string {
    return typeof v === 'number' ? v.toFixed(2) : '—';
  }
  function shortSHA(v: string | undefined): string {
    return v ? v.slice(0, 10) : '—';
  }

  // The run's current-belief verdict (Trustworthy Verdicts S4). Unlike
  // run.State — immutable terminal history — this is what we believe NOW:
  // a ghost-spark closure of an escalated run supersedes the escalation and
  // resolves to merged_after_escalation. The class is rendered VERBATIM;
  // never map merged_after_escalation onto "merged", or the correction goes
  // invisible and the drawer starts lying about clean merges.
  let runVerdict = $derived(detail?.evidence?.verdict ?? null);

  // A corrected verdict is one something superseded. Keyed off prior_class /
  // superseded rather than by sniffing the class string, so later supersede
  // sources (regression attribution, operator override) light up the accent
  // without touching this file.
  let verdictCorrected = $derived(isVerdictCorrected(runVerdict));

  // Render rule. The operator sends a verdict for EVERY non-nil run, live
  // ones included — an in-flight run arrives as {class: "implementing",
  // occurred_at: "0001-01-01T00:00:00Z"} rather than as verdict:null (no
  // omitempty on the Go OccurredAt). So a bare null check would paint a
  // duplicate chip echoing the header's state pill, dated year 1, on every
  // run on the floor. Show the chip only when the verdict says something the
  // state pill does not: it was corrected, or its class differs from State
  // (an escalated run resolves to its escalation class). verdict:null and a
  // missing evidence block both render nothing, which is the live-run
  // contract either way.
  let showRunVerdict = $derived(shouldShowRunVerdict(runVerdict, detail?.run.State));

  // Same zero-time trap on the display side: Date-formatting 0001-01-01
  // yields a real-looking timestamp. Suppress it instead of citing it.
  let verdictWhen = $derived.by<string>(() => {
    const at = runVerdict?.occurred_at;
    if (!at || at.startsWith('0001-01-01')) return '';
    return fmtRunTime(at);
  });

  // Tooltip on a corrected chip cites what moved the verdict and when.
  let verdictTitle = $derived.by<string>(() => {
    if (!runVerdict) return '';
    if (!verdictCorrected) {
      return verdictWhen
        ? `Current-belief verdict for this run, as of ${verdictWhen}`
        : 'Current-belief verdict for this run';
    }
    const prior = runVerdict.prior_class || detail?.run.State || 'its original class';
    const via = runVerdict.source ? ` via ${runVerdict.source}` : '';
    const when = verdictWhen ? ` on ${verdictWhen}` : '';
    return `Verdict corrected from ${prior} to ${runVerdict.class}${via}${when} — the run row keeps its original terminal state.`;
  });

  // Reasons off the failing gates enrich the strand's terminal spark node.
  // Only the detail payload carries them (a list row can't), which is exactly
  // why the enrichment is optional in lineageFor — undefined when nothing
  // failed, so the strand never invents a "why".
  let failingGateReasons = $derived.by<string[] | undefined>(() => {
    const reasons = (detail?.gates ?? [])
      .filter((g) => g.Outcome === 'fail')
      .flatMap((g) => g.Reasons ?? []);
    return reasons.length > 0 ? reasons : undefined;
  });

  // Escalation metadata chips (plan S6). Only rendered when the operator
  // stamped a class / dependency on the run — an escalated run. FailureClass
  // is dropped when it just echoes EscalationClass so the row stays terse.
  // EscalationRetryable is tri-state (true/false/absent), so a `typeof …
  // boolean` test — not truthiness — decides whether the retry chip appears.
  type EscChip = { label: string; value: string; variant: 'info' | 'warning' | 'error' | 'accent' };
  let escalationChips = $derived.by<EscChip[]>(() => {
    const r = detail?.run;
    if (!r) return [];
    const chips: EscChip[] = [];
    if (r.EscalationClass) {
      chips.push({ label: 'class', value: r.EscalationClass, variant: 'warning' });
    }
    if (r.FailureClass && r.FailureClass !== r.EscalationClass) {
      chips.push({ label: 'failure', value: r.FailureClass, variant: 'warning' });
    }
    if (r.ExternalDependency) {
      chips.push({ label: 'dependency', value: r.ExternalDependency, variant: 'error' });
    }
    if (typeof r.EscalationRetryable === 'boolean') {
      chips.push({
        label: 'retry',
        value: r.EscalationRetryable ? 'retryable' : 'terminal',
        variant: r.EscalationRetryable ? 'info' : 'accent',
      });
    }
    return chips;
  });

  // copyRunID drops the full run id onto the clipboard so users can
  // share or paste into a terminal without manually selecting the
  // mono text in the table. Falls back silently if clipboard access
  // is denied (e.g. non-HTTPS dev contexts).
  async function copyRunID(): Promise<void> {
    const id = detail?.run.ID;
    if (!id) return;
    try {
      await navigator.clipboard.writeText(id);
    } catch {
      /* swallow — UI feedback isn't worth a toast for a missing perm */
    }
  }
</script>

<DetailDrawer
  {open}
  title="Pipeline run detail"
  width="min(640px, 96vw)"
  contentPadding="0"
  closeLabel="Close run detail"
  onClose={close}
>
  {#snippet titleContent()}
    <div class="run-title">
      <span class="run-kicker">Pipeline Run</span>
      {#if detail}
        <span class="run-state state-{detail.run.State}">{detail.run.State}</span>
      {/if}
    </div>
  {/snippet}

  {#snippet headerActions()}
    {#if canEscalate}
      <button
        type="button"
        class="run-escalate"
        disabled={escalating}
        onclick={() => (confirmEscalate = true)}
        title="Force-escalate this run — stops the pipeline and hands ownership to the operator"
      >
        {escalating ? 'Escalating…' : '⚠ Escalate'}
      </button>
    {/if}
  {/snippet}

  {#if load?.status === 'loading' && !detail}
    <div class="run-loading" role="status" aria-live="polite">
      <span class="run-spinner" aria-hidden="true"></span>
      <div class="run-loading-text">
        <span class="run-loading-title">Loading run detail…</span>
        <span class="run-loading-hint">Press Esc or ✕ to close — the request stops if the operator is slow.</span>
      </div>
    </div>
  {:else if load?.status === 'error'}
    <div class="run-error">
      <div class="run-error-title">Couldn't load run detail</div>
      <div class="run-error-msg">{load.message}</div>
      {#if transientError}
        <div class="run-error-hint">
          The Mills operator may be restarting (it redeploys with a brief gap).
          This drawer retries automatically on the next refresh.
        </div>
      {/if}
      <button type="button" class="run-retry" onclick={() => millsStore.openRunDetail(millsStore.selectedRunID ?? '')}>
        Retry
      </button>
    </div>
  {:else if detail}
    <section class="run-summary">
      <!-- Where this run sits on the floor, before any of the meta: warp ══
           stage picks ══► terminal bolt / spark. First element of the
           summary so the operator places the run at a glance. -->
      <LineageRibbon
        mode="strand"
        segments={millsStore.lineageFor(detail.run, failingGateReasons)}
      />
      {#if regression}
        <!-- The one fact that outranks everything else in the drawer: this
             run's merged work was later reverted on main. -->
        <div class="regression-banner" role="alert">
          <span class="regression-word">reverted on main</span>
          <span class="regression-detail" title={regression.revert_title}>
            {regression.revert_title || 'revert commit'} · <span class="mono">{shortSHA(regression.revert_sha)}</span>
          </span>
        </div>
      {/if}
      {#if showRunVerdict && runVerdict}
        <!-- What we believe about this run now, which can outrank the state
             pill in the header: a rescued escalation reads
             merged_after_escalation here while the row stays "escalated". -->
        <div class="esc-chips" aria-label="Run verdict">
          <span
            class="esc-chip run-verdict-chip esc-{verdictCorrected ? 'accent' : 'info'}"
            class:verdict-corrected={verdictCorrected}
            title={verdictTitle}
          >
            <span class="esc-chip-label">verdict</span>
            <span class="esc-chip-value">{runVerdict.class}</span>
          </span>
          {#if verdictCorrected && runVerdict.prior_class}
            <span class="verdict-history mono">
              was {runVerdict.prior_class} → {runVerdict.class}{runVerdict.source
                ? ` via ${runVerdict.source}`
                : ''}
            </span>
          {/if}
        </div>
      {/if}
      {#if escalationChips.length > 0}
        <div class="esc-chips" aria-label="Escalation metadata">
          {#each escalationChips as chip (chip.label)}
            <span class="esc-chip esc-{chip.variant}">
              <span class="esc-chip-label">{chip.label}</span>
              <span class="esc-chip-value">{chip.value}</span>
            </span>
          {/each}
        </div>
      {/if}
      {#if canRequeue || requeueOutcome}
        <div class="requeue-zone">
          {#if canRequeue}
            <button
              type="button"
              class="run-requeue"
              disabled={requeuing}
              onclick={() => (confirmRequeue = true)}
              title="Requeue this escalated item — flips it back to queued and starts a fresh pipeline run"
            >
              {requeuing ? 'Requeuing…' : '↻ Requeue'}
            </button>
          {/if}
          {#if requeueOutcome}
            <div class="requeue-result requeue-{requeueOutcome.kind}" role="status" aria-live="polite">
              {requeueOutcome.message}
            </div>
          {/if}
        </div>
      {/if}
      {#if canPause}
        <button type="button" class="run-requeue" disabled={pausing} onclick={() => {
          pauseReason = window.prompt('Why stop this run?', pauseReason) ?? '';
          if (pauseReason.trim()) confirmPause = true;
        }}>
          {pausing ? 'Stopping…' : '■ Stop'}
        </button>
      {/if}
      <dl class="run-meta">
        <div class="run-meta-row">
          <dt>Run ID</dt>
          <dd class="mono run-id">
            <span title={detail.run.ID}>{detail.run.ID}</span>
            <button type="button" class="run-copy" onclick={copyRunID} title="Copy run id">⧉</button>
          </dd>
        </div>
        <div class="run-meta-row">
          <dt>Backlog</dt>
          <dd class="mono">{detail.run.BacklogID}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Template</dt>
          <dd class="mono">{detail.run.Template || '—'}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Current stage</dt>
          <dd>
            {#if detail.run.CurrentStage}
              <span class="stage-chip">{detail.run.CurrentStage}</span>
            {:else}
              <span class="muted">—</span>
            {/if}
          </dd>
        </div>
        <div class="run-meta-row">
          <dt>Started</dt>
          <dd class="mono">{fmtRunTime(detail.run.StartedAt)}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Ended</dt>
          <dd class="mono">{fmtRunTime(detail.run.EndedAt)}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Duration</dt>
          <dd class="mono">{fmtDuration(elapsedMs(detail.run.StartedAt, detail.run.EndedAt))}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Attempts</dt>
          <dd class="mono">{detail.run.Attempts}</dd>
        </div>
        <div class="run-meta-row">
          <dt>Cost</dt>
          <dd class="mono cost">{fmtCost(detail.run.CostUSD)}</dd>
        </div>
        {#if detail.run.WorktreePath}
          <div class="run-meta-row">
            <dt>Worktree</dt>
            <dd class="mono path" title={detail.run.WorktreePath}>{detail.run.WorktreePath}</dd>
          </div>
        {/if}
        {#if detail.run.MRIID}
          <div class="run-meta-row">
            <dt>Merge Request</dt>
            <dd class="mono">
              <a
                class="mr-link"
                href={mrURL(targetProject(detail.run.BacklogID), detail.run.MRIID)}
                target="_blank"
                rel="noreferrer noopener"
                onclick={(event) => event.stopPropagation()}
              >
                !{detail.run.MRIID}
              </a>
            </dd>
          </div>
        {/if}
        {#if detail.run.ParentRunID}
          <div class="run-meta-row">
            <dt>Parent run</dt>
            <dd class="mono path" title={detail.run.ParentRunID}>{detail.run.ParentRunID}</dd>
          </div>
        {/if}
      </dl>
    </section>

    {#if verdicts.length > 0 || provenance}
      <section class="run-section">
        <h3 class="run-section-title">
          Evidence
          {#if verdicts.length > 0}<span class="section-count">{verdicts.length}</span>{/if}
        </h3>
        {#if verdicts.length > 0}
          <!-- The judges' own scores, oldest-first: retried gates read as a
               chronology, and a pass that followed a fail is visible as
               exactly that rather than replacing it. -->
          <ol class="verdict-list" aria-label="Judge verdicts, oldest first">
            {#each verdicts as v, i (i)}
              <li class="verdict-row" data-pass={v.pass ? 'pass' : 'fail'}>
                <span class="verdict-outcome">{v.pass ? 'PASS' : 'FAIL'}</span>
                <span class="verdict-gate mono">{v.gate ?? '—'}</span>
                <span class="verdict-score mono">{fmtScore(v.score)} / {fmtScore(v.threshold)}</span>
                <span class="verdict-meta" title={v.judge_model ?? ''}>
                  {v.role ?? 'judge'}{typeof v.attempt === 'number' ? ` · attempt ${v.attempt}` : ''}
                </span>
              </li>
            {/each}
          </ol>
        {/if}
        {#if provenance}
          <div class="provenance-row" aria-label="Run provenance stamp">
            <span class="prov-chip" title="sha256 of the policy the operator was running">
              policy <span class="mono">{shortSHA(provenance.policy_checksum)}</span>
            </span>
            {#if provenance.lane}
              <span class="prov-chip">lane <span class="mono">{provenance.lane}</span></span>
            {/if}
            {#each stageModelEntries as [stage, model] (stage)}
              <span class="prov-chip" title="model resolved for the {stage} stage at run start">
                {stage} <span class="mono">{model}</span>
              </span>
            {/each}
          </div>
        {/if}
      </section>
    {/if}

    <section class="run-section">
      <h3 class="run-section-title">
        Stages
        <span class="section-count">{detail.stages.length}</span>
      </h3>
      {#if detail.stages.length === 0}
        <div class="section-empty">No stage attempts recorded yet.</div>
      {:else}
        <ol class="stage-list">
          {#each detail.stages as stage (stage.ID)}
            {@const expanded = expandedStages.has(stage.ID)}
            {@const artifacts = stageArtifactEntries(stage)}
            <li class="stage-row" data-outcome={stage.Outcome ?? 'pending'}>
              <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
              <button
                type="button"
                class="stage-head"
                aria-expanded={expanded}
                onclick={() => toggleStage(stage.ID)}
              >
                <span class="stage-glyph">{expanded ? '▾' : '▸'}</span>
                <span class="stage-name">{stage.Stage}</span>
                <span class="stage-attempt">try {stage.Attempt}</span>
                <span class="stage-outcome o-{stage.Outcome ?? 'pending'}">
                  {stageOutcomeLabel(stage)}
                </span>
                <span class="stage-duration mono">{fmtDuration(elapsedMs(stage.StartedAt, stage.EndedAt))}</span>
                <span class="stage-cost mono">{fmtCost(stage.CostUSD)}</span>
              </button>
              {#if expanded}
                <div class="stage-body">
                  <div class="stage-times mono">
                    {fmtRunTime(stage.StartedAt)} → {fmtRunTime(stage.EndedAt)}
                    {#if stage.SpawnID}
                      <span class="stage-spawn" title="Spawn id">spawn: {stage.SpawnID}</span>
                    {/if}
                  </div>
                  {#if artifacts.length > 0}
                    <div class="stage-artifacts">
                      <div class="kv-title">Artifacts</div>
                      <dl class="kv">
                        {#each artifacts as [k, v]}
                          <dt>{k}</dt>
                          <dd class="mono">{v}</dd>
                        {/each}
                      </dl>
                    </div>
                  {/if}
                  {#if stage.LogTail && stage.LogTail.trim().length > 0}
                    {@const clamped = clampLog(stage.LogTail)}
                    <div class="stage-logs">
                      <div class="kv-title">Log tail</div>
                      {#if clamped.clamped}
                        <div class="logtail-note">
                          Showing the last {(MAX_LOGTAIL_CHARS / 1000).toFixed(0)}k of {(stage.LogTail.length / 1000).toFixed(0)}k characters.
                        </div>
                      {/if}
                      <pre class="logtail">{clamped.text}</pre>
                    </div>
                  {/if}
                  {#if artifacts.length === 0 && (!stage.LogTail || stage.LogTail.trim().length === 0)}
                    <div class="stage-empty-detail">No artifacts or log captured for this attempt.</div>
                  {/if}
                </div>
              {/if}
            </li>
          {/each}
        </ol>
      {/if}
    </section>

    <section class="run-section">
      <h3 class="run-section-title">
        Gates
        <span class="section-count">{detail.gates.length}</span>
      </h3>
      {#if detail.gates.length === 0}
        <div class="section-empty">No gate evaluations yet.</div>
      {:else}
        <ul class="gate-list">
          {#each sortedGates as gate (gate.ID)}
            <li class="gate-row" data-outcome={gate.Outcome}>
              <div class="gate-head">
                <span class="gate-outcome o-{gate.Outcome}">{gate.Outcome}</span>
                <span class="gate-name">{gate.GateName}</span>
                <span class="gate-after mono">after {gate.AfterStage}</span>
                <span class="gate-time mono">{fmtRunTime(gate.EvaluatedAt)}</span>
              </div>
              {#if gate.JudgedBy}
                <div class="gate-judge mono">judged by {gate.JudgedBy}</div>
              {/if}
              {#if gate.Reasons && gate.Reasons.length > 0}
                <ul class="gate-reasons">
                  {#each gate.Reasons as reason}
                    <li>{reason}</li>
                  {/each}
                </ul>
              {/if}
            </li>
          {/each}
        </ul>
      {/if}
    </section>
  {/if}
</DetailDrawer>

<ConfirmDialog
  open={confirmEscalate}
  title="Force-escalate this run?"
  message="Marks the pipeline run as escalated and stops automated progress. The reconciler will not auto-retry it — an operator owns the next move."
  confirmLabel="Escalate"
  variant="danger"
  onConfirm={doEscalate}
  onCancel={() => (confirmEscalate = false)}
/>

<ConfirmDialog
  open={confirmPause}
  title="Stop this run?"
  message="Stops the live worker and holds this run until an operator resumes it."
  confirmLabel="Stop"
  variant="danger"
  onConfirm={doPause}
  onCancel={() => (confirmPause = false)}
/>

<ConfirmDialog
  open={confirmRequeue}
  title="Requeue this escalated run?"
  message="Flips the backlog item back to queued and starts a fresh Mills pipeline run. This consumes budget and may open a new merge request."
  confirmLabel="Requeue"
  variant="warn"
  onConfirm={doRequeue}
  onCancel={() => (confirmRequeue = false)}
/>

<style>
  /* Drawer chrome (scrim, panel, header row, ✕, slide-in, mobile sheet) lives
     in shared/DetailDrawer.svelte. What remains here is the run-specific
     header content it renders through the titleContent/headerActions
     snippets, plus the body. */
  .run-title {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .run-kicker {
    font-size: var(--text-sm);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
  }

  .run-state {
    padding: 0.1rem 0.45rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    background: var(--bg-tertiary);
    color: var(--fg-muted);
  }
  .run-state.state-queued   { background: var(--bg-subtle); color: var(--text-muted); }
  .run-state.state-running,
  .run-state.state-planning,
  .run-state.state-slicing,
  .run-state.state-implementing,
  .run-state.state-testing,
  .run-state.state-reviewing,
  .run-state.state-mr,
  .run-state.state-ci,
  .run-state.state-merging {
    background: rgba(var(--info-rgb), 0.15); color: var(--info);
  }
  .run-state.state-merged    { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .run-state.state-done      { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .run-state.state-failed    { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .run-state.state-escalated { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .run-state.state-paused    { background: rgba(var(--warning-rgb), 0.1); color: var(--fg-secondary); }

  .run-escalate {
    padding: 4px 10px;
    border: 1px solid rgba(var(--warning-rgb), 0.5);
    background: rgba(var(--warning-rgb), 0.1);
    color: var(--warning);
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .run-escalate:hover:not(:disabled) { background: rgba(var(--warning-rgb), 0.2); }
  .run-escalate:disabled { opacity: 0.5; cursor: not-allowed; }

  .run-loading,
  .run-error,
  .section-empty {
    padding: var(--space-4);
    font-size: var(--text-sm);
    color: var(--fg-muted);
  }
  .run-loading {
    display: flex;
    align-items: center;
    gap: 0.7rem;
  }
  .run-spinner {
    flex: none;
    width: 16px;
    height: 16px;
    border-radius: 50%;
    border: 2px solid color-mix(in srgb, var(--accent) 30%, transparent);
    border-top-color: var(--accent);
    animation: run-spin 0.7s linear infinite;
  }
  .run-loading-text {
    display: flex;
    flex-direction: column;
    gap: 0.15rem;
    min-width: 0;
  }
  .run-loading-title { color: var(--fg-secondary); }
  .run-loading-hint {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }
  @keyframes run-spin {
    to { transform: rotate(360deg); }
  }
  @media (prefers-reduced-motion: reduce) {
    .run-spinner { animation-duration: 1.6s; }
  }
  .logtail-note {
    font-size: var(--text-2xs);
    color: var(--text-muted);
    margin-bottom: 0.3rem;
    font-style: italic;
  }
  .run-error-title { color: var(--error); font-weight: 600; }
  .run-error-msg { margin-top: 0.4rem; font-family: var(--font-mono); font-size: var(--text-xs); }
  .run-error-hint {
    margin-top: 0.5rem;
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    line-height: 1.5;
    max-width: 42ch;
  }
  .run-retry {
    margin-top: 0.8rem;
    padding: 4px 10px;
    border: 1px solid var(--border);
    background: transparent;
    color: var(--fg-secondary);
    border-radius: var(--radius-xs);
    cursor: pointer;
  }

  .run-summary {
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }

  .esc-chips {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem;
    margin-bottom: 0.7rem;
  }
  .esc-chip {
    display: inline-flex;
    align-items: center;
    gap: 0.35rem;
    padding: 0.1rem 0.45rem;
    border-radius: var(--radius-xs);
    border: 1px solid;
    font-size: var(--text-2xs);
    line-height: 1.4;
  }
  .esc-chip-label {
    text-transform: uppercase;
    letter-spacing: 0.05em;
    font-size: var(--text-2xs);
    opacity: 0.75;
  }
  .esc-chip-value {
    font-family: var(--font-mono);
    font-weight: 600;
  }
  .esc-chip.esc-info {
    background: rgba(var(--info-rgb), 0.12);
    border-color: rgba(var(--info-rgb), 0.35);
    color: var(--info);
  }
  .esc-chip.esc-warning {
    background: rgba(var(--warning-rgb), 0.12);
    border-color: rgba(var(--warning-rgb), 0.35);
    color: var(--warning);
  }
  .esc-chip.esc-error {
    background: rgba(var(--error-rgb), 0.12);
    border-color: rgba(var(--error-rgb), 0.35);
    color: var(--error);
  }
  .esc-chip.esc-accent {
    background: rgba(var(--accent-rgb), 0.12);
    border-color: rgba(var(--accent-rgb), 0.35);
    color: var(--accent);
  }

  /* A corrected verdict borrows the accent chip and adds one weight step, so
     it reads apart from the plain escalation-class chips beneath it without
     inventing a second chip vocabulary. */
  .esc-chip.verdict-corrected {
    border-color: rgba(var(--accent-rgb), 0.6);
    font-weight: 600;
  }
  .verdict-history {
    display: inline-flex;
    align-items: center;
    font-size: var(--text-2xs);
    color: var(--text-muted);
  }

  .requeue-zone {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.5rem;
    margin-bottom: 0.7rem;
  }
  .run-requeue {
    padding: 4px 10px;
    border: 1px solid rgba(var(--info-rgb), 0.5);
    background: rgba(var(--info-rgb), 0.1);
    color: var(--info);
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    cursor: pointer;
  }
  .run-requeue:hover:not(:disabled) { background: rgba(var(--info-rgb), 0.2); }
  .run-requeue:disabled { opacity: 0.5; cursor: not-allowed; }
  .requeue-result {
    flex: 1 1 12rem;
    min-width: 0;
    padding: 0.35rem 0.55rem;
    border: 1px solid;
    border-radius: var(--radius-xs);
    font-size: var(--text-xs);
    line-height: 1.4;
  }
  .requeue-result.requeue-started {
    background: rgba(var(--success-rgb), 0.12);
    border-color: rgba(var(--success-rgb), 0.35);
    color: var(--success);
  }
  .requeue-result.requeue-conflict {
    background: rgba(var(--warning-rgb), 0.12);
    border-color: rgba(var(--warning-rgb), 0.35);
    color: var(--warning);
  }
  .requeue-result.requeue-forbidden,
  .requeue-result.requeue-error {
    background: rgba(var(--error-rgb), 0.12);
    border-color: rgba(var(--error-rgb), 0.35);
    color: var(--error);
  }

  .run-meta {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.35rem 0.9rem;
    margin: 0;
  }
  .run-meta-row {
    display: contents;
  }
  .run-meta dt {
    color: var(--text-muted);
    font-size: var(--text-xs);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .run-meta dd {
    margin: 0;
    color: var(--fg-primary);
    font-size: var(--text-12);
    min-width: 0;
  }
  .run-meta dd.path {
    word-break: break-all;
  }
  .run-meta dd a {
    color: var(--info);
    text-decoration: none;
  }
  .run-meta dd a:hover {
    text-decoration: underline;
  }
  .run-id {
    display: inline-flex;
    align-items: center;
    gap: 0.4rem;
    word-break: break-all;
  }
  .run-copy {
    background: transparent;
    border: 1px solid var(--border-subtle);
    color: var(--text-muted);
    border-radius: var(--radius-xs);
    padding: 1px 6px;
    cursor: pointer;
    font-size: var(--text-xs);
  }
  .run-copy:hover {
    color: var(--fg-primary);
    border-color: var(--border-active);
  }
  .cost { color: var(--fg-muted); }
  .stage-chip {
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    border: 1px solid color-mix(in srgb, var(--accent) 32%, var(--border-subtle));
    background: color-mix(in srgb, var(--accent) 10%, transparent);
    color: var(--fg-secondary);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
  }
  .muted { color: var(--text-muted); }
  .mono { font-family: var(--font-mono); }

  .run-section {
    padding: var(--space-3) var(--space-4);
    border-bottom: 1px solid var(--border-subtle);
  }
  .run-section:last-child { border-bottom: none; }

  .run-section-title {
    margin: 0 0 0.6rem;
    font-size: var(--text-12);
    font-weight: 600;
    color: var(--fg-primary);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    display: flex;
    align-items: center;
    gap: 0.5rem;
  }
  .section-count {
    padding: 0.05rem 0.45rem;
    border-radius: var(--radius-full);
    font-size: var(--text-2xs);
    background: var(--bg-tertiary);
    color: var(--text-muted);
    font-family: var(--font-mono);
  }

  .stage-list,
  .gate-list {
    list-style: none;
    padding: 0;
    margin: 0;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }

  .stage-row {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    background: var(--bg-primary);
    overflow: hidden;
  }
  .stage-row[data-outcome="error"]     { border-color: rgba(var(--error-rgb), 0.4); }
  .stage-row[data-outcome="gate_fail"] { border-color: rgba(var(--warning-rgb), 0.4); }
  .stage-row[data-outcome="success"]   { border-color: rgba(var(--success-rgb), 0.3); }

  .stage-head {
    width: 100%;
    background: transparent;
    border: none;
    padding: 0.5rem 0.7rem;
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto auto auto auto;
    gap: 0.55rem;
    align-items: center;
    color: var(--fg-primary);
    cursor: pointer;
    font-size: var(--text-xs);
    text-align: left;
  }
  .stage-head:hover { background: var(--bg-tertiary); }
  .stage-glyph { color: var(--text-muted); font-size: var(--text-xs); }
  .stage-name {
    font-family: var(--font-mono);
    color: var(--fg-primary);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .stage-attempt {
    font-size: var(--text-2xs);
    color: var(--text-muted);
    font-family: var(--font-mono);
  }
  .stage-outcome {
    padding: 0.05rem 0.4rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
  }
  .stage-outcome.o-success   { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .stage-outcome.o-error     { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .stage-outcome.o-gate_fail { background: rgba(var(--warning-rgb), 0.15); color: var(--warning); }
  .stage-outcome.o-pending   { background: var(--bg-subtle); color: var(--text-muted); }
  .stage-duration,
  .stage-cost {
    font-size: var(--text-xs);
    color: var(--text-muted);
  }

  .stage-body {
    padding: 0.5rem 0.8rem 0.8rem;
    background: rgba(255, 255, 255, 0.015);
    border-top: 1px solid var(--border-subtle);
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .stage-times {
    font-size: var(--text-xs);
    color: var(--text-muted);
    display: flex;
    gap: 0.8rem;
    flex-wrap: wrap;
  }
  .stage-spawn {
    color: var(--fg-secondary);
  }
  .kv-title {
    font-size: var(--text-2xs);
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted);
    margin-bottom: 0.25rem;
  }
  .kv {
    display: grid;
    grid-template-columns: max-content 1fr;
    gap: 0.2rem 0.6rem;
    margin: 0;
    font-size: var(--text-xs);
  }
  .kv dt { color: var(--text-muted); }
  .kv dd {
    margin: 0;
    color: var(--fg-primary);
    word-break: break-all;
  }
  .logtail {
    margin: 0;
    padding: 0.5rem 0.7rem;
    background: var(--bg-primary);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    color: var(--fg-secondary);
    white-space: pre-wrap;
    word-break: break-word;
    max-height: 320px;
    overflow-y: auto;
  }
  .stage-empty-detail {
    font-size: var(--text-xs);
    color: var(--text-muted);
    font-style: italic;
  }

  .gate-row {
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-xs);
    padding: 0.5rem 0.7rem;
    background: var(--bg-primary);
    display: flex;
    flex-direction: column;
    gap: 0.3rem;
  }
  .gate-row[data-outcome="fail"] { border-color: rgba(var(--error-rgb), 0.4); }
  .gate-row[data-outcome="skip"] { border-color: rgba(var(--warning-rgb), 0.3); }

  .gate-head {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto auto;
    gap: 0.55rem;
    align-items: center;
    font-size: var(--text-xs);
  }
  .gate-outcome {
    padding: 0.05rem 0.45rem;
    border-radius: var(--radius-xs);
    font-size: var(--text-2xs);
    font-family: var(--font-mono);
    text-transform: uppercase;
  }
  .gate-outcome.o-pass { background: rgba(var(--success-rgb), 0.15); color: var(--success); }
  .gate-outcome.o-fail { background: rgba(var(--error-rgb), 0.15); color: var(--error); }
  .gate-outcome.o-skip { background: rgba(var(--warning-rgb), 0.1); color: var(--fg-secondary); }
  .gate-name {
    font-family: var(--font-mono);
    color: var(--fg-primary);
  }
  .gate-after,
  .gate-time {
    font-size: var(--text-2xs);
    color: var(--text-muted);
  }
  .gate-judge {
    font-size: var(--text-2xs);
    color: var(--text-muted);
  }
  .gate-reasons {
    margin: 0.15rem 0 0;
    padding-left: 1rem;
    font-size: var(--text-xs);
    color: var(--fg-secondary);
  }
  .gate-reasons li + li { margin-top: 0.15rem; }

  /* ── the answerable floor: evidence section ── */
  .regression-banner {
    display: flex;
    align-items: baseline;
    gap: var(--space-3);
    padding: var(--space-2) var(--space-3);
    border: 1px solid color-mix(in srgb, var(--error) 55%, var(--border));
    border-radius: var(--radius-md);
    background: rgba(var(--error-rgb), 0.08);
  }
  .regression-word {
    color: var(--error);
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: var(--tracking-wide);
    white-space: nowrap;
  }
  .regression-detail {
    color: var(--fg-secondary);
    font-size: var(--text-xs);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .verdict-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
  }
  .verdict-row {
    display: grid;
    grid-template-columns: 44px minmax(100px, 1.4fr) 92px minmax(80px, 1fr);
    gap: var(--space-3);
    align-items: baseline;
    padding: 3px var(--space-2);
    border-bottom: 1px solid var(--border-subtle);
    font-size: var(--text-xs);
  }
  .verdict-row:last-child { border-bottom: none; }
  .verdict-outcome {
    font-family: var(--font-mono);
    font-size: var(--text-2xs);
    font-weight: 700;
    letter-spacing: var(--tracking-wide);
  }
  .verdict-row[data-pass='pass'] .verdict-outcome { color: var(--success); }
  .verdict-row[data-pass='fail'] .verdict-outcome { color: var(--warning); }
  .verdict-gate { color: var(--fg-primary); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .verdict-score { color: var(--fg-secondary); font-variant-numeric: tabular-nums; }
  .verdict-meta { color: var(--fg-muted); font-size: var(--text-2xs); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }

  .provenance-row {
    display: flex;
    flex-wrap: wrap;
    gap: var(--space-2);
    margin-top: var(--space-2);
  }
  .prov-chip {
    display: inline-flex;
    align-items: baseline;
    gap: 0.4em;
    padding: 2px var(--space-2);
    border: 1px solid var(--border-subtle);
    border-radius: var(--radius-md);
    background: var(--bg-secondary);
    color: var(--fg-muted);
    font-size: var(--text-2xs);
    white-space: nowrap;
  }
  .prov-chip .mono { color: var(--fg-secondary); }
</style>
