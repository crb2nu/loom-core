// Pure builders for the Factory's live floor surfaces: the shuttle board
// (one lane per active run), the press lanes (serial merge queue), the
// spark split (24h escalations by fault class) and the overseer strip.
//
// Rune-free on purpose, like factoryHelpers/departureHelpers: every function
// is a total mapping from already-fetched store state to a render model, so
// the whole floor is unit-testable without mounting the canvas.

import type {
  BacklogItem,
  MergeQueueEntry,
  MergeQueueSnapshot,
  PipelineRun,
  PipelineRunDetail,
  StageResult,
} from '../stores/mills.svelte.ts';
import type { PromotionReport } from '../stores/mills_staff.svelte.ts';
import type { OverseerAgent, OverseersStatus, OverseerSoak, OverseerEvent } from '../stores/mills_overseers.svelte.ts';
import { PIPELINE_STAGES, priorityTone } from '../components/mills/shared/lineage.ts';
import type { BadgeVariant } from './tokens.ts';
import { stageLabel } from './factoryHelpers.ts';
import { stageFuseMs, type StageObservation } from './departureHelpers.ts';

// ---------------------------------------------------------------------------
// Shuttle board
// ---------------------------------------------------------------------------

export type StageNodeState = 'done' | 'active' | 'pending' | 'failed';

export interface StageNode {
  stage: string;
  label: string;
  state: StageNodeState;
}

export interface ShuttleLane {
  runID: string;
  backlogID: string;
  /** Short run id for the flight column, e.g. `…c01a6b16190a`. */
  flight: string;
  title: string;
  priority: string;
  priorityTone: BadgeVariant;
  project: string;
  depth: number;
  state: string;
  stage: string;
  stageLabel: string;
  nodes: StageNode[];
  /** ms observed in the current stage, or null before the first observation. */
  stageAgeMs: number | null;
  delayed: boolean;
  attempts: number;
  /** Attempt number of the current stage record, when detail has landed. */
  stageAttempt: number | null;
  /**
   * Who is weaving this pick. `weaverSource` says where the answer came
   * from: the stage record itself ('record'), the run's provenance routing
   * for a pick still in flight ('routed' — the record's Model is empty until
   * the pick ends), the spawn id alone ('spawn' — backend known, model not
   * yet), or the operator for the picks no model runs ('operator': tests,
   * mr, ci_watch, merge, cleanup).
   */
  model: string | null;
  agent: string | null;
  weaverSource: WeaverSource | null;
  costUSD: number | null;
  mrIID: number | null;
  /**
   * Last non-empty log line of the newest stage record — what it is doing
   * now when that record is the in-flight stage, or what the last finished
   * pick said when the current stage has no record yet (records land at
   * pick end). `nowStage` names which; the board labels it when it is not
   * the current stage.
   */
  now: string | null;
  nowStage: string | null;
  gatesPassed: number;
  gatesFailed: number;
  lastFailedGate: string | null;
  detail: 'pending' | 'loaded' | 'unavailable';
  startedAt: string | undefined;
}

const NOW_LINE_MAX = 160;

/** The last non-empty line of a log tail, trimmed and clipped. */
export function lastLogLine(tail: string | null | undefined): string | null {
  if (!tail) return null;
  const lines = tail.split('\n').map((l) => l.trim()).filter((l) => l.length > 0);
  if (lines.length === 0) return null;
  const line = lines[lines.length - 1];
  return line.length > NOW_LINE_MAX ? `${line.slice(0, NOW_LINE_MAX - 1)}…` : line;
}

/** Short tail of a run id: the trailing uuid chunk the boards already use. */
export function flightOf(runID: string): string {
  const tail = runID.slice(-12);
  return runID.length > 12 ? `…${tail}` : runID;
}

/** The newest stage record for `stage` (highest attempt, then latest start). */
export function currentStageRecord(
  stages: StageResult[] | undefined | null,
  stage: string | undefined,
): StageResult | null {
  if (!stages || !stage) return null;
  let best: StageResult | null = null;
  for (const rec of stages) {
    if (rec.Stage !== stage) continue;
    if (!best) {
      best = rec;
      continue;
    }
    if (rec.Attempt > best.Attempt) best = rec;
    else if (rec.Attempt === best.Attempt && rec.StartedAt > best.StartedAt) best = rec;
  }
  return best;
}

/** The newest stage record of any stage (highest start, then attempt). */
export function latestStageRecord(stages: StageResult[] | undefined | null): StageResult | null {
  if (!stages) return null;
  let best: StageResult | null = null;
  for (const rec of stages) {
    if (!best || rec.StartedAt > best.StartedAt || (rec.StartedAt === best.StartedAt && rec.Attempt > best.Attempt)) {
      best = rec;
    }
  }
  return best;
}

export type WeaverSource = 'record' | 'routed' | 'spawn' | 'operator';

export interface Weaver {
  model: string | null;
  agent: string | null;
  source: WeaverSource | null;
}

/** Picks the operator runs itself — no model is ever attached to them. */
const OPERATOR_STAGES = new Set(['tests', 'mr', 'ci_watch', 'merge', 'cleanup']);

/**
 * Who is weaving a pick. Stage records are opened at pick start with empty
 * Model/Backend and filled at pick end, so an in-flight agent pick has to be
 * read from what is known before the record completes:
 *
 *   1. the record's agent_routing / Model / Backend (complete picks)
 *   2. a `weaver-<model>` spawn id (flexinfer research picks carry the model
 *      in the spawn id and no routing block)
 *   3. the run's provenance stage_models — the routed model for that stage
 *   4. a `spawn-…` spawn id — backend known, model not yet
 *
 * Operator-run picks (tests, mr, ci_watch, merge, cleanup) answer 'operator'
 * up front so the lane never shows a dash for a pick that has no weaver by
 * design.
 */
export function weaverFor(
  rec: StageResult | null,
  stage: string,
  stageModels: Record<string, string> | null | undefined,
): Weaver {
  const art = (rec?.Artifacts ?? {}) as Record<string, unknown>;
  const route = (art.agent_routing ?? {}) as Record<string, unknown>;
  const routedModel = typeof route.model === 'string' && route.model ? route.model : null;
  const routedAgent = typeof route.agent === 'string' && route.agent ? route.agent : null;
  const model = routedModel || rec?.Model || null;
  const agent = routedAgent || rec?.Backend || null;
  if (model || agent) return { model, agent, source: 'record' };

  if (OPERATOR_STAGES.has(stage)) return { model: null, agent: 'operator', source: 'operator' };

  const spawn = rec?.SpawnID ?? '';
  if (spawn.startsWith('weaver-') && spawn.length > 'weaver-'.length) {
    return { model: spawn.slice('weaver-'.length), agent: 'flexinfer', source: 'record' };
  }
  const provenance = stageModels?.[stage];
  if (provenance) {
    return { model: provenance, agent: spawn.startsWith('spawn-') ? 'spawn' : null, source: 'routed' };
  }
  if (spawn.startsWith('spawn-')) return { model: null, agent: 'spawn', source: 'spawn' };
  return { model: null, agent: null, source: null };
}

export function stageNodes(run: PipelineRun): StageNode[] {
  const state = (run.State ?? '').toLowerCase();
  const escalated = state === 'escalated';
  const idx = PIPELINE_STAGES.indexOf((run.CurrentStage ?? '') as (typeof PIPELINE_STAGES)[number]);
  return PIPELINE_STAGES.map((stage, i) => {
    let nodeState: StageNodeState;
    if (idx < 0) nodeState = 'pending';
    else if (i < idx) nodeState = 'done';
    else if (i === idx) nodeState = escalated ? 'failed' : 'active';
    else nodeState = 'pending';
    return { stage, label: stageLabel(stage), state: nodeState };
  });
}

export interface LaneDetailInput {
  status: 'pending' | 'loaded' | 'unavailable';
  detail?: PipelineRunDetail | null;
}

/**
 * One lane of the shuttle board. `detail` is the lazily-loaded stage/gate
 * record set (or pending/unavailable); `obs` is the stage-entry observation
 * the departures board also keeps, so both call DELAYED at the same moment.
 */
export function buildLane(
  run: PipelineRun,
  backlog: BacklogItem[] | undefined | null,
  detail: LaneDetailInput,
  obs: StageObservation | undefined,
  now: number,
): ShuttleLane {
  const item = (backlog ?? []).find((b) => b.ID === run.BacklogID);
  const stage = run.CurrentStage ?? '';
  // Clamped: the observation is stamped by an effect that can run a few ms
  // after the board's clock was read, and a negative age renders as "—".
  const heldFor = obs && obs.stage === stage ? Math.max(0, now - obs.since) : null;
  const delayed = heldFor != null && heldFor > stageFuseMs(stage);

  // The current stage's record exists only once that pick has ended (or is
  // being retried); until then the newest record is the last finished pick,
  // and it is the honest source for weaver and the log line.
  const stages = detail.status === 'loaded' ? detail.detail?.stages : null;
  const rec = currentStageRecord(stages, stage) ?? latestStageRecord(stages);
  const recIsCurrent = rec != null && rec.Stage === stage;
  // The weaver is always answered for the CURRENT pick: a finished record
  // from an earlier stage says who wove that pick, not this one.
  const weaver = weaverFor(
    recIsCurrent ? rec : null,
    stage,
    detail.status === 'loaded' ? detail.detail?.evidence?.provenance?.stage_models : null,
  );
  const nowLine = rec ? lastLogLine(rec.LogTail) : null;
  const gates = detail.status === 'loaded' ? (detail.detail?.gates ?? []) : [];
  let gatesPassed = 0;
  let gatesFailed = 0;
  let lastFailedGate: string | null = null;
  for (const g of gates) {
    if (g.Outcome === 'pass') gatesPassed++;
    else if (g.Outcome === 'fail') {
      gatesFailed++;
      lastFailedGate = g.GateName;
    }
  }

  return {
    runID: run.ID,
    backlogID: run.BacklogID ?? '',
    flight: flightOf(run.ID),
    title: item?.Title || run.BacklogID || run.ID,
    priority: item?.Priority ?? '',
    priorityTone: priorityTone(item?.Priority),
    project: item?.TargetProject ?? '',
    depth: run.Depth ?? 0,
    state: (run.State ?? '').toLowerCase(),
    stage,
    stageLabel: stageLabel(stage || undefined),
    nodes: stageNodes(run),
    stageAgeMs: heldFor,
    delayed,
    attempts: run.Attempts ?? 0,
    stageAttempt: recIsCurrent ? rec.Attempt : null,
    model: weaver.model,
    agent: weaver.agent,
    weaverSource: weaver.source,
    costUSD: run.CostUSD ?? null,
    mrIID: run.MRIID ?? null,
    now: nowLine,
    nowStage: nowLine && rec ? rec.Stage : null,
    gatesPassed,
    gatesFailed,
    lastFailedGate,
    detail: detail.status,
    startedAt: run.StartedAt,
  };
}

/**
 * Lanes for the board, hottest first: delayed runs, then by stage age
 * descending so the longest-sitting pick is on top, then by priority.
 */
export function sortLanes(lanes: ShuttleLane[]): ShuttleLane[] {
  const rank = (p: string): number => {
    const n = Number.parseInt(p.replace(/^P/i, ''), 10);
    return Number.isFinite(n) ? n : 9;
  };
  return [...lanes].sort((a, b) => {
    if (a.delayed !== b.delayed) return a.delayed ? -1 : 1;
    const pa = rank(a.priority);
    const pb = rank(b.priority);
    if (pa !== pb) return pa - pb;
    return (b.stageAgeMs ?? -1) - (a.stageAgeMs ?? -1);
  });
}

// ---------------------------------------------------------------------------
// Press lanes (serial merge queue)
// ---------------------------------------------------------------------------

export interface PressRow {
  key: string;
  mrIID: number;
  project: string;
  backlogID: string;
  runID: string;
  state: string;
  phrase: string;
  tone: BadgeVariant;
  attempts: number;
  ageMs: number | null;
  settled: boolean;
  reason: string | null;
}

const PRESS_PHRASE: Record<string, [string, BadgeVariant]> = {
  queued: ['waiting for the press', 'muted'],
  rebasing: ['rebasing onto main', 'info'],
  awaiting_pipeline: ['re-proving on CI', 'info'],
  merging: ['pressing', 'accent'],
  merged: ['pressed · merged', 'success'],
  evicted: ['evicted', 'warning'],
};

function pressRow(e: MergeQueueEntry, now: number, settled: boolean): PressRow {
  const state = (e.state ?? '').toLowerCase();
  const [phrase, tone] = PRESS_PHRASE[state] ?? [state.replaceAll('_', ' ') || 'unknown', 'muted'];
  const stamp = Date.parse((settled ? e.settled_at ?? e.updated_at : e.enqueued_at) ?? '');
  return {
    key: `${e.id}`,
    mrIID: e.mr_iid,
    project: e.project,
    backlogID: e.backlog_id,
    runID: e.pipeline_run_id,
    state,
    phrase: state === 'evicted' && e.eviction_reason ? `${phrase} · ${e.eviction_reason}` : phrase,
    tone,
    attempts: e.attempts ?? 0,
    ageMs: Number.isFinite(stamp) ? now - stamp : null,
    settled,
    reason: e.eviction_reason ?? null,
  };
}

/** Active press entries in queue order, then the last `settledMax` settled. */
export function pressRows(
  snapshot: MergeQueueSnapshot | null | undefined,
  now: number,
  settledMax = 3,
): PressRow[] {
  if (!snapshot) return [];
  const active = (snapshot.active ?? []).map((e) => pressRow(e, now, false));
  const settled = (snapshot.recent_settled ?? []).slice(0, settledMax).map((e) => pressRow(e, now, true));
  return [...active, ...settled];
}

// ---------------------------------------------------------------------------
// Spark split — 24h escalations by fault class
// ---------------------------------------------------------------------------

/** Fault classes an operator requeues through rather than fixes. */
export function isInfraClass(c: string | undefined): boolean {
  const k = (c ?? '').toLowerCase();
  return k === 'infra' || k === 'transient' || k === 'transient_quota' || k === 'external' || k === 'quota';
}

export interface SparkSplit {
  total: number;
  infra: number;
  real: number;
  classes: Array<{ cls: string; n: number; infra: boolean }>;
}

export function sparkSplit(byClass: Record<string, number> | undefined | null): SparkSplit | null {
  if (!byClass) return null;
  const classes = Object.entries(byClass)
    .filter(([, n]) => Number.isFinite(n) && n > 0)
    .map(([cls, n]) => ({ cls, n, infra: isInfraClass(cls) }))
    .sort((a, b) => b.n - a.n);
  if (classes.length === 0) return null;
  const infra = classes.filter((c) => c.infra).reduce((s, c) => s + c.n, 0);
  const total = classes.reduce((s, c) => s + c.n, 0);
  return { total, infra, real: total - infra, classes };
}

// ---------------------------------------------------------------------------
// Overseer strip — groomer / sentinel / foreman
// ---------------------------------------------------------------------------

export type OverseerState = 'ticking' | 'silent' | 'paused' | 'suppressed' | 'disabled' | 'error';

export interface OverseerRow {
  name: string;
  state: OverseerState;
  tone: BadgeVariant;
  tickAgeMs: number | null;
  inspected: number;
  acted: number;
  planned: number;
  errored: number;
  note: string | null;
  dryRun: boolean;
}

/** An overseer that has not ticked in this long is "silent". */
export const OVERSEER_SILENT_AFTER_MS = 10 * 60_000;

function overseerRow(a: OverseerAgent, now: number): OverseerRow {
  const stamp = a.last_tick_at ? Date.parse(a.last_tick_at) : NaN;
  const tickAgeMs = Number.isFinite(stamp) ? now - stamp : null;
  let state: OverseerState;
  let tone: BadgeVariant;
  if (!a.enabled) [state, tone] = ['disabled', 'muted'];
  else if (a.paused) [state, tone] = ['paused', 'warning'];
  else if (a.suppression) [state, tone] = ['suppressed', 'warning'];
  else if (a.last_error) [state, tone] = ['error', 'error'];
  else if (tickAgeMs == null || tickAgeMs > OVERSEER_SILENT_AFTER_MS) [state, tone] = ['silent', 'muted'];
  else [state, tone] = ['ticking', 'success'];
  const r = a.last_result;
  return {
    name: a.name,
    state,
    tone,
    tickAgeMs,
    inspected: r?.inspected ?? 0,
    acted: r?.acted ?? 0,
    planned: r?.planned ?? 0,
    errored: r?.errored ?? 0,
    note: a.last_error || a.suppression?.reason || r?.note || null,
    dryRun: !!a.dry_run,
  };
}

export function overseerRows(status: OverseersStatus | null | undefined, now: number): OverseerRow[] {
  return (status?.agents ?? []).map((a) => overseerRow(a, now));
}


export type ReadinessLabel = 'no evidence' | `soaking (${number}/7d)` | 'review' | 'promoted';
export interface OverseerActionReadiness {
  action: string;
  dry: number;
  executed: number;
  subjects: number;
  /** Complete UTC dates spanned by observations, not certified green days. */
  evidenceDays: number;
  label: ReadinessLabel;
  reasons: string[];
  sample: string | null;
  sampleNewest: boolean;
}

const UTC_DAY_MS = 86_400_000;

/** Read-only review aid. Aggregate soak telemetry cannot approve an action class. */
export function overseerReadiness(
  agent: OverseerAgent,
  report: PromotionReport | null | undefined,
  soak: OverseerSoak | null | undefined,
  events: OverseerEvent[] | null = [],
): { mode: 'dry' | 'live'; label: ReadinessLabel; actions: OverseerActionReadiness[]; reasons: string[] } {
  const start = Date.parse(report?.window_start ?? '');
  const end = Date.parse(report?.window_end ?? '');
  const closed = Number.isFinite(start) && Number.isFinite(end) &&
    start % UTC_DAY_MS === 0 && end % UTC_DAY_MS === 0 && end - start >= 7 * UTC_DAY_MS;
  const actor = `overseer.${agent.name}`;
  const actions = (report?.per_actor?.find((a) => a.actor === actor)?.per_action ?? [])
    .map((a): OverseerActionReadiness => {
      const first = Date.parse(a.first);
      const last = Date.parse(a.last);
      const valid = Number.isFinite(first) && Number.isFinite(last) && first >= start && last <= end && first <= last;
      const evidenceDays = valid ? Math.max(0, Math.min(7,
        Math.min(Math.floor(last / UTC_DAY_MS) + 1, Math.floor(end / UTC_DAY_MS)) -
        Math.max(Math.floor(first / UTC_DAY_MS), Math.ceil(start / UTC_DAY_MS)),
      )) : 0;
      const days = Math.min(evidenceDays, Math.max(0, Math.floor(soak?.mills_overseer_soak_elapsed_days ?? 0)));
      const reasons: string[] = [];
      if (!closed) reasons.push('Report does not cover seven complete UTC days.');
      if (!valid) reasons.push('Action timestamps are missing or outside the report window.');
      if (evidenceDays < 7) reasons.push('Action evidence does not span seven complete UTC dates.');
      if (!soak) reasons.push('Soak telemetry is unavailable.');
      else {
        if (soak.mills_overseer_soak_elapsed_days < 7) reasons.push('Dry-run soak has not reached seven days.');
        if (soak.fail_closed || soak.mills_overseer_soak_divergences > 0) reasons.push('Soak telemetry reports a failed condition.');
        reasons.push(...(soak.failure_reasons ?? []));
      }
      let label: ReadinessLabel;
      if (a.dry_run + a.executed === 0) {
        label = 'no evidence';
        reasons.unshift('No reviewable intervention evidence is not green.');
      } else if (a.executed > 0 && !agent.dry_run) {
        label = 'promoted';
        reasons.unshift('Live execution observed; this does not certify prior approval.');
      } else {
        if (a.executed > 0) reasons.push('Execution occurred while the agent is currently dry; investigate and restart the window.');
        if (!agent.dry_run) reasons.push('Agent is live without observed execution; dry-run continuity is unverified.');
        label = reasons.length === 0 ? 'review' : `soaking (${days}/7d)`;
        reasons.push('Human review must confirm zero false positives, zero regressions, and no configuration change, then approve this action class.');
      }
      // The report sample is alphabetical. Only a matching latest event can
      // establish recency; never infer it from the sample array position.
      const newest = (events ?? []).find((e) => e.Actor === actor &&
        (e.Kind === `${actor}.${a.action}` || e.Kind === `${actor}.${a.action}.dryrun`) &&
        Date.parse(e.OccurredAt) === last && valid && e.SubjectKind && e.SubjectID);
      return { action: a.action, dry: a.dry_run, executed: a.executed, subjects: a.unique_subjects,
        evidenceDays, label, reasons,
        sample: newest ? `${newest.SubjectKind}/${newest.SubjectID}` : a.subject_sample?.[0] ?? null,
        sampleNewest: !!newest };
    });
  const label: ReadinessLabel = actions.length === 0 ? 'no evidence' :
    actions.find((a) => a.label.startsWith('soaking'))?.label ??
    actions.find((a) => a.label === 'no evidence')?.label ??
    actions.find((a) => a.label === 'review')?.label ?? 'promoted';
  return { mode: agent.dry_run ? 'dry' : 'live', label, actions,
    reasons: actions.length ? [] : [report ? 'No reviewable intervention evidence is not green.' : 'Promotion report unavailable; evidence is unknown.'] };
}
