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

// TestAgentBridgeEngramStringProof pins the ACTUAL agent_engram_list payload:
// engramItemToMap emits "proof" as a bare STRING (the proof text), which the
// typed EngramProof decode rejected with a type error — 502ing /api/engrams
// and /api/engrams/graph for any catalog with a proof-bearing engram (i.e.
// always; proof is a required field on agent_engram_add).
func TestAgentBridgeEngramStringProof(t *testing.T) {
	caller := &stubCaller{callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
		switch name {
		case "agent_context__agent_engram_list":
			return toolPayload(t, map[string]any{"items": []any{
				map[string]any{
					"id": "mem-123", "uri": "engram://go-cli/flagset", "title": "Go CLI tool",
					"tier": 1, "proof_status": "stale", "prerequisites": []string{},
					"proof": "cmd/loom/main.go:40", "last_verified": "2026-08-01T00:00:00Z",
				},
			}}), nil
		case "agent_context__agent_engram_graph":
			// Root-less full-graph payload: rich nodes keyed by uri, string proof.
			return toolPayload(t, map[string]any{
				"nodes": []any{
					map[string]any{"uri": "engram://go-cli/flagset", "title": "Go CLI tool", "tier": 1, "proof_status": "stale", "prerequisites": []string{}, "proof": "cmd/loom/main.go:40"},
					map[string]any{"uri": "engram://go-cli/base", "title": "", "tier": 1, "proof_status": "unverified", "prerequisites": []string{}},
				},
				"edges":     []any{map[string]any{"from": "engram://go-cli/flagset", "to": "engram://go-cli/base"}},
				"truncated": false,
			}), nil
		default:
			return nil, errors.New("unexpected tool")
		}
	}}
	b := NewAgentBridge(caller)

	items, err := b.EngramList()
	if err != nil {
		t.Fatalf("string proof must decode, not 502: %v", err)
	}
	if len(items) != 1 || items[0].Proof.Kind != "" {
		t.Fatalf("items = %#v", items)
	}
	if len(items[0].Proof.Refs) != 1 || items[0].Proof.Refs[0] != "cmd/loom/main.go:40" {
		t.Fatalf("string proof should surface as a single ref: %#v", items[0].Proof)
	}

	graph, err := b.EngramGraph()
	if err != nil {
		t.Fatalf("rich full-graph payload must decode: %v", err)
	}
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("graph = %#v", graph)
	}
	// Nodes must be keyed by URI so edges (which reference URIs) join.
	if graph.Nodes[0].ID != "engram://go-cli/flagset" {
		t.Fatalf("node ID must be the engram URI, got %q", graph.Nodes[0].ID)
	}
}
