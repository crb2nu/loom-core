// Presentation-only helpers for server-composed shift and bolt cards.
// Classification, windowing, narrative, and markdown belong to the operator.

export function formatChange(files: number, added: number, removed: number): string {
  return `${files} file${files === 1 ? '' : 's'} +${added}/-${removed}`;
}

export function formatScore(score: number | null | undefined): string {
  return score == null ? '—' : score.toFixed(2);
}

export function formatCoverage(value: number | null | undefined): string {
  return value == null ? '' : `${(value * 100).toFixed(1)}%`;
}
