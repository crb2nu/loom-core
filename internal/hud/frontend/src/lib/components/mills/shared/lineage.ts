// Pure lineage helpers for the mill-floor views (Warps · Shuttles · Sparks ·
// Bolts). Rune-free so vitest can exercise the run→lineage mapping without a
// Svelte runtime, mirroring the factoryHelpers / shiftHelpers pattern.
//
// Both segment builders live here so LineageRibbon.svelte stays a pure
// renderer with zero data logic (spec §2.2: "no fetch inside the component").
// `lineageFor` threads one run's warp→stages→terminal strand; `spineSegments`
// builds the floor-nav ribbon shared at the top of every mill-floor view.

import type { BacklogItem, PipelineRun } from '../../../stores/mills.svelte.ts';
import type { BadgeVariant } from '../../../utils/tokens.ts';
import { stageLabel } from '../../../utils/factoryHelpers.ts';

// The four canonical warp buckets plus the catch-all. Priority values that
// aren't P0..P3 land in `other` so nothing silently vanishes off the beam.
export const WARP_PRIORITIES = ['P0', 'P1', 'P2', 'P3'] as const;
export type WarpPriority = (typeof WARP_PRIORITIES)[number];
export type WarpBucket = WarpPriority | 'other';

// Canonical pipeline stage order — the shuttle's left→right path across the
// shed. Mirrors STAGE_LABEL in factoryHelpers.ts; kept as a local const (that
// map isn't exported) so a stage rename there is a one-line follow-up here and
// the unit tests pin the ordering.
export const PIPELINE_STAGES = [
  'plan_slice',
  'research',
  'implement',
  'tests',
  'pr_self_review',
  'mr',
  'ci_watch',
  'merge',
  'cleanup',
] as const;

// LineageSegment is the discriminated node the ribbon draws (spec §2.2). Both
// modes consume the same union: spine reads warp/shuttle/bolt/spark, strand
// reads warp/stage/bolt/spark. `count` is optional across the count-bearing
// kinds so the spine can show live tallies (e.g. "12 bolts", "2 sparks") while
// a strand's terminal bolt/spark carries none.
export type LineageSegment =
  | { kind: 'warp'; label: string; count?: number; href?: string; tone?: BadgeVariant }
  | { kind: 'shuttle'; label: string; count?: number; href?: string; active?: boolean }
  | { kind: 'stage'; stage: string; state: 'done' | 'active' | 'pending' | 'failed' }
  | { kind: 'bolt'; mriid: number | null; count?: number; href?: string }
  | { kind: 'spark'; class: string; count?: number; reasons?: string[]; href?: string };

/** Warm→cool tone ramp for a priority bucket (P0 hottest, P3 coolest). */
export function priorityTone(priority: string | undefined): BadgeVariant {
  switch ((priority ?? '').toUpperCase()) {
    case 'P0':
      return 'error';
    case 'P1':
      return 'accent';
    case 'P2':
      return 'warning';
    case 'P3':
      return 'info';
    default:
      return 'muted';
  }
}

// Run states that count as "on the take-up roll" (a woven bolt).
const BOLT_STATES = new Set(['done', 'merged']);

/** True when a run's terminal state means it produced a bolt. */
export function isBoltState(state: string | undefined): boolean {
  return BOLT_STATES.has((state ?? '').toLowerCase());
}

/** True when a run flew off as a spark and awaits a human. */
export function isSparkState(state: string | undefined): boolean {
  const s = (state ?? '').toLowerCase();
  return s === 'escalated' || s === 'paused';
}

/**
 * lineageFor threads one run's strand: warp (joined backlog item for
 * priority/plan) → the canonical stage nodes with per-node state → the
 * terminal bolt or spark. Pure and DOM-free so it unit-tests directly; the
 * store's `lineageFor(run)` method delegates here with `this.backlog`.
 *
 * `reasons` is an optional enrichment: a caller that already fetched the run's
 * failing gates can pass them so the terminal spark carries its "why". The
 * list row alone can't — reasons live only on the detail payload — so absent
 * is the honest default.
 */
export function lineageFor(
  run: PipelineRun,
  backlog: BacklogItem[],
  reasons?: string[],
): LineageSegment[] {
  const segments: LineageSegment[] = [];

  // Warp node: join BacklogID → backlog item (spec §7 join key). Degrades to
  // the raw backlog id when the item isn't in the co-fetched list.
  const item = (backlog ?? []).find((b) => b.ID === run.BacklogID);
  const priority = item?.Priority ?? '';
  const planLabel = item?.PlanID || item?.Title || run.BacklogID || 'unstrung';
  segments.push({
    kind: 'warp',
    label: priority ? `${priority} · ${planLabel}` : planLabel,
    tone: priorityTone(priority),
    href: run.BacklogID ? `#mills/warps/${run.BacklogID}` : undefined,
  });

  // Stage nodes: canonical order, state derived from the run row. A terminal
  // bolt shows every pick done; an unknown CurrentStage leaves all pending
  // rather than inventing progress.
  const state = (run.State ?? '').toLowerCase();
  const bolt = isBoltState(state);
  const escalated = state === 'escalated';
  const paused = state === 'paused';
  const currentIdx = PIPELINE_STAGES.indexOf(
    (run.CurrentStage ?? '') as (typeof PIPELINE_STAGES)[number],
  );
  for (let i = 0; i < PIPELINE_STAGES.length; i++) {
    let state: 'done' | 'active' | 'pending' | 'failed';
    if (bolt) {
      state = 'done';
    } else if (currentIdx < 0) {
      state = 'pending';
    } else if (i < currentIdx) {
      state = 'done';
    } else if (i === currentIdx) {
      state = escalated ? 'failed' : 'active';
    } else {
      state = 'pending';
    }
    segments.push({ kind: 'stage', stage: PIPELINE_STAGES[i], state });
  }

  // Terminal node: bolt (merged/done) or spark (escalated/paused). Paused
  // runs are held, not failed, so they get a warning spark without forcing the
  // current stage into the failed color.
  if (bolt) {
    segments.push({
      kind: 'bolt',
      mriid: run.MRIID ?? null,
      href: run.ID ? `#mills/bolts/${run.ID}` : undefined,
    });
  } else if (escalated || paused) {
    segments.push({
      kind: 'spark',
      class: paused ? 'held' : run.EscalationClass || run.FailureClass || 'unclassified',
      reasons: !paused && reasons && reasons.length > 0 ? reasons : undefined,
      href: run.ID ? `#mills/sparks/${run.ID}` : undefined,
    });
  }

  return segments;
}

/** Inputs the floor-nav spine ribbon summarizes. */
export interface SpineInput {
  /** Priority-bucketed backlog on the beam (millsStore.backlogByPriority). */
  backlogByPriority: Record<string, BacklogItem[]>;
  /** Active pipeline runs in flight (shuttles). */
  activeShuttles: number;
  /** Bolts wound in the window (merged/done terminal runs). */
  bolts: number;
  /** Sparks on the floor (escalated runs). */
  sparks: number;
}

/**
 * spineSegments builds the horizontal floor ribbon: one warp node per priority
 * bucket, a shuttle node for in-flight runs, and the take-up bolt + spark
 * tallies. Every node is a nav link to its view; counts are live, never
 * decorative (spec §2.2 "structure not decoration").
 */
export function spineSegments(input: SpineInput): LineageSegment[] {
  const byPriority = input.backlogByPriority ?? {};
  const segments: LineageSegment[] = [];

  for (const p of WARP_PRIORITIES) {
    segments.push({
      kind: 'warp',
      label: p,
      count: (byPriority[p] ?? []).length,
      href: '#mills/warps',
      tone: priorityTone(p),
    });
  }

  const shuttles = input.activeShuttles ?? 0;
  segments.push({
    kind: 'shuttle',
    label: 'shuttles',
    count: shuttles,
    href: '#mills/shuttles',
    active: shuttles > 0,
  });

  segments.push({
    kind: 'bolt',
    mriid: null,
    count: input.bolts ?? 0,
    href: '#mills/bolts',
  });

  segments.push({
    kind: 'spark',
    class: 'sparks',
    count: input.sparks ?? 0,
    href: '#mills/sparks',
  });

  return segments;
}

/** aria-label for one stage node, spoken via the shared loom vocabulary. */
export function stageNodeLabel(stage: string, state: string): string {
  return `${stageLabel(stage)} — ${state}`;
}
