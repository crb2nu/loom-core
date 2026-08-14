import { describe, expect, it } from 'vitest';
import {
  DELAYED_AFTER_MS,
  clockLabel,
  delayAge,
  departureRows,
  flightID,
  nextStageSince,
  squeezeSlug,
  suppressionRows,
  type StageObservation,
} from './departureHelpers.ts';
import type { PipelineRun } from '../stores/mills.svelte.ts';

function run(over: Partial<PipelineRun>): PipelineRun {
  return {
    ID: 'run-x',
    BacklogID: 'psl-x',
    Template: 'implement',
    State: 'running',
    Attempts: 1,
    ...over,
  } as PipelineRun;
}

const T0 = 1_000_000;

describe('nextStageSince', () => {
  it('stamps first sight, keeps the stamp while the stage holds, re-stamps on advance', () => {
    const a = run({ ID: 'r1', CurrentStage: 'implement' });
    const first = nextStageSince(new Map(), [a], T0);
    expect(first.get('r1')).toEqual({ stage: 'implement', since: T0 });

    const held = nextStageSince(first, [a], T0 + 60_000);
    expect(held.get('r1')!.since).toBe(T0); // still implementing — clock keeps running

    const advanced = nextStageSince(held, [run({ ID: 'r1', CurrentStage: 'tests' })], T0 + 90_000);
    expect(advanced.get('r1')).toEqual({ stage: 'tests', since: T0 + 90_000 });
  });

  it('drops runs that vanished from the active set', () => {
    const prev = new Map<string, StageObservation>([['gone', { stage: 'mr', since: T0 }]]);
    expect(nextStageSince(prev, [], T0 + 1).size).toBe(0);
  });
});

describe('delayAge', () => {
  it('reads minutes under the hour and h+m above it, never negative', () => {
    expect(delayAge(0)).toBe('0m');
    expect(delayAge(12 * 60_000 + 59_000)).toBe('12m'); // floors, never rounds up
    expect(delayAge(59 * 60_000)).toBe('59m');
    expect(delayAge(60 * 60_000)).toBe('1h 0m');
    expect(delayAge(-5000)).toBe('0m');
  });
});

describe('departureRows', () => {
  const obs = (id: string, stage: string, since: number) =>
    new Map<string, StageObservation>([[id, { stage, since }]]);

  it('an active run is EN ROUTE until observed past the fuse, then DELAYED', () => {
    const a = run({ ID: 'r1', CurrentStage: 'implement' });
    const fresh = departureRows([a], [], obs('r1', 'implement', T0), T0 + DELAYED_AFTER_MS);
    expect(fresh[0]).toMatchObject({ status: 'en route', tone: 'hot', via: 'laying weft' });

    const late = departureRows([a], [], obs('r1', 'implement', T0), T0 + DELAYED_AFTER_MS + 1);
    expect(late[0]).toMatchObject({ status: 'delayed', tone: 'wr' });
  });

  it('a delayed row carries the observed age; other statuses carry no note', () => {
    const a = run({ ID: 'r1', CurrentStage: 'implement' });
    const fresh = departureRows([a], [], obs('r1', 'implement', T0), T0 + DELAYED_AFTER_MS);
    expect(fresh[0].note).toBeUndefined();

    const late = departureRows([a], [], obs('r1', 'implement', T0), T0 + 20 * 60_000);
    expect(late[0].note).toBe('20m');

    const veryLate = departureRows([a], [], obs('r1', 'implement', T0), T0 + 185 * 60_000);
    expect(veryLate[0].note).toBe('3h 5m');
  });

  it('ci_watch gets the longer fuse', () => {
    const a = run({ ID: 'r1', CurrentStage: 'ci_watch' });
    const at = (ms: number) => departureRows([a], [], obs('r1', 'ci_watch', T0), T0 + ms)[0].status;
    expect(at(DELAYED_AFTER_MS + 1)).toBe('en route'); // would be late for any other stage
    expect(at(25 * 60_000 + 1)).toBe('delayed');
  });

  it('a stale observation from a previous stage never marks the new stage delayed', () => {
    const a = run({ ID: 'r1', CurrentStage: 'tests' });
    const rows = departureRows([a], [], obs('r1', 'implement', 0), T0 + 10 * DELAYED_AFTER_MS);
    expect(rows[0].status).toBe('en route');
  });

  it('history maps to arrived / diverted / held and fills after active rows', () => {
    const rows = departureRows(
      [run({ ID: 'a1', CurrentStage: 'mr' })],
      [
        run({ ID: 'h1', State: 'merged' }),
        run({ ID: 'h2', State: 'escalated' }),
        run({ ID: 'h3', State: 'paused' }),
        run({ ID: 'h4', State: 'failed' }), // not a board state — skipped
      ],
      new Map(),
      T0,
    );
    expect(rows.map((r) => r.status)).toEqual(['en route', 'arrived', 'diverted', 'held']);
    expect(rows[1].tone).toBe('ok');
    expect(rows[2].via).toContain('broken pick');
  });

  it('bounds the board and reports empty as no rows', () => {
    const many = Array.from({ length: 10 }, (_, i) => run({ ID: `a${i}`, CurrentStage: 'mr' }));
    expect(departureRows(many, [], new Map(), T0).length).toBe(7);
    expect(departureRows(many, [], new Map(), T0, { maxRows: 3 }).length).toBe(3);
    expect(departureRows([], [], new Map(), T0)).toEqual([]);
  });

  it('stamps the board clock: StartedAt for departures, EndedAt for arrivals', () => {
    const started = '2026-08-05T14:07:00';
    const ended = '2026-08-05T15:42:00';
    const rows = departureRows(
      [run({ ID: 'a1', CurrentStage: 'mr', StartedAt: started })],
      [run({ ID: 'h1', State: 'merged', StartedAt: started, EndedAt: ended })],
      new Map(),
      T0,
    );
    expect(rows[0].when).toBe('14:07');
    expect(rows[1].when).toBe('15:42');
  });

  it('leaves the clock blank when the feed carries no usable stamp', () => {
    const rows = departureRows(
      [run({ ID: 'a1', CurrentStage: 'mr' })],
      [run({ ID: 'h1', State: 'merged', EndedAt: 'not-a-date' })],
      new Map(),
      T0,
    );
    expect(rows[0].when).toBeUndefined();
    expect(rows[1].when).toBeUndefined();
  });
});

describe('flightID', () => {
  it('keeps the distinctive tail, not the shared transport prefix', () => {
    const id = 'PIPE-psl-plan-council-classify-external-dependency-a-2-01';
    expect(flightID(id)).toBe('…ency-a-2-01');
    expect(flightID(id).length).toBe(12);
  });

  it('leaves short ids alone', () => {
    expect(flightID('PIPE-r1')).toBe('r1');
    expect(flightID('run-42')).toBe('run-42');
  });

  it('two sibling slices stay distinguishable', () => {
    const a = flightID('PIPE-psl-plan-council-some-long-item-1-01');
    const b = flightID('PIPE-psl-plan-council-some-long-item-2-01');
    expect(a).not.toBe(b);
  });
});

describe('squeezeSlug', () => {
  it('keeps both ends of a long slug', () => {
    const slug = 'psl-plan-council-classify-external-dependency-workspace-signals-fail-closed-a-2';
    const out = squeezeSlug(slug);
    expect(out.length).toBeLessThanOrEqual(46);
    expect(out.startsWith('psl-plan-council')).toBe(true);
    expect(out.endsWith('fail-closed-a-2')).toBe(true);
    expect(out).toContain('…');
  });

  it('returns short slugs untouched', () => {
    expect(squeezeSlug('bl-short-item')).toBe('bl-short-item');
  });
});

describe('suppressionRows', () => {
  const item = (title: string, over: Record<string, unknown> = {}) => ({
    occurred_at: '2026-08-05T16:20:00',
    proposal_title: title,
    merged_title: 'shipped thing',
    merged_ref: '!42',
    ...over,
  });

  it('maps a suppression to a dimmed council row with the board clock', () => {
    const rows = suppressionRows([item('restated proposal')] as never);
    expect(rows[0]).toMatchObject({
      flight: 'council',
      destination: 'restated proposal',
      status: 'suppressed',
      tone: 'dm',
      when: '16:20',
    });
    expect(rows[0].via).toContain('already woven');
    expect(rows[0].via).toContain('shipped thing');
  });

  it('caps rows, skips titleless items, and survives null input', () => {
    const many = [item('a'), item('b'), item('c'), item('d'), item('', { proposal_title: '' })];
    expect(suppressionRows(many as never, 2).length).toBe(2);
    expect(suppressionRows(null as never)).toEqual([]);
    expect(suppressionRows([item('', { proposal_title: '' })] as never)).toEqual([]);
  });
});

describe('clockLabel', () => {
  it('formats HH:MM with leading zeros and rejects junk', () => {
    expect(clockLabel('2026-08-05T09:05:00')).toBe('09:05');
    expect(clockLabel(undefined)).toBeUndefined();
    expect(clockLabel('')).toBeUndefined();
    expect(clockLabel('yesterday-ish')).toBeUndefined();
  });
});
