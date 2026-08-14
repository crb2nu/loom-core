// Pure helpers for the Mills Factory panel (the "dark factory" loom).
// Rune-free so vitest can exercise the data→weave mapping without a
// Svelte runtime, mirroring the plansHelpers/spinRunsHelpers pattern.

import type { PipelineRun } from '../stores/mills.svelte.ts';

/** A row the loom should weave in response to a real pipeline event. */
export interface WeaveEvent {
  kind: 'bolt' | 'spark';
  runID: string;
  backlogID: string;
}

/**
 * Diff the terminal-run history against the set of run IDs the loom has
 * already woven. Returns weave events for unseen terminal runs, oldest
 * first (so the cloth accumulates in chronological order), and the
 * updated seen-set. Runs in states other than done/merged/escalated
 * (e.g. paused) are marked seen but produce no row — a paused run is a
 * held thread, not cloth.
 */
export function diffTerminalRuns(
  seen: ReadonlySet<string>,
  history: PipelineRun[],
): { events: WeaveEvent[]; seen: Set<string> } {
  const nextSeen = new Set(seen);
  const events: WeaveEvent[] = [];
  // History arrives newest-first; walk backwards for chronological weave.
  for (let i = history.length - 1; i >= 0; i--) {
    const run = history[i];
    if (!run?.ID || nextSeen.has(run.ID)) continue;
    nextSeen.add(run.ID);
    const state = (run.State ?? '').toLowerCase();
    if (state === 'done' || state === 'merged') {
      events.push({ kind: 'bolt', runID: run.ID, backlogID: run.BacklogID ?? '' });
    } else if (state === 'escalated') {
      events.push({ kind: 'spark', runID: run.ID, backlogID: run.BacklogID ?? '' });
    }
  }
  return { events, seen: nextSeen };
}

/** A shuttle pick — one active run advancing into a stage. */
export interface StagePick {
  runID: string;
  backlogID: string;
  stage: string;
}

/**
 * Diff active runs' CurrentStage against the previously observed map.
 * A run seen for the first time, or observed in a new stage, produces one
 * pick — the real event that earns a shuttle pass. Returns the next
 * observation map; runs that vanished are dropped (their terminal weaving
 * is handled by diffTerminalRuns on the history feed).
 */
export function diffStagePicks(
  prev: ReadonlyMap<string, string>,
  active: PipelineRun[],
): { picks: StagePick[]; stages: Map<string, string> } {
  const stages = new Map<string, string>();
  const picks: StagePick[] = [];
  for (const run of active) {
    if (!run?.ID) continue;
    const stage = run.CurrentStage ?? '';
    stages.set(run.ID, stage);
    if (prev.get(run.ID) !== stage) {
      picks.push({ runID: run.ID, backlogID: run.BacklogID ?? '', stage });
    }
  }
  return { picks, stages };
}

/** xmur3-style 32-bit string hash — seed source for deterministic weaving. */
function hash32(s: string): number {
  let h = 1779033703 ^ s.length;
  for (let i = 0; i < s.length; i++) {
    h = Math.imul(h ^ s.charCodeAt(i), 3432918353);
    h = (h << 13) | (h >>> 19);
  }
  h = Math.imul(h ^ (h >>> 16), 2246822507);
  h = Math.imul(h ^ (h >>> 13), 3266489909);
  return (h ^ (h >>> 16)) >>> 0;
}

/** mulberry32 PRNG over a 32-bit seed. */
function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/**
 * Deterministic run-length thread pattern for one woven row. The same seed
 * always weaves the same cloth, so a run's row is stable across reloads and
 * resizes re-derive instead of mutating — the fabric is a reproducible
 * artifact of the run history, not RNG noise.
 */
export function seededPattern(seed: string, warpN: number): boolean[] {
  const rnd = mulberry32(hash32(seed));
  const cells = new Array<boolean>(warpN);
  let bit = rnd() > 0.5;
  let runLen = 0;
  for (let i = 0; i < warpN; i++) {
    if (runLen <= 0) {
      bit = !bit;
      runLen = 1 + Math.floor(rnd() * 4);
    }
    cells[i] = bit;
    runLen--;
  }
  return cells;
}

/**
 * Seed for the jacquard punch-card tape: a pure function of the live
 * policy, so the tape IS the program. A policy version bump (or a
 * kill-switch flip) re-punches the whole chain.
 */
export function policyTapeSeed(
  policy: { version?: number; enabled?: boolean } | null | undefined,
): number {
  if (!policy) return 0;
  return hash32(`policy-v${policy.version ?? 0}-${policy.enabled ? 'on' : 'off'}`);
}

/** Deterministic punched/blank state for one tape hole (~40% punched). */
export function tapeHole(policySeed: number, row: number, col: number): boolean {
  return ((Math.imul(row + 1, 2654435761) ^ Math.imul(col + 1, 40503) ^ policySeed) >>> 13) % 5 < 2;
}

/** Human stage names for the shuttle's current position. */
const STAGE_LABEL: Record<string, string> = {
  plan_slice: 'threading the warp',
  research: 'reading the pattern',
  implement: 'laying weft',
  tests: 'counting picks',
  pr_self_review: 'checking the selvage',
  mr: 'presenting the bolt',
  ci_watch: 'under the inspection lamp',
  merge: 'winding the take-up roll',
  cleanup: 'sweeping the floor',
};

export function stageLabel(stage: string | undefined): string {
  if (!stage) return 'in the shed';
  return STAGE_LABEL[stage] ?? stage.replaceAll('_', ' ');
}

/** Fuel-gauge reading for the rolling-24h pipeline budget. */
export interface FuelReading {
  /** Fraction of the tank remaining, or null when unknowable. */
  frac: number | null;
  /** `$spent / $cap` (or spent-only when uncapped, em dash when absent). */
  label: string;
  tone: 'ok' | 'wr' | 'er' | 'cy';
}

function fmtUSD(v: number): string {
  return v >= 100 ? `$${Math.round(v)}` : `$${v.toFixed(2)}`;
}

/**
 * Fold the operator's budget window into a fuel reading. No data → an
 * em dash (never a guessed level); an uncapped tier shows spend but no
 * level; a capped tier maps remaining fraction to the usual tones.
 */
export function fuelReading(
  usage: { spent_usd?: number; cap_usd?: number } | null | undefined,
): FuelReading {
  const spent = usage?.spent_usd;
  if (usage == null || typeof spent !== 'number' || !Number.isFinite(spent)) {
    return { frac: null, label: '—', tone: 'cy' };
  }
  const cap = usage.cap_usd ?? 0;
  if (!(cap > 0)) {
    return { frac: null, label: `${fmtUSD(spent)} · no cap`, tone: 'cy' };
  }
  const frac = Math.max(0, Math.min(1, 1 - spent / cap));
  return {
    frac,
    label: `${fmtUSD(spent)} / ${fmtUSD(cap)}`,
    tone: frac > 0.5 ? 'ok' : frac > 0.25 ? 'wr' : 'er',
  };
}

/**
 * Warp-thread count for the canvas: scales with how much work is strung
 * on the beam (queued + running backlog), clamped to what reads well and
 * to what the viewport can fit.
 */
export function warpCountFor(
  backlogActive: number,
  viewportMax: number,
  bounds: { min?: number; max?: number } = {},
): number {
  const min = bounds.min ?? 24;
  const max = Math.min(bounds.max ?? 72, Math.max(min, viewportMax));
  if (!Number.isFinite(backlogActive) || backlogActive <= 0) return min;
  // 1 backlog item ≈ 2 threads; saturates at max.
  return Math.max(min, Math.min(max, min + Math.round(backlogActive * 2)));
}
