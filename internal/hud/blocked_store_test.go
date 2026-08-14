package hud

import (
	"encoding/json"
	"testing"
	"time"
)

func TestBlockedStoreBlockUnblockList(t *testing.T) {
	s := newBlockedStore()
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	s.block(BlockedSession{SessionID: "s1", AgentID: "claude-code", Reason: "permission", ToolName: "Bash", Since: t0.Add(-30 * time.Second)}, t0)
	s.block(BlockedSession{SessionID: "s2", AgentID: "claude-code", Reason: "permission", Since: t0.Add(-90 * time.Second)}, t0)

	got := s.list(t0)
	if len(got) != 2 {
		t.Fatalf("list len = %d, want 2", len(got))
	}
	// Longest wait first: s2 (90s) before s1 (30s).
	if got[0].SessionID != "s2" || got[1].SessionID != "s1" {
		t.Errorf("order = [%s,%s], want [s2,s1]", got[0].SessionID, got[1].SessionID)
	}

	// Unblock clears one; the other remains.
	s.unblock("s2")
	got = s.list(t0)
	if len(got) != 1 || got[0].SessionID != "s1" {
		t.Fatalf("after unblock got %v, want only s1", got)
	}

	// Unblocking an unknown session is a no-op.
	s.unblock("nope")
	if len(s.list(t0)) != 1 {
		t.Errorf("unblock of unknown session changed the set")
	}
}

func TestBlockedStoreTTLPrune(t *testing.T) {
	s := newBlockedStore()
	t0 := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s.block(BlockedSession{SessionID: "stale", Since: t0.Add(-2 * blockedTTL)}, t0)
	s.block(BlockedSession{SessionID: "fresh", Since: t0.Add(-1 * time.Minute)}, t0)

	got := s.list(t0)
	if len(got) != 1 || got[0].SessionID != "fresh" {
		t.Fatalf("TTL prune: got %v, want only fresh", got)
	}
	// The stale entry was pruned from the map, not just filtered.
	if _, ok := s.m["stale"]; ok {
		t.Error("stale entry not pruned from the map")
	}
}

func TestBlockedStoreZeroSinceStampsNow(t *testing.T) {
	s := newBlockedStore()
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	s.block(BlockedSession{SessionID: "s1"}, now) // Since zero
	got := s.list(now)
	if len(got) != 1 || !got[0].Since.Equal(now) {
		t.Fatalf("zero Since not stamped now: %v", got)
	}
}

func TestBlockedFromEvent(t *testing.T) {
	ts := time.Date(2026, 6, 15, 12, 0, 5, 0, time.UTC)
	data := json.RawMessage(`{"agent_id":"claude-code","session_id":"s1","reason":"permission","tool_name":"Bash","cwd":"/repo","since":"2026-06-15T12:00:00Z"}`)

	b := blockedFromEvent(ts, data)
	if b.SessionID != "s1" || b.AgentID != "claude-code" || b.ToolName != "Bash" || b.Cwd != "/repo" {
		t.Errorf("parsed = %+v", b)
	}
	// `since` from the payload wins over ts.
	if !b.Since.Equal(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("Since = %v, want the payload's 12:00:00Z", b.Since)
	}

	// Missing `since` falls back to ts; missing reason defaults to permission.
	b2 := blockedFromEvent(ts, json.RawMessage(`{"session_id":"s2"}`))
	if !b2.Since.Equal(ts) {
		t.Errorf("fallback Since = %v, want ts %v", b2.Since, ts)
	}
	if b2.Reason != "permission" {
		t.Errorf("default reason = %q, want permission", b2.Reason)
	}
}

// TestIngestDaemonEventFeedsBlockedStore is the integration point: agent.blocked
// then agent.unblocked through IngestDaemonEvent moves the dashboard-visible set.
func TestIngestDaemonEventFeedsBlockedStore(t *testing.T) {
	a := &App{blocked: newBlockedStore()}
	ts := time.Now()

	a.IngestDaemonEvent("agent.blocked", ts, json.RawMessage(`{"agent_id":"claude-code","session_id":"s1","reason":"permission"}`))
	if n := len(a.blocked.list(ts)); n != 1 {
		t.Fatalf("after agent.blocked, blocked count = %d, want 1", n)
	}

	a.IngestDaemonEvent("agent.unblocked", ts, json.RawMessage(`{"session_id":"s1"}`))
	if n := len(a.blocked.list(ts)); n != 0 {
		t.Fatalf("after agent.unblocked, blocked count = %d, want 0", n)
	}
}

// TestBlockedSessionsAdapter covers the App→mobile wire conversion: the
// dashboard adapter formats Since as RFC3339 and computes waited_seconds.
func TestBlockedSessionsAdapter(t *testing.T) {
	a := &App{blocked: newBlockedStore()}
	// Block ~2 minutes ago so waited_seconds is a stable, positive value.
	a.blocked.block(BlockedSession{
		SessionID: "s1", AgentID: "claude-code", Reason: "permission",
		ToolName: "Bash", Cwd: "/repo", Since: time.Now().Add(-120 * time.Second),
	}, time.Now())

	rows := a.BlockedSessions()
	if len(rows) != 1 {
		t.Fatalf("BlockedSessions len = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.SessionID != "s1" || r.AgentID != "claude-code" || r.ToolName != "Bash" || r.Cwd != "/repo" {
		t.Errorf("adapter row = %+v", r)
	}
	if r.WaitedSeconds < 110 || r.WaitedSeconds > 130 {
		t.Errorf("waited_seconds = %d, want ~120", r.WaitedSeconds)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Since); err != nil {
		t.Errorf("Since %q not RFC3339Nano: %v", r.Since, err)
	}

	// A nil store yields nil (defensive — App constructed without the store).
	if got := (&App{}).BlockedSessions(); got != nil {
		t.Errorf("nil-store BlockedSessions = %v, want nil", got)
	}
}
