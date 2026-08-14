// telemetryHelpers — rune-free types + pure number-crunching for the Mill
// Telemetry panel (plan .loom/plan-mills-telemetry-optimization-2026-07-16 §S6).
//
// Kept out of the .svelte component so vitest can exercise the waterfall /
// gate-health / Pareto math against a captured production response
// (telemetryHelpers.fixture.json — test-only, imported by nothing else)
// without a Svelte runtime — the same separation computeSystemHealth uses.

// ---- Wire contract (GET /api/mills/telemetry/stages?window=) --------------
// Field names are snake_case because the S5 handler tags them explicitly
// (see telemetryHelpers.fixture.json). This is the exact shape the panel
// renders and the handler must honour.

export type TelemetryWindow = '1d' | '7d' | '30d';

export interface TelemetryRuns {
  total: number;
  done: number;
  escalated: number;
  retry_burn_cost_usd: number;
  retry_burn_seconds: number;
}

export interface StageTelemetry {
  stage: string;
  attempts: number;
  errors: number;
  error_rate: number;
  p50_seconds: number;
  p90_seconds: number;
  max_seconds: number;
  total_seconds: number;
  cost_usd: number;
  retry_attempts: number;
  retry_cost_usd: number;
}

export interface GateTelemetry {
  gate: string;
  evaluations: number;
  passes: number;
  fails: number;
  skips: number;
  // unparseable is the count of gate_outcomes judged "flexinfer:unparseable"
  // — a judge-harness defect, NOT a quality fail. It is a SUBSET of `fails`
  // (the soft-fail path records Outcome=fail, JudgedBy=…unparseable), so it
  // must never be added on top of fails when stacking a bar.
  unparseable: number;
}

export interface EscalationFunnelEntry {
  last_stage: string;
  outcome: string;
  count: number;
}

export interface FailureClassEntry {
  stage: string;
  class: string;
  count: number;
}

// ModelEconomicsEntry attributes stage cost + reliability to one (model,
// backend) tier over the window. Unattributed rows (historical, or a worker
// that doesn't surface identity) arrive bucketed as model="unknown"/
// backend="unknown" so the tier totals stay complete.
export interface ModelEconomicsEntry {
  model: string;
  backend: string;
  calls: number;
  cost_usd: number;
  errors: number;
  error_rate: number;
  avg_seconds: number;
}

export interface StageTelemetryReport {
  window_seconds: number;
  generated_at: string;
  runs: TelemetryRuns;
  stages: StageTelemetry[];
  gates: GateTelemetry[];
  escalation_funnel: EscalationFunnelEntry[];
  failure_classes: FailureClassEntry[];
  model_economics: ModelEconomicsEntry[];
}

// ---- Window param mapping -------------------------------------------------

export const TELEMETRY_WINDOWS: readonly TelemetryWindow[] = ['1d', '7d', '30d'];

const WINDOW_SECONDS: Record<TelemetryWindow, number> = {
  '1d': 86_400,
  '7d': 604_800,
  '30d': 2_592_000,
};

/** Seconds a window param maps to — matches the report's window_seconds. */
export function windowSeconds(w: TelemetryWindow): number {
  return WINDOW_SECONDS[w] ?? WINDOW_SECONDS['7d'];
}

/** Short human label for a window param (24h / 7d / 30d). */
export function windowLabel(w: TelemetryWindow): string {
  return w === '1d' ? '24h' : w;
}

/** Type-guard: is `v` one of the three accepted window params? */
export function isTelemetryWindow(v: unknown): v is TelemetryWindow {
  return v === '1d' || v === '7d' || v === '30d';
}

// ---- Formatting -----------------------------------------------------------

export function fmtDurationSeconds(seconds: number | undefined | null): string {
  if (seconds == null || !Number.isFinite(seconds)) return '—';
  if (seconds <= 0) return '0s';
  if (seconds < 1) return '<1s';
  if (seconds < 60) return `${Math.round(seconds)}s`;
  const m = seconds / 60;
  if (m < 60) return `${m.toFixed(1)}m`;
  const h = m / 60;
  return `${h.toFixed(1)}h`;
}

/** Whole-minute rendering for the retry-burn tile ("489m"). */
export function fmtMinutes(seconds: number | undefined | null): string {
  if (seconds == null || !Number.isFinite(seconds)) return '—';
  return `${Math.round(seconds / 60)}m`;
}

export function fmtUSD(v: number | undefined | null): string {
  if (v == null || !Number.isFinite(v)) return '—';
  if (v === 0) return '$0';
  if (v < 0.01) return '<$0.01';
  if (v >= 100) return `$${v.toFixed(0)}`;
  if (v >= 10) return `$${v.toFixed(1)}`;
  return `$${v.toFixed(2)}`;
}

export function fmtPct(v: number | undefined | null, digits = 0): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${(v * 100).toFixed(digits)}%`;
}

export function fmtCount(v: number | undefined | null): string {
  if (v == null || !Number.isFinite(v)) return '—';
  return `${Math.round(v)}`;
}

// ---- Derived models -------------------------------------------------------

// A stage's error rate above this reads red on the waterfall — the plan's
// ">25%" high-water mark for a stage that's failing more than it succeeds.
export const HIGH_ERROR_RATE = 0.25;

function clamp01(n: number): number {
  if (!Number.isFinite(n)) return 0;
  return n < 0 ? 0 : n > 1 ? 1 : n;
}

export interface StageBar {
  stage: string;
  p50_seconds: number;
  p90_seconds: number;
  max_seconds: number;
  error_rate: number;
  attempts: number;
  errors: number;
  cost_usd: number;
  retry_cost_usd: number;
  /** p50 bar width [0,1] against the widest stage's p90. */
  p50Frac: number;
  /** p90 overlay width [0,1] against the widest stage's p90. */
  p90Frac: number;
  /** true when error_rate exceeds HIGH_ERROR_RATE (render red). */
  highError: boolean;
}

// stageWaterfall sorts stages by p50 descending (slowest first) and scales
// every bar against the single widest p90 so p50 and its p90 overlay share
// one axis across the whole chart.
export function stageWaterfall(stages: StageTelemetry[]): StageBar[] {
  const maxP90 = stages.reduce(
    (m, s) => Math.max(m, s.p90_seconds, s.p50_seconds),
    0,
  );
  const scale = maxP90 > 0 ? maxP90 : 1;
  return [...stages]
    .sort((a, b) => b.p50_seconds - a.p50_seconds)
    .map((s) => ({
      stage: s.stage,
      p50_seconds: s.p50_seconds,
      p90_seconds: s.p90_seconds,
      max_seconds: s.max_seconds,
      error_rate: s.error_rate,
      attempts: s.attempts,
      errors: s.errors,
      cost_usd: s.cost_usd,
      retry_cost_usd: s.retry_cost_usd,
      p50Frac: clamp01(s.p50_seconds / scale),
      p90Frac: clamp01(s.p90_seconds / scale),
      highError: s.error_rate > HIGH_ERROR_RATE,
    }));
}

export interface GateSegment {
  gate: string;
  evaluations: number;
  passes: number;
  fails: number; // genuine fails only (fails - unparseable), for the bar
  skips: number;
  unparseable: number;
  passFrac: number;
  /** genuine fails, excluding unparseable, as a fraction of evaluations. */
  failFrac: number;
  skipFrac: number;
  /** unparseable (judge-harness defect) as its own distinct segment. */
  unparseableFrac: number;
}

// gateHealth splits each gate's evaluations into a stacked pass / skip /
// fail / unparseable bar. Because unparseable ⊆ fails on the wire, the
// genuine-fail segment subtracts it out so a bar reads pass+skip+fail+
// unparseable = evaluations and unparseable never double-counts.
export function gateHealth(gates: GateTelemetry[]): GateSegment[] {
  return gates.map((g) => {
    const denom = g.evaluations > 0 ? g.evaluations : 1;
    const realFails = Math.max(0, g.fails - g.unparseable);
    return {
      gate: g.gate,
      evaluations: g.evaluations,
      passes: g.passes,
      fails: realFails,
      skips: g.skips,
      unparseable: g.unparseable,
      passFrac: clamp01(g.passes / denom),
      failFrac: clamp01(realFails / denom),
      skipFrac: clamp01(g.skips / denom),
      unparseableFrac: clamp01(g.unparseable / denom),
    };
  });
}

export interface FunnelBar extends EscalationFunnelEntry {
  /** count relative to the largest funnel entry [0,1]. */
  frac: number;
}

// escalationFunnel sorts last-stage:outcome buckets by count descending and
// scales each against the largest so the panel can draw comparable bars.
export function escalationFunnel(entries: EscalationFunnelEntry[]): FunnelBar[] {
  const max = entries.reduce((m, e) => Math.max(m, e.count), 0) || 1;
  return [...entries]
    .sort((a, b) => b.count - a.count)
    .map((e) => ({ ...e, frac: clamp01(e.count / max) }));
}

export interface ParetoRow extends FailureClassEntry {
  /** this class's share of all failures [0,1]. */
  share: number;
  /** running cumulative share through this row [0,1]. */
  cumulative: number;
  /** count relative to the largest class [0,1], for the share bar. */
  frac: number;
}

// failurePareto sorts failure classes by count descending and annotates each
// with its share, running cumulative share, and a bar fraction vs the top
// class — the classic Pareto "vital few" read.
export function failurePareto(classes: FailureClassEntry[]): ParetoRow[] {
  const total = classes.reduce((s, c) => s + c.count, 0);
  const sorted = [...classes].sort((a, b) => b.count - a.count);
  const max = sorted.length > 0 ? sorted[0].count : 0;
  let cum = 0;
  return sorted.map((c) => {
    const share = total > 0 ? c.count / total : 0;
    cum += share;
    return {
      ...c,
      share,
      cumulative: clamp01(cum),
      frac: max > 0 ? clamp01(c.count / max) : 0,
    };
  });
}

export interface ModelEconomicsRow extends ModelEconomicsEntry {
  /** this tier's cost as a fraction of the top tier's cost [0,1], for the bar. */
  costFrac: number;
  /** true when error_rate exceeds HIGH_ERROR_RATE (render red). */
  highError: boolean;
}

// modelEconomics sorts the (model, backend) tiers by cost descending — the money
// view the panel headlines — and scales each cost bar against the costliest
// tier. The DAO already returns cost-descending, but sorting here keeps the
// helper self-contained and robust to sample-fixture edits. Rows are copied so
// the input array is never mutated.
export function modelEconomics(entries: ModelEconomicsEntry[]): ModelEconomicsRow[] {
  const sorted = [...entries].sort((a, b) => b.cost_usd - a.cost_usd);
  const maxCost = sorted.reduce((m, e) => Math.max(m, e.cost_usd), 0);
  const scale = maxCost > 0 ? maxCost : 1;
  return sorted.map((e) => ({
    ...e,
    costFrac: clamp01(e.cost_usd / scale),
    highError: e.error_rate > HIGH_ERROR_RATE,
  }));
}

/** escalated / total over the window, guarded against a zero-run window. */
export function escalationRate(runs: TelemetryRuns): number {
  return runs.total > 0 ? runs.escalated / runs.total : 0;
}

// aggregateGatePassRate is the window-wide gate pass rate across every gate:
// total passes / total evaluations. The panel uses it as the sample-mode
// fallback when the windowed KPI snapshot (which carries the canonical
// gate_pass_rate) isn't available yet.
export function aggregateGatePassRate(gates: GateTelemetry[]): number {
  let passes = 0;
  let evals = 0;
  for (const g of gates) {
    passes += g.passes;
    evals += g.evaluations;
  }
  return evals > 0 ? passes / evals : 0;
}
