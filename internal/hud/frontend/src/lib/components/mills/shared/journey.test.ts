import { describe, it, expect } from 'vitest';
import { journeyEntries, eventLabel, eventTone, runTone, payloadSummary } from './journey.ts';
import type { MillsEvent, PipelineRun } from '../../../stores/mills.svelte.ts';

const ev = (over: Partial<MillsEvent>): MillsEvent => ({
  ID: 1,
  OccurredAt: '2026-08-16T12:00:00Z',
  Actor: 'reconciler',
  Kind: 'reconciler.auto_requeued',
  SubjectKind: 'backlog_item',
  SubjectID: 'bl-1',
  Payload: null,
  ...over,
});

const run = (over: Partial<PipelineRun>): PipelineRun => ({
  ID: 'run-1',
  BacklogID: 'bl-1',
  Template: 'default',
  State: 'merged',
  Attempts: 1,
  ...over,
});

describe('eventLabel', () => {
  it('drops the actor namespace and reads as prose', () => {
    expect(eventLabel('reconciler.auto_requeued')).toBe('auto requeued');
    expect(eventLabel('operator.override.requeue')).toBe('override requeue');
    expect(eventLabel('pipeline.agent_routed')).toBe('agent routed');
  });

  it('degrades safely', () => {
    expect(eventLabel('')).toBe('event');
    expect(eventLabel(undefined)).toBe('event');
    // A kind with no namespace keeps its whole self rather than vanishing.
    expect(eventLabel('escalated')).toBe('escalated');
  });
});

describe('eventTone / runTone', () => {
  it('maps meaning to tone', () => {
    expect(eventTone('reconciler.bootstrap_escalated')).toBe('error');
    expect(eventTone('reconciler.auto_requeued')).toBe('warning');
    expect(eventTone('operator.override.requeue')).toBe('accent');
    expect(eventTone('something.unremarkable')).toBe('muted');
    expect(runTone('merged')).toBe('success');
    expect(runTone('escalated')).toBe('error');
    expect(runTone('paused')).toBe('warning');
    expect(runTone('running')).toBe('info');
    expect(runTone(undefined)).toBe('muted');
  });
});

describe('payloadSummary', () => {
  it('surfaces curated keys in a fixed order', () => {
    expect(
      payloadSummary({ stage: 'implement', reason: 'timeout', target_project: 'services/flexdeck' }),
    ).toBe('reason=timeout · stage=implement · target_project=services/flexdeck');
  });

  it('skips empties, objects, and truncates long prose', () => {
    expect(payloadSummary({ reason: '', stage: null, nested: { a: 1 } })).toBe('');
    expect(payloadSummary(null)).toBe('');
    const long = payloadSummary({ reason: 'x'.repeat(200) });
    // Bounded so one verbose bootstrap error can't blow the drawer width open.
    expect(long.length).toBeLessThan(100);
    expect(long.endsWith('…')).toBe(true);
  });

  it('ignores keys outside the curated set', () => {
    expect(payloadSummary({ item: 'bl-1', retryable: true })).toBe('');
  });
});

describe('journeyEntries', () => {
  it('merges events and run boundaries newest-first', () => {
    const entries = journeyEntries(
      [
        ev({ ID: 1, OccurredAt: '2026-08-16T10:00:00Z', Kind: 'reconciler.bootstrap_escalated' }),
        ev({ ID: 2, OccurredAt: '2026-08-16T14:00:00Z', Kind: 'reconciler.auto_requeued' }),
      ],
      [run({ ID: 'r1', StartedAt: '2026-08-16T11:00:00Z', EndedAt: '2026-08-16T13:00:00Z' })],
    );

    expect(entries.map((e) => e.label)).toEqual([
      'auto requeued', // 14:00
      'run merged', // 13:00
      'run started', // 11:00
      'bootstrap escalated', // 10:00
    ]);
  });

  it('gives a run two entries so the escalate-then-requeue arc is legible', () => {
    const entries = journeyEntries(
      [],
      [run({ ID: 'r1', State: 'escalated', StartedAt: '2026-08-16T11:00:00Z', EndedAt: '2026-08-16T12:00:00Z' })],
    );
    expect(entries).toHaveLength(2);
    expect(entries[0].label).toBe('run escalated');
    expect(entries[0].tone).toBe('error');
    expect(entries[1].label).toBe('run started');
    expect(entries.every((e) => e.runID === 'r1')).toBe(true);
  });

  it('emits only a start for a run still in flight', () => {
    const entries = journeyEntries([], [run({ ID: 'r1', State: 'running', StartedAt: '2026-08-16T11:00:00Z' })]);
    expect(entries).toHaveLength(1);
    expect(entries[0].label).toBe('run started');
  });

  it('carries run context on the terminal entry', () => {
    const entries = journeyEntries(
      [],
      [
        run({
          ID: 'r1',
          State: 'escalated',
          CurrentStage: 'ci_watch',
          EscalationClass: 'infra',
          MRIID: 42,
          StartedAt: '2026-08-16T11:00:00Z',
          EndedAt: '2026-08-16T12:00:00Z',
        }),
      ],
    );
    expect(entries[0].detail).toBe('stage=ci_watch · class=infra · mr=!42');
  });

  it('handles empty and nullish inputs', () => {
    expect(journeyEntries([], [])).toEqual([]);
    expect(journeyEntries(null, null)).toEqual([]);
    expect(journeyEntries(undefined, undefined)).toEqual([]);
  });

  it('is deterministic when timestamps tie', () => {
    const at = '2026-08-16T12:00:00Z';
    const first = journeyEntries([ev({ ID: 1, OccurredAt: at }), ev({ ID: 2, OccurredAt: at })], []);
    const second = journeyEntries([ev({ ID: 2, OccurredAt: at }), ev({ ID: 1, OccurredAt: at })], []);
    expect(first.map((e) => e.key)).toEqual(second.map((e) => e.key));
  });

  it('survives an unparseable timestamp without dropping the entry', () => {
    const entries = journeyEntries([ev({ ID: 1, OccurredAt: 'not-a-date' })], []);
    expect(entries).toHaveLength(1);
    expect(entries[0].at).toBe(0);
  });
});
