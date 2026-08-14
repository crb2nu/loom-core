// Mill Staff store — the five operator evidence reports behind the Mill Staff
// panel, proxied through /api/mills/{promotion-report,judge-calibration,
// regressions,config-outcomes,signature-candidates} by the HUD's domain/mills
// package.
//
// Every report is an independent slot: one unreachable endpoint must not blank
// the other four, so each carries its own data/error/disabled rather than the
// single panel-wide error the smaller Mills stores use. A 503 from the proxy
// means the HUD has no operator URL set — a calm "not configured" state, not a
// fetch error.
//
// Payloads are normalised at the fetch boundary (Go nil slices arrive as JSON
// `null`), so no component $derived ever spreads or iterates a null. The
// cadence is 60s, not the Mills-standard 15s: each report is a window-bounded
// scan over the events table, and the numbers move on the order of hours.

import { untrack } from 'svelte';
import { createPoller } from '../utils/poller.ts';

// ---- Wire shapes (snake_case, matching the operator JSON) ----

export interface PromotionAction {
  action: string;
  dry_run: number;
  executed: number;
  unique_subjects: number;
  subject_sample: string[];
  first: string;
  last: string;
}

export interface PromotionActor {
  actor: string;
  per_action: PromotionAction[];
}

export interface PromotionReport {
  actor_prefix: string;
  window_start: string;
  window_end: string;
  total_actions: number;
  total_dry_run: number;
  total_executed: number;
  per_actor: PromotionActor[];
  zero_evidence: boolean;
}

export interface JudgeScoreBucket {
  bucket: string;
  merged: number;
  escalated: number;
  other: number;
}

export interface JudgeGate {
  gate: string;
  verdicts: number;
  passed: number;
  pass_rate: number;
  merged_verdicts: number;
  escalated_verdicts: number;
  other_verdicts: number;
  mean_score_merged: number;
  mean_score_escalated: number;
  histogram: JudgeScoreBucket[];
}

export interface JudgeModel {
  model: string;
  role: string;
  verdicts: number;
}

export interface JudgeCalibrationReport {
  window_start: string;
  window_end: string;
  total_verdicts: number;
  joined_verdicts: number;
  per_gate: JudgeGate[];
  buckets: string[];
  outcomes: string[];
  models: JudgeModel[];
  zero_evidence: boolean;
}

export interface RegressionAttribution {
  regressed_mr_iid: number;
  merged_sha: string;
  revert_sha: string;
  revert_title: string;
  attributed_at: string;
}

export interface RegressionsReport {
  window: string;
  since: string;
  count: number;
  regressions: RegressionAttribution[];
}

// ConfigOutcomeStats is embedded in Go, so its fields flatten into each group.
export interface ConfigOutcomeStats {
  runs: number;
  merged: number;
  escalated: number;
  other: number;
  merge_rate: number;
  judge_graded_runs: number;
  mean_judge_score: number;
  judge_pass_rate: number;
  costed_runs: number;
  total_cost_usd: number;
  mean_cost_usd: number;
  regressions: number;
}

export interface PolicyOutcomeGroup extends ConfigOutcomeStats {
  policy_checksum: string;
}

export interface StageModelOutcomeGroup extends ConfigOutcomeStats {
  stage: string;
  model: string;
}

export interface ConfigRegressionSummary {
  total: number;
  linked: number;
  unlinked: number;
}

export interface ConfigOutcomeReport {
  window_start: string;
  window_end: string;
  stamped_runs: number;
  uncovered_runs: number;
  totals: ConfigOutcomeStats;
  per_policy_checksum: PolicyOutcomeGroup[];
  per_stage_model: StageModelOutcomeGroup[];
  regressions: ConfigRegressionSummary;
  zero_evidence: boolean;
}

export interface SignatureCandidate {
  fingerprint: string;
  phrase: string;
  member_count: number;
  window_match_count: number;
  sample_evidence: string[];
  first_seen?: string;
  last_seen?: string;
  proposed_at: string;
}

export interface SignatureCandidatesReport {
  window: string;
  since: string;
  count: number;
  candidates: SignatureCandidate[];
}

// ---- Slots ----

// ReportSlot is one report's independent state. `data` holds the last good
// snapshot: a transient operator blip surfaces via `error` on top of stale
// numbers rather than blanking the tile.
export interface ReportSlot<T> {
  data: T | null;
  error: string | null;
  disabled: boolean;
  lastUpdated: Date | null;
}

function emptySlot<T>(): ReportSlot<T> {
  return { data: null, error: null, disabled: false, lastUpdated: null };
}

// Window options offered by the panel. The operator defaults to 336h for four
// of the five reports; the promotion report defaults to 168h. The panel drives
// all five off one window so the tiles can be read against each other.
export const STAFF_WINDOWS = ['168h', '336h', '720h'] as const;
export type StaffWindow = (typeof STAFF_WINDOWS)[number];

const zeroStats: ConfigOutcomeStats = {
  runs: 0,
  merged: 0,
  escalated: 0,
  other: 0,
  merge_rate: 0,
  judge_graded_runs: 0,
  mean_judge_score: 0,
  judge_pass_rate: 0,
  costed_runs: 0,
  total_cost_usd: 0,
  mean_cost_usd: 0,
  regressions: 0,
};

const zeroRegressionSummary: ConfigRegressionSummary = { total: 0, linked: 0, unlinked: 0 };

// ---- Normalisers: every list and every nested struct defaulted here ----

function normalisePromotion(raw: PromotionReport | null): PromotionReport | null {
  if (!raw) return null;
  return {
    ...raw,
    actor_prefix: raw.actor_prefix ?? '',
    total_actions: raw.total_actions ?? 0,
    total_dry_run: raw.total_dry_run ?? 0,
    total_executed: raw.total_executed ?? 0,
    per_actor: (raw.per_actor ?? []).map((a) => ({
      actor: a?.actor ?? '',
      per_action: (a?.per_action ?? []).map((x) => ({
        ...x,
        dry_run: x?.dry_run ?? 0,
        executed: x?.executed ?? 0,
        unique_subjects: x?.unique_subjects ?? 0,
        subject_sample: x?.subject_sample ?? [],
      })),
    })),
    zero_evidence: raw.zero_evidence === true,
  };
}

function normaliseJudge(raw: JudgeCalibrationReport | null): JudgeCalibrationReport | null {
  if (!raw) return null;
  return {
    ...raw,
    total_verdicts: raw.total_verdicts ?? 0,
    joined_verdicts: raw.joined_verdicts ?? 0,
    per_gate: (raw.per_gate ?? []).map((g) => ({
      ...g,
      gate: g?.gate ?? '',
      verdicts: g?.verdicts ?? 0,
      passed: g?.passed ?? 0,
      pass_rate: g?.pass_rate ?? 0,
      merged_verdicts: g?.merged_verdicts ?? 0,
      escalated_verdicts: g?.escalated_verdicts ?? 0,
      other_verdicts: g?.other_verdicts ?? 0,
      mean_score_merged: g?.mean_score_merged ?? 0,
      mean_score_escalated: g?.mean_score_escalated ?? 0,
      histogram: g?.histogram ?? [],
    })),
    buckets: raw.buckets ?? [],
    outcomes: raw.outcomes ?? [],
    models: raw.models ?? [],
    zero_evidence: raw.zero_evidence === true,
  };
}

function normaliseRegressions(raw: RegressionsReport | null): RegressionsReport | null {
  if (!raw) return null;
  const regressions = raw.regressions ?? [];
  return {
    ...raw,
    window: raw.window ?? '',
    // count and the list can disagree only if the wire truncated; trust the list.
    count: raw.count ?? regressions.length,
    regressions,
  };
}

function normaliseConfigOutcomes(raw: ConfigOutcomeReport | null): ConfigOutcomeReport | null {
  if (!raw) return null;
  return {
    ...raw,
    stamped_runs: raw.stamped_runs ?? 0,
    uncovered_runs: raw.uncovered_runs ?? 0,
    totals: { ...zeroStats, ...(raw.totals ?? {}) },
    per_policy_checksum: (raw.per_policy_checksum ?? []).map((g) => ({ ...zeroStats, ...g })),
    per_stage_model: (raw.per_stage_model ?? []).map((g) => ({ ...zeroStats, ...g })),
    regressions: { ...zeroRegressionSummary, ...(raw.regressions ?? {}) },
    zero_evidence: raw.zero_evidence === true,
  };
}

function normaliseSignatures(
  raw: SignatureCandidatesReport | null,
): SignatureCandidatesReport | null {
  if (!raw) return null;
  const candidates = (raw.candidates ?? []).map((c) => ({
    ...c,
    phrase: c?.phrase ?? '',
    member_count: c?.member_count ?? 0,
    window_match_count: c?.window_match_count ?? 0,
    sample_evidence: c?.sample_evidence ?? [],
  }));
  return { ...raw, window: raw.window ?? '', count: raw.count ?? candidates.length, candidates };
}

class MillsStaffStore {
  promotion = $state<ReportSlot<PromotionReport>>(emptySlot());
  councilPromotion = $state<ReportSlot<PromotionReport>>(emptySlot());
  judge = $state<ReportSlot<JudgeCalibrationReport>>(emptySlot());
  regressions = $state<ReportSlot<RegressionsReport>>(emptySlot());
  configOutcomes = $state<ReportSlot<ConfigOutcomeReport>>(emptySlot());
  signatures = $state<ReportSlot<SignatureCandidatesReport>>(emptySlot());

  window = $state<StaffWindow>('336h');
  loading = $state(false);

  private poller = createPoller(() => {
    void this.refresh();
  }, 60000);

  async refresh(): Promise<void> {
    // Snapshot every reactive read OUTSIDE the caller's tracking context.
    // refresh() runs synchronously inside panel mount $effects; a tracked
    // read of the slots it writes on completion re-runs the effect per
    // finished round — an infinite refetch loop at network round-trip
    // cadence (observed hammering the operator at ~1.5s from one open tab).
    const { w, prev } = untrack(() => ({
      w: encodeURIComponent(this.window),
      prev: {
        promotion: this.promotion,
        councilPromotion: this.councilPromotion,
        judge: this.judge,
        regressions: this.regressions,
        configOutcomes: this.configOutcomes,
        signatures: this.signatures,
      },
    }));
    this.loading = true;
    try {
      const [promotion, councilPromotion, judge, regressions, configOutcomes, signatures] =
        await Promise.all([
          fetchSlot(
            `/api/mills/promotion-report?actor=overseer.&window=${w}`,
            prev.promotion,
            normalisePromotion,
          ),
          // The Drawing Office's guarded writer is council.mutator; the same
          // report scoped to that prefix is the department's own
          // dry-run→promote evidence.
          fetchSlot(
            `/api/mills/promotion-report?actor=council.&window=${w}`,
            prev.councilPromotion,
            normalisePromotion,
          ),
          fetchSlot(`/api/mills/judge-calibration?window=${w}`, prev.judge, normaliseJudge),
          fetchSlot(`/api/mills/regressions?window=${w}`, prev.regressions, normaliseRegressions),
          fetchSlot(
            `/api/mills/config-outcomes?window=${w}`,
            prev.configOutcomes,
            normaliseConfigOutcomes,
          ),
          fetchSlot(
            `/api/mills/signature-candidates?window=${w}`,
            prev.signatures,
            normaliseSignatures,
          ),
        ]);
      this.promotion = promotion;
      this.councilPromotion = councilPromotion;
      this.judge = judge;
      this.regressions = regressions;
      this.configOutcomes = configOutcomes;
      this.signatures = signatures;
    } finally {
      this.loading = false;
    }
  }

  setWindow(next: StaffWindow): void {
    if (next === this.window) return;
    this.window = next;
    void this.refresh();
  }

  startPolling(intervalMs = 60000): void {
    void this.refresh();
    this.poller.start(intervalMs);
  }

  stopPolling(): void {
    this.poller.stop();
  }
}

// fetchSlot resolves one report into its next slot state. It never rejects: a
// failed report is a state, not an exception, so Promise.all over the six
// reports cannot let one outage cancel the other five.
async function fetchSlot<T>(
  path: string,
  prev: ReportSlot<T>,
  normalise: (raw: T | null) => T | null,
): Promise<ReportSlot<T>> {
  try {
    const raw = await getJSON<T>(path);
    return { data: normalise(raw), error: null, disabled: false, lastUpdated: new Date() };
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    const notConfigured = msg.includes('503') || msg.toLowerCase().includes('not configured');
    return {
      // Keep the last good snapshot so a transient blip doesn't blank the tile
      // — the error rides on top of stale numbers.
      data: notConfigured ? null : prev.data,
      error: notConfigured ? null : msg,
      disabled: notConfigured,
      lastUpdated: prev.lastUpdated,
    };
  }
}

async function getJSON<T>(path: string): Promise<T | null> {
  const res = await globalThis.fetch(path);
  if (res.status === 503) {
    throw new Error('mills proxy: 503 (operator not configured)');
  }
  if (res.status === 404) {
    return null;
  }
  if (!res.ok) {
    // The operator explains its refusals ({"error": "... narrow the window
    // ..."}); surface that instead of a bare status so the tile tells the
    // operator what to do, not just that something 500'd.
    let detail = '';
    try {
      const body = (await res.json()) as { error?: unknown };
      if (typeof body.error === 'string') detail = body.error;
    } catch {
      // Non-JSON body — fall through to the status-only message.
    }
    throw new Error(detail ? `${detail} (HTTP ${res.status})` : `${path}: ${res.status}`);
  }
  const text = await res.text();
  if (!text) return null;
  return JSON.parse(text) as T;
}

export const millsStaffStore = new MillsStaffStore();
