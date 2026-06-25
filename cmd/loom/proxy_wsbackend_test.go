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
		})
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
