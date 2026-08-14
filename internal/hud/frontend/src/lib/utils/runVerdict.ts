import type { RunVerdictEvidence } from '../stores/mills.svelte.ts';

/** True when the verdict records a supersession of the run's prior belief. */
export function verdictCorrected(verdict: RunVerdictEvidence | null | undefined): boolean {
  return !!verdict && (!!verdict.prior_class || verdict.superseded === true);
}

/**
 * Verdicts are emitted for live runs too. Only show one when it adds
 * information beyond the immutable run state, or explicitly corrects it.
 */
export function showRunVerdict(
  verdict: RunVerdictEvidence | null | undefined,
  runState: string | null | undefined,
): boolean {
  return !!verdict?.class && (verdictCorrected(verdict) || verdict.class !== runState);
}
