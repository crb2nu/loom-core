// Runnable fixture / regression test for the live-sessions ↔ fleet-snapshot
// reconciliation — the fix for the HUD "Live Sessions never match" bug. The
// spectator store removed entries only on a session.end SSE event, so any
// missed event left a zombie "active" row forever and the card drifted away
// from the fleet-snapshot count in its own header.
//
// The HUD frontend ships no test runner, so this is a self-check:
//   pnpm dlx tsx src/lib/utils/sessionReconcile.fixture.ts

import {
  sessionsToEnd,
  SNAPSHOT_ABSENT_GRACE_MS,
  type ReconcilableSession,
  type SnapshotSessionLite,
} from './sessionReconcile';

function expect(label: string, actual: unknown, want: unknown): boolean {
  const a = JSON.stringify(actual);
  const w = JSON.stringify(want);
  const ok = a === w;
  if (ok) console.log(`PASS ${label}: got=${a}`);
  else console.error(`FAIL ${label}: got=${a} want=${w}`);
  return ok;
}

const NOW = 1_750_000_000_000;
const QUIET = NOW - SNAPSHOT_ABSENT_GRACE_MS - 1; // past grace
const FRESH = NOW - 1_000; // within grace

function live(id: string, lastSeen: number, endedAt?: number): ReconcilableSession {
  return { session_id: id, first_seen: lastSeen, last_activity: lastSeen, ended_at: endedAt };
}

function snap(id: string, status: string, endedAt = ''): SnapshotSessionLite {
  return { id, status, ended_at: endedAt };
}

let allOk = true;

// Zombie: quiet entry the snapshot reports ended → end it.
allOk =
  expect(
    'quiet entry ended in snapshot is reaped',
    sessionsToEnd([live('s1', QUIET)], [snap('s1', 'ended', '2026-06-10T12:00:00Z')], NOW),
    ['s1'],
  ) && allOk;

// Zombie: quiet entry absent from the snapshot entirely → end it.
allOk =
  expect(
    'quiet entry absent from snapshot is reaped',
    sessionsToEnd([live('s1', QUIET)], [snap('other', 'active')], NOW),
    ['s1'],
  ) && allOk;

// Snapshot-confirmed active stays, regardless of quiet time.
allOk =
  expect(
    'active-in-snapshot entry is kept',
    sessionsToEnd([live('s1', QUIET)], [snap('s1', 'active')], NOW),
    [],
  ) && allOk;

// Just-started session missing from a snapshot that predates it survives.
allOk =
  expect(
    'fresh entry absent from snapshot survives the grace window',
    sessionsToEnd([live('s1', FRESH)], [snap('other', 'active')], NOW),
    [],
  ) && allOk;

// Resumed session: lagging snapshot still says ended, but recent activity wins.
allOk =
  expect(
    'recently-active entry survives a lagging ended-status snapshot',
    sessionsToEnd([live('s1', FRESH)], [snap('s1', 'ended', '2026-06-10T12:00:00Z')], NOW),
    [],
  ) && allOk;

// Already-ended entries are never re-reported.
allOk =
  expect(
    'already-ended entry is not re-reported',
    sessionsToEnd([live('s1', QUIET, NOW - 5_000)], [snap('s1', 'ended')], NOW),
    [],
  ) && allOk;

// Empty snapshot = indistinguishable from upstream fetch failure → no-op.
allOk =
  expect(
    'empty snapshot never mass-ends',
    sessionsToEnd([live('s1', QUIET), live('s2', QUIET)], [], NOW),
    [],
  ) && allOk;

// Mixed fleet: one zombie among live claude + codex sessions.
allOk =
  expect(
    'mixed fleet reaps only the zombie',
    sessionsToEnd(
      [live('claude-s', QUIET), live('codex-s', QUIET), live('zombie', QUIET)],
      [snap('claude-s', 'active'), snap('codex-s', 'active'), snap('zombie', 'summarized')],
      NOW,
    ),
    ['zombie'],
  ) && allOk;

if (!allOk) {
  // throw (not process.exit) so the fixture stays runtime-agnostic — the
  // browser-oriented tsconfig has no node globals, and an uncaught throw
  // exits nonzero under node/tsx just the same.
  throw new Error('sessionReconcile fixture FAILED');
}
console.log('sessionReconcile fixture OK');
