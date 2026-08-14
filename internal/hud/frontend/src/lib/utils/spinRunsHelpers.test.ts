import { describe, expect, it } from 'vitest';
import {
  briefHeadline,
  competitiveGroups,
  elapsedMs,
  formatElapsed,
  hasLiveSpin,
  isLive,
  isStuck,
  liveCount,
  spinPhase,
  spinPhaseLabel,
  spinPhaseVariant,
  stuckCount,
  visibleRuns,
  SPIN_SLOW_AFTER_MS,
  SPIN_STUCK_AFTER_MS,
  type SpinRun,
} from './spinRunsHelpers.ts';

const T0 = Date.parse('2026-07-05T12:00:00Z');

function run(over: Partial<SpinRun> = {}): SpinRun {
  return {
    id: 'spin-1',
    brief: 'do a thing',
    frames: ['jacquard'],
    status: 'running',
    plan_ids: [],
    competitive: false,
    started_at: new Date(T0).toISOString(),
    ...over,
  };
}

describe('isLive / isTerminalStatus', () => {
  it('treats pending + running as live, terminal states as done', () => {
    expect(isLive(run({ status: 'pending' }))).toBe(true);
    expect(isLive(run({ status: 'running' }))).toBe(true);
    expect(isLive(run({ status: 'succeeded' }))).toBe(false);
    expect(isLive(run({ status: 'failed' }))).toBe(false);
    expect(isLive(run({ status: 'timeout' }))).toBe(false);
  });
});

describe('elapsedMs', () => {
  it('uses ended_at for terminal runs and now for live runs', () => {
    const live = run({ started_at: new Date(T0).toISOString() });
    expect(elapsedMs(live, T0 + 30_000)).toBe(30_000);

    const done = run({
      status: 'succeeded',
      started_at: new Date(T0).toISOString(),
      ended_at: new Date(T0 + 42_000).toISOString(),
    });
    // `now` is ignored once ended_at is set.
    expect(elapsedMs(done, T0 + 999_000)).toBe(42_000);
  });

  it('never goes negative and tolerates unparseable dates', () => {
    expect(elapsedMs(run({ started_at: 'nonsense' }), T0)).toBe(0);
    expect(elapsedMs(run(), T0 - 5_000)).toBe(0);
  });
});

describe('spinPhase — slow/stuck escalation', () => {
  it('keeps a fresh live run at its wire status', () => {
    expect(spinPhase(run({ status: 'pending' }), T0 + 10_000)).toBe('pending');
    expect(spinPhase(run({ status: 'running' }), T0 + 10_000)).toBe('running');
  });

  it('escalates a live run to slow past the slow threshold', () => {
    expect(spinPhase(run({ status: 'running' }), T0 + SPIN_SLOW_AFTER_MS)).toBe('slow');
    // still slow, not yet stuck
    expect(spinPhase(run({ status: 'running' }), T0 + SPIN_STUCK_AFTER_MS - 1)).toBe('slow');
  });

  it('escalates a live run to stuck past the stuck threshold', () => {
    expect(spinPhase(run({ status: 'running' }), T0 + SPIN_STUCK_AFTER_MS)).toBe('stuck');
    // a long-pending (never-scheduled) run is stuck too
    expect(spinPhase(run({ status: 'pending' }), T0 + SPIN_STUCK_AFTER_MS + 5_000)).toBe('stuck');
  });

  it('passes terminal statuses through unchanged regardless of age', () => {
    expect(spinPhase(run({ status: 'succeeded' }), T0 + 999_000)).toBe('succeeded');
    expect(spinPhase(run({ status: 'failed' }), T0 + 999_000)).toBe('failed');
    expect(spinPhase(run({ status: 'timeout' }), T0 + 999_000)).toBe('timeout');
  });

  it('buckets an unknown wire status to unknown', () => {
    expect(spinPhase(run({ status: 'weird' }), T0)).toBe('unknown');
  });
});

describe('isStuck', () => {
  it('only a live, aged run is stuck', () => {
    expect(isStuck(run({ status: 'running' }), T0 + SPIN_STUCK_AFTER_MS)).toBe(true);
    expect(isStuck(run({ status: 'running' }), T0 + 10_000)).toBe(false);
    // a terminal run past the threshold is NOT stuck
    expect(
      isStuck(
        run({ status: 'timeout', ended_at: new Date(T0 + SPIN_STUCK_AFTER_MS).toISOString() }),
        T0 + 999_000,
      ),
    ).toBe(false);
  });
});

describe('phase variant + label', () => {
  it('maps phases to badge colours', () => {
    expect(spinPhaseVariant('succeeded')).toBe('success');
    expect(spinPhaseVariant('failed')).toBe('error');
    expect(spinPhaseVariant('stuck')).toBe('error');
    expect(spinPhaseVariant('slow')).toBe('warning');
    expect(spinPhaseVariant('timeout')).toBe('warning');
    expect(spinPhaseVariant('running')).toBe('info');
    expect(spinPhaseVariant('pending')).toBe('info');
  });
  it('labels slow/stuck plainly', () => {
    expect(spinPhaseLabel('slow')).toBe('slow');
    expect(spinPhaseLabel('stuck')).toBe('stuck');
    expect(spinPhaseLabel('succeeded')).toBe('succeeded');
  });
});

describe('briefHeadline', () => {
  it('returns the first non-empty line', () => {
    expect(briefHeadline('\n\n  Harden the importer  \nmore detail')).toBe('Harden the importer');
    expect(briefHeadline('single line')).toBe('single line');
  });
});

describe('formatElapsed', () => {
  it('formats seconds, minutes, and hours', () => {
    expect(formatElapsed(45_000)).toBe('45s');
    expect(formatElapsed(125_000)).toBe('2m 05s');
    expect(formatElapsed(3_780_000)).toBe('1h 03m');
  });
});

describe('visibleRuns', () => {
  it('keeps live runs and recent terminals, drops old terminals, sorts live-first', () => {
    const now = T0 + 100_000;
    const runs: SpinRun[] = [
      run({ id: 'old-done', status: 'succeeded', started_at: new Date(T0 - 48 * 3600 * 1000).toISOString(), ended_at: new Date(T0 - 48 * 3600 * 1000).toISOString() }),
      run({ id: 'recent-done', status: 'succeeded', started_at: new Date(T0 - 1000).toISOString(), ended_at: new Date(T0).toISOString() }),
      run({ id: 'live-a', status: 'running', started_at: new Date(T0 - 5000).toISOString() }),
      run({ id: 'live-b', status: 'pending', started_at: new Date(T0 - 2000).toISOString() }),
    ];
    const vis = visibleRuns(runs, { now });
    const ids = vis.map((r) => r.id);
    expect(ids).not.toContain('old-done');
    // live first (most-recent-started among live), then recent terminal
    expect(ids).toEqual(['live-b', 'live-a', 'recent-done']);
  });

  it('honours the limit', () => {
    const runs = Array.from({ length: 20 }, (_, i) =>
      run({ id: `r${i}`, status: 'running', started_at: new Date(T0 + i * 1000).toISOString() }),
    );
    expect(visibleRuns(runs, { now: T0 + 100_000, limit: 5 })).toHaveLength(5);
  });
});

describe('live/stuck counts + hasLiveSpin', () => {
  const now = T0 + SPIN_STUCK_AFTER_MS + 1000;
  const runs: SpinRun[] = [
    run({ id: 'a', status: 'running', started_at: new Date(T0).toISOString() }), // stuck
    run({ id: 'b', status: 'pending', started_at: new Date(now - 1000).toISOString() }), // live, fresh
    run({ id: 'c', status: 'succeeded', ended_at: new Date(now).toISOString() }),
  ];
  it('counts live and stuck', () => {
    expect(hasLiveSpin(runs)).toBe(true);
    expect(liveCount(runs)).toBe(2);
    expect(stuckCount(runs, now)).toBe(1);
  });
  it('reports no live spin when all terminal', () => {
    expect(hasLiveSpin([run({ status: 'failed' })])).toBe(false);
    expect(liveCount([run({ status: 'failed' })])).toBe(0);
  });
});

describe('competitiveGroups', () => {
  it('groups only runs that authored 2+ drafts', () => {
    const runs: SpinRun[] = [
      run({ id: 's1', competitive: true, frames: ['jacquard', 'mule'], plan_ids: ['plan-a', 'plan-b'], status: 'succeeded' }),
      run({ id: 's2', competitive: false, frames: ['ring'], plan_ids: ['plan-c'], status: 'succeeded' }),
    ];
    const groups = competitiveGroups(runs);
    expect(groups.get('plan-a')?.spinId).toBe('s1');
    expect(groups.get('plan-a')?.planIds).toEqual(['plan-a', 'plan-b']);
    expect(groups.get('plan-b')?.frames).toEqual(['jacquard', 'mule']);
    // a solo draft is not grouped
    expect(groups.has('plan-c')).toBe(false);
  });

  it('newest run wins when a plan id appears in two runs', () => {
    const runs: SpinRun[] = [
      run({ id: 'older', plan_ids: ['dup', 'x'], started_at: new Date(T0).toISOString() }),
      run({ id: 'newer', plan_ids: ['dup', 'y'], started_at: new Date(T0 + 10_000).toISOString() }),
    ];
    expect(competitiveGroups(runs).get('dup')?.spinId).toBe('newer');
  });
});
