package hud

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// RecentToolCallsForSession returns this session's tool.call* event payloads
// from the EventLog with a timestamp strictly newer than sinceUnixNano (newest
// first, up to limit), plus the max timestamp seen across ALL matching events
// (so the HUD mirror can advance its per-session cursor past everything it has
// observed, not just the truncated batch). Implements mirror.ToolCallReader so
// a distributed daemon can forward local tool-call activity to the central HUD.
func (a *App) RecentToolCallsForSession(sessionID string, sinceUnixNano int64, limit int) ([]map[string]any, int64) {
	if a.eventLog == nil || sessionID == "" {
		return nil, sinceUnixNano
	}
	out := make([]map[string]any, 0, limit)
	maxTS := sinceUnixNano
	for _, e := range a.eventLog.All(1000) {
		if !strings.HasPrefix(e.EventType, "tool.call") {
			continue
		}
		ts := e.Timestamp.UnixNano()
		if ts <= sinceUnixNano {
			continue
		}
		var data map[string]any
		if len(e.Data) > 0 {
			if err := json.Unmarshal(e.Data, &data); err != nil {
				continue
			}
		}
		if data == nil {
			continue
		}
		if sid, _ := data["session_id"].(string); sid != sessionID {
			continue
		}
		if ts > maxTS {
			maxTS = ts
		}
		if len(out) < limit {
			if _, ok := data["ts"]; !ok {
				data["ts"] = e.Timestamp.UTC().Format(time.RFC3339Nano)
			}
			out = append(out, data)
		}
	}
	return out, maxTS
}

// daemonEventLogTypes are the daemon EventBus event types the HUD appends to
// its in-memory EventLog (which buildSessionTrace reads for per-session
// activity). session.* and agent.status.change drive the roster/timeline;
// tool.call* carry per-session tool activity. Other event types are still
// broadcast to browser SSE clients but not retained in the activity log.
var daemonEventLogTypes = map[string]bool{
	"session.start":       true,
	"session.end":         true,
	"agent.status.change": true,
	"decomp.hint":         true,
	"tool.call":           true,
	"tool.call.start":     true,
	"tool.call.end":       true,
}

// daemonEventFleetRefreshTypes are the subset whose arrival should trigger an
// immediate fleet refresh. tool.call* is deliberately excluded: it is
// high-frequency and roster-neutral, so refreshing on it would hammer the
// monitor for no roster change.
var daemonEventFleetRefreshTypes = map[string]bool{
	"session.start":       true,
	"session.end":         true,
	"agent.status.change": true,
}

// IngestDaemonEvent feeds a single daemon EventBus event into the HUD's
// EventLog and SSE hub. It is the embedded-mode equivalent of the standalone
// SSE event consumer (see startStandaloneEventConsumer): the embedded HUD runs
// in the daemon process and bypasses that consumer, so without this bridge no
// daemon event (session.*, agent.status.change, tool.call*) ever reaches the
// EventLog that buildSessionTrace reads — which is why per-session activity
// renders empty on the embedded/in-cluster HUD.
//
// data is the JSON-encoded event payload; eventMatchesSessionTrace correlates
// it to a session via its "session_id" field.
func (a *App) IngestDaemonEvent(eventType string, ts time.Time, data json.RawMessage) {
	if ts.IsZero() {
		ts = time.Now()
	}

	if a.eventLog != nil && daemonEventLogTypes[eventType] {
		agentID, _ := jsonStringField(data, "agent_id")
		agentType, _ := jsonStringField(data, "agent_type")
		a.eventLog.Append(TimelineEntry{
			Timestamp: ts,
			EventType: eventType,
			AgentID:   agentID,
			AgentType: agentType,
			Data:      data,
		})
	}

	if a.fleetMonitor != nil && daemonEventFleetRefreshTypes[eventType] {
		a.fleetMonitor.Refresh()
	}

	if a.sseHub != nil {
		a.sseHub.Broadcast(bridge.SSEEvent{
			ID:        fmt.Sprintf("%s-%d", eventType, ts.UnixMilli()),
			Type:      eventType,
			Timestamp: ts,
			Data:      data,
		})
	}
}

// jsonStringField extracts a string field from a raw JSON object, returning
// ("", false) when absent or not a string.
func jsonStringField(data json.RawMessage, key string) (string, bool) {
	if len(data) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return "", false
	}
	if v, ok := m[key].(string); ok {
		return v, true
	}
	return "", false
}
