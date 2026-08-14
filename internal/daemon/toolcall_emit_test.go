package daemon

import (
	"log/slog"
	"testing"
	"time"
)

// TestEmitAuditPublishesSessionScopedToolCall covers the source side of the
// Live-Sessions activity channel: emitAudit publishes a tool.call event
// (carrying session_id + the resolved agent_id) for session-scoped calls, and
// stays silent for calls without a session_id so ambient daemon traffic does
// not flood the bus.
func TestEmitAuditPublishesSessionScopedToolCall(t *testing.T) {
	eb := NewEventBus(slog.Default())
	id, ch := eb.Subscribe()
	defer eb.Unsubscribe(id)

	d := &Daemon{
		eventBus: eb,
		sessions: NewSessionManager(10, time.Minute, 1, slog.Default()),
	}
	sess := d.sessions.Open(SessionClientInfo{PresenceAgentID: "claude-code"}, "")

	d.emitAudit(
		callParams{Method: "tools/call", SessionID: sess.ID},
		"git", "status", "local", time.Now(),
		"success", "", false, nil, "execute", 0, 0, auditTimings{},
	)

	select {
	case evt := <-ch:
		if evt.Type != EventToolCall {
			t.Fatalf("event type = %q, want %q", evt.Type, EventToolCall)
		}
		data, ok := evt.Data.(map[string]any)
		if !ok {
			t.Fatalf("event data type = %T, want map[string]any", evt.Data)
		}
		if data["session_id"] != sess.ID {
			t.Fatalf("event session_id = %v, want %q", data["session_id"], sess.ID)
		}
		if data["agent_id"] != "claude-code" {
			t.Fatalf("event agent_id = %v, want %q", data["agent_id"], "claude-code")
		}
	default:
		t.Fatal("expected a tool.call event to be published, got none")
	}

	// Call without a session_id: must NOT publish.
	d.emitAudit(
		callParams{Method: "tools/call"},
		"git", "log", "local", time.Now(),
		"success", "", false, nil, "execute", 0, 0, auditTimings{},
	)
	select {
	case evt := <-ch:
		t.Fatalf("unexpected event published for session-less call: %+v", evt)
	default:
	}
}
