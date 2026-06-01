package hud

import (
	"encoding/json"
	"testing"
	"time"
)

// TestIngestDaemonEventSurfacesToolCallInTrace covers the embedded-mode bridge:
// a tool.call event ingested from the daemon EventBus must land in the EventLog
// and surface in buildSessionTrace's events for the matching session. Without
// the bridge the embedded HUD shows "no captured activity" for every session.
func TestIngestDaemonEventSurfacesToolCallInTrace(t *testing.T) {
	const sessionID = "sess-embed-1"

	a := &App{eventLog: NewEventLog(10)}

	mustData := func(m map[string]any) json.RawMessage {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}

	now := time.Now()
	a.IngestDaemonEvent("tool.call", now, mustData(map[string]any{
		"session_id": sessionID, "agent_id": "claude-code", "tool": "status", "server": "git",
	}))
	// A tool.call for a different session must not leak into this session.
	a.IngestDaemonEvent("tool.call", now, mustData(map[string]any{
		"session_id": "other", "tool": "log",
	}))
	// A non-logged event type must be ignored by the EventLog.
	a.IngestDaemonEvent("server.health", now, mustData(map[string]any{"session_id": sessionID}))

	events := a.sessionTraceEvents(sessionID, nil, "", 100)
	if len(events) != 1 {
		t.Fatalf("sessionTraceEvents = %d events, want 1 (only the matching tool.call)", len(events))
	}
	if events[0].EventType != "tool.call" {
		t.Fatalf("event type = %q, want tool.call", events[0].EventType)
	}
	if events[0].AgentID != "claude-code" {
		t.Fatalf("event agent_id = %q, want claude-code", events[0].AgentID)
	}
}

// TestIngestDaemonEventNilSafe ensures ingestion is safe before monitors/SSE
// are wired (e.g. very early startup), since it runs from a daemon goroutine.
func TestIngestDaemonEventNilSafe(t *testing.T) {
	a := &App{} // no eventLog, fleetMonitor, or sseHub
	a.IngestDaemonEvent("tool.call", time.Now(), json.RawMessage(`{"session_id":"x"}`))
	a.IngestDaemonEvent("session.start", time.Time{}, nil)
}

// TestRecentToolCallsForSession filters tool.call events by session + cursor and
// advances maxTS past everything seen (so the mirror cursor never re-sends).
func TestRecentToolCallsForSession(t *testing.T) {
	log := NewEventLog(20)
	mk := func(sid, tool string, tsNano int64) {
		b, _ := json.Marshal(map[string]any{"session_id": sid, "tool": tool})
		log.Append(TimelineEntry{Timestamp: time.Unix(0, tsNano), EventType: "tool.call", Data: b})
	}
	mk("S", "a", 100)
	mk("other", "x", 150)
	mk("S", "b", 200)
	mk("S", "c", 300)

	a := &App{eventLog: log}

	// since=0: all three S calls, maxTS=300.
	calls, maxTS := a.RecentToolCallsForSession("S", 0, 25)
	if len(calls) != 3 {
		t.Fatalf("calls=%d want 3", len(calls))
	}
	if maxTS != 300 {
		t.Fatalf("maxTS=%d want 300", maxTS)
	}

	// since=200: only the 300 call.
	calls, maxTS = a.RecentToolCallsForSession("S", 200, 25)
	if len(calls) != 1 || calls[0]["tool"] != "c" {
		t.Fatalf("incremental calls=%v", calls)
	}
	if maxTS != 300 {
		t.Fatalf("maxTS=%d want 300", maxTS)
	}

	// limit truncates the batch but maxTS still advances past all matches.
	calls, maxTS = a.RecentToolCallsForSession("S", 0, 1)
	if len(calls) != 1 {
		t.Fatalf("limited calls=%d want 1", len(calls))
	}
	if maxTS != 300 {
		t.Fatalf("maxTS=%d want 300 (must advance past truncated batch)", maxTS)
	}
}
