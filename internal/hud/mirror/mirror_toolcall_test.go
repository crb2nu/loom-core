package mirror

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
	"github.com/crb2nu/loom/internal/visibility/contracts/presence"
)

// fakeToolCalls returns a fixed batch until the cursor advances past maxTS,
// then yields nothing — modelling the EventLog cursor behaviour.
type fakeToolCalls struct {
	mu    sync.Mutex
	calls map[string][]map[string]any // session -> batch
	maxTS int64
}

func (f *fakeToolCalls) RecentToolCallsForSession(sessionID string, since int64, _ int) ([]map[string]any, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if since >= f.maxTS {
		return nil, since // already consumed
	}
	return f.calls[sessionID], f.maxTS
}

func TestMirrorOnce_ForwardsToolCallsThenDedups(t *testing.T) {
	reader := &fakeReader{
		agents:   []presence.PresenceInfo{{AgentID: "claude-code-1", AgentType: "claude-code", Status: "active", SessionID: "sess-1"}},
		sessions: []bridge.SessionInfo{{ID: "sess-1", AgentID: "claude-code-1", Status: "active"}},
	}
	tc := &fakeToolCalls{
		maxTS: 999,
		calls: map[string][]map[string]any{
			"sess-1": {
				{"session_id": "sess-1", "tool": "status", "server": "git"},
				{"session_id": "sess-1", "tool": "log", "server": "git"},
			},
		},
	}

	var (
		mu   sync.Mutex
		hits []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(body, &got)
		mu.Lock()
		hits = append(hits, got)
		mu.Unlock()
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	s := New(Config{URL: srv.URL, Interval: time.Hour, Timeout: time.Second}, reader, srv.Client(), nil)
	s.SetToolCalls(tc)

	// First cycle forwards the tool calls.
	s.MirrorOnce(context.Background())
	// Second cycle: cursor advanced past maxTS, no tool calls forwarded.
	s.MirrorOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 2 {
		t.Fatalf("got %d heartbeats, want 2", len(hits))
	}

	first, ok := hits[0]["recent_tool_calls"].([]any)
	if !ok || len(first) != 2 {
		t.Fatalf("first heartbeat recent_tool_calls = %v, want 2 entries", hits[0]["recent_tool_calls"])
	}
	if _, present := hits[1]["recent_tool_calls"]; present {
		t.Fatalf("second heartbeat re-sent tool calls (cursor dedup failed): %v", hits[1]["recent_tool_calls"])
	}
}
