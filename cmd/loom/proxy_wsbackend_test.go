package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

// scriptedBackend is a fake upstream MCP transport: for every request it
// receives it queues a response echoing the request id, with a result derived
// from the method. Notifications are accepted and dropped.
type scriptedBackend struct {
	mu      sync.Mutex
	pending []*mcp.Message
	notify  chan struct{}
	closed  bool
}

func newScriptedBackend() *scriptedBackend {
	return &scriptedBackend{notify: make(chan struct{}, 64)}
}

func (s *scriptedBackend) Send(_ context.Context, msg *mcp.Message) error {
	if msg.ID == nil {
		return nil // notification: accept + drop
	}
	var result any
	switch msg.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]any{"name": "fake"}}
	case "tools/call":
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok: true\nmarker: KILLTEST-20260624"}}}
	default:
		result = map[string]any{"echo": msg.Method}
	}
	resp, _ := mcp.NewResponse(msg.ID, result)
	s.mu.Lock()
	s.pending = append(s.pending, resp)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return nil
}

func (s *scriptedBackend) Recv(ctx context.Context) (*mcp.Message, error) {
	for {
		s.mu.Lock()
		if len(s.pending) > 0 {
			m := s.pending[0]
			s.pending = s.pending[1:]
			s.mu.Unlock()
			return m, nil
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.notify:
		}
	}
}

func (s *scriptedBackend) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}

// TestWSBackendPump_ForwardsRequestsAndMatchesIDs drives the pump through an
// in-memory client pipe and a scripted backend, asserting standard MCP
// request/response forwarding with id matching (the core of the spawn-pod
// Plan Store bridge).
func TestWSBackendPump_ForwardsRequestsAndMatchesIDs(t *testing.T) {
	clientSide, testSide := mcp.NewPipeTransport()
	backend := newScriptedBackend()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- wsBackendPump(ctx, clientSide, func(context.Context) (mcp.Transport, error) {
			return backend, nil
		}, wsBackendFilter{})
	}()

	send := func(id any, method string, params any) {
		raw, _ := json.Marshal(params)
		m := &mcp.Message{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
		if err := testSide.Send(ctx, m); err != nil {
			t.Fatalf("client send %s: %v", method, err)
		}
	}

	// initialize → response echoes id 1
	send(float64(1), "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	resp, err := testSide.Recv(ctx)
	if err != nil {
		t.Fatalf("recv initialize: %v", err)
	}
	if !jsonRPCIDEqual(resp.ID, float64(1)) || resp.Result == nil {
		t.Fatalf("initialize response mismatch: id=%v result=%s err=%v", resp.ID, resp.Result, resp.Error)
	}

	// A client notification must NOT produce a response (next Recv is the
	// tools/call answer, not a stray reply).
	_ = testSide.Send(ctx, &mcp.Message{JSONRPC: "2.0", Method: "notifications/initialized"})

	// tools/call agent_plan_get → response echoes id 2 with marker
	send(float64(2), "tools/call", map[string]any{"name": "agent_plan_get", "arguments": map[string]any{"plan_id": "p"}})
	resp, err = testSide.Recv(ctx)
	if err != nil {
		t.Fatalf("recv tools/call: %v", err)
	}
	if !jsonRPCIDEqual(resp.ID, float64(2)) {
		t.Fatalf("tools/call response id = %v, want 2", resp.ID)
	}
	if !strings.Contains(string(resp.Result), "KILLTEST-20260624") {
		t.Fatalf("tools/call result missing marker: %s", resp.Result)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after cancel")
	}
}

func TestJSONRPCIDEqual(t *testing.T) {
	cases := []struct {
		a, b any
		want bool
	}{
		{float64(1), float64(1), true},
		{float64(2), float64(1), false},
		{"abc", "abc", true},
		{float64(1), "1", true}, // representational tolerance
		{"x", float64(1), false},
	}
	for _, c := range cases {
		if got := jsonRPCIDEqual(c.a, c.b); got != c.want {
			t.Errorf("jsonRPCIDEqual(%v,%v)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}

// toolsListBackend answers tools/list with a fixed, un-namespaced tool set
// (exactly how mcp-agent-context speaks over its /ws endpoint) plus a sibling
// field the pump must preserve.
type toolsListBackend struct {
	*scriptedBackend
	names []string
}

func newToolsListBackend(names ...string) *toolsListBackend {
	return &toolsListBackend{scriptedBackend: newScriptedBackend(), names: names}
}

func (s *toolsListBackend) Send(ctx context.Context, msg *mcp.Message) error {
	if msg.ID == nil || msg.Method != "tools/list" {
		return s.scriptedBackend.Send(ctx, msg)
	}
	tools := make([]map[string]any, 0, len(s.names))
	for _, n := range s.names {
		tools = append(tools, map[string]any{
			"name":        n,
			"description": n + " description",
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	resp, _ := mcp.NewResponse(msg.ID, map[string]any{"tools": tools, "nextCursor": "c1"})
	s.mu.Lock()
	s.pending = append(s.pending, resp)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return nil
}

func wsToolsListNames(t *testing.T, filter wsBackendFilter, backendNames ...string) ([]string, string) {
	t.Helper()

	clientSide, testSide := mcp.NewPipeTransport()
	backend := newToolsListBackend(backendNames...)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- wsBackendPump(ctx, clientSide, func(context.Context) (mcp.Transport, error) {
			return backend, nil
		}, filter)
	}()

	if err := testSide.Send(ctx, &mcp.Message{JSONRPC: "2.0", ID: float64(1), Method: "tools/list"}); err != nil {
		t.Fatalf("send tools/list: %v", err)
	}
	resp, err := testSide.Recv(ctx)
	if err != nil {
		t.Fatalf("recv tools/list: %v", err)
	}

	var payload struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(resp.Result, &payload); err != nil {
		t.Fatalf("decode tools/list result %q: %v", resp.Result, err)
	}
	names := make([]string, 0, len(payload.Tools))
	for _, tool := range payload.Tools {
		names = append(names, tool.Name)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not exit after cancel")
	}
	return names, payload.NextCursor
}

// TestWSBackendPump_ToolsListUnfilteredByDefault pins the default: Mills spawn
// pods must keep seeing the full, un-namespaced backend tool set.
func TestWSBackendPump_ToolsListUnfilteredByDefault(t *testing.T) {
	backendTools := []string{"agent_plan_get", "agent_session_prune", "agent_engram_add", "agent_recall"}

	names, cursor := wsToolsListNames(t, wsBackendFilter{}, backendTools...)

	if len(names) != len(backendTools) {
		t.Fatalf("default pass-through returned %d tools, want %d: %v", len(names), len(backendTools), names)
	}
	for i, want := range backendTools {
		if names[i] != want {
			t.Errorf("tool[%d] = %q, want %q (order must be preserved)", i, names[i], want)
		}
	}
	if cursor != "c1" {
		t.Errorf("nextCursor = %q, want c1 (sibling fields must survive)", cursor)
	}
}

// TestWSBackendPump_ToolsListAppliesProfile proves LOOM_PROXY_WS_PROFILE
// actually shapes the response, and that the priority patterns match despite
// the backend speaking un-namespaced names.
func TestWSBackendPump_ToolsListAppliesProfile(t *testing.T) {
	t.Setenv("LOOM_PROXY_WS_PROFILE", proxyToolProfileAntigravityCore)

	filter := wsBackendFilterFromEnv()
	if !filter.enabled() {
		t.Fatal("LOOM_PROXY_WS_PROFILE did not enable filtering")
	}
	if filter.profile != proxyToolProfileAntigravityCore {
		t.Fatalf("profile = %q, want %q", filter.profile, proxyToolProfileAntigravityCore)
	}
	if filter.serverNS != wsBackendDefaultServerNS {
		t.Fatalf("serverNS = %q, want %q", filter.serverNS, wsBackendDefaultServerNS)
	}

	// Cap of 2 forces a real selection: the priority list must win over the
	// backend's declaration order, and names must come back un-namespaced.
	filter.maxTools = 2
	backendTools := []string{"agent_session_prune", "agent_engram_add", "agent_recall", "agent_plan_get"}

	names, _ := wsToolsListNames(t, filter, backendTools...)

	if len(names) != 2 {
		t.Fatalf("filtered tools = %v, want 2 entries", names)
	}
	for _, n := range names {
		if strings.Contains(n, "__") {
			t.Errorf("tool %q leaked the matching namespace back to the client", n)
		}
	}
	// agent_recall and agent_plan_get are both in coreRequiredPatterns;
	// agent_recall appears earlier in the list, so it must be selected first.
	if names[0] != "agent_recall" {
		t.Errorf("names[0] = %q, want agent_recall (priority order, not backend order)", names[0])
	}
	for _, n := range names {
		if n == "agent_session_prune" || n == "agent_engram_add" {
			t.Errorf("non-priority tool %q survived a 2-tool cap: %v", n, names)
		}
	}
}

// TestWSBackendFilter_MalformedResultIsPassedThrough: shaping must never be
// able to corrupt or drop a response.
func TestWSBackendFilter_MalformedResultIsPassedThrough(t *testing.T) {
	filter := wsBackendFilter{profile: proxyToolProfileLLMCore, serverNS: wsBackendDefaultServerNS}
	resp := &mcp.Message{JSONRPC: "2.0", ID: float64(1), Result: json.RawMessage(`{"tools":"not-an-array"}`)}
	filter.applyToToolsList(resp)
	if string(resp.Result) != `{"tools":"not-an-array"}` {
		t.Errorf("malformed result was rewritten: %s", resp.Result)
	}
}
