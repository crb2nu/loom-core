import { describe, expect, it } from 'vitest';
import { andonState, freshnessLabel, isStorm, odometerDigits } from './andonHelpers.ts';

const BASE = { stale: false, paused: false, activeRuns: 0, escalated24h: 0, merged24h: 0 };

describe('andonState', () => {
  it('a dead feed outranks everything — the board never glows green on stale data', () => {
    const r = andonState({ ...BASE, stale: true, paused: true, activeRuns: 5, escalated24h: 9 });
    expect(r.state).toBe('stale');
  });

  it('paused outranks storm and weaving', () => {
    const r = andonState({ ...BASE, paused: true, activeRuns: 5, escalated24h: 9 });
    expect(r.state).toBe('paused');
  });

  it('storm outranks weaving', () => {
    const r = andonState({ ...BASE, activeRuns: 5, escalated24h: 4, merged24h: 2 });
    expect(r.state).toBe('storm');
    expect(r.caption).toContain('4 sparks');
  });

  it('weaving when shuttles are in flight, with a count in the caption', () => {
    expect(andonState({ ...BASE, activeRuns: 1 })).toMatchObject({
      state: 'weaving',
      caption: expect.stringContaining('1 shuttle in flight'),
    });
    expect(andonState({ ...BASE, activeRuns: 3 }).caption).toContain('3 shuttles');
  });

  it('idle when nothing is happening and the feed is healthy', () => {
    expect(andonState(BASE).state).toBe('idle');
  });
});

describe('isStorm', () => {
  it('needs at least 3 sparks', () => {
    expect(isStorm(2, 0)).toBe(false);
    expect(isStorm(3, 0)).toBe(true);
  });

  it('3 sparks against a healthy merge rate is not a storm', () => {
    expect(isStorm(3, 20)).toBe(false); // 3*2 < 20
    expect(isStorm(3, 6)).toBe(true); // boundary: sparks at half the bolt rate
    expect(isStorm(3, 7)).toBe(false);
  });
});

describe('odometerDigits', () => {
  it('left-pads to minDigits', () => {
    expect(odometerDigits(7)).toEqual([0, 0, 7]);
    expect(odometerDigits(42, 4)).toEqual([0, 0, 4, 2]);
  });

  it('grows past minDigits instead of truncating', () => {
    expect(odometerDigits(1234, 3)).toEqual([1, 2, 3, 4]);
  });

  it('never lies with a minus sign or NaN wheel', () => {
    expect(odometerDigits(undefined)).toEqual([0, 0, 0]);
    expect(odometerDigits(NaN)).toEqual([0, 0, 0]);
    expect(odometerDigits(-5)).toEqual([0, 0, 0]);
    expect(odometerDigits(2.9)).toEqual([0, 0, 2]);
  });
});

describe('freshnessLabel', () => {
  const now = new Date('2026-07-08T12:00:00Z');
  const ago = (ms: number) => new Date(now.getTime() - ms);

  it('handles missing and just-now timestamps', () => {
    expect(freshnessLabel(null, now)).toBe('no data yet');
    expect(freshnessLabel(ago(500), now)).toBe('updated just now');
    expect(freshnessLabel(ago(-5000), now)).toBe('updated just now'); // clock skew
  });

  it('buckets seconds, minutes, hours', () => {
    expect(freshnessLabel(ago(14_000), now)).toBe('updated 14s ago');
    expect(freshnessLabel(ago(200_000), now)).toBe('updated 3m 20s ago');
    expect(freshnessLabel(ago(2 * 3600_000 + 5 * 60_000), now)).toBe('updated 2h 5m ago');
  });
});
