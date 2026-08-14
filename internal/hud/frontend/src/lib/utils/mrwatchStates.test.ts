import { describe, expect, it } from 'vitest';
import {
  MRWATCH_STATES,
  MRWATCH_STATE_VARIANTS,
  isHealthyMRWatchState,
  isLiveMRWatchState,
} from './mrwatchStates.ts';

describe('mrwatch state presentation contract', () => {
  it('maps every canonical state to a badge variant', () => {
    expect(Object.keys(MRWATCH_STATE_VARIANTS)).toEqual(MRWATCH_STATES);
  });

  it('treats only open non-ok records as unhealthy live work', () => {
    expect(isHealthyMRWatchState('ok')).toBe(true);
    expect(isHealthyMRWatchState('merged')).toBe(true);
    expect(isLiveMRWatchState('merged')).toBe(false);
    expect(isLiveMRWatchState('closed')).toBe(false);
    expect(isLiveMRWatchState('ci_running')).toBe(true);
  });
});
