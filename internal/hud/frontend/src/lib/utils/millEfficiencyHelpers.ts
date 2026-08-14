import type { PipelineRun } from '../stores/mills.svelte.ts';

const DAY_MS = 24 * 60 * 60 * 1_000;

function stateOf(run: PipelineRun): string {
  return (run.State ?? '').trim().toLowerCase();
}

function isBolt(run: PipelineRun): boolean {
  const state = stateOf(run);
  return state === 'done' || state === 'merged';
}

function isEscalated(run: PipelineRun): boolean {
  return stateOf(run) === 'escalated';
}

function runTime(run: PipelineRun): number | null {
  const value = run.EndedAt || run.StartedAt;
  if (!value) return null;
  const timestamp = new Date(value).getTime();
  return Number.isFinite(timestamp) ? timestamp : null;
}

function windowStart(now: Date): number {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() - 6);
  return start.getTime();
}

function windowEnd(now: Date): number {
  const end = new Date(now);
  end.setHours(0, 0, 0, 0);
  end.setDate(end.getDate() + 1);
  return end.getTime();
}

function runsInWindow(runs: PipelineRun[], now: Date): PipelineRun[] {
  const start = windowStart(now);
  const end = windowEnd(now);
  return runs.filter((run) => {
    const timestamp = runTime(run);
    return timestamp !== null && timestamp >= start && timestamp < end;
  });
}

function spendOf(run: PipelineRun): number {
  return typeof run.CostUSD === 'number' && Number.isFinite(run.CostUSD) && run.CostUSD >= 0
    ? run.CostUSD
    : 0;
}

export interface FirstPassYield {
  /** Oldest-to-newest daily ratios; idle days are safe zeroes for charting. */
  daily: number[];
  /** Today's ratio, absent when no bolt or escalation completed today. */
  today: number | undefined;
}

/** Seven daily done/(done+escalated) values and today's display ratio. */
export function firstPassYield(runs: PipelineRun[], now = new Date()): FirstPassYield {
  if (!Number.isFinite(now.getTime())) return { daily: Array(7).fill(0), today: undefined };
  const start = windowStart(now);
  const done = Array(7).fill(0) as number[];
  const escalated = Array(7).fill(0) as number[];

  for (const run of runsInWindow(runs, now)) {
    if (!isBolt(run) && !isEscalated(run)) continue;
    const timestamp = runTime(run);
    if (timestamp === null) continue;
    const date = new Date(timestamp);
    date.setHours(0, 0, 0, 0);
    const day = Math.round((date.getTime() - start) / DAY_MS);
    if (day < 0 || day > 6) continue;
    if (isBolt(run)) done[day]++;
    else escalated[day]++;
  }

  const daily = done.map((bolts, day) => {
    const terminal = bolts + escalated[day];
    return terminal > 0 ? bolts / terminal : 0;
  });
  const todayTotal = done[6] + escalated[6];
  return { daily, today: todayTotal > 0 ? done[6] / todayTotal : undefined };
}

export interface BoltCosts {
  /** All successful and escalated spend divided by successful bolts. */
  trueCostPerBolt: number | undefined;
  /** Successful-run spend divided by successful bolts. */
  rawCostPerBolt: number | undefined;
  bolts: number;
}

/** Compare the true seven-day bolt cost with the unburdened successful-run mean. */
export function boltCosts(runs: PipelineRun[], now = new Date()): BoltCosts {
  const terminal = runsInWindow(runs, now).filter((run) => isBolt(run) || isEscalated(run));
  const bolts = terminal.filter(isBolt);
  if (bolts.length === 0) {
    return { trueCostPerBolt: undefined, rawCostPerBolt: undefined, bolts: 0 };
  }
  const rawSpend = bolts.reduce((total, run) => total + spendOf(run), 0);
  const totalSpend = terminal.reduce((total, run) => total + spendOf(run), 0);
  return {
    trueCostPerBolt: totalSpend / bolts.length,
    rawCostPerBolt: rawSpend / bolts.length,
    bolts: bolts.length,
  };
}

/** Seven-day dollars consumed by escalated runs. */
export function escalatedWaste(runs: PipelineRun[], now = new Date()): number {
  return runsInWindow(runs, now)
    .filter(isEscalated)
    .reduce((total, run) => total + spendOf(run), 0);
}

/** Escalated cloth explicitly marked retryable within the seven-day window. */
export function mendingPile(runs: PipelineRun[], now = new Date()): number {
  return runsInWindow(runs, now)
    .filter((run) => isEscalated(run) && run.EscalationRetryable === true)
    .length;
}
