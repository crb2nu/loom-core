/**
 * Journey helpers — one backlog item's timeline, assembled from the two
 * sources that actually record it.
 *
 * Rune-free so vitest exercises the merge without a Svelte runtime (the
 * lineage.ts / provenance.ts precedent).
 *
 * WHY TWO SOURCES: the events table records only transitions routed through
 * BacklogDAO.TransitionStateWithEvent (bootstrap escalation, auto-requeue,
 * groomer actions) plus explicit appends (operator overrides, agent routing).
 * A plain queued→running claim advances ClaimVersion and writes NO event. So
 * the ledger alone would show an item escalating and being requeued with no
 * sign of the runs in between. Interleaving the item's pipeline runs fills
 * exactly that gap, and the caller labels the result as recorded history
 * rather than a complete lifecycle — see `partial` on the API response.
 */

import type { MillsEvent, PipelineRun } from '../../../stores/mills.svelte.ts';
import type { BadgeVariant } from '../../../utils/tokens.ts';

export interface JourneyEntry {
  /** Stable key for keyed each-blocks. */
  key: string;
  source: 'event' | 'run';
  /** Epoch ms used for ordering; 0 when the timestamp was unparseable. */
  at: number;
  /** Raw timestamp for the relative-time stamp. */
  timestamp: string;
  /** Short headline ("escalated", "auto requeued", "run merged"). */
  label: string;
  /** Who did it. */
  actor: string;
  /** Optional supporting text (failure class, stage, reason). */
  detail?: string;
  tone: BadgeVariant;
  /** Pipeline run id, when this entry is (or belongs to) a run. */
  runID?: string;
}

function ms(timestamp: string | undefined | null): number {
  if (!timestamp) return 0;
  const t = Date.parse(timestamp);
  return Number.isFinite(t) ? t : 0;
}

/**
 * Humanize an event kind: "reconciler.auto_requeued" → "auto requeued".
 * The actor is rendered separately, so the dotted namespace prefix is noise.
 */
export function eventLabel(kind: string | undefined | null): string {
  const raw = (kind ?? '').trim();
  if (!raw) return 'event';
  const tail = raw.split('.').filter(Boolean).slice(1).join(' ') || raw;
  return tail.replace(/[_.]+/g, ' ').trim();
}

/**
 * Tone for an event kind, by what it means for the operator.
 *
 * Operator overrides are matched FIRST: "operator.override.requeue" also
 * contains "requeue", and the salient fact about it is that a human
 * intervened, not that a requeue happened. Toning it like an automatic
 * reconciler requeue would hide every manual intervention in the strip.
 */
export function eventTone(kind: string | undefined | null): BadgeVariant {
  const k = (kind ?? '').toLowerCase();
  if (k.includes('override') || k.includes('operator')) return 'accent';
  if (k.includes('escalat') || k.includes('fail')) return 'error';
  if (k.includes('requeue') || k.includes('retry')) return 'warning';
  if (k.includes('merged') || k.includes('done')) return 'success';
  return 'muted';
}

/** Tone for a pipeline run's terminal/current state. */
export function runTone(state: string | undefined | null): BadgeVariant {
  const s = (state ?? '').toLowerCase();
  if (s === 'merged' || s === 'done') return 'success';
  if (s === 'escalated' || s === 'failed') return 'error';
  if (s === 'paused') return 'warning';
  if (s === 'running') return 'info';
  return 'muted';
}

/**
 * Pull the salient payload keys onto one line.
 *
 * Payloads are open maps and some carry long prose (a bootstrap escalation
 * embeds the raw error). Only a curated set is surfaced, in a fixed order, so
 * the strip stays scannable and never blows the drawer width open.
 */
export function payloadSummary(payload: Record<string, unknown> | null | undefined): string {
  if (!payload) return '';
  const keys = [
    'from',
    'to',
    'state',
    'reason',
    'class',
    'failure_code',
    'error_class',
    'stage',
    'agent',
    'target_project',
    'attempt',
  ];
  const parts: string[] = [];
  for (const k of keys) {
    const v = payload[k];
    if (v == null || v === '') continue;
    if (typeof v === 'object') continue;
    let text = String(v);
    if (text.length > 80) text = `${text.slice(0, 77)}…`;
    parts.push(`${k}=${text}`);
  }
  return parts.join(' · ');
}

/**
 * Merge an item's recorded events and its pipeline runs into one newest-first
 * timeline.
 *
 * A run contributes up to two entries — started and ended — because "ran for
 * 40 minutes then escalated" is two facts an operator reads at different
 * points on the strip. A run with no EndedAt contributes only its start.
 */
export function journeyEntries(
  events: MillsEvent[] | undefined | null,
  runs: PipelineRun[] | undefined | null,
): JourneyEntry[] {
  const out: JourneyEntry[] = [];

  for (const ev of events ?? []) {
    if (!ev) continue;
    out.push({
      key: `event-${ev.ID}`,
      source: 'event',
      at: ms(ev.OccurredAt),
      timestamp: ev.OccurredAt,
      label: eventLabel(ev.Kind),
      actor: ev.Actor ?? '',
      detail: payloadSummary(ev.Payload),
      tone: eventTone(ev.Kind),
    });
  }

  for (const run of runs ?? []) {
    if (!run?.ID) continue;
    if (run.StartedAt) {
      out.push({
        key: `run-start-${run.ID}`,
        source: 'run',
        at: ms(run.StartedAt),
        timestamp: run.StartedAt,
        label: 'run started',
        actor: 'pipeline',
        detail: run.Template ? `template=${run.Template}` : undefined,
        tone: 'info',
        runID: run.ID,
      });
    }
    if (run.EndedAt) {
      const state = (run.State ?? '').toLowerCase();
      const bits: string[] = [];
      if (run.CurrentStage) bits.push(`stage=${run.CurrentStage}`);
      if (run.EscalationClass) bits.push(`class=${run.EscalationClass}`);
      if (run.MRIID != null) bits.push(`mr=!${run.MRIID}`);
      out.push({
        key: `run-end-${run.ID}`,
        source: 'run',
        at: ms(run.EndedAt),
        timestamp: run.EndedAt,
        label: `run ${state || 'ended'}`,
        actor: 'pipeline',
        detail: bits.join(' · ') || undefined,
        tone: runTone(state),
        runID: run.ID,
      });
    }
  }

  // Newest-first, matching the ledger endpoint's order. Ties break on key so
  // the order is deterministic across renders (events and a run boundary can
  // share a timestamp to the second).
  return out.sort((a, b) => b.at - a.at || a.key.localeCompare(b.key));
}
