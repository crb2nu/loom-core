package hud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// broadcastTelemetryEvent broadcasts a telemetry event via SSE.
func (o *SpawnOrchestrator) broadcastTelemetryEvent(eventType string, agentID string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	o.sseHub.Broadcast(bridge.SSEEvent{
		ID:        fmt.Sprintf("%s-%s-%d", eventType, agentID, time.Now().UnixMilli()),
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      payload,
	})
}

// spawnTelemetryPublisher adapts an *SSEHub to the bridge.TelemetryPublisher
// interface so SpawnTelemetryAccumulator can emit tool.call.start/end events
// into the same SSE stream consumed by /api/events. Marshal failures are
// swallowed: telemetry is best-effort and must never panic the spawn loop.
type spawnTelemetryPublisher struct {
	hub *SSEHub
}

func newSpawnTelemetryPublisher(hub *SSEHub) *spawnTelemetryPublisher {
	return &spawnTelemetryPublisher{hub: hub}
}

func (p *spawnTelemetryPublisher) Publish(eventType string, payload any) {
	if p == nil || p.hub == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	p.hub.Broadcast(bridge.SSEEvent{
		ID:        fmt.Sprintf("%s-%d", eventType, time.Now().UnixMilli()),
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      body,
	})
}

// recordSpawnTelemetryMetrics records OTel metrics from the telemetry snapshot
// attached to a terminal spawn state. Called from completeSpawn and failSpawn.
func (o *SpawnOrchestrator) recordSpawnTelemetryMetrics(ctx context.Context, state *SpawnState) {
	tel := state.Telemetry
	attrs := metric.WithAttributes(attribute.String("agent_type", state.Request.AgentType))

	o.metrics.SpawnTokensTotal.Add(ctx, int64(tel.TokenUsage.InputTokens),
		attrs, metric.WithAttributes(attribute.String("direction", "input")))
	o.metrics.SpawnTokensTotal.Add(ctx, int64(tel.TokenUsage.OutputTokens),
		attrs, metric.WithAttributes(attribute.String("direction", "output")))

	if tel.TotalCostUSD > 0 {
		o.metrics.SpawnCostTotal.Add(ctx, tel.TotalCostUSD, attrs)
	}
	o.metrics.SpawnTurnsTotal.Add(ctx, int64(tel.TurnCount), attrs)
	o.metrics.SpawnToolCallsTotal.Add(ctx, int64(len(tel.ToolCalls)), attrs)

	for _, fc := range tel.FileChanges {
		o.metrics.SpawnFileChangesTotal.Add(ctx, 1,
			attrs, metric.WithAttributes(attribute.String("kind", fc.Kind)))
	}
	for _, e := range tel.Errors {
		o.metrics.SpawnErrorsTotal.Add(ctx, 1,
			attrs, metric.WithAttributes(attribute.String("error_type", e.Type)))
	}
}

// registerSpawnSession registers the agent-context session for a spawn at
// spawn-start and returns its session id ("" when registration fails). The
// failure is logged at Warn rather than swallowed: an empty session id makes
// persistTelemetrySummary skip the telemetry write, silently dropping the
// spawn's turn-level summary (turn_count / stop_reason / last_message). That
// silent drop is what hid the in-VM codex failure during the Mills A2
// first-autonomous-merge kill-test — codex exited with turn_count=0 and an
// empty diff, invisible from operator logs. Make the registration failure
// observable so the next live debug is not blind.
func (o *SpawnOrchestrator) registerSpawnSession(req SpawnRequest, agentID string) string {
	if o.agentBridge == nil {
		return ""
	}
	sessRes, sessErr := o.agentBridge.StartSession(bridge.SessionStartParams{
		Namespace:   req.Namespace,
		AgentID:     agentID,
		AgentType:   req.AgentType,
		Description: req.TaskDescription,
	})
	switch {
	case sessErr != nil:
		o.logger.Warn("spawn session registration failed; turn-level telemetry will be dropped for this spawn",
			"agent_id", agentID,
			"agent_type", req.AgentType,
			"namespace", req.Namespace,
			"error", sessErr)
		return ""
	case sessRes == nil || strings.TrimSpace(sessRes.SessionID) == "":
		o.logger.Warn("spawn session registration returned no session id; turn-level telemetry will be dropped for this spawn",
			"agent_id", agentID,
			"agent_type", req.AgentType,
			"namespace", req.Namespace)
		return ""
	default:
		return sessRes.SessionID
	}
}

// resolveSpawnSessionID returns the agent-context session ID under which this
// spawn's telemetry and error entries should be recorded. Resolution order:
//
//  1. state.SessionID — the session created for this spawn at spawn-start.
//  2. state.Request.ParentSessionID — the proxy session that originated it.
//  3. the agent's currently-active session, looked up by agent_id.
//
// Returns "" when none resolves (e.g., the spawn failed before its session was
// registered, or agent-context is unreachable). Callers must skip persistence
// on an empty result — agent_context_add rejects an empty session_id.
func (o *SpawnOrchestrator) resolveSpawnSessionID(state *SpawnState) string {
	if state == nil {
		return ""
	}
	if id := strings.TrimSpace(state.SessionID); id != "" {
		return id
	}
	if id := strings.TrimSpace(state.Request.ParentSessionID); id != "" {
		return id
	}
	if o.agentBridge != nil && strings.TrimSpace(state.AgentID) != "" {
		if active, err := o.agentBridge.GetActiveSession(state.AgentID); err == nil && active != nil {
			return active.ID
		}
	}
	return ""
}

// persistTelemetrySummary writes a structured telemetry summary to the
// agent-context session associated with this spawn. Called from completeSpawn
// and failSpawn after the telemetry snapshot has been attached to state.
//
// Uses a short background context (not the spawn context) because failSpawn
// may be invoked on a canceled or timed-out parent context. Errors from
// ContextAdd are logged but do not fail the spawn transition.
func (o *SpawnOrchestrator) persistTelemetrySummary(state *SpawnState, status string) {
	if o.agentBridge == nil || state == nil {
		return
	}
	if state.Request.Namespace == "" {
		return
	}
	if state.Telemetry == nil {
		return
	}

	// agent_context_add requires a session_id that resolves to an existing
	// session. Resolve one from the spawn's own session (set at spawn-start)
	// before falling back to the originating proxy session. If neither
	// resolves, skip the persist rather than emit a guaranteed-to-fail call
	// that floods logs with "session_id: is required".
	sessionID := o.resolveSpawnSessionID(state)
	if sessionID == "" {
		o.logger.Debug("skipping spawn telemetry summary persist: no session id",
			"spawn_id", state.SpawnID)
		return
	}

	tel := state.Telemetry
	summary := map[string]any{
		"spawn_id":       state.SpawnID,
		"agent_id":       state.AgentID,
		"agent_type":     state.Request.AgentType,
		"status":         status,
		"total_cost_usd": tel.TotalCostUSD,
		"turn_count":     tel.TurnCount,
		"stop_reason":    tel.StopReason,
		"tool_count":     len(tel.ToolCalls),
		"file_count":     len(tel.FileChanges),
		"error_count":    len(tel.Errors),
		"token_usage":    tel.TokenUsage,
		"last_message":   tel.LastMessage,
	}
	content, err := json.Marshal(summary)
	if err != nil {
		o.logger.Warn("failed to marshal spawn telemetry summary",
			"spawn_id", state.SpawnID, "error", err)
		return
	}

	entry := map[string]any{
		"entry_type": "decision",
		"title":      fmt.Sprintf("Spawn %s: %s", state.SpawnID, status),
		"content":    string(content),
		"metadata": map[string]any{
			"spawn_id":   state.SpawnID,
			"agent_id":   state.AgentID,
			"agent_type": state.Request.AgentType,
			"namespace":  state.Request.Namespace,
			"status":     status,
		},
	}

	// Use a short, independent timeout — the spawn context may already be
	// canceled on error paths. ContextAdd itself doesn't accept a context,
	// so we run it in a goroutine with a timeout guard to avoid blocking
	// terminal state transitions on a slow MCP bridge.
	persistCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	go func() {
		defer cancel()
		errCh := make(chan error, 1)
		go func() {
			errCh <- o.agentBridge.ContextAdd(sessionID, []map[string]any{entry})
		}()
		select {
		case err := <-errCh:
			if err != nil {
				o.logger.Warn("failed to persist spawn telemetry summary",
					"spawn_id", state.SpawnID, "error", err)
			}
		case <-persistCtx.Done():
			o.logger.Warn("spawn telemetry summary persist timed out",
				"spawn_id", state.SpawnID)
		}
	}()
}

// GetSpawnTelemetry returns a snapshot of the current telemetry for a spawn.
// For running spawns, it reads from the live accumulator. For completed/failed
// spawns, telemetry is attached to the State directly.
func (o *SpawnOrchestrator) GetSpawnTelemetry(spawnID string) (*bridge.SpawnTelemetry, bool) {
	// Check live accumulator first (spawn still running).
	if accVal, ok := o.telemetry.Load(spawnID); ok {
		acc := accVal.(*bridge.SpawnTelemetryAccumulator)
		snap := acc.Snapshot()
		return &snap, true
	}
	// Fall back to completed state's attached telemetry.
	if state, ok := o.ctrl.Get(spawnID); ok && state.Telemetry != nil {
		return state.Telemetry, true
	}
	return nil, false
}

// broadcastSpawnEvent sends a spawn lifecycle event to the SSE hub.
// When the spawn was initiated by the weaver router (metadata carries
// weaver_query_id), also emits a one-shot agent.spawn.weaver_parent
// event on first broadcast so HUD clients can wire the "came from
// weaver query X" badge without polling the spawn detail endpoint.
func (o *SpawnOrchestrator) broadcastSpawnEvent(eventType string, state *SpawnState) {
	if o.sseHub == nil {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	o.sseHub.Broadcast(bridge.SSEEvent{
		ID:        fmt.Sprintf("%s-%s-%d", eventType, state.SpawnID, time.Now().UnixMilli()),
		Type:      eventType,
		Timestamp: time.Now(),
		Data:      data,
	})

	// First lifecycle broadcast for a weaver-originated spawn also emits
	// a sidecar event carrying just the correlation keys. "agent.spawn.building"
	// is the earliest lifecycle event; later transitions don't re-broadcast
	// to avoid spamming the hub.
	if eventType == "agent.spawn.building" {
		o.broadcastWeaverParentIfApplicable(state)
	}
}

// broadcastWeaverParentIfApplicable emits agent.spawn.weaver_parent when
// the spawn's metadata carries weaver_query_id. No-op otherwise.
func (o *SpawnOrchestrator) broadcastWeaverParentIfApplicable(state *SpawnState) {
	queryID := state.Request.Metadata["weaver_query_id"]
	if queryID == "" {
		return
	}
	payload := map[string]string{
		"spawn_id":        state.SpawnID,
		"weaver_query_id": queryID,
		"weaver_domain":   state.Request.Metadata["weaver_domain"],
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	o.sseHub.Broadcast(bridge.SSEEvent{
		ID:        fmt.Sprintf("agent.spawn.weaver_parent-%s-%d", state.SpawnID, time.Now().UnixMilli()),
		Type:      "agent.spawn.weaver_parent",
		Timestamp: time.Now(),
		Data:      data,
	})
}
