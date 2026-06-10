// sessionReconcile — decides which live-session entries the spectator store
// must mark ended, given the canonical session list from a fleet snapshot.
//
// The liveSessions store is event-driven: entries appear on session.start /
// tool.call.* SSE events and are removed only when a session.end event
// arrives. Any missed end event (page loaded mid-flight, SSE reconnect gap,
// daemon restart) leaves a zombie "active" row forever, so the Live Sessions
// card drifts away from the fleet snapshot the header count comes from — the
// "17 active vs 19 rows" inconsistency, asymmetric per agent platform because
// Claude's per-conversation agent ids mint many short sessions (more end
// events to miss) while Codex's don't.
//
// The fleet snapshot already flows into the store (mount seed + hud.fleet
// SSE); this makes that merge two-way: entries the snapshot reports ended (or
// omits entirely) are ended once they have been quiet past a grace window.
// The grace covers sessions started or resumed so recently that the snapshot
// predates them, and keeps anything still emitting activity alive.

export interface SnapshotSessionLite {
  id: string;
  status: string;
  ended_at: string;
}

export interface ReconcilableSession {
  session_id: string;
  /** Wall-clock ms when the store first saw this session. */
  first_seen: number;
  /** Wall-clock ms of most recent activity. */
  last_activity: number;
  /** Already marked ended — never re-reported. */
  ended_at?: number;
}

/** Quiet time an absent-from-snapshot session must accrue before it is ended. */
export const SNAPSHOT_ABSENT_GRACE_MS = 60_000;

export function sessionsToEnd(
  current: Iterable<ReconcilableSession>,
  snapshot: SnapshotSessionLite[],
  now: number,
  graceMs: number = SNAPSHOT_ABSENT_GRACE_MS,
): string[] {
  // An empty canonical list is indistinguishable from an upstream session
  // fetch failure (the monitor carries over partial data); never mass-end on
  // it. A truly all-ended fleet still lists the ended sessions with their
  // status, which the present-branch below handles.
  if (snapshot.length === 0) return [];

  const byId = new Map<string, SnapshotSessionLite>();
  for (const s of snapshot) {
    if (s.id) byId.set(s.id, s);
  }

  const out: string[] = [];
  for (const session of current) {
    if (session.ended_at !== undefined) continue;
    // The quiet-grace applies to BOTH branches: snapshots lag live events by
    // up to a monitor refresh, so a session that just (re)started or is still
    // emitting activity must survive a snapshot that predates it. Zombies are
    // quiet by definition, so they still end on the first snapshot once the
    // grace has accrued.
    const lastSeen = Math.max(session.first_seen, session.last_activity);
    if (now - lastSeen <= graceMs) continue;
    const snap = byId.get(session.session_id);
    if (!snap || snap.status !== 'active' || snap.ended_at) out.push(session.session_id);
  }
  return out;
}
