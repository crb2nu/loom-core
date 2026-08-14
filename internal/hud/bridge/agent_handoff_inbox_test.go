package bridge

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
	"testing"
)

// handoffListFixture stands up a mock daemon whose agent_presence_list returns
// presentAgents and whose agent_handoff_inbox returns one handoff per queried
// agent id ("ho-<agent_id>"). It records every agent id the bridge polled.
func handoffListFixture(t *testing.T, presentAgents ...string) (*AgentBridge, func() []string) {
	t.Helper()
	sockPath, handlers := mockDaemon(t)

	var mu sync.Mutex
	var polled []string

	handlers.handle("tools/call", func(params json.RawMessage) (any, error) {
		var req struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		var text string
		switch req.Name {
		case "agent_context__agent_presence_list":
			agents := make([]map[string]any, 0, len(presentAgents))
			for _, id := range presentAgents {
				agents = append(agents, map[string]any{"agent_id": id, "status": "active"})
			}
			body, _ := json.Marshal(map[string]any{"agents": agents})
			text = string(body)
		case "agent_context__agent_handoff_inbox":
			agentID, _ := req.Arguments["agent_id"].(string)
			mu.Lock()
			polled = append(polled, agentID)
			mu.Unlock()
			body, _ := json.Marshal(map[string]any{
				"handoffs": []map[string]any{{
					"handoff_id":   "ho-" + agentID,
					"source_agent": "loom-mills-operator",
					"status":       "pending",
					"summary":      "work for " + agentID,
				}},
			})
			text = string(body)
		default:
			t.Errorf("unexpected tool call: %s", req.Name)
			text = `{"ok":true}`
		}
		return map[string]any{
			"isError": false,
			"content": []map[string]any{{"type": "text", "text": text}},
		}, nil
	})

	client := NewDaemonClient(sockPath, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return NewAgentBridge(client), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), polled...)
		sort.Strings(out)
		return out
	}
}

func handoffIDs(handoffs []HandoffInfo) []string {
	out := make([]string, 0, len(handoffs))
	for _, h := range handoffs {
		out = append(out, h.ID)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandoffList_IncludesWellKnownInboxes is the regression for the Mills
// handoff-discovery gap: "mills-merges" and "human-on-call" receive handoffs
// but never register presence, so a presence-only enumeration rendered every
// Mills handoff invisible in the HUD.
func TestHandoffList_IncludesWellKnownInboxes(t *testing.T) {
	bridge, polledAgents := handoffListFixture(t, "claude-code")

	handoffs, err := bridge.HandoffList()
	if err != nil {
		t.Fatalf("HandoffList: %v", err)
	}

	wantPolled := []string{"claude-code", "human-on-call", "mills-merges"}
	if got := polledAgents(); !equalStrings(got, wantPolled) {
		t.Errorf("polled inboxes = %v, want %v", got, wantPolled)
	}
	wantIDs := []string{"ho-claude-code", "ho-human-on-call", "ho-mills-merges"}
	if got := handoffIDs(handoffs); !equalStrings(got, wantIDs) {
		t.Errorf("handoff ids = %v, want %v", got, wantIDs)
	}
}

// TestHandoffList_DedupesPresenceAndStaticInboxes proves a well-known inbox
// that DOES register presence is polled exactly once, so its handoffs are not
// double-counted in the HUD list.
func TestHandoffList_DedupesPresenceAndStaticInboxes(t *testing.T) {
	// "mills-merges" appears in presence AND in the static list; "claude-code"
	// is duplicated within presence itself.
	bridge, polledAgents := handoffListFixture(t, "claude-code", "mills-merges", "claude-code")

	handoffs, err := bridge.HandoffList()
	if err != nil {
		t.Fatalf("HandoffList: %v", err)
	}

	wantPolled := []string{"claude-code", "human-on-call", "mills-merges"}
	if got := polledAgents(); !equalStrings(got, wantPolled) {
		t.Errorf("polled inboxes = %v, want each exactly once %v", got, wantPolled)
	}
	if len(handoffs) != 3 {
		t.Errorf("handoffs = %d (%v), want 3 deduped", len(handoffs), handoffIDs(handoffs))
	}
}

// TestHandoffList_StaticInboxEnvOverride proves a deployment can replace the
// built-in passive-inbox list without a rebuild.
func TestHandoffList_StaticInboxEnvOverride(t *testing.T) {
	t.Setenv(handoffStaticInboxesEnv, " release-bot , ,mills-merges ")
	bridge, polledAgents := handoffListFixture(t, "claude-code")

	if _, err := bridge.HandoffList(); err != nil {
		t.Fatalf("HandoffList: %v", err)
	}
	// "human-on-call" is NOT polled: the env value replaces the built-in list.
	wantPolled := []string{"claude-code", "mills-merges", "release-bot"}
	if got := polledAgents(); !equalStrings(got, wantPolled) {
		t.Errorf("polled inboxes = %v, want %v", got, wantPolled)
	}
}

func TestStaticHandoffInboxes_UnsetUsesBuiltIn(t *testing.T) {
	t.Setenv(handoffStaticInboxesEnv, "sentinel")
	os.Unsetenv(handoffStaticInboxesEnv) // t.Setenv restores the original on cleanup
	if got := staticHandoffInboxes(); !equalStrings(got, wellKnownHandoffInboxes) {
		t.Errorf("staticHandoffInboxes() = %v, want built-in %v", got, wellKnownHandoffInboxes)
	}
}

// An explicitly empty value is the kill switch back to presence-only lookup.
func TestStaticHandoffInboxes_EmptyValueDisables(t *testing.T) {
	t.Setenv(handoffStaticInboxesEnv, "  ")
	if got := staticHandoffInboxes(); len(got) != 0 {
		t.Errorf("staticHandoffInboxes() = %v, want none", got)
	}
}
