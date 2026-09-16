import { describe, expect, it } from 'vitest';
import {
  sessionsToEnd,
  SNAPSHOT_ABSENT_GRACE_MS,
  type ReconcilableSession,
  type SnapshotSessionLite,
} from './sessionReconcile.ts';

// Regression coverage for the live-sessions ↔ fleet-snapshot reconciliation —
// the fix for the HUD "Live Sessions never match" bug. The spectator store
// removed entries only on a session.end SSE event, so any missed event left a
// zombie "active" row forever and the card drifted away from the
// fleet-snapshot count in its own header.

const NOW = 1_750_000_000_000;
const QUIET = NOW - SNAPSHOT_ABSENT_GRACE_MS - 1; // past grace
const FRESH = NOW - 1_000; // within grace

function live(id: string, lastSeen: number, endedAt?: number): ReconcilableSession {
  return { session_id: id, first_seen: lastSeen, last_activity: lastSeen, ended_at: endedAt };
}

function snap(id: string, status: string, endedAt = ''): SnapshotSessionLite {
  return { id, status, ended_at: endedAt };
}

describe('sessionReconcile: sessionsToEnd', () => {
  // Zombie: quiet entry the snapshot reports ended → end it.
  it('quiet entry ended in snapshot is reaped', () => {
    expect(
      sessionsToEnd([live('s1', QUIET)], [snap('s1', 'ended', '2026-06-10T12:00:00Z')], NOW),
    ).toEqual(['s1']);
  });

  // Zombie: quiet entry absent from the snapshot entirely → end it.
  it('quiet entry absent from snapshot is reaped', () => {
    expect(sessionsToEnd([live('s1', QUIET)], [snap('other', 'active')], NOW)).toEqual(['s1']);
  });

  // Snapshot-confirmed active stays, regardless of quiet time.
  it('active-in-snapshot entry is kept', () => {
    expect(sessionsToEnd([live('s1', QUIET)], [snap('s1', 'active')], NOW)).toEqual([]);
  });

  // Just-started session missing from a snapshot that predates it survives.
  it('fresh entry absent from snapshot survives the grace window', () => {
    expect(sessionsToEnd([live('s1', FRESH)], [snap('other', 'active')], NOW)).toEqual([]);
  });

  // Resumed session: lagging snapshot still says ended, but recent activity wins.
  it('recently-active entry survives a lagging ended-status snapshot', () => {
    expect(
      sessionsToEnd([live('s1', FRESH)], [snap('s1', 'ended', '2026-06-10T12:00:00Z')], NOW),
    ).toEqual([]);
  });

  // Already-ended entries are never re-reported.
  it('already-ended entry is not re-reported', () => {
    expect(sessionsToEnd([live('s1', QUIET, NOW - 5_000)], [snap('s1', 'ended')], NOW)).toEqual([]);
  });

  // Empty snapshot = indistinguishable from upstream fetch failure → no-op.
  it('empty snapshot never mass-ends', () => {
    expect(sessionsToEnd([live('s1', QUIET), live('s2', QUIET)], [], NOW)).toEqual([]);
  });

  // Mixed fleet: one zombie among live claude + codex sessions.
  it('mixed fleet reaps only the zombie', () => {
    expect(
      sessionsToEnd(
        [live('claude-s', QUIET), live('codex-s', QUIET), live('zombie', QUIET)],
        [snap('claude-s', 'active'), snap('codex-s', 'active'), snap('zombie', 'summarized')],
        NOW,
      ),
    ).toEqual(['zombie']);
  });
});
