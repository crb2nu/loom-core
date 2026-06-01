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
