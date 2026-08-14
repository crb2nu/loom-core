import { afterEach, describe, expect, it, vi } from 'vitest';
import { elapsedMs, fmtCost, fmtCostExact, fmtDuration, fmtPct, fmtRunTime, shortRunID } from './format.ts';

describe('Mills shared formatters', () => {
  it('formats costs by magnitude and preserves exact journal costs separately', () => {
    expect(fmtCost(undefined)).toBe('—');
    expect(fmtCost(Number.NaN)).toBe('—');
    expect(fmtCost(0)).toBe('$0');
    expect(fmtCost(0.004)).toBe('<$0.01');
    expect(fmtCost(1.234)).toBe('$1.23');
    expect(fmtCost(12.34)).toBe('$12.3');
    expect(fmtCost(123.4)).toBe('$123');
    expect(fmtCostExact(0.0042)).toBe('$0.0042');
  });

  it('formats ratios and millisecond durations with an em dash for missing values', () => {
    expect(fmtPct(null)).toBe('—');
    expect(fmtPct(0.1234)).toBe('12.3%');
    expect(fmtDuration(undefined)).toBe('—');
    expect(fmtDuration(42)).toBe('42ms');
    expect(fmtDuration(1_500)).toBe('1.5s');
    expect(fmtDuration(61_000)).toBe('1m 1s');
    expect(fmtDuration(3_660_000)).toBe('1h 1m');
  });

  it('uses common timestamp and ID conventions', () => {
    expect(fmtRunTime(null)).toBe('—');
    expect(fmtRunTime('not a date')).toBe('—');
    expect(fmtRunTime('2026-07-25T12:34:56Z')).not.toBe('—');
    expect(shortRunID(null)).toBe('—');
    expect(shortRunID('123456789')).toBe('12345678…');
  });

  it('derives elapsed milliseconds for completed and live runs', () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-07-25T12:00:05Z'));
    expect(elapsedMs('2026-07-25T12:00:00Z', '2026-07-25T12:00:01.5Z')).toBe(1_500);
    expect(elapsedMs('2026-07-25T12:00:00Z')).toBe(5_000);
    expect(elapsedMs('bad date', '2026-07-25T12:00:01Z')).toBeUndefined();
    vi.useRealTimers();
  });
});
