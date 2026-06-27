package hud

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type blockedAPIRow struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	Reason        string `json:"reason"`
	ToolName      string `json:"tool_name"`
	Cwd           string `json:"cwd"`
	Since         string `json:"since"`
	WaitedSeconds int    `json:"waited_seconds"`
}

type blockedAPIResponse struct {
	Blocked []blockedAPIRow `json:"blocked"`
	Count   int             `json:"count"`
}

func getBlocked(t *testing.T, mux *http.ServeMux) (int, blockedAPIResponse, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/blocked", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	var out blockedAPIResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode /api/blocked: %v (body=%s)", err, body)
		}
	}
	return rec.Code, out, body
}

func blockEvent(sid, agentID, reason, tool, cwd string, since time.Time) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"session_id": sid,
		"agent_id":   agentID,
		"reason":     reason,
		"tool_name":  tool,
		"cwd":        cwd,
		"since":      since.UTC().Format(time.RFC3339Nano),
	})
	return b
}

// TestAPIBlocked_KillTest exercises the load-bearing assumption of the
// Flightdeck-in-the-IDE plan end-to-end at the code level: an agent.blocked
// daemon event — the exact shape the flightdeck-hud-bridge emits — folds through
// the REAL IngestDaemonEvent path into the blocked store and is then served to
// an unauthenticated desktop client at GET /api/blocked in the BlockedSessionInfo
// wire shape the VS Code extension consumes. It also covers the empty,
// longest-wait-first ordering, unblock, and TTL-prune cases.
func TestAPIBlocked_KillTest(t *testing.T) {
	app, mux := newTestApp(t)
	app.blocked = newBlockedStore()

	// 1. Empty store -> 200 with an empty (non-null) array so clients iterate
	//    unconditionally.
	code, resp, body := getBlocked(t, mux)
	if code != http.StatusOK {
		t.Fatalf("empty: status %d body=%s", code, body)
	}
	if resp.Count != 0 || len(resp.Blocked) != 0 {
		t.Fatalf("empty: want 0 blocked, got %d (%+v)", resp.Count, resp.Blocked)
	}
	if !strings.Contains(body, `"blocked":[]`) {
		t.Fatalf("empty: want blocked encoded as [], got %s", body)
	}

	// 2. Real fold path: two agent.blocked events arrive via IngestDaemonEvent
	//    (sess-1 has waited longer than sess-2).
	now := time.Now()
	app.IngestDaemonEvent("agent.blocked", now,
		blockEvent("sess-2", "codex", "permission", "Bash", "/repo/b", now.Add(-30*time.Second)))
	app.IngestDaemonEvent("agent.blocked", now,
		blockEvent("sess-1", "claude-code", "permission", "Edit", "/repo/a", now.Add(-2*time.Minute)))

	code, resp, body = getBlocked(t, mux)
	if code != http.StatusOK {
		t.Fatalf("folded: status %d body=%s", code, body)
	}
	if resp.Count != 2 {
		t.Fatalf("folded: want 2 blocked, got %d (%s)", resp.Count, body)
	}
	// Longest wait first: sess-1 (2m) before sess-2 (30s).
	if resp.Blocked[0].SessionID != "sess-1" || resp.Blocked[1].SessionID != "sess-2" {
		t.Fatalf("folded: want order [sess-1, sess-2], got [%s, %s]",
			resp.Blocked[0].SessionID, resp.Blocked[1].SessionID)
	}
	first := resp.Blocked[0]
	if first.AgentID != "claude-code" || first.Reason != "permission" ||
		first.ToolName != "Edit" || first.Cwd != "/repo/a" {
		t.Fatalf("folded: wrong fields on sess-1: %+v", first)
	}
	if first.WaitedSeconds < 100 {
		t.Fatalf("folded: want waited_seconds ~120 for sess-1, got %d", first.WaitedSeconds)
	}

	// 3. unblock removes only the matching session.
	app.IngestDaemonEvent("agent.unblocked", time.Now(),
		json.RawMessage(`{"session_id":"sess-1"}`))
	if _, resp, _ = getBlocked(t, mux); resp.Count != 1 || resp.Blocked[0].SessionID != "sess-2" {
		t.Fatalf("unblock: want only sess-2 left, got %+v", resp.Blocked)
	}

	// 4. TTL prune: a block older than blockedTTL is dropped on read (missed
	//    unblock safety net), so a stale stall never pins the badge.
	app.IngestDaemonEvent("agent.blocked", time.Now(),
		blockEvent("sess-stale", "gemini", "permission", "Bash", "/repo/c",
			time.Now().Add(-blockedTTL-time.Minute)))
	if _, resp, _ = getBlocked(t, mux); resp.Count != 1 {
		t.Fatalf("ttl: want stale block pruned (count stays 1), got %d (%+v)", resp.Count, resp.Blocked)
	}
	for _, b := range resp.Blocked {
		if b.SessionID == "sess-stale" {
			t.Fatalf("ttl: stale block should be pruned, still present: %+v", resp.Blocked)
		}
	}
}
