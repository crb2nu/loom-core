package monitor

import (
	"encoding/json"
	"sync/atomic"
	"testing"
)

// A forced refresh is how startup, reload and presence hooks say "read the
// store now". The bridge reuses light session projections for a short window
// to keep overlapping monitors from stacking scrolls; a forced refresh must
// bypass that window, otherwise a session that just started stays invisible
// until the window expires (the session-trace handlers read this snapshot).
func TestFleetMonitor_ForcedRefreshBypassesSessionListReuse(t *testing.T) {
	sockPath, handlers := mockDaemon(t)
	client, agent := newBridges(t, sockPath)

	var sessionVisible atomic.Bool

	handlers.handle("loom/status", func(_ json.RawMessage) (any, error) {
		return map[string]any{"running": true, "servers": 1, "activeConns": 0, "idleConns": 0, "processes": []string{"agent_context"}}, nil
	})
	handlers.handle("tools/call", func(params json.RawMessage) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		switch req.Name {
		case "agent_context__agent_session_list":
			if !sessionVisible.Load() {
				return toolEnvelope(map[string]any{"sessions": []map[string]any{}}), nil
			}
			return toolEnvelope(map[string]any{
				"sessions": []map[string]any{
					{"id": "s-new", "agent_id": "a1", "status": "active", "total_tokens": 1},
				},
			}), nil
		case "agent_context__agent_task_list":
			return toolEnvelope(map[string]any{"tasks": []map[string]any{}}), nil
		case "agent_context__agent_presence_list":
			return toolEnvelope(map[string]any{"agents": []map[string]any{}}), nil
		default:
			return toolEnvelope(map[string]any{}), nil
		}
	})

	monitor := NewFleetMonitor(client, agent, nil)

	// First refresh sees an empty store and leaves the empty projection in
	// the bridge's reuse window.
	if err := monitor.RefreshForce(); err != nil {
		t.Fatalf("initial refresh: %v", err)
	}
	if got := len(monitor.Snapshot().Sessions); got != 0 {
		t.Fatalf("initial snapshot has %d sessions, want 0", got)
	}

	// The session lands. A forced refresh right away must scroll the store
	// again instead of serving the cached empty list.
	sessionVisible.Store(true)
	if err := monitor.RefreshForce(); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}
	snap := monitor.Snapshot()
	if len(snap.Sessions) != 1 || snap.Sessions[0].ID != "s-new" {
		t.Fatalf("forced refresh served the reused empty projection: sessions=%+v", snap.Sessions)
	}
}
