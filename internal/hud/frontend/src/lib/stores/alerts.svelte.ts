// Alerting store — the pipeline alert engine and the auto-fix proposal queue.
//
// Backed by internal/hud/domain/alerting (registrations in alerting.go,
// handlers in handlers.go), over two engines with very different wiring status.
// Read the asymmetry before touching the UI copy:
//
//   ALERT ENGINE — live. internal/hud/embed.go:272 constructs it and feeds it
//   the pipeline monitor's snapshots every cycle.
//     GET  /api/alerts[?limit&severity]  → {alerts:[alerting.Alert]} newest-first
//     GET  /api/alerts/rules             → {rules:[alerting.AlertRule]}
//     PUT  /api/alerts/rules             → {updated,count}   (admin-gated)
//     POST /api/alerts/{id}/ack          → {acked,id}        (NOT admin-gated)
//
//   AUTO-FIX ENGINE — declared and never constructed. `App.autofixEngine`
//   (internal/hud/app.go:262) has no assignment anywhere outside tests, so on
//   every shipped HUD it is nil. The consequence is deliberately visible here:
//     GET  /api/autofix/proposals   → 200 {"proposals":[]}   (nil ⇒ empty, not 503)
//     GET  /api/autofix/executions  → 200 {"executions":[]}  (same)
//     POST /api/alerts/diagnose     → 503 "auto-fix engine not configured"
//     POST /api/autofix/proposals/{id}/approve → 503 (admin-gated first)
//     POST /api/autofix/proposals/{id}/reject  → 503 (NOT admin-gated)
//   The two list routes answer 200-with-[] whether the engine is nil or merely
//   idle, so the lists alone cannot tell "not configured" from "nothing
//   pending". Only a mutation can, which is why `autofixUnavailable` is set
//   from a 503 on diagnose/approve/reject rather than guessed from the lists.
//
// Alert lifecycle: the engine sets AckedAt on ack and never sets ResolvedAt
// (nothing in engine.go writes it), so "active" here means un-acked. History
// is a 200-entry ring.
//
// Proposal lifecycle: AutoFixProposal carries NO status field. Approve appends
// an AutoFixExecution (running/failed) and reject appends one with
// status:"rejected" — both keyed by proposal_id. "Pending" is therefore
// derived: a proposal with no execution naming it. That is the server's own
// definition, not a frontend convention.

import { createPoller } from '../utils/poller.ts';
import { errorMessage, fetchJSON } from '../utils/apiJson.ts';
import { adminFetch } from './labsAuth.svelte.ts';

/** alerting.PipelineRef — the pipeline an alert fired against. */
export interface AlertPipelineRef {
  id: number;
  project: string;
  ref: string;
  status: string;
  url?: string;
}

/** alerting.Alert. */
export interface Alert {
  id: string;
  rule_id: string;
  rule_name: string;
  /** critical | warning | info */
  severity: string;
  title: string;
  message: string;
  pipeline: AlertPipelineRef;
  fired_at: string;
  acked_at?: string | null;
  acked_by?: string;
  resolved_at?: string | null;
  autofix_id?: string;
}

/** alerting.AlertCondition. `duration` is a Go time.Duration — NANOSECONDS. */
export interface AlertCondition {
  type: string;
  threshold: number;
  duration?: number;
  projects?: string[] | null;
}

/** alerting.AlertRule. `cooldown` is a Go time.Duration — NANOSECONDS. */
export interface AlertRule {
  id: string;
  name: string;
  enabled: boolean;
  condition: AlertCondition;
  severity: string;
  cooldown: number;
  /** Zero value serialises as "0001-01-01T00:00:00Z", i.e. never fired. */
  last_fired: string;
}

/** autofix.AutoFixProposal. Note the json tag: Files → `estimated_files`. */
export interface AutoFixProposal {
  id: string;
  diagnosis_id: string;
  description: string;
  /** agent_fix | retry | manual */
  strategy: string;
  estimated_files?: string[] | null;
  confidence: number;
  requires_approval: boolean;
  created_at: string;
}

/** autofix.AutoFixExecution. */
export interface AutoFixExecution {
  id: string;
  proposal_id: string;
  /** pending_approval | running | succeeded | failed | rejected */
  status: string;
  agent_id?: string;
  spawn_id?: string;
  result?: string;
  started_at: string;
  completed_at?: string | null;
}

/** autofix.Diagnosis — the LLM's structured read of a failed pipeline. */
export interface Diagnosis {
  pipeline_id: number;
  project: string;
  root_cause: string;
  /** test_failure | build_error | lint | dependency | infra */
  category: string;
  suggested_fix: string;
  /** 0..1, clamped server-side. 0.3 is the parse-failure fallback. */
  confidence: number;
  failed_jobs?: string[] | null;
  log_snippets?: string[] | null;
}

/** POST /api/alerts/diagnose response — the handler proposes off the diagnosis. */
export interface DiagnoseResult {
  diagnosis: Diagnosis;
  proposal?: AutoFixProposal;
}

interface AlertsResponse {
  alerts?: Alert[] | null;
}
interface RulesResponse {
  rules?: AlertRule[] | null;
}
interface ProposalsResponse {
  proposals?: AutoFixProposal[] | null;
}
interface ExecutionsResponse {
  executions?: AutoFixExecution[] | null;
}

/** Severity → badge tone. Unknown severities stay neutral rather than vanish. */
export function severityTone(severity: string): 'crit' | 'warn' | 'info' | 'muted' {
  if (severity === 'critical') return 'crit';
  if (severity === 'warning') return 'warn';
  if (severity === 'info') return 'info';
  return 'muted';
}

/** Render a Go time.Duration (nanoseconds) as "5m" / "30m" / "2h" / "—". */
export function formatGoDuration(ns: number | undefined | null): string {
  if (!ns || ns <= 0) return '—';
  const secs = Math.round(ns / 1e9);
  if (secs < 60) return `${secs}s`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.round(hours / 24)}d`;
}

/** True for Go's zero time, which the rules endpoint serves for "never fired". */
export function isZeroTime(ts: string | null | undefined): boolean {
  return !ts || ts.startsWith('0001-01-01');
}

/** Message the alerting handlers use when an engine is nil. */
const NOT_CONFIGURED = 'not configured';

/**
 * postAlerting issues a mutation and normalises the failure modes. Throws on
 * any non-2xx so callers ride the shared runAdminAction toast path; a 503
 * naming an unconfigured engine additionally flips `autofixUnavailable`, which
 * is the only signal the backend gives that the auto-fix engine is nil.
 */
async function postAlerting<T>(
  url: string,
  opts: { requireToken?: boolean; action?: string; body?: unknown } = {},
): Promise<{ data: T | null; notConfigured: boolean }> {
  const res = await adminFetch(url, {
    method: 'POST',
    requireToken: opts.requireToken ?? false,
    action: opts.action ?? 'This action',
    headers: opts.body === undefined ? undefined : { 'content-type': 'application/json' },
    body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
  });

  const text = await res.text();
  let parsed: unknown = null;
  try {
    parsed = text.trim() === '' ? null : JSON.parse(text);
  } catch {
    parsed = null;
  }

  if (!res.ok) {
    let detail = text.slice(0, 200);
    if (parsed && typeof (parsed as { error?: unknown }).error === 'string') {
      detail = (parsed as { error: string }).error;
    }
    const notConfigured = res.status === 503 && detail.includes(NOT_CONFIGURED);
    const err = new Error(`HTTP ${res.status}: ${detail}`) as Error & { notConfigured?: boolean };
    err.notConfigured = notConfigured;
    throw err;
  }

  return { data: parsed as T | null, notConfigured: false };
}

class AlertsStore {
  alerts = $state<Alert[]>([]);
  rules = $state<AlertRule[]>([]);
  proposals = $state<AutoFixProposal[]>([]);
  executions = $state<AutoFixExecution[]>([]);

  loading = $state(false);
  error = $state<string | null>(null);
  /** True when this HUD build registers no /api/alerts routes at all. */
  unavailable = $state(false);
  /**
   * True once a mutation has proven the auto-fix engine is nil (503). Starts
   * false: the list routes answer 200-with-[] either way, so this cannot be
   * known until something is attempted.
   */
  autofixUnavailable = $state(false);

  /** Alert id currently being acked / diagnosed, for per-row button state. */
  acking = $state<string | null>(null);
  diagnosing = $state<string | null>(null);
  /** Proposal id currently being approved/rejected. */
  busyProposal = $state<string | null>(null);

  /** Diagnosis results keyed by alert id, kept for the session. */
  diagnoses = $state<Record<string, DiagnoseResult>>({});

  // 20s: alerts are evaluated off the pipeline monitor's own cycle, so a
  // tighter poll just re-reads the same ring.
  private poller = createPoller(() => this.fetch(), 20000);

  /**
   * Un-acked alerts. `GET /api/alerts` already withholds resolved alerts (the
   * engine resolves a stuck alert once its pipeline settles), so this filter
   * is belt-and-braces for anything served before that landed.
   */
  get activeAlerts(): Alert[] {
    return this.alerts.filter((a) => !a.acked_at && !a.resolved_at);
  }

  /** Acked alerts, i.e. the triaged tail of the 200-entry ring. */
  get handledAlerts(): Alert[] {
    return this.alerts.filter((a) => a.acked_at || a.resolved_at);
  }

  get criticalCount(): number {
    return this.activeAlerts.filter((a) => a.severity === 'critical').length;
  }

  /** Active alerts bucketed by severity, largest bucket first, zeroes dropped. */
  get severityCounts(): Array<[string, number]> {
    const counts: Record<string, number> = {};
    for (const a of this.activeAlerts) counts[a.severity] = (counts[a.severity] ?? 0) + 1;
    return Object.entries(counts).sort((x, y) => y[1] - x[1]);
  }

  /** Executions naming a given proposal, newest-first as served. */
  executionsFor(proposalID: string): AutoFixExecution[] {
    return this.executions.filter((e) => e.proposal_id === proposalID);
  }

  /**
   * Proposals with no execution recorded against them. The server has no
   * proposal status field — approve and reject BOTH append an execution — so
   * this is the authoritative "still awaiting a decision" set.
   */
  get pendingProposals(): AutoFixProposal[] {
    const decided = new Set(this.executions.map((e) => e.proposal_id));
    return this.proposals.filter((p) => !decided.has(p.id));
  }

  get enabledRuleCount(): number {
    return this.rules.filter((r) => r.enabled).length;
  }

  async fetch(): Promise<void> {
    this.loading = true;
    try {
      const [alerts, rules, proposals, executions] = await Promise.all([
        fetchJSON<AlertsResponse>('/api/alerts?limit=100', { absentStatuses: [503] }),
        fetchJSON<RulesResponse>('/api/alerts/rules', { absentStatuses: [503] }),
        fetchJSON<ProposalsResponse>('/api/autofix/proposals', { absentStatuses: [503] }),
        fetchJSON<ExecutionsResponse>('/api/autofix/executions', { absentStatuses: [503] }),
      ]);

      // The alert list is the panel's spine: absent ⇒ this build has no
      // alerting domain, and nothing below is worth rendering.
      if (alerts === null) {
        this.unavailable = true;
        this.alerts = [];
        this.rules = [];
        this.proposals = [];
        this.executions = [];
        this.error = null;
        return;
      }

      this.unavailable = false;
      this.alerts = alerts.alerts ?? [];
      // Rules and the auto-fix surfaces are independently absent-able; a miss
      // on any of them must not blank the alert table.
      this.rules = rules?.rules ?? [];
      this.proposals = proposals?.proposals ?? [];
      this.executions = executions?.executions ?? [];
      this.error = null;
    } catch (e) {
      this.error = errorMessage(e);
    } finally {
      this.loading = false;
    }
  }

  /**
   * Acknowledge an alert. Server-side this is NOT admin-gated; adminFetch is
   * still used so a pasted token rides along when the deployment sits behind
   * one. Throws on failure (runAdminAction path).
   */
  async ack(alertID: string, ackedBy = 'hud-user'): Promise<void> {
    this.acking = alertID;
    try {
      await postAlerting(`/api/alerts/${encodeURIComponent(alertID)}/ack`, {
        body: { acked_by: ackedBy },
      });
      await this.fetch();
    } finally {
      this.acking = null;
    }
  }

  /**
   * Run LLM diagnosis over the alert's pipeline. Admin-gated server-side, and
   * slow — it fetches the pipeline detail, pulls every failed job's trace, and
   * waits on a completion. The handler also auto-proposes a fix from the
   * diagnosis and returns it alongside.
   *
   * Returns null when the auto-fix engine is not configured (503), which is
   * the state of every currently shipped HUD.
   */
  async diagnose(alert: Alert): Promise<DiagnoseResult | null> {
    this.diagnosing = alert.id;
    try {
      const { data } = await postAlerting<DiagnoseResult>('/api/alerts/diagnose', {
        requireToken: true,
        action: 'Diagnosis',
        body: { project: alert.pipeline?.project, pipeline_id: alert.pipeline?.id },
      });
      if (data) {
        this.diagnoses = { ...this.diagnoses, [alert.id]: data };
        // A diagnosis mints a proposal server-side; pick it up.
        await this.fetch();
      }
      return data;
    } catch (e) {
      if ((e as { notConfigured?: boolean }).notConfigured) {
        this.autofixUnavailable = true;
      }
      throw e;
    } finally {
      this.diagnosing = null;
    }
  }

  /**
   * Approve a proposal. Admin-gated. Server-side this calls ExecuteAutoFix,
   * which spawns a fixer agent for `agent_fix`, is a NO-OP that records
   * "pipeline retry requested" for `retry` (internal/hud/autofix/autofix.go:278
   * — no pipeline is actually re-run), and records a failure for `manual`.
   */
  async approve(proposalID: string): Promise<void> {
    this.busyProposal = proposalID;
    try {
      await postAlerting(`/api/autofix/proposals/${encodeURIComponent(proposalID)}/approve`, {
        requireToken: true,
        action: 'Approving a proposal',
      });
      await this.fetch();
    } catch (e) {
      if ((e as { notConfigured?: boolean }).notConfigured) this.autofixUnavailable = true;
      throw e;
    } finally {
      this.busyProposal = null;
    }
  }

  /**
   * Reject a proposal. NOT admin-gated server-side. Records a `rejected`
   * execution against the proposal; the proposal itself is never removed.
   */
  async reject(proposalID: string): Promise<void> {
    this.busyProposal = proposalID;
    try {
      await postAlerting(`/api/autofix/proposals/${encodeURIComponent(proposalID)}/reject`, {
        action: 'Rejecting a proposal',
      });
      await this.fetch();
    } catch (e) {
      if ((e as { notConfigured?: boolean }).notConfigured) this.autofixUnavailable = true;
      throw e;
    } finally {
      this.busyProposal = null;
    }
  }

  startPolling(intervalMs = 20000): void {
    void this.fetch();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

export const alertsStore = new AlertsStore();
