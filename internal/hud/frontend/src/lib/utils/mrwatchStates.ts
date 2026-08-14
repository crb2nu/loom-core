// The wire taxonomy emitted by internal/hud/mrwatch. Keep this list in sync
// with src/testdata/mrwatch-states.json: Go verifies that fixture against
// mrwatch.AllStates(), making taxonomy drift fail CI.
import type { BadgeVariant } from './tokens.ts';

export const MRWATCH_STATES = [
  'ok',
  'awaiting_pipeline',
  'ci_running',
  'ci_failed_flaky',
  'ci_failed_deterministic',
  'conflict',
  'automerge_unarmed',
  'pipeline_skipped',
  'stale_branch',
  'draft_idle',
  'merged',
  'closed',
] as const;

export type MRWatchState = (typeof MRWATCH_STATES)[number];

export const MRWATCH_STATE_VARIANTS: Record<MRWatchState, BadgeVariant> = {
  ok: 'success',
  awaiting_pipeline: 'info',
  ci_running: 'info',
  ci_failed_flaky: 'warning',
  ci_failed_deterministic: 'error',
  conflict: 'error',
  automerge_unarmed: 'warning',
  pipeline_skipped: 'warning',
  stale_branch: 'warning',
  draft_idle: 'muted',
  merged: 'success',
  closed: 'muted',
};

export function isTerminalMRWatchState(state: string): boolean {
  return state === 'merged' || state === 'closed';
}

export function isLiveMRWatchState(state: string): boolean {
  return !isTerminalMRWatchState(state);
}

export function isHealthyMRWatchState(state: string): boolean {
  return state === 'ok' || state === 'merged';
}
