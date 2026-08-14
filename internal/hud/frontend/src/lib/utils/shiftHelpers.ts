// Pure helpers for the Factory panel's shift report — the end-of-shift
// overlay that turns the last 24 hours of terminal runs into a short,
// deterministic narrative ("the floor wove 6 bolts, struck 2 sparks,
// pattern go-rest-service stamped twice") plus a standup-pasteable
// markdown export. No summarizer: same runs in → same words out.
// Rune-free so vitest covers the runs→report mapping without a Svelte
// runtime, mirroring factoryHelpers/tartanHelpers.

import type { BacklogItem, BoltGrade, PipelineRun } from '../stores/mills.svelte.ts';
import type { PatternInfo } from '../stores/patterns.svelte.ts';
import { stampedPatternSlug } from './patternBooks.ts';

/** One terminal run inside the shift window. */
export interface ShiftRun {
  kind: 'bolt' | 'spark';
  runID: string;
  backlogID: string;
  template: string;
  attempts: number;
  /** Epoch ms the run left the beam (EndedAt, fallback StartedAt). */
  endedAt: number;
  costUSD?: number;
  grade?: BoltGrade;
  gradeNote?: string;
}

/**
 * Filter terminal runs to the shift window — the `hours` hours ending at
 * `now` — using the SAME weave rule as the loom and the tartan: done/merged
 * is a bolt, escalated is a spark, everything else (paused = held thread)
 * is not cloth. A run lands on the moment it ENDED (fallback: started);
 * runs without a usable timestamp are dropped. Oldest first.
 */
export function shiftWindow(runs: PipelineRun[], now: Date, hours = 24): ShiftRun[] {
  const end = now.getTime();
  const start = end - hours * 3_600_000;
  const out: ShiftRun[] = [];
  for (const run of runs) {
    if (!run?.ID) continue;
    const state = (run.State ?? '').toLowerCase();
    const kind = state === 'done' || state === 'merged' ? 'bolt'
      : state === 'escalated' ? 'spark'
      : null;
    if (!kind) continue;
    const raw = run.EndedAt ?? run.StartedAt;
    if (!raw) continue;
    const t = Date.parse(raw);
    if (!Number.isFinite(t) || t < start || t > end) continue;
    out.push({
      kind,
      runID: run.ID,
      backlogID: run.BacklogID ?? '',
      template: run.Template ?? '',
      attempts: run.Attempts ?? 1,
      endedAt: t,
      costUSD: run.CostUSD,
      grade: run.Grade,
      gradeNote: run.GradeNote,
    });
  }
  out.sort((a, b) => a.endedAt - b.endedAt);
  return out;
}

/** One pattern-book line: "go-rest-service stamped twice, both merged". */
export interface ShiftStamp {
  slug: string;
  name: string;
  bolts: number;
  sparks: number;
}

/** Everything the narrative and the markdown are generated from. */
export interface ShiftStats {
  hours: number;
  bolts: ShiftRun[];
  sparks: ShiftRun[];
  costUSD: number;
  /** Runs that needed more than one attempt, worst first. */
  retried: ShiftRun[];
  /** Local hour-of-day (0-23) with the most departures, with its count. */
  busiestHour: { hour: number; count: number } | null;
  /** Pattern-book attribution for the shift's runs, most-woven first. */
  stamps: ShiftStamp[];
}

/**
 * Aggregate a shift's runs. Pattern attribution reuses the shelf's chain
 * (run → backlog → PlanID → catalog slug) — derived, never guessed; runs
 * that don't trace to an approved pattern simply don't stamp a book.
 */
export function shiftStats(
  runs: ShiftRun[],
  patterns: PatternInfo[],
  backlog: BacklogItem[],
  hours = 24,
): ShiftStats {
  const bolts = runs.filter((r) => r.kind === 'bolt');
  const sparks = runs.filter((r) => r.kind === 'spark');
  let costUSD = 0;
  for (const r of runs) costUSD += r.costUSD ?? 0;

  const retried = runs.filter((r) => r.attempts > 1).sort((a, b) => b.attempts - a.attempts);

  const byHour = new Map<number, number>();
  for (const r of runs) {
    const h = new Date(r.endedAt).getHours();
    byHour.set(h, (byHour.get(h) ?? 0) + 1);
  }
  let busiestHour: ShiftStats['busiestHour'] = null;
  for (const [hour, count] of byHour) {
    if (!busiestHour || count > busiestHour.count) busiestHour = { hour, count };
  }

  const approved = patterns.filter((p) => p.status === 'approved' && p.slug);
  const slugs = approved.map((p) => p.slug);
  const nameBySlug = new Map(approved.map((p) => [p.slug, p.name]));
  const planByBacklog = new Map<string, string>();
  for (const item of backlog) {
    if (item?.ID && item.PlanID) planByBacklog.set(item.ID, item.PlanID);
  }
  const stampMap = new Map<string, ShiftStamp>();
  for (const r of runs) {
    const slug = stampedPatternSlug(planByBacklog.get(r.backlogID), slugs);
    if (!slug) continue;
    let s = stampMap.get(slug);
    if (!s) {
      s = { slug, name: nameBySlug.get(slug) ?? slug, bolts: 0, sparks: 0 };
      stampMap.set(slug, s);
    }
    if (r.kind === 'bolt') s.bolts++;
    else s.sparks++;
  }
  const stamps = [...stampMap.values()].sort(
    (a, b) => b.bolts + b.sparks - (a.bolts + a.sparks) || a.name.localeCompare(b.name),
  );

  return { hours, bolts, sparks, costUSD, retried, busiestHour, stamps };
}

function plural(n: number, one: string, many?: string): string {
  return `${n} ${n === 1 ? one : (many ?? one + 's')}`;
}

function hourRange(h: number): string {
  const f = (x: number) => `${String(x % 24).padStart(2, '0')}:00`;
  return `${f(h)}–${f(h + 1)}`;
}

/**
 * The shift's story as deterministic prose lines, headline first. Truth
 * over theater: a quiet shift says so plainly, and nothing here implies
 * more activity than the runs prove.
 */
export function shiftNarrative(stats: ShiftStats): string[] {
  const lines: string[] = [];
  const total = stats.bolts.length + stats.sparks.length;
  if (total === 0) {
    lines.push(`The loom sat quiet — no cloth came off the beam in the last ${stats.hours} hours.`);
    return lines;
  }
  const boltPart = plural(stats.bolts.length, 'bolt');
  const sparkPart = stats.sparks.length === 0
    ? 'no sparks'
    : plural(stats.sparks.length, 'spark');
  lines.push(`The floor wove ${boltPart} and struck ${sparkPart} over the last ${stats.hours} hours.`);

  for (const s of stats.stamps) {
    const times = s.bolts + s.sparks;
    const stampedPart = `Pattern ${s.name} stamped ${times === 1 ? 'once' : times === 2 ? 'twice' : `${times} times`}`;
    const outcome = s.sparks === 0
      ? (times === 1 ? 'merged on green' : 'all merged on green')
      : s.bolts === 0
        ? (times === 1 ? 'escalated' : 'all escalated')
        : `${plural(s.bolts, 'merge')}, ${plural(s.sparks, 'escalation')}`;
    lines.push(`${stampedPart} — ${outcome}.`);
  }

  if (stats.retried.length > 0) {
    const worst = stats.retried[0];
    lines.push(
      `${plural(stats.retried.length, 'run')} needed extra passes (worst: ${worst.backlogID || worst.runID} at ${worst.attempts} attempts).`,
    );
  }

  // A "busiest hour" of one departure is noise, not a peak.
  if (stats.busiestHour && stats.busiestHour.count > 1) {
    lines.push(
      `Busiest hour ${hourRange(stats.busiestHour.hour)} — ${plural(stats.busiestHour.count, 'departure')}.`,
    );
  }

  if (stats.costUSD > 0) {
    lines.push(`The shift burned $${stats.costUSD.toFixed(2)} of pipeline fuel.`);
  }
  return lines;
}

/** Failing-gate summary for one spark, resolved by the overlay. */
export interface SparkGateSummary {
  runID: string;
  /** Names of gates that failed, in evaluation order; [] = none found. */
  failedGates: string[];
}

/**
 * Standup-pasteable markdown. Narrative first, then per-spark detail
 * (with failing gates when the caller resolved them), then the raw
 * departures table. Deterministic given the same inputs.
 */
export function shiftMarkdown(
  stats: ShiftStats,
  narrative: string[],
  generatedAt: Date,
  gateSummaries: SparkGateSummary[] = [],
): string {
  const gatesByRun = new Map(gateSummaries.map((g) => [g.runID, g.failedGates]));
  const out: string[] = [];
  out.push(`# Mills shift report — ${generatedAt.toISOString().slice(0, 16).replace('T', ' ')} UTC`);
  out.push('');
  for (const line of narrative) out.push(line);
  if (stats.sparks.length > 0) {
    out.push('');
    out.push('## Sparks');
    for (const s of stats.sparks) {
      const gates = gatesByRun.get(s.runID);
      const why = gates && gates.length > 0 ? ` — failed ${gates.join(', ')}` : '';
      out.push(`- \`${s.backlogID || s.runID}\` (${s.template || 'pipeline'}, ${plural(s.attempts, 'attempt')})${why}`);
    }
  }
  const total = stats.bolts.length + stats.sparks.length;
  if (total > 0) {
    out.push('');
    out.push('## Departures');
    out.push('| when (UTC) | outcome | item | template | attempts |');
    out.push('|---|---|---|---|---|');
    const all = [...stats.bolts, ...stats.sparks].sort((a, b) => a.endedAt - b.endedAt);
    for (const r of all) {
      const hhmm = new Date(r.endedAt).toISOString().slice(11, 16);
      out.push(
        `| ${hhmm} | ${r.kind === 'bolt' ? '🟢 bolt' : '🟡 spark'} | ${r.backlogID || r.runID} | ${r.template || '—'} | ${r.attempts} |`,
      );
    }
  }
  out.push('');
  return out.join('\n');
}
