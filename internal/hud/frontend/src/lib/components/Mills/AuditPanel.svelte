<script lang="ts">
  import { millsAuditStore, type AuditFinding } from '../../stores/mills_audit.svelte.ts';
  import { flexinferModelsStore, type ModelStatus } from '../../stores/flexinfer_models.svelte.ts';
  import PanelShell from '../shared/PanelShell.svelte';

  $effect(() => {
    millsAuditStore.startPolling(15000);
    flexinferModelsStore.startPolling();
    return () => {
      millsAuditStore.stopPolling();
      flexinferModelsStore.stopPolling();
    };
  });

  let entries = $derived(millsAuditStore.state);
  let loading = $derived(millsAuditStore.loading && entries.length === 0);
  let disabled = $derived(millsAuditStore.disabled);
  let error = $derived(millsAuditStore.error);
  let details = $derived(millsAuditStore.details);
  let counts = $derived(millsAuditStore.severityCounts);
  let avgSurvival = $derived(millsAuditStore.averageSurvival);
  let topCritical = $derived(millsAuditStore.topCritical);

  // expanded finding id → fetch full detail (Findings array + auditor pool)
  // on demand. Detail is fetched once per expand to keep the table calm.
  let expanded = $state<number | null>(null);

  function toggle(id: number): void {
    if (expanded === id) {
      expanded = null;
      return;
    }
    expanded = id;
    if (!details[id]) {
      void millsAuditStore.fetchDetail(id);
    }
  }

  function fmtScore(v: number | undefined): string {
    if (v == null || !Number.isFinite(v)) return '—';
    return v.toFixed(2);
  }

  function fmtPct(v: number | null): string {
    if (v == null || !Number.isFinite(v)) return '—';
    return `${(v * 100).toFixed(1)}%`;
  }

  function fmtCost(c: number | undefined): string {
    if (c == null || !Number.isFinite(c)) return '—';
    return `$${c.toFixed(4)}`;
  }

  function fmtTime(s: string): string {
    if (!s) return '—';
    const d = new Date(s);
    if (Number.isNaN(d.getTime())) return s;
    return d.toLocaleString();
  }

  function severityClass(severity: string): string {
    switch (severity) {
      case 'critical': return 'sev-critical';
      case 'warn': return 'sev-warn';
      case 'info': return 'sev-info';
      default: return 'sev-unknown';
    }
  }

  function survivalClass(score: number): string {
    if (score >= 0.85) return 'survival-good';
    if (score >= 0.6) return 'survival-warn';
    return 'survival-bad';
  }

  function subjectLabel(kind: string): string {
    switch (kind) {
      case 'council_artifact': return 'council';
      case 'pipeline_merge': return 'pipeline';
      default: return kind || 'unknown';
    }
  }

  function poolStatusTitle(status: ModelStatus, model: string): string {
    switch (status) {
      case 'ready':   return `${model}: Ready in flexinfer-system — accepting requests.`;
      case 'idle':    return `${model}: Idle / scaled-to-zero — requests will trigger a cold start (or fail if probe is offline).`;
      case 'unknown': return `${model}: not present in the flexinfer-system /v1/models registry, OR the registry hasn't loaded yet. If it's the former, the dispatcher will 404 on every call to this model.`;
    }
  }

  // A finding with score==0 AND cost==0 is suspect: a real adverse
  // finding still costs the auditor model an inference call. Zero on
  // both axes almost always means the dispatch failed (404, parse
  // error, timeout) before the model could score anything. Surfacing
  // this state explicitly stops operators from reading "0.0% survival,
  // 6 critical" as a real code-quality signal when it's actually
  // backend health.
  function isLikelyNoRun(entry: { SurvivalScore?: number; CostUSD?: number }): boolean {
    const score = entry.SurvivalScore ?? 0;
    const cost = entry.CostUSD ?? 0;
    return score === 0 && cost === 0;
  }
  // True when every visible entry looks like it never actually ran —
  // i.e., this isn't a code-quality problem, it's a backend outage.
  let allLikelyNoRun = $derived(
    entries.length > 0 && entries.every((e) => isLikelyNoRun(e))
  );
</script>

<PanelShell
  title="Audit"
  icon="◎"
  count={entries.length}
  loading={loading}
  empty={entries.length === 0}
  emptyIcon={disabled ? '◯' : '□'}
  emptyMessage={disabled
    ? 'Mills operator not configured'
    : (error ? 'Failed to load audit findings' : 'No audit findings yet')}
  emptyHint={disabled
    ? 'Set LOOM_MILLS_OPERATOR_URL on the HUD to connect.'
    : (error ?? 'Findings appear when council artifacts commit and pipeline merges land.')}
>
  {#if allLikelyNoRun}
    <div class="all-no-run-banner" role="alert">
      <strong>Every audit returned 0 with no cost.</strong>
      <span>
        This usually means the auditor pool models aren't producing
        output (404, parse error, or timeout) — not that every audit
        target is critically broken. Expand a row and check the
        <em>Auditor pool</em> section, then verify those models are
        Ready in flexinfer.
      </span>
    </div>
  {/if}

  <header class="audit-summary">
    <div class="summary-card">
      <span class="summary-label">avg survival</span>
      <span class="summary-value">{fmtPct(avgSurvival)}</span>
      <span class="summary-sub">{entries.length} findings in window</span>
    </div>
    <div class="summary-card">
      <span class="summary-label">severity</span>
      <div class="sev-bar" role="img" aria-label="severity histogram">
        <span class="sev-info" style="flex:{Math.max(counts.info, 0)}" aria-label="{counts.info} info"></span>
        <span class="sev-warn" style="flex:{Math.max(counts.warn, 0)}" aria-label="{counts.warn} warn"></span>
        <span class="sev-critical" style="flex:{Math.max(counts.critical, 0)}" aria-label="{counts.critical} critical"></span>
      </div>
      <span class="summary-sub">
        {counts.info} info · {counts.warn} warn · {counts.critical} critical
      </span>
    </div>
    {#if topCritical.length > 0}
      <div class="summary-card top-list">
        <span class="summary-label">lowest survival</span>
        <ul class="top-critical">
          {#each topCritical as f (f.ID)}
            <li class={survivalClass(f.SurvivalScore)}>
              <span class="critical-score">{fmtScore(f.SurvivalScore)}</span>
              <span class="critical-subject">{subjectLabel(f.SubjectKind)}/{f.SubjectID}</span>
            </li>
          {/each}
        </ul>
      </div>
    {/if}
  </header>

  <div class="audit-list">
    {#each entries as entry (entry.ID)}
      {@const detail = details[entry.ID] ?? entry}
      <article
        class="audit-row {severityClass(entry.Severity)}"
        class:expanded={expanded === entry.ID}
      >
        <button
          type="button"
          class="audit-row-btn"
          onclick={() => toggle(entry.ID)}
          aria-expanded={expanded === entry.ID}
        >
          <span class="audit-toggle">{expanded === entry.ID ? '▾' : '▸'}</span>
          <span class="audit-subject">
            <span class="subject-kind">{subjectLabel(entry.SubjectKind)}</span>
            <span class="subject-id">{entry.SubjectID}</span>
          </span>
          <span class="audit-score {survivalClass(entry.SurvivalScore)}">
            {fmtScore(entry.SurvivalScore)}
          </span>
          <span class="audit-sev-wrap">
            <span class="audit-sev sev-pill-{entry.Severity}">{entry.Severity}</span>
            {#if isLikelyNoRun(entry)}
              <span class="audit-badge badge-no-run" title="Score is 0 and no cost was recorded — the auditor model probably never produced a usable response">no-run</span>
            {/if}
          </span>
          <span class="audit-cost">{fmtCost(entry.CostUSD)}</span>
          <span class="audit-time">{fmtTime(entry.CreatedAt)}</span>
        </button>

        {#if expanded === entry.ID}
          <div class="audit-detail">
            {#if detail.Findings && detail.Findings.length > 0}
              <h3 class="detail-heading">Findings</h3>
              <ul class="finding-list">
                {#each detail.Findings as item (item.id ?? item.title ?? Math.random())}
                  <li class="finding-item">
                    <div class="finding-head">
                      <span class="finding-id">{item.id ?? '—'}</span>
                      <span class="finding-title">{item.title ?? '(no title)'}</span>
                      <span class="finding-sev sev-pill-{item.severity}">{item.severity ?? 'info'}</span>
                    </div>
                    {#if item.detail}
                      <p class="finding-detail">{item.detail}</p>
                    {/if}
                  </li>
                {/each}
              </ul>
            {:else if isLikelyNoRun(entry)}
              <p class="detail-empty detail-empty-warn">
                <strong>No findings recorded.</strong>
                Combined with the zero cost above, this strongly suggests
                the auditor pool never produced a parseable response.
                Check the Auditor pool below.
              </p>
            {:else}
              <p class="detail-empty">No structured findings — score speaks for itself.</p>
            {/if}

            {#if detail.AuditorPool && detail.AuditorPool.length > 0}
              <h3 class="detail-heading">Auditor pool</h3>
              <ul class="pool-list">
                {#each detail.AuditorPool as m, i (i)}
                  {@const status = m.backend === 'flexinfer' ? flexinferModelsStore.statusFor(m.model ?? '') : 'unknown'}
                  <li class="pool-item">
                    <span class="pool-backend">{m.backend ?? '—'}</span>
                    <span class="pool-model">{m.model ?? '—'}</span>
                    {#if m.backend === 'flexinfer'}
                      <span
                        class="pool-status pool-status-{status}"
                        title={poolStatusTitle(status, m.model ?? '')}
                      >{status}</span>
                    {:else}
                      <span class="pool-status pool-status-unknown" title="Status checks are only available for flexinfer-backed models today.">—</span>
                    {/if}
                    <span class="pool-role role-{m.role ?? 'unknown'}">{m.role ?? 'unknown'}</span>
                  </li>
                {/each}
              </ul>
            {/if}

            <p class="detail-meta">
              rubric <code>{detail.RubricID || 'audit_v1'}</code>
              · finding <code>#{entry.ID}</code>
            </p>
          </div>
        {/if}
      </article>
    {/each}
  </div>
</PanelShell>

<style>
  .all-no-run-banner {
    margin: 0.25rem 0.25rem 0.75rem;
    padding: 0.7rem 0.9rem;
    background: rgba(224, 108, 117, 0.10);
    border: 1px solid rgba(224, 108, 117, 0.4);
    border-left: 3px solid var(--danger, #e06c75);
    border-radius: 6px;
    color: var(--text-default, #eef);
    font-size: 0.85rem;
    line-height: 1.5;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }
  .all-no-run-banner strong { color: var(--danger, #e06c75); }
  .all-no-run-banner em { color: var(--text-default, #eef); font-style: normal; font-weight: 500; }

  .audit-summary {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));
    gap: 0.75rem;
    padding: 0.5rem 0.25rem 0.75rem;
  }
  .summary-card {
    background: var(--bg-subtle, #1a1f2a);
    border: 1px solid var(--border-subtle, #233);
    border-radius: 6px;
    padding: 0.6rem 0.8rem;
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  .summary-label {
    font-size: 0.7rem;
    color: var(--text-muted, #97a3b6);
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .summary-value {
    font-size: 1.5rem;
    font-weight: 600;
    color: var(--text-default, #eef);
  }
  .summary-sub {
    font-size: 0.75rem;
    color: var(--text-muted, #97a3b6);
  }

  .sev-bar {
    display: flex;
    height: 0.65rem;
    border-radius: 4px;
    overflow: hidden;
    background: var(--bg-deep, #11151c);
  }
  .sev-bar > span { display: block; min-width: 0; }
  .sev-bar .sev-info { background: var(--success, #4ec9b0); }
  .sev-bar .sev-warn { background: var(--warning, #d7a03a); }
  .sev-bar .sev-critical { background: var(--danger, #e06c75); }

  .top-list { padding-bottom: 0.5rem; }
  .top-critical {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
    font-size: 0.78rem;
  }
  .top-critical li {
    display: flex;
    gap: 0.5rem;
    align-items: baseline;
  }
  .critical-score {
    font-variant-numeric: tabular-nums;
    width: 2.7rem;
  }
  .critical-subject {
    color: var(--text-muted, #97a3b6);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .audit-list {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    padding: 0 0.25rem 0.5rem;
  }
  .audit-row {
    background: var(--bg-subtle, #1a1f2a);
    border: 1px solid var(--border-subtle, #233);
    border-radius: 6px;
    overflow: hidden;
  }
  .audit-row.expanded {
    border-color: var(--border-strong, #345);
  }
  .audit-row.sev-critical {
    border-left: 3px solid var(--danger, #e06c75);
  }
  .audit-row.sev-warn {
    border-left: 3px solid var(--warning, #d7a03a);
  }
  .audit-row.sev-info {
    border-left: 3px solid var(--success, #4ec9b0);
  }
  .audit-row-btn {
    display: grid;
    /* Severity column widened from 4rem → 9rem so it can fit the
       severity pill AND the optional 'no-run' badge side-by-side
       without spilling into the Cost column. The previous 4rem was
       big enough for just the pill (CRITICAL is the widest at ~7ch)
       but the no-run badge added in MR !502 pushed the wrap beyond
       the cell, mashing into '$0.0000' as 'NO-$Q0000'. */
    grid-template-columns: 1.2rem minmax(0, 1.6fr) 4rem 9rem 5rem 1fr;
    align-items: center;
    gap: 0.6rem;
    width: 100%;
    text-align: left;
    background: transparent;
    border: none;
    padding: 0.55rem 0.75rem;
    color: var(--text-default, #eef);
    cursor: pointer;
    font-size: 0.85rem;
  }
  .audit-row-btn:hover .subject-id {
    color: var(--text-link, #8cc8ff);
  }
  .audit-toggle {
    color: var(--text-muted, #97a3b6);
  }
  .audit-subject {
    display: flex;
    flex-direction: column;
    gap: 0.1rem;
    overflow: hidden;
  }
  .subject-kind {
    font-size: 0.65rem;
    text-transform: uppercase;
    color: var(--text-muted, #97a3b6);
    letter-spacing: 0.04em;
  }
  .subject-id {
    font-family: var(--font-mono, monospace);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .audit-score {
    font-variant-numeric: tabular-nums;
    text-align: right;
  }
  .survival-good { color: var(--success, #4ec9b0); }
  .survival-warn { color: var(--warning, #d7a03a); }
  .survival-bad  { color: var(--danger, #e06c75); }
  .audit-sev,
  .finding-sev {
    text-align: center;
    border-radius: 4px;
    padding: 0.1rem 0.45rem;
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .sev-pill-info { background: rgba(78, 201, 176, 0.18); color: var(--success, #4ec9b0); }
  .sev-pill-warn { background: rgba(215, 160, 58, 0.20); color: var(--warning, #d7a03a); }
  .sev-pill-critical { background: rgba(224, 108, 117, 0.22); color: var(--danger, #e06c75); }
  .audit-sev-wrap {
    display: flex;
    align-items: center;
    gap: 0.35rem;
    min-width: 0;
  }
  .audit-badge {
    text-align: center;
    border-radius: 4px;
    padding: 0.1rem 0.45rem;
    font-size: 0.65rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    font-family: ui-monospace, monospace;
    white-space: nowrap;
  }
  .badge-no-run {
    background: rgba(215, 160, 58, 0.18);
    color: var(--warning, #d7a03a);
    border: 1px solid rgba(215, 160, 58, 0.4);
  }
  .audit-cost {
    color: var(--text-muted, #97a3b6);
    font-variant-numeric: tabular-nums;
    text-align: right;
  }
  .audit-time {
    color: var(--text-muted, #97a3b6);
    font-size: 0.78rem;
    text-align: right;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .audit-detail {
    border-top: 1px solid var(--border-subtle, #233);
    padding: 0.65rem 0.85rem;
    display: flex;
    flex-direction: column;
    gap: 0.55rem;
  }
  .detail-heading {
    margin: 0;
    font-size: 0.78rem;
    color: var(--text-muted, #97a3b6);
    text-transform: uppercase;
    letter-spacing: 0.05em;
  }
  .detail-empty {
    margin: 0;
    color: var(--text-muted, #97a3b6);
    font-style: italic;
    font-size: 0.85rem;
  }
  .detail-empty-warn {
    padding: 0.5rem 0.7rem;
    background: rgba(215, 160, 58, 0.10);
    border-left: 3px solid var(--warning, #d7a03a);
    border-radius: 3px;
    color: var(--text-default, #eef);
    font-style: normal;
    line-height: 1.45;
  }
  .detail-empty-warn strong { color: var(--warning, #d7a03a); }
  .detail-meta {
    margin: 0;
    color: var(--text-muted, #97a3b6);
    font-size: 0.75rem;
  }
  .detail-meta code {
    background: var(--bg-deep, #11151c);
    padding: 0.05rem 0.3rem;
    border-radius: 3px;
  }
  .finding-list,
  .pool-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
  }
  .finding-item {
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  .finding-head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.85rem;
    flex-wrap: wrap;
  }
  .finding-id {
    font-family: var(--font-mono, monospace);
    font-size: 0.75rem;
    color: var(--text-muted, #97a3b6);
  }
  .finding-title {
    font-weight: 500;
    flex: 1 1 auto;
  }
  .finding-detail {
    margin: 0;
    color: var(--text-muted, #97a3b6);
    font-size: 0.82rem;
    line-height: 1.4;
  }
  .pool-item {
    display: grid;
    grid-template-columns: 5rem 1fr 4.5rem 5rem;
    align-items: center;
    gap: 0.5rem;
    font-size: 0.78rem;
  }
  .pool-backend {
    color: var(--text-muted, #97a3b6);
  }
  .pool-model {
    font-family: var(--font-mono, monospace);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .pool-status {
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
  .pool-status-ready {
    background: rgba(78, 201, 176, 0.18);
    color: var(--success, #4ec9b0);
    border: 1px solid rgba(78, 201, 176, 0.4);
  }
  .pool-status-idle {
    background: rgba(215, 160, 58, 0.18);
    color: var(--warning, #d7a03a);
    border: 1px solid rgba(215, 160, 58, 0.4);
  }
  .pool-status-unknown {
    background: rgba(224, 108, 117, 0.15);
    color: var(--danger, #e06c75);
    border: 1px solid rgba(224, 108, 117, 0.35);
  }
  .pool-role {
    text-align: right;
    color: var(--text-muted, #97a3b6);
    font-size: 0.7rem;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }
  .role-bulk { color: var(--text-default, #eef); }
  .role-escalation { color: var(--warning, #d7a03a); }
</style>
