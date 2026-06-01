package fleet

import (
	"testing"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

type captureDeps struct {
	*mockDeps
	events []capturedToolCall
}

type capturedToolCall struct {
	eventType string
	payload   map[string]any
}

func (c *captureDeps) BroadcastAgentEvent(eventType string, payload any) {
	m, _ := payload.(map[string]any)
	c.events = append(c.events, capturedToolCall{eventType, m})
}

// TestIngestRecentToolCalls re-broadcasts mirror-forwarded tool calls as
// tool.call events (so they land in the central EventLog) and backfills
// session_id/agent_id from the heartbeat when the mirror omitted them.
func TestIngestRecentToolCalls(t *testing.T) {
	deps := &captureDeps{mockDeps: &mockDeps{}}
	d := New(deps)

	body := bridge.HeartbeatRequest{
		AgentID:   "claude-code-1",
		SessionID: "sess-1",
		RecentToolCalls: []map[string]any{
			{"session_id": "sess-1", "tool": "status", "server": "git"},
			{"tool": "log"}, // missing session_id/agent_id -> backfilled
			nil,             // skipped
		},
	}

	d.ingestRecentToolCalls(body)

	if len(deps.events) != 2 {
		t.Fatalf("broadcast count = %d, want 2", len(deps.events))
	}
	for _, e := range deps.events {
		if e.eventType != "tool.call" {
			t.Fatalf("event type = %q, want tool.call", e.eventType)
		}
		if e.payload["session_id"] != "sess-1" {
			t.Fatalf("session_id = %v, want sess-1 (backfill failed)", e.payload["session_id"])
		}
	}
	if deps.events[1].payload["agent_id"] != "claude-code-1" {
		t.Fatalf("agent_id backfill failed: %v", deps.events[1].payload["agent_id"])
	}
}

// TestIngestRecentToolCalls_BoundsPerHeartbeat caps how many calls a single
// noisy heartbeat can inject into the EventLog.
func TestIngestRecentToolCalls_BoundsPerHeartbeat(t *testing.T) {
	deps := &captureDeps{mockDeps: &mockDeps{}}
	d := New(deps)

	calls := make([]map[string]any, 200)
	for i := range calls {
		calls[i] = map[string]any{"session_id": "s", "tool": "t"}
	}
	d.ingestRecentToolCalls(bridge.HeartbeatRequest{AgentID: "a", SessionID: "s", RecentToolCalls: calls})

	if len(deps.events) != 50 {
		t.Fatalf("broadcast count = %d, want 50 (bounded)", len(deps.events))
	}
}
