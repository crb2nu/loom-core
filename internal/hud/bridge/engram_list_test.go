package bridge

import (
	"encoding/json"
	"errors"
	"testing"
)

func toolPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return mcpTextResult(string(quoted))
}

func TestAgentBridgeEngramListAndGraph(t *testing.T) {
	caller := &stubCaller{callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
		switch name {
		case "agent_context__agent_engram_list":
			return toolPayload(t, map[string]any{"items": []any{
				map[string]any{"id": "engram-1", "title": "HTTP", "tier": 2, "prerequisites": []string{"engram-0"}, "proof_kind": "command", "proof_refs": []string{"go test ./..."}},
				map[string]any{"uri": "engram://legacy/go", "name": "Legacy"},
			}}), nil
		case "agent_context__agent_engram_graph":
			return toolPayload(t, map[string]any{"nodes": []any{"engram://base/go", map[string]any{"id": "engram-1", "name": "HTTP", "proof_status": "verified"}}, "edges": []any{map[string]any{"from": "engram-1", "to": "engram://base/go"}}}), nil
		default:
			return nil, errors.New("unexpected tool")
		}
	}}
	b := NewAgentBridge(caller)
	items, err := b.EngramList()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ProofStatus != "unverified" || items[0].Proof.Kind != "command" || items[1].ID != "engram://legacy/go" {
		t.Fatalf("items = %#v", items)
	}
	if items[0].Prerequisites == nil || items[0].Proof.Refs == nil {
		t.Fatal("collections must be non-nil")
	}
	graph, err := b.EngramGraph()
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 || graph.Nodes[0].ID != "engram://base/go" {
		t.Fatalf("graph = %#v", graph)
	}
}

func TestAgentBridgeEngramEmptyAndError(t *testing.T) {
	b := NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) {
		return toolPayload(t, map[string]any{"items": []any{}}), nil
	}})
	items, err := b.EngramList()
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	b = NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("offline") }})
	if _, err := b.EngramList(); err == nil {
		t.Fatal("expected tool error")
	}
}

func TestAgentBridgeEngramGraphEmptyAndError(t *testing.T) {
	b := NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) { return toolPayload(t, map[string]any{}), nil }})
	graph, err := b.EngramGraph()
	if err != nil || graph.Nodes == nil || graph.Edges == nil || len(graph.Nodes) != 0 || len(graph.Edges) != 0 {
		t.Fatalf("graph=%#v err=%v", graph, err)
	}
	b = NewAgentBridge(&stubCaller{callToolFn: func(string, map[string]any) (json.RawMessage, error) { return nil, errors.New("offline") }})
	if _, err := b.EngramGraph(); err == nil {
		t.Fatal("expected tool error")
	}
}
