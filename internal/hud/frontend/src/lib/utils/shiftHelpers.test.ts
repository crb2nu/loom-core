import { describe, expect, it } from 'vitest';
import { formatChange, formatCoverage, formatScore } from './shiftHelpers.ts';

describe('shift presentation helpers', () => {
  it('formats diff counts', () => {
    expect(formatChange(1, 12, 3)).toBe('1 file +12/-3');
    expect(formatChange(4, 0, 8)).toBe('4 files +0/-8');
  });

  it('formats optional scores and served coverage', () => {
    expect(formatScore(0.875)).toBe('0.88');
    expect(formatScore(null)).toBe('—');
    expect(formatCoverage(0.6)).toBe('60.0%');
    expect(formatCoverage(undefined)).toBe('');
  });
});
