// Unit coverage for the shared formatters. Focused on relativeTime's
// two directions: the long-standing "N ago" past chain (which must not
// drift) and the forward branch added so unexpired deadlines — overseer
// suppression leases especially — stop reading as 'just now'.
//
// The tier-boundary block at the bottom exists because this function is now
// the HUD's only relative-time formatter: five hand-rolled copies (agent
// cards, servers, tasks, presence, spawn) were deleted in favor of it. Each
// of those topped out at hours, so a three-day-old heartbeat read "72h ago",
// and each rounded where this one floors. Those are the two behaviors the
// consolidation depends on, so they are pinned explicitly rather than left
// implied by the representative cases above.

import { afterEach, describe, expect, it, vi } from 'vitest';
import { relativeTime } from './format.ts';

const NOW = new Date('2026-07-24T12:00:00.000Z').getTime();

function at(offsetMs: number): string {
  return new Date(NOW + offsetMs).toISOString();
}

function freeze(): void {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
}

afterEach(() => {
  vi.useRealTimers();
});

describe('relativeTime — past', () => {
  it('returns the placeholder for empty or unparseable input', () => {
    expect(relativeTime(null)).toBe('---');
    expect(relativeTime(undefined)).toBe('---');
    expect(relativeTime('')).toBe('---');
    expect(relativeTime('not a date')).toBe('---');
  });

  it('renders seconds, minutes, hours, and days ago', () => {
    freeze();
    expect(relativeTime(at(0))).toBe('0s ago');
    expect(relativeTime(at(-3_000))).toBe('3s ago');
    expect(relativeTime(at(-5 * 60_000))).toBe('5m ago');
    expect(relativeTime(at(-2 * 3_600_000))).toBe('2h ago');
    expect(relativeTime(at(-3 * 86_400_000))).toBe('3d ago');
  });

  it('accepts Date and epoch-millis inputs', () => {
    freeze();
    expect(relativeTime(new Date(NOW - 90_000))).toBe('1m ago');
    expect(relativeTime(NOW - 90_000)).toBe('1m ago');
  });
});

describe('relativeTime — future', () => {
  it('reads forward instead of collapsing to "just now"', () => {
    freeze();
    expect(relativeTime(at(10 * 60_000))).toBe('in 10m');
    expect(relativeTime(at(45_000))).toBe('in 45s');
    expect(relativeTime(at(2 * 3_600_000))).toBe('in 2h');
    expect(relativeTime(at(3 * 86_400_000))).toBe('in 3d');
  });

  it('keeps "just now" inside the sub-second window, where it is still true', () => {
    freeze();
    expect(relativeTime(at(400))).toBe('just now');
  });

  it('renders a live suppression lease as remaining, not elapsed', () => {
    freeze();
    // Regression: an overseer lease expiring in 30m rendered "until just now"
    // inside a role="alert", reading as an already-lapsed suppression.
    expect(`expires ${relativeTime(at(30 * 60_000))}`).toBe('expires in 30m');
  });
});

describe('relativeTime — day tier', () => {
  const HOUR = 3_600_000;
  const DAY = 86_400_000;

  it('rolls a 3-day age into days rather than reporting 72 hours', () => {
    freeze();
    // The exact regression the formatter consolidation fixed: the deleted
    // per-panel formatters ended at `${Math.floor(diff / 3600)}h ago`, so an
    // agent last seen three days ago rendered "72h ago" on its card.
    expect(relativeTime(at(-72 * HOUR))).toBe('3d ago');
  });

  it('switches tiers exactly at 24 hours', () => {
    freeze();
    expect(relativeTime(at(-23 * HOUR))).toBe('23h ago');
    expect(relativeTime(at(-24 * HOUR + 1_000))).toBe('23h ago');
    expect(relativeTime(at(-24 * HOUR))).toBe('1d ago');
  });

  it('keeps counting in days past the first week', () => {
    freeze();
    // No upper tier: a long-dead presence row reads "30d ago", not "4w" and
    // not a clamped "7d+".
    expect(relativeTime(at(-10 * DAY))).toBe('10d ago');
    expect(relativeTime(at(-30 * DAY))).toBe('30d ago');
  });

  it('floors the day count instead of rounding it up', () => {
    freeze();
    // 1d 23h is still "1d ago" — rounding here would age a row a whole day
    // early, which is what the old Math.round copies did at every tier.
    expect(relativeTime(at(-(DAY + 23 * HOUR)))).toBe('1d ago');
  });
});

describe('relativeTime — sub-10s window', () => {
  it('counts single seconds rather than collapsing to "just now"', () => {
    freeze();
    // A row that updated 4s ago should say so: 'just now' is reserved for the
    // sub-second forward window, where it is literally true.
    expect(relativeTime(at(-1_000))).toBe('1s ago');
    expect(relativeTime(at(-4_000))).toBe('4s ago');
    expect(relativeTime(at(-9_000))).toBe('9s ago');
  });

  it('floors fractional seconds', () => {
    freeze();
    // 9.9s is '9s ago', not '10s ago' — the deleted copies used Math.round
    // and disagreed with each other by a second at every boundary.
    expect(relativeTime(at(-9_900))).toBe('9s ago');
    expect(relativeTime(at(-999))).toBe('0s ago');
  });

  it('switches to minutes exactly at 60 seconds', () => {
    freeze();
    expect(relativeTime(at(-59_000))).toBe('59s ago');
    expect(relativeTime(at(-60_000))).toBe('1m ago');
  });
});
