// Pure helpers for the Factory panel's departure board — the floor log
// as a split-flap airport board. Marquees read as ambient noise; a
// flap-flip on a stage transition reads as news. One row per run, and
// every status is derived from observed data: DELAYED means "we watched
// this run sit in the same stage past the threshold", not a guess.
// Rune-free so vitest covers the mapping without a Svelte runtime.

import type { DemandLogRow, PipelineRun } from '../stores/mills.svelte.ts';
import { stageLabel } from './factoryHelpers.ts';

/** One departure-board row. */
export interface DepartureRow {
  /** Stable row identity for keyed rendering (run ID). */
  key: string;
  /** Short run id — the "flight number". */
  flight: string;
  /** Backlog item the bolt is being woven for. */
  destination: string;
  /** Current stage (active) or terminal verb (history). */
  via: string;
  status: 'en route' | 'delayed' | 'arrived' | 'diverted' | 'held' | 'suppressed';
  /**
   * Status qualifier — how long a DELAYED row has been observed sitting in
   * its stage. The board already knows the age (it is what tripped the fuse);
   * printing the bare word "delayed" throws that away, and "delayed 4m" reads
   * very differently from "delayed 3h". Absent on every other status.
   */
  note?: string;
  tone: 'ok' | 'hot' | 'wr' | 'cy' | 'dm';
  /**
   * Subrun recursion depth (0 = top-level run). Populated for active rows so
   * a board that groups subruns under their parent can indent them into a
   * tree. Absent/0 for history rows and callers that ignore lineage depth
   * (e.g. FactoryPanel), which keeps the field backward-compatible.
   */
  depth?: number;
  /**
   * Board clock — departure time for active rows (StartedAt), arrival time
   * for terminal ones (EndedAt, falling back to StartedAt). HH:MM local:
   * a departure board states the time, not an age, and minute granularity
   * matches the poll cadence. Absent when the feed carries no timestamp.
   */
  when?: string;
}

/** Stage-entry observation for one run: which stage, first seen when. */
export interface StageObservation {
  stage: string;
  since: number;
}

/**
 * A run counts as DELAYED once it has been *observed* in the same stage
 * for this long. Client-observed, not operator p90 — honest about what
 * the HUD actually knows. ci_watch legitimately takes the pipeline's
 * wall-clock, so it gets a longer fuse.
 */
export const DELAYED_AFTER_MS = 12 * 60_000;
const STAGE_DELAY_MS: Record<string, number> = {
  ci_watch: 25 * 60_000,
};

/**
 * Advance the stage-entry observation map from one poll to the next:
 * a run first seen, or seen in a new stage, is stamped `now`; a run
 * still in its stage keeps its original stamp; vanished runs drop.
 */
export function nextStageSince(
  prev: ReadonlyMap<string, StageObservation>,
  active: PipelineRun[],
  now: number,
): Map<string, StageObservation> {
  const next = new Map<string, StageObservation>();
  for (const run of active) {
    if (!run?.ID) continue;
    const stage = run.CurrentStage ?? '';
    const before = prev.get(run.ID);
    next.set(run.ID, before && before.stage === stage ? before : { stage, since: now });
  }
  return next;
}

/**
 * Flight number for the board. Run IDs share a long common prefix
 * (`PIPE-psl-plan-council-…`), so the head of the ID is the one part that
 * carries no information — every row used to render an identical
 * "PIPE-psl-p". The distinctive half is the TAIL (slice ordinal + attempt),
 * so strip the transport prefix and keep the end.
 */
export function flightID(id: string): string {
  const trimmed = id.replace(/^PIPE-/, '');
  if (trimmed.length <= 12) return trimmed;
  return '…' + trimmed.slice(-11);
}

/**
 * Middle-truncate a backlog slug. CSS end-ellipsis amputates exactly the
 * informative half of council slugs (`…-fail-closed-a-2` is the part that
 * distinguishes siblings), so keep both ends and drop the middle. Callers
 * keep the full value in a title attribute.
 */
export function squeezeSlug(s: string, max = 46): string {
  if (s.length <= max) return s;
  const head = Math.floor((max - 1) * 0.45);
  const tail = max - 1 - head;
  return s.slice(0, head) + '…' + s.slice(-tail);
}

/**
 * Demand-side rows: proposals the council declined to mint because they
 * restated recently-merged work. On the board they are the factory saying
 * "no, and here is why" — the flight is the council itself, the warp is the
 * declined proposal, and the stage names the shipped work it duplicated.
 * Dimmed tone: a suppression is a deliberate non-event, not an alarm.
 */
export function suppressionRows(log: DemandLogRow[], max = 3): DepartureRow[] {
  const rows: DepartureRow[] = [];
  for (const item of log ?? []) {
    if (rows.length >= max) break;
    if (!item?.proposal_title) continue;
    rows.push({
      key: `demand-${item.merged_ref ?? item.merged_title}-${item.occurred_at}`,
      flight: 'council',
      destination: item.proposal_title,
      via: item.merged_title ? `already woven — ${item.merged_title}` : 'already woven',
      status: 'suppressed',
      tone: 'dm',
      when: clockLabel(item.occurred_at),
    });
  }
  return rows;
}

/** HH:MM local for the board clock; undefined for absent/invalid stamps. */
export function clockLabel(iso?: string): string | undefined {
  if (!iso) return undefined;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return undefined;
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

/**
 * Compact stage age for a DELAYED note. Minute granularity is the honest
 * resolution — the shortest fuse is 12 minutes — and there is no "ago"
 * suffix because the status word already says what is being measured.
 * Mirrors the h/m shape of freshnessLabel in andonHelpers.
 */
export function delayAge(ms: number): string {
  const mins = Math.max(0, Math.floor(ms / 60_000));
  if (mins < 60) return `${mins}m`;
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
}

/**
 * Build the board: active runs first (the departures), then recent
 * terminal runs (arrivals and diversions) up to maxRows. Ordering
 * within each group follows the feed (active as served; history
 * newest-first).
 */
export function departureRows(
  active: PipelineRun[],
  history: PipelineRun[],
  stageSince: ReadonlyMap<string, StageObservation>,
  now: number,
  opts?: { maxRows?: number },
): DepartureRow[] {
  const maxRows = opts?.maxRows ?? 7;
  const rows: DepartureRow[] = [];
  for (const run of active) {
    if (!run?.ID || rows.length >= maxRows) break;
    const stage = run.CurrentStage ?? '';
    const obs = stageSince.get(run.ID);
    const fuse = STAGE_DELAY_MS[stage] ?? DELAYED_AFTER_MS;
    const heldFor = obs && obs.stage === stage ? now - obs.since : 0;
    const delayed = !!obs && obs.stage === stage && heldFor > fuse;
    rows.push({
      key: run.ID,
      flight: flightID(run.ID),
      destination: run.BacklogID || run.ID,
      via: stageLabel(stage),
      status: delayed ? 'delayed' : 'en route',
      note: delayed ? delayAge(heldFor) : undefined,
      tone: delayed ? 'wr' : 'hot',
      depth: run.Depth ?? 0,
      when: clockLabel(run.StartedAt),
    });
  }
  for (const run of history) {
    if (rows.length >= maxRows) break;
    if (!run?.ID) continue;
    const state = (run.State ?? '').toLowerCase();
    const when = clockLabel(run.EndedAt ?? run.StartedAt);
    if (state === 'done' || state === 'merged') {
      rows.push({
        key: run.ID,
        flight: flightID(run.ID),
        destination: run.BacklogID || run.ID,
        via: 'rolled off the beam',
        status: 'arrived',
        tone: 'ok',
        when,
      });
    } else if (state === 'escalated') {
      rows.push({
        key: run.ID,
        flight: flightID(run.ID),
        destination: run.BacklogID || run.ID,
        via: 'broken pick — human eye',
        status: 'diverted',
        tone: 'wr',
        when,
      });
    } else if (state === 'paused') {
      rows.push({
        key: run.ID,
        flight: flightID(run.ID),
        destination: run.BacklogID || run.ID,
        via: 'held on the beam',
        status: 'held',
        tone: 'cy',
        when,
      });
    }
  }
  return rows;
}
