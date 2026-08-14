package fleetview

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/visibility/contracts/presence"
)

func mustTime(t *testing.T, s string) string {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return s
}

func TestJoin_PresenceWithoutSession_HasSessionFalse(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", AgentType: "claude-code", LastHeartbeat: mustTime(t, "2026-04-21T11:59:30Z")},
	}
	rows := Join(presences, nil, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	got := rows[0]
	if got.HasSession {
		t.Fatalf("presence without session must not report HasSession=true; got %+v", got)
	}
	if got.Source != "presence" {
		t.Fatalf("want source=presence, got %q", got.Source)
	}
	if !got.HasPresence {
		t.Fatalf("want HasPresence=true")
	}
}

func TestJoin_StaleHasSessionFlagIsCleared(t *testing.T) {
	// Regression: a previous Join (or an upstream writer) may have populated
	// HasSession=true. The new join must reset it if no active session
	// matches now.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:          "claude-code-ghost",
			Status:           "active",
			HasSession:       true, // stale
			SessionID:        "sess-old",
			SessionStatus:    "active",
			SessionStartedAt: mustTime(t, "2026-04-21T10:00:00Z"),
			LastHeartbeat:    mustTime(t, "2026-04-21T11:59:30Z"),
		},
	}
	// No sessions at all — so HasSession must become false.
	rows := Join(presences, nil, now)
	got := rows[0]
	if got.HasSession {
		t.Fatalf("stale HasSession must be reset; got %+v", got)
	}
	if got.SessionStatus != "" {
		t.Fatalf("stale SessionStatus must be cleared; got %q", got.SessionStatus)
	}
	if got.SessionStartedAt != "" {
		t.Fatalf("stale SessionStartedAt must be cleared; got %q", got.SessionStartedAt)
	}
}

func TestJoin_EndedSessionDoesNotSatisfyHasSession(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", SessionID: "sess-ended", LastHeartbeat: mustTime(t, "2026-04-21T11:59:30Z")},
	}
	sessions := []bridge.SessionInfo{
		{ID: "sess-ended", AgentID: "claude-code-1", Status: "ended", StartedAt: mustTime(t, "2026-04-21T10:00:00Z")},
	}
	rows := Join(presences, sessions, now)
	if rows[0].HasSession {
		t.Fatalf("ended session must not mark HasSession=true; got %+v", rows[0])
	}
}

func TestJoin_ActiveSessionMatchedBySessionID(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", SessionID: "sess-live", LastHeartbeat: mustTime(t, "2026-04-21T11:59:30Z")},
	}
	sessions := []bridge.SessionInfo{
		{ID: "sess-live", AgentID: "claude-code-1", Namespace: "loom-core/main", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	got := rows[0]
	if !got.HasSession {
		t.Fatalf("active session match must set HasSession=true")
	}
	if got.SessionID != "sess-live" || got.SessionStatus != "active" {
		t.Fatalf("session fields not propagated: %+v", got)
	}
	if got.Source != "presence+session" {
		t.Fatalf("want source=presence+session, got %q", got.Source)
	}
}

func TestJoin_ActiveSessionMatchedByAgentIDWhenSessionIDMissing(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", LastHeartbeat: mustTime(t, "2026-04-21T11:59:30Z")},
	}
	sessions := []bridge.SessionInfo{
		{ID: "sess-live", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	if !rows[0].HasSession || rows[0].SessionID != "sess-live" {
		t.Fatalf("agent_id fallback match failed: %+v", rows[0])
	}
}

func TestJoin_SessionWithoutPresenceYieldsSyntheticRow(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	sessions := []bridge.SessionInfo{
		{ID: "sess-a", AgentID: "claude-code-ghost", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:00:00Z")},
	}
	rows := Join(nil, sessions, now)
	if len(rows) != 1 {
		t.Fatalf("want 1 synthetic row, got %d", len(rows))
	}
	got := rows[0]
	if got.Source != "session" {
		t.Fatalf("want source=session, got %q", got.Source)
	}
	if !got.HasSession || got.HasPresence {
		t.Fatalf("synthetic row: HasSession=%v HasPresence=%v", got.HasSession, got.HasPresence)
	}
	if got.TelemetryStatus != "session_only" {
		t.Fatalf("want telemetry_status=session_only, got %q", got.TelemetryStatus)
	}
}

func TestJoin_MultipleAgents_CounterMatchesBadges(t *testing.T) {
	// End-to-end invariant: the number of rows with HasSession=true must
	// equal the number of active sessions that joined. This is the UI
	// contract — badge count must equal counter count.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active"},
		{AgentID: "claude-code-2", Status: "active"},
		{AgentID: "claude-code-3", Status: "active"},
		{AgentID: "claude-code-4", Status: "active"},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:00:00Z")},
		{ID: "s2", AgentID: "claude-code-2", Status: "ended", StartedAt: mustTime(t, "2026-04-21T10:00:00Z")},
		{ID: "s3", AgentID: "claude-code-3", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	withSession := 0
	for _, r := range rows {
		if r.HasSession {
			withSession++
		}
	}
	if withSession != 2 {
		t.Fatalf("want 2 rows with HasSession=true, got %d (rows=%+v)", withSession, rows)
	}
	if len(rows) != 4 {
		t.Fatalf("want 4 rows total, got %d", len(rows))
	}
}

func TestJoin_PrefersMostRecentActiveSessionPerAgent(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active"},
	}
	sessions := []bridge.SessionInfo{
		{ID: "older", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T09:00:00Z")},
		{ID: "newer", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:00:00Z")},
	}
	rows := Join(presences, sessions, now)
	if rows[0].SessionID != "newer" {
		t.Fatalf("want newer session, got %q", rows[0].SessionID)
	}
}

func TestJoin_SkipsEmptyAgentIDs(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "", Status: "active"},
		{AgentID: "claude-code-1", Status: "active"},
	}
	rows := Join(presences, nil, now)
	if len(rows) != 1 {
		t.Fatalf("empty agent_id must be dropped; got %d rows", len(rows))
	}
	if rows[0].AgentID != "claude-code-1" {
		t.Fatalf("kept wrong row: %+v", rows[0])
	}
}

func TestJoin_ComputesHeartbeatAgeAndTelemetryStatus(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	// heartbeat 1200s ago -> downgraded to offline (way past
	// LivePresenceStaleAfter; also an orphan via the heartbeat fallback).
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", LastHeartbeat: mustTime(t, "2026-04-21T11:40:00Z")},
	}
	rows := Join(presences, nil, now)
	got := rows[0]
	if got.HeartbeatAgeSeconds != 1200 {
		t.Fatalf("heartbeat age wrong: %d", got.HeartbeatAgeSeconds)
	}
	if got.Status != "offline" {
		t.Fatalf("want status=offline after downgrade, got %q", got.Status)
	}
	if got.TelemetryStatus != "offline" {
		t.Fatalf("want telemetry_status=offline, got %q", got.TelemetryStatus)
	}
}

// --- Live-presence staleness downgrade -----------------------------------

// TestJoin_DowngradesStalePresence pins the single staleness threshold
// (LivePresenceStaleAfter = 90s) at the fleetview layer, where every
// consumer (fleet monitor snapshot, web HUD, mobile API) inherits it.
// Status and TelemetryStatus must agree within each row because the
// downgrade runs before TelemetryStatus is derived.
func TestJoin_DowngradesStalePresence(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	hb := func(age time.Duration) string {
		return now.Add(-age).Format(time.RFC3339)
	}
	reg := hb(3 * time.Minute) // registered 3min ago, with a session: never orphan

	presences := []presence.PresenceInfo{
		{AgentID: "agent-fresh", Status: "active", SessionID: "s-fresh", RegisteredAt: reg, LastHeartbeat: hb(15 * time.Second)},
		{AgentID: "agent-just-below", Status: "active", SessionID: "s-just-below", RegisteredAt: reg, LastHeartbeat: hb(89 * time.Second)},
		{AgentID: "agent-at-threshold", Status: "active", SessionID: "s-at-threshold", RegisteredAt: reg, LastHeartbeat: hb(LivePresenceStaleAfter)},
		{AgentID: "agent-stale-idle", Status: "idle", SessionID: "s-stale-idle", RegisteredAt: reg, LastHeartbeat: hb(120 * time.Second)},
		// Orphan with a FRESH heartbeat (keepalive without a session,
		// registered 5min ago): downgraded via IsOrphan, not heartbeat age.
		{AgentID: "agent-orphan", Status: "active", RegisteredAt: hb(5 * time.Minute), LastHeartbeat: hb(25 * time.Second)},
		// Registry already says offline: stays offline, no stale relabel.
		{AgentID: "agent-registry-offline", Status: "offline", RegisteredAt: reg, LastHeartbeat: hb(20 * time.Minute)},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s-fresh", AgentID: "agent-fresh", Status: "active", StartedAt: reg},
		{ID: "s-just-below", AgentID: "agent-just-below", Status: "active", StartedAt: reg},
		{ID: "s-at-threshold", AgentID: "agent-at-threshold", Status: "active", StartedAt: reg},
		{ID: "s-stale-idle", AgentID: "agent-stale-idle", Status: "active", StartedAt: reg},
		// Synthetic session-only row: no presence, no heartbeat clock.
		{ID: "s-only", AgentID: "agent-session-only", Status: "active", StartedAt: reg},
	}

	rows := Join(presences, sessions, now)
	byAgent := make(map[string]presence.PresenceInfo, len(rows))
	for _, r := range rows {
		byAgent[r.AgentID] = r
	}

	cases := []struct {
		agentID       string
		wantStatus    string
		wantTelemetry string
		reason        string
	}{
		{"agent-fresh", "active", "live", "fresh heartbeat (15s)"},
		{"agent-just-below", "active", "live", "89s heartbeat, below the 90s threshold"},
		{"agent-at-threshold", "offline", "offline", "exactly 90s heartbeat, >= threshold downgrades"},
		{"agent-stale-idle", "offline", "offline", "stale idle rows downgrade like active ones"},
		{"agent-orphan", "offline", "offline", "orphans are not live work even with fresh heartbeats"},
		{"agent-registry-offline", "offline", "offline", "registry-declared offline stays offline"},
		{"agent-session-only", "active", "session_only", "synthetic session rows have no heartbeat clock"},
	}
	for _, tc := range cases {
		got, ok := byAgent[tc.agentID]
		if !ok {
			t.Fatalf("missing agent %s in joined rows", tc.agentID)
		}
		if got.Status != tc.wantStatus {
			t.Errorf("agent %s: want status=%q (%s), got %q (heartbeat_age=%ds, is_orphan=%v)",
				tc.agentID, tc.wantStatus, tc.reason, got.Status, got.HeartbeatAgeSeconds, got.IsOrphan)
		}
		if got.TelemetryStatus != tc.wantTelemetry {
			t.Errorf("agent %s: want telemetry_status=%q (%s), got %q",
				tc.agentID, tc.wantTelemetry, tc.reason, got.TelemetryStatus)
		}
	}
}

// TestJoin_DowngradeIsIdempotent: re-joining a snapshot that already passed
// through Join (exactly what the mobile /agents handler does with the fleet
// monitor's snapshot) must not change any row's Status or TelemetryStatus.
// This is the cross-surface agreement guarantee: web HUD (snapshot) and
// mobile API (re-join of the snapshot) render the same state.
func TestJoin_DowngradeIsIdempotent(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	hb := func(age time.Duration) string {
		return now.Add(-age).Format(time.RFC3339)
	}
	reg := hb(3 * time.Minute)
	presences := []presence.PresenceInfo{
		{AgentID: "agent-live", Status: "active", SessionID: "s1", RegisteredAt: reg, LastHeartbeat: hb(10 * time.Second)},
		{AgentID: "agent-stale", Status: "active", SessionID: "s2", RegisteredAt: reg, LastHeartbeat: hb(2 * time.Minute)},
		{AgentID: "agent-orphan", Status: "active", RegisteredAt: hb(5 * time.Minute), LastHeartbeat: hb(20 * time.Second)},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "agent-live", Status: "active", StartedAt: reg},
		{ID: "s2", AgentID: "agent-stale", Status: "active", StartedAt: reg},
	}

	first := Join(presences, sessions, now)
	second := Join(first, sessions, now)

	firstByAgent := make(map[string]presence.PresenceInfo, len(first))
	for _, r := range first {
		firstByAgent[r.AgentID] = r
	}
	for _, got := range second {
		want, ok := firstByAgent[got.AgentID]
		if !ok {
			t.Fatalf("agent %s appeared only in second join", got.AgentID)
		}
		if got.Status != want.Status {
			t.Errorf("agent %s: status drifted across re-join: %q -> %q", got.AgentID, want.Status, got.Status)
		}
		if got.TelemetryStatus != want.TelemetryStatus {
			t.Errorf("agent %s: telemetry_status drifted across re-join: %q -> %q", got.AgentID, want.TelemetryStatus, got.TelemetryStatus)
		}
	}
}

// TestTelemetryStatus_StaleBranchUsesLiveThreshold pins the defense-in-depth
// "stale" branch (for rows that bypass the downgrade because their Status is
// neither active, idle, nor offline) to the same 90s horizon. No surface may
// label an aged heartbeat "live" past LivePresenceStaleAfter.
func TestTelemetryStatus_StaleBranchUsesLiveThreshold(t *testing.T) {
	staleSecs := int(LivePresenceStaleAfter.Seconds())
	row := presence.PresenceInfo{
		AgentID:             "agent-busy",
		Status:              "busy", // not subject to the active/idle downgrade
		HasPresence:         true,
		HeartbeatAgeSeconds: staleSecs,
	}
	if got := TelemetryStatus(row); got != "stale" {
		t.Fatalf("want stale at %ds heartbeat age, got %q", staleSecs, got)
	}
	row.HeartbeatAgeSeconds = staleSecs - 1
	if got := TelemetryStatus(row); got != "unknown" {
		t.Fatalf("want unknown below threshold for status=busy, got %q", got)
	}
}

func TestJoin_DoesNotMutateInput(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", HasSession: true, SessionID: "old"},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:00:00Z")},
	}
	_ = Join(presences, sessions, now)
	// Input should be unchanged — Join returns new copies.
	if presences[0].HasSession != true {
		t.Fatalf("Join mutated input HasSession")
	}
	if presences[0].SessionID != "old" {
		t.Fatalf("Join mutated input SessionID")
	}
}

// --- Orphan detection ---------------------------------------------------

func TestOrphan_YoungPresenceWithoutSessionIsNotOrphan(t *testing.T) {
	// Agent registered 30s ago, no session yet — still within the grace
	// window for session bootstrap. Should NOT be flagged.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", RegisteredAt: mustTime(t, "2026-04-21T11:59:30Z"), LastHeartbeat: mustTime(t, "2026-04-21T11:59:55Z")},
	}
	rows := Join(presences, nil, now)
	if rows[0].IsOrphan {
		t.Fatalf("young presence must not be flagged orphan: %+v", rows[0])
	}
	if rows[0].OrphanAgeSeconds != 0 {
		t.Fatalf("orphan age must be zero when not orphan, got %d", rows[0].OrphanAgeSeconds)
	}
}

func TestOrphan_StalePresenceWithoutSessionIsOrphan(t *testing.T) {
	// Agent registered 5min ago, still heartbeating, but never obtained a
	// session. This is the screenshot's 9-orphans case.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-ghost", Status: "active", RegisteredAt: mustTime(t, "2026-04-21T11:55:00Z"), LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z")},
	}
	rows := Join(presences, nil, now)
	if !rows[0].IsOrphan {
		t.Fatalf("stale presence without session must be orphan: %+v", rows[0])
	}
	if rows[0].OrphanAgeSeconds < 290 || rows[0].OrphanAgeSeconds > 310 {
		t.Fatalf("orphan age should be ~300s, got %d", rows[0].OrphanAgeSeconds)
	}
}

func TestOrphan_PresenceWithActiveSessionIsNotOrphan(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", SessionID: "s1", RegisteredAt: mustTime(t, "2026-04-21T11:00:00Z")},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	if rows[0].IsOrphan {
		t.Fatalf("presence with matched session must not be orphan: %+v", rows[0])
	}
}

func TestOrphan_SessionOnlyRowIsNeverOrphan(t *testing.T) {
	// Synthetic session-only row: Source="session", HasPresence=false.
	// By definition no orphan because there's no dangling presence.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(nil, sessions, now)
	if rows[0].IsOrphan {
		t.Fatalf("session-only row must not be orphan: %+v", rows[0])
	}
}

func TestOrphan_EndedSessionProducesOrphan(t *testing.T) {
	// Presence is heartbeating but its session has ended. After the grace
	// window, the row should be flagged — this catches sessions that
	// terminate silently without deregistering presence.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-ghost", Status: "active", SessionID: "s-ended", RegisteredAt: mustTime(t, "2026-04-21T11:00:00Z"), LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z")},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s-ended", AgentID: "claude-code-ghost", Status: "ended", StartedAt: mustTime(t, "2026-04-21T10:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	if !rows[0].IsOrphan {
		t.Fatalf("presence with only an ended session must be orphan: %+v", rows[0])
	}
}

func TestOrphan_StaleFlagIsReset(t *testing.T) {
	// An incoming presence row that somehow carries IsOrphan=true from
	// upstream must have that reset before the new computation runs,
	// otherwise Join would compound stale state.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:          "claude-code-1",
			Status:           "active",
			IsOrphan:         true,
			OrphanAgeSeconds: 9999,
			SessionID:        "s1",
			RegisteredAt:     mustTime(t, "2026-04-21T11:00:00Z"),
		},
	}
	sessions := []bridge.SessionInfo{
		{ID: "s1", AgentID: "claude-code-1", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:30:00Z")},
	}
	rows := Join(presences, sessions, now)
	if rows[0].IsOrphan {
		t.Fatalf("stale IsOrphan must be reset when session joins: %+v", rows[0])
	}
	if rows[0].OrphanAgeSeconds != 0 {
		t.Fatalf("stale OrphanAgeSeconds must be reset, got %d", rows[0].OrphanAgeSeconds)
	}
}

func TestOrphan_FallsBackToLastHeartbeatWhenRegisteredAtMissing(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{AgentID: "claude-code-1", Status: "active", LastHeartbeat: mustTime(t, "2026-04-21T11:55:00Z")},
	}
	rows := Join(presences, nil, now)
	if !rows[0].IsOrphan {
		t.Fatalf("should fall back to LastHeartbeat when RegisteredAt missing: %+v", rows[0])
	}
}

func TestInferAgentType(t *testing.T) {
	cases := []struct{ in, want string }{
		{"claude-code-12345", "claude-code"},
		{"codex-abc", "codex"},
		{"gemini-9", "gemini-cli"},
		{"zed-session", "codex"},
		{"proxy-x", "codex"},
		{"copilot-x", "copilot"},
		{"kilocode-k", "kilocode"},
		{"custom-agent", "custom"},
		{"", "unknown"},
		{"weirdname", "weirdname"},
	}
	for _, c := range cases {
		if got := InferAgentType(c.in); got != c.want {
			t.Errorf("InferAgentType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJoin_ProxyBaseReconcilesToScopedSession(t *testing.T) {
	// The background proxy/mirror heartbeat registers a scopeless workspace base
	// ("codex-4188162495") while the CLI hooks own the scoped session
	// ("codex-4188162495-2303882182"). The base presence must reconcile to that
	// session instead of being flagged a bogus orphan.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:       "codex-4188162495",
			Status:        "active",
			AgentType:     "codex",
			RegisteredAt:  mustTime(t, "2026-04-21T11:50:00Z"), // >120s old: would orphan if unmatched
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:55Z"),
		},
	}
	sessions := []bridge.SessionInfo{
		{ID: "sess-1", AgentID: "codex-4188162495-2303882182", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:58:00Z")},
	}
	rows := Join(presences, sessions, now)

	var proxy *presence.PresenceInfo
	for i := range rows {
		if rows[i].AgentID == "codex-4188162495" {
			proxy = &rows[i]
		}
	}
	if proxy == nil {
		t.Fatalf("proxy base row missing; got %+v", rows)
	}
	if !proxy.HasSession {
		t.Fatalf("proxy base presence must reconcile to scoped session; got %+v", *proxy)
	}
	if proxy.IsOrphan {
		t.Fatalf("reconciled presence must not be an orphan; got %+v", *proxy)
	}
	if proxy.SessionID != "sess-1" {
		t.Fatalf("want SessionID=sess-1, got %q", proxy.SessionID)
	}
}

func TestJoin_DifferentWSHashStaysOrphan(t *testing.T) {
	// Worktree divergence: the proxy resolved a different git toplevel than the
	// hooks, so the WS_HASH segments differ. There is no shared sub-id to link
	// them, so the base presence must NOT be reconciled to an unrelated session.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:       "codex-4095021609",
			Status:        "active",
			AgentType:     "codex",
			RegisteredAt:  mustTime(t, "2026-04-21T11:50:00Z"),
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:55Z"),
		},
	}
	sessions := []bridge.SessionInfo{
		{ID: "sess-other", AgentID: "codex-1713039686-1588666389", Status: "active", StartedAt: mustTime(t, "2026-04-21T11:58:00Z")},
	}
	rows := Join(presences, sessions, now)

	var proxy *presence.PresenceInfo
	for i := range rows {
		if rows[i].AgentID == "codex-4095021609" {
			proxy = &rows[i]
		}
	}
	if proxy == nil {
		t.Fatalf("proxy base row missing; got %+v", rows)
	}
	if proxy.HasSession {
		t.Fatalf("must not reconcile across WS_HASH; got %+v", *proxy)
	}
	if !proxy.IsOrphan {
		t.Fatalf("unmatched stale presence should be an orphan; got %+v", *proxy)
	}
}

// --- Orphan grace (JoinOpts.LastSessionSeen) --------------------------------
//
// The "day one" flap: markOrphan's only in-call anchor is RegisteredAt, so a
// long-registered agent whose session misses ONE poll's session list was
// flagged orphan instantly (now-RegisteredAt >> OrphanStaleAfter) and then
// un-flagged on the next successful poll. These tests pin the hysteresis that
// fixes it.

func TestJoinOpts_RecentSessionEvidenceSuppressesOrphanFlap(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:       "claude-code-1-999",
			Status:        "active",
			AgentType:     "claude-code",
			RegisteredAt:  mustTime(t, "2026-04-21T08:00:00Z"), // 4h ago
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z"),
		},
	}
	// Session list happens to miss the agent THIS poll, but it was joined to
	// an active session ten seconds ago.
	rows := JoinOpts(presences, nil, now, JoinOptions{
		LastSessionSeen: map[string]time.Time{"claude-code-1-999": now.Add(-10 * time.Second)},
	})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].IsOrphan {
		t.Fatalf("recent session evidence must suppress the orphan flap; got %+v", rows[0])
	}
	if rows[0].Status != "active" {
		t.Fatalf("graced row must not be downgraded; want active, got %q", rows[0].Status)
	}
}

func TestJoinOpts_PersistentDivergenceStillOrphans(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	lastSeen := now.Add(-10 * time.Minute) // divergence has persisted well past OrphanStaleAfter
	presences := []presence.PresenceInfo{
		{
			AgentID:       "claude-code-1-999",
			Status:        "active",
			AgentType:     "claude-code",
			RegisteredAt:  mustTime(t, "2026-04-21T08:00:00Z"),
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z"),
		},
	}
	rows := JoinOpts(presences, nil, now, JoinOptions{
		LastSessionSeen: map[string]time.Time{"claude-code-1-999": lastSeen},
	})
	if !rows[0].IsOrphan {
		t.Fatalf("divergence persisting past OrphanStaleAfter must orphan; got %+v", rows[0])
	}
	// The orphan clock measures the DIVERGENCE (since last session evidence),
	// not time since registration.
	if want := int(now.Sub(lastSeen).Seconds()); rows[0].OrphanAgeSeconds != want {
		t.Fatalf("orphan age must anchor on last session evidence: want %d, got %d", want, rows[0].OrphanAgeSeconds)
	}
}

func TestJoin_StatelessNeverRegisteredStillOrphans(t *testing.T) {
	// No evidence map at all: an agent that registered long ago and never
	// produced a session keeps the original RegisteredAt-anchored behavior.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:       "claude-code-1-999",
			Status:        "active",
			RegisteredAt:  mustTime(t, "2026-04-21T11:00:00Z"),
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z"),
		},
	}
	rows := Join(presences, nil, now)
	if !rows[0].IsOrphan {
		t.Fatalf("stateless join of a never-sessioned old registration must orphan; got %+v", rows[0])
	}
}

func TestSessionEvidence_PropagatesGraceThroughReJoin(t *testing.T) {
	// A monitor join suppressed a flap via grace evidence. A mobile handler
	// re-joining the SAME snapshot (without the monitor's private map) must
	// not resurrect the orphan flag — SessionEvidence carries the verdict.
	now, _ := time.Parse(time.RFC3339, "2026-04-21T12:00:00Z")
	presences := []presence.PresenceInfo{
		{
			AgentID:       "claude-code-1-999",
			Status:        "active",
			AgentType:     "claude-code",
			RegisteredAt:  mustTime(t, "2026-04-21T08:00:00Z"),
			LastHeartbeat: mustTime(t, "2026-04-21T11:59:50Z"),
		},
	}
	monitorRows := JoinOpts(presences, nil, now, JoinOptions{
		LastSessionSeen: map[string]time.Time{"claude-code-1-999": now.Add(-10 * time.Second)},
	})
	if monitorRows[0].IsOrphan {
		t.Fatalf("precondition: monitor join must have suppressed the flap")
	}
	reJoined := JoinOpts(monitorRows, nil, now, JoinOptions{
		LastSessionSeen: SessionEvidence(monitorRows, now),
	})
	if reJoined[0].IsOrphan {
		t.Fatalf("re-join with SessionEvidence resurrected the suppressed orphan: %+v", reJoined[0])
	}

	// And an adjudicated orphan must stay an orphan through the re-join.
	orphanRows := Join(presences, nil, now) // stateless: registered 4h ago, no session -> orphan
	if !orphanRows[0].IsOrphan {
		t.Fatalf("precondition: stateless join must orphan")
	}
	reOrphan := JoinOpts(orphanRows, nil, now, JoinOptions{
		LastSessionSeen: SessionEvidence(orphanRows, now),
	})
	if !reOrphan[0].IsOrphan {
		t.Fatalf("SessionEvidence must not grace an already-adjudicated orphan")
	}
}
