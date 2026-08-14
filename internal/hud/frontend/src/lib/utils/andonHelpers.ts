// Pure helpers for the Factory panel's andon mode — the fullscreen
// office-TV board. An andon board exists to be glanced at from 3–10 m,
// so the state model is deliberately tiny and the priority order is
// safety-first: a dead feed outranks everything (the board may never
// glow green on stale data). Rune-free so vitest can exercise the
// state machine without a Svelte runtime, mirroring factoryHelpers.

/** Board states, most urgent first. */
export type AndonState = 'stale' | 'paused' | 'storm' | 'weaving' | 'idle';

export interface AndonInput {
  /** Feed is stale or the last fetch errored. */
  stale: boolean;
  /** Mills kill-switch is off (status.Enabled === false). */
  paused: boolean;
  /** Active pipeline runs right now. */
  activeRuns: number;
  /** Escalated runs in the KPI window (24 h). */
  escalated24h: number;
  /** Merged runs in the KPI window (24 h). */
  merged24h: number;
}

export interface AndonReading {
  state: AndonState;
  /** Big lamp word, uppercase-ready. */
  word: string;
  /** One-line caption under the lamp. */
  caption: string;
}

/**
 * Escalation storm rule: at least STORM_MIN sparks in the window AND
 * sparks running at half the bolt rate or worse. Three sparks against
 * twenty merges is a healthy floor; three against two is a storm.
 */
const STORM_MIN = 3;
export function isStorm(escalated24h: number, merged24h: number): boolean {
  return escalated24h >= STORM_MIN && escalated24h * 2 >= merged24h;
}

/** Fold live Mills state into the single andon reading. */
export function andonState(input: AndonInput): AndonReading {
  if (input.stale) {
    return {
      state: 'stale',
      word: 'feed stale',
      caption: 'displayed data is frozen — the board refuses to guess',
    };
  }
  if (input.paused) {
    return {
      state: 'paused',
      word: 'paused',
      caption: 'mills kill-switch engaged — jacquard halted by hand',
    };
  }
  if (isStorm(input.escalated24h, input.merged24h)) {
    return {
      state: 'storm',
      word: 'escalation storm',
      caption: `${input.escalated24h} sparks in 24h — human eyes needed on the floor`,
    };
  }
  if (input.activeRuns > 0) {
    return {
      state: 'weaving',
      word: 'weaving',
      caption: `${input.activeRuns} shuttle${input.activeRuns === 1 ? '' : 's'} in flight — floor running lights-out`,
    };
  }
  return {
    state: 'idle',
    word: 'idle',
    caption: 'warp strung, shuttles racked — awaiting dispatch',
  };
}

/**
 * Digits for the mechanical odometer, left-padded to minDigits.
 * Non-finite or negative input reads 0 — the counter never lies with
 * a minus sign or NaN wheel.
 */
export function odometerDigits(value: number | undefined, minDigits = 3): number[] {
  const v = Number.isFinite(value) && (value as number) > 0 ? Math.floor(value as number) : 0;
  const s = String(v).padStart(minDigits, '0');
  return Array.from(s, (c) => Number(c));
}

/**
 * Human freshness line for the footer. The exact age is the honesty
 * affordance — a TV viewer can see at a glance whether "weaving" means
 * now or three minutes ago.
 *
 * Deliberately NOT relativeTime() from utils/format.ts, though it computes
 * the same quantity. Three things here are load-bearing for the andon board
 * and absent from the shared formatter:
 *
 *   - Compound resolution ('3m 20s ago', '2h 5m ago'). relativeTime()
 *     truncates to one unit by design, so a wallboard read from across the
 *     room could not distinguish a 3-minute stall from a 3m59s one. The
 *     andon exists to make that difference visible. delayAge() in
 *     departureHelpers.ts mirrors this h/m shape for the same reason.
 *   - An injected `now`, which keeps the label deterministic under test
 *     (see andonHelpers.test.ts) instead of reading the wall clock.
 *   - Andon vocabulary: 'no data yet' for a missing timestamp rather than
 *     '---', and 'updated just now' absorbing both the sub-2s window and
 *     negative skew, so a wallboard never displays a future age.
 *
 * Keep the two in step on semantics, not on wording: both must read forward
 * time as "now or later", never as a lapsed age.
 */
export function freshnessLabel(lastUpdated: Date | null | undefined, now: Date): string {
  if (!lastUpdated) return 'no data yet';
  const ms = now.getTime() - lastUpdated.getTime();
  if (!Number.isFinite(ms) || ms < 0) return 'updated just now';
  const s = Math.floor(ms / 1000);
  if (s < 2) return 'updated just now';
  if (s < 90) return `updated ${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 90) return `updated ${m}m ${s % 60}s ago`;
  const h = Math.floor(m / 60);
  return `updated ${h}h ${m % 60}m ago`;
}
