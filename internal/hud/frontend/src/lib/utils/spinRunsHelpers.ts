// Pure semantics for the Spinning Room status surface — the desktop port of the
// mobile Kit's MillsSpinBoard (apps/loom-companion-ios .../Mills/MillsSpinModels.swift).
//
// The async-spin backend is fully durable: POST /api/mills/spin/async returns
// 202 + a spin_id, the operator spins in a detached goroutine, and every run's
// status lives in the SQLite `spin_runs` table (migration 007), read back via
// GET /api/mills/spin/runs[/{id}] (proxied through the HUD). This module turns
// that raw run list into the view-model the tray + board render: derived phase
// (incl. slow/stuck detection the wire status doesn't carry), elapsed time,
// what's worth showing, and how competitive siblings group.
//
// Everything here is a pure function of (runs, now) so it's unit-tested and the
// Svelte components stay presentation-only — same split as plansHelpers.ts.

/** One async Spinning-Room spin. Mirrors pkg/mills/store.SpinRun JSON tags. */
export interface SpinRun {
  id: string;
  brief: string;
  frames: string[];
  priority?: string;
  project?: string;
  namespace?: string;
  /** Wire status: pending | running | succeeded | failed | timeout. */
  status: string;
  /** 0..N draft plan ids the spin authored (>1 for a competitive spin). */
  plan_ids: string[];
  /** Failure reason on failed/timeout, or a partial-failure summary otherwise. */
  error?: string;
  competitive: boolean;
  started_at: string;
  ended_at?: string;
}

export type BadgeVariant = 'info' | 'success' | 'warning' | 'error' | 'accent';

/**
 * Display phase for a spin. Extends the four wire statuses with two derived,
 * time-based states the server doesn't report:
 *   - `slow`  — still pending/running past SPIN_SLOW_AFTER_MS (a frontier frame
 *               legitimately takes a few minutes; this just flags "taking a while").
 *   - `stuck` — still pending/running past SPIN_STUCK_AFTER_MS, i.e. approaching
 *               the operator's hard 10-minute budget (spinAsyncBudget). At this
 *               point the run is very likely wedged and about to time out.
 */
export type SpinPhase =
  | 'pending'
  | 'running'
  | 'slow'
  | 'stuck'
  | 'succeeded'
  | 'failed'
  | 'timeout'
  | 'unknown';

// A frontier frame (claude-opus-4-8, adaptive thinking) runs minutes; the fast
// flexinfer frames land in ~55s. 2min = "slower than a fast frame, watch it".
export const SPIN_SLOW_AFTER_MS = 2 * 60 * 1000;
// The operator caps one spin at spinAsyncBudget = 10min. Past 8min a live spin
// is near that ceiling and almost certainly wedged — surface it loudly.
export const SPIN_STUCK_AFTER_MS = 8 * 60 * 1000;

/** Terminal = the spin finished (well or badly); non-terminal keeps polling. */
export function isTerminalStatus(status: string): boolean {
  return status === 'succeeded' || status === 'failed' || status === 'timeout';
}

/** A run still doing work (or queued to) — the poll set. */
export function isLive(run: SpinRun): boolean {
  return !isTerminalStatus(run.status);
}

/** Wall-clock a run has been (or was) running, in ms. */
export function elapsedMs(run: SpinRun, now: number): number {
  const start = Date.parse(run.started_at);
  if (Number.isNaN(start)) return 0;
  const end = run.ended_at ? Date.parse(run.ended_at) : now;
  const ref = Number.isNaN(end) ? now : end;
  return Math.max(0, ref - start);
}

/**
 * Derived phase for a run at time `now`. Terminal rows map straight through;
 * live rows escalate pending/running → slow → stuck as they age past the
 * thresholds, so the UI can warn before the operator's timeout does.
 */
export function spinPhase(run: SpinRun, now: number): SpinPhase {
  const s = run.status;
  if (s === 'succeeded' || s === 'failed' || s === 'timeout') return s;
  if (s !== 'pending' && s !== 'running') return 'unknown';
  const age = elapsedMs(run, now);
  if (age >= SPIN_STUCK_AFTER_MS) return 'stuck';
  if (age >= SPIN_SLOW_AFTER_MS) return 'slow';
  return s;
}

/** True while a run is live AND has aged past the stuck threshold. */
export function isStuck(run: SpinRun, now: number): boolean {
  return isLive(run) && spinPhase(run, now) === 'stuck';
}

/** Badge colour for a phase. */
export function spinPhaseVariant(phase: SpinPhase): BadgeVariant {
  switch (phase) {
    case 'succeeded':
      return 'success';
    case 'failed':
      return 'error';
    case 'stuck':
      return 'error';
    case 'slow':
    case 'timeout':
      return 'warning';
    case 'pending':
    case 'running':
      return 'info';
    default:
      return 'info';
  }
}

/** Short human label for a phase (badge text). */
export function spinPhaseLabel(phase: SpinPhase): string {
  switch (phase) {
    case 'slow':
      return 'slow';
    case 'stuck':
      return 'stuck';
    default:
      return phase;
  }
}

/** First non-empty line of the brief, for dense rows. */
export function briefHeadline(brief: string): string {
  for (const line of brief.split('\n')) {
    const t = line.trim();
    if (t) return t;
  }
  return brief.trim();
}

/** Compact elapsed like "45s", "2m 05s", "1h 03m". */
export function formatElapsed(ms: number): string {
  const totalSec = Math.floor(ms / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const min = Math.floor(totalSec / 60);
  const sec = totalSec % 60;
  if (min < 60) return `${min}m ${String(sec).padStart(2, '0')}s`;
  const hr = Math.floor(min / 60);
  const rem = min % 60;
  return `${hr}h ${String(rem).padStart(2, '0')}m`;
}

export interface VisibleRunsOptions {
  now?: number;
  /** Keep terminal runs that ended within this window (default 24h). */
  terminalWindowMs?: number;
  /** Cap the list (default 8). */
  limit?: number;
}

/**
 * Runs worth showing on the tray: everything still live, plus terminal runs
 * that ended within `terminalWindowMs` — enough to confirm "my spin landed"
 * without unbounded history. Live spins sort first, then most-recent-started.
 */
export function visibleRuns(runs: SpinRun[], opts: VisibleRunsOptions = {}): SpinRun[] {
  const now = opts.now ?? Date.now();
  const terminalWindowMs = opts.terminalWindowMs ?? 24 * 3600 * 1000;
  const limit = opts.limit ?? 8;
  const visible = runs.filter((run) => {
    if (isLive(run)) return true;
    const endedRaw = run.ended_at || run.started_at;
    const ended = Date.parse(endedRaw);
    if (Number.isNaN(ended)) return false;
    return now - ended <= terminalWindowMs;
  });
  const sorted = [...visible].sort((a, b) => {
    const aLive = isLive(a);
    const bLive = isLive(b);
    if (aLive !== bLive) return aLive ? -1 : 1;
    return startedTs(b) - startedTs(a);
  });
  return sorted.slice(0, limit);
}

function startedTs(run: SpinRun): number {
  const t = Date.parse(run.started_at);
  return Number.isNaN(t) ? 0 : t;
}

/** Whether any run still needs polling. */
export function hasLiveSpin(runs: SpinRun[]): boolean {
  return runs.some(isLive);
}

/** Count of live (pending/running) runs. */
export function liveCount(runs: SpinRun[]): number {
  return runs.reduce((n, r) => (isLive(r) ? n + 1 : n), 0);
}

/** Count of live runs that have aged into the `stuck` state. */
export function stuckCount(runs: SpinRun[], now: number): number {
  return runs.reduce((n, r) => (isStuck(r, now) ? n + 1 : n), 0);
}

/** One draft plan's membership in a competitive spin (2+ sibling drafts). */
export interface CompetitiveGroup {
  spinId: string;
  frames: string[];
  /** All draft plan ids the spin authored (this plan + its siblings). */
  planIds: string[];
}

/**
 * Map each draft plan id → the competitive spin it belongs to, so the board can
 * badge sibling drafts ("⟳ competing (2)") instead of showing two disconnected
 * cards. Only runs that authored 2+ drafts qualify — a lone draft has no
 * siblings to group. Newest run wins if an id somehow appears twice.
 */
export function competitiveGroups(runs: SpinRun[]): Map<string, CompetitiveGroup> {
  const out = new Map<string, CompetitiveGroup>();
  const ordered = [...runs].sort((a, b) => startedTs(a) - startedTs(b)); // old→new, newest overwrites
  for (const run of ordered) {
    const ids = run.plan_ids ?? [];
    if (ids.length < 2) continue;
    const group: CompetitiveGroup = { spinId: run.id, frames: run.frames ?? [], planIds: ids };
    for (const id of ids) out.set(id, group);
  }
  return out;
}
