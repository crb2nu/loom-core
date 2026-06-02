package hud

import (
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/spawn"
)

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
