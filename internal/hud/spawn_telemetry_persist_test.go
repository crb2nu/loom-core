package hud

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

// captureHandler is a slog.Handler that records emitted records so tests can
// assert that a specific log line (level + message substring) fired.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) has(level slog.Level, substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Level == level && strings.Contains(r.Message, substr) {
			return true
		}
	}
	return false
}

// TestResolveSpawnSessionID covers the session-id resolution order used by the
// telemetry/error persist path. The agentBridge is nil here, so the
// GetActiveSession fallback is exercised only by returning "".
func TestResolveSpawnSessionID(t *testing.T) {
	o := &SpawnOrchestrator{logger: slog.Default()}

	tests := []struct {
		name  string
		state *SpawnState
		want  string
	}{
		{
			name:  "nil state",
			state: nil,
			want:  "",
		},
		{
			name: "spawn session id wins",
			state: &SpawnState{
				SessionID: "sess-spawn-1",
				Request:   spawn.Request{ParentSessionID: "sess-parent"},
			},
			want: "sess-spawn-1",
		},
		{
			name: "falls back to parent session id",
			state: &SpawnState{
				Request: spawn.Request{ParentSessionID: "sess-parent"},
			},
			want: "sess-parent",
		},
		{
			name: "no session resolvable (nil bridge)",
			state: &SpawnState{
				AgentID: "codex-vm",
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := o.resolveSpawnSessionID(tt.state); got != tt.want {
				t.Fatalf("resolveSpawnSessionID() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPersistTelemetrySummary_ThreadsSpawnSessionID is the regression guard for
// the A2 kill-test bug: spawn-driven runs (no explicit session_id on the
// request) persisted telemetry with an empty session_id, which the store
// rejected with "session_id: is required", silently dropping turn-level
// telemetry. The orchestrator now stamps the spawn's own session_id onto the
// state, and persistTelemetrySummary must thread it into agent_context_add.
func TestPersistTelemetrySummary_ThreadsSpawnSessionID(t *testing.T) {
	sockPath, handlers := newMockDaemonForApp(t)

	var (
		mu       sync.Mutex
		gotCalls []map[string]any
		callMade = make(chan struct{}, 1)
	)
	handlers.handle("tools/call", func(params json.RawMessage) (any, error) {
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if req.Name == "agent_context__agent_context_add" {
			mu.Lock()
			gotCalls = append(gotCalls, req.Arguments)
			mu.Unlock()
			select {
			case callMade <- struct{}{}:
			default:
			}
		}
		return map[string]any{
			"isError": false,
			"content": []map[string]any{
				{"type": "text", "text": `{"ok":true,"ids":["entry-1"]}`},
			},
		}, nil
	})

	client := bridge.NewDaemonClient(sockPath, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect to mock daemon: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	o := &SpawnOrchestrator{
		agentBridge: bridge.NewAgentBridge(client),
		logger:      slog.Default(),
	}

	// Spawn-origin state: no explicit session_id on the request, but the
	// orchestrator created a session at spawn-start and stamped it on the
	// state. Telemetry must persist under that session.
	state := &SpawnState{
		SpawnID:   "spawn-a2-canary",
		AgentID:   "codex-vm",
		SessionID: "sess-spawn-a2",
		Request: spawn.Request{
			AgentType: "codex",
			Namespace: "mills/harvester-vm",
		},
		Telemetry: &bridge.SpawnTelemetry{
			TurnCount:   0,
			StopReason:  "end_turn",
			LastMessage: "no diff produced",
		},
	}

	o.persistTelemetrySummary(state, string(SpawnStatusCompleted))

	select {
	case <-callMade:
	case <-time.After(5 * time.Second):
		t.Fatal("agent_context_add was never called for spawn telemetry summary")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotCalls) != 1 {
		t.Fatalf("expected exactly 1 agent_context_add call, got %d", len(gotCalls))
	}
	if sid, _ := gotCalls[0]["session_id"].(string); sid != "sess-spawn-a2" {
		t.Fatalf("agent_context_add session_id = %q, want sess-spawn-a2", sid)
	}
}

// TestPersistTelemetrySummary_SkipsWhenNoSession ensures we do not emit a
// guaranteed-to-fail agent_context_add (empty session_id) when no session can
// be resolved — e.g. the spawn failed before its session was registered.
func TestPersistTelemetrySummary_SkipsWhenNoSession(t *testing.T) {
	sockPath, handlers := newMockDaemonForApp(t)

	var (
		mu       sync.Mutex
		addCalls int
	)
	handlers.handle("tools/call", func(params json.RawMessage) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		if req.Name == "agent_context__agent_context_add" {
			mu.Lock()
			addCalls++
			mu.Unlock()
		}
		return map[string]any{
			"isError": false,
			"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}},
		}, nil
	})

	client := bridge.NewDaemonClient(sockPath, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect to mock daemon: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	o := &SpawnOrchestrator{
		agentBridge: bridge.NewAgentBridge(client),
		logger:      slog.Default(),
	}

	state := &SpawnState{
		SpawnID: "spawn-early-fail",
		AgentID: "codex-vm",
		// No SessionID, no ParentSessionID; GetActiveSession over the mock
		// daemon returns no active session.
		Request: spawn.Request{
			AgentType: "codex",
			Namespace: "mills/harvester-vm",
		},
		Telemetry: &bridge.SpawnTelemetry{StopReason: "error"},
	}

	o.persistTelemetrySummary(state, string(SpawnStatusFailed))

	// Give any erroneously-spawned goroutine a chance to fire.
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if addCalls != 0 {
		t.Fatalf("expected no agent_context_add call when no session resolves, got %d", addCalls)
	}
}

// startSessionMockDaemon wires a mock daemon whose agent_session_start either
// fails (isError) or returns the given session id, and whose active-session
// lookup reports no active session (so StartSession proceeds to start).
func startSessionMockDaemon(t *testing.T, startFails bool, sessionID string) *SpawnOrchestrator {
	t.Helper()
	sockPath, handlers := newMockDaemonForApp(t)
	handlers.handle("tools/call", func(params json.RawMessage) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			t.Fatalf("unmarshal params: %v", err)
		}
		switch req.Name {
		case "agent_context__agent_session_start":
			if startFails {
				return map[string]any{
					"isError": true,
					"content": []map[string]any{{"type": "text", "text": "simulated session-start failure"}},
				}, nil
			}
			return map[string]any{
				"isError": false,
				"content": []map[string]any{{"type": "text", "text": `{"session_id":"` + sessionID + `"}`}},
			}, nil
		default:
			// agent_session_list (active lookup), presence, etc. → benign empty.
			return map[string]any{
				"isError": false,
				"content": []map[string]any{{"type": "text", "text": `{"sessions":[]}`}},
			}, nil
		}
	})

	client := bridge.NewDaemonClient(sockPath, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect to mock daemon: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return &SpawnOrchestrator{agentBridge: bridge.NewAgentBridge(client)}
}

// TestRegisterSpawnSession_LogsOnStartFailure is the regression guard for the
// A2 blind spot: when spawn-start session registration fails, the error must be
// logged (not silently swallowed), because an empty session id makes
// persistTelemetrySummary skip the write and the spawn's turn-level telemetry
// vanishes — exactly what hid the in-VM codex failure.
func TestRegisterSpawnSession_LogsOnStartFailure(t *testing.T) {
	o := startSessionMockDaemon(t, true, "")
	capLog := &captureHandler{}
	o.logger = slog.New(capLog)

	sid := o.registerSpawnSession(SpawnRequest{
		AgentType:       "codex",
		Namespace:       "mills/harvester-vm",
		TaskDescription: "canary",
	}, "codex-vm")

	if sid != "" {
		t.Fatalf("expected empty session id on registration failure, got %q", sid)
	}
	if !capLog.has(slog.LevelWarn, "session registration failed") {
		t.Fatal("expected a Warn log when spawn session registration fails (the A2 blind spot)")
	}
}

// TestRegisterSpawnSession_ReturnsSessionOnSuccess confirms the happy path
// returns the new session id and logs no failure warning.
func TestRegisterSpawnSession_ReturnsSessionOnSuccess(t *testing.T) {
	o := startSessionMockDaemon(t, false, "sess-new-1")
	capLog := &captureHandler{}
	o.logger = slog.New(capLog)

	sid := o.registerSpawnSession(SpawnRequest{
		AgentType: "codex",
		Namespace: "mills/x",
	}, "codex-vm")

	if sid != "sess-new-1" {
		t.Fatalf("registerSpawnSession() = %q, want sess-new-1", sid)
	}
	if capLog.has(slog.LevelWarn, "session registration failed") {
		t.Fatal("did not expect a failure Warn on the success path")
	}
}

// TestRegisterSpawnSession_NilBridge returns empty without panicking when no
// agent bridge is configured.
func TestRegisterSpawnSession_NilBridge(t *testing.T) {
	o := &SpawnOrchestrator{logger: slog.Default()}
	if sid := o.registerSpawnSession(SpawnRequest{Namespace: "x"}, "a"); sid != "" {
		t.Fatalf("expected empty session id with nil bridge, got %q", sid)
	}
}
