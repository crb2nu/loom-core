import { describe, expect, it } from 'vitest';
import { showRunVerdict, verdictCorrected } from './runVerdict.ts';

describe('run verdict presentation', () => {
  it('shows an ordinary verdict that diverges from the run state', () => {
    const verdict = { class: 'code' };
    expect(showRunVerdict(verdict, 'escalated')).toBe(true);
    expect(verdictCorrected(verdict)).toBe(false);
  });

  it('shows and marks a prior-class correction', () => {
    const verdict = { class: 'merged_after_escalation', prior_class: 'code' };
    expect(showRunVerdict(verdict, 'escalated')).toBe(true);
    expect(verdictCorrected(verdict)).toBe(true);
  });

  it('shows and marks an explicitly superseded correction', () => {
    const verdict = { class: 'merged_after_escalation', superseded: true };
    expect(showRunVerdict(verdict, 'escalated')).toBe(true);
    expect(verdictCorrected(verdict)).toBe(true);
  });

  it('hides an unsuperseded verdict that only echoes the run state', () => {
    const verdict = { class: 'implementing', superseded: false };
    expect(showRunVerdict(verdict, 'implementing')).toBe(false);
    expect(verdictCorrected(verdict)).toBe(false);
  });
});
