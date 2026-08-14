import { describe, expect, it } from 'vitest';
import { STATUS_VARIANTS } from './tokens.ts';

// STATUS_VARIANTS is the one status→tone map the whole HUD reads. A missing
// key is invisible: the lookup falls through to 'muted' and the badge renders
// grey, which reads as "nothing to see" rather than "not in the map". The
// mills run/backlog states below all shipped that way — every state a mill
// row can actually carry is pinned here so the ramp can't silently regress.

describe('STATUS_VARIANTS — mills run/backlog states', () => {
  it('gives every mills state its run-state tone, none falling through to grey', () => {
    expect(STATUS_VARIANTS.queued).toBe('muted'); // waiting, deliberately quiet
    expect(STATUS_VARIANTS.running).toBe('info');
    expect(STATUS_VARIANTS.merged).toBe('success');
    expect(STATUS_VARIANTS.done).toBe('success');
    expect(STATUS_VARIANTS.failed).toBe('error');
    expect(STATUS_VARIANTS.escalated).toBe('warning');
    expect(STATUS_VARIANTS.paused).toBe('warning');
  });

  it('keeps merged/done and escalated/failed distinguishable from each other', () => {
    expect(STATUS_VARIANTS.escalated).not.toBe(STATUS_VARIANTS.failed);
    expect(STATUS_VARIANTS.merged).not.toBe(STATUS_VARIANTS.escalated);
  });
});
