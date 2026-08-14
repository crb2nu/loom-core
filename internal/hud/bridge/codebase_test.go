package bridge

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// stubCaller is a minimal Caller that routes CallTool to a configurable function.
type stubCaller struct {
	callToolFn func(name string, args map[string]any) (json.RawMessage, error)
}

func (s *stubCaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (s *stubCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (s *stubCaller) CallTool(name string, args map[string]any) (json.RawMessage, error) {
	if s.callToolFn != nil {
		return s.callToolFn(name, args)
	}
	return nil, fmt.Errorf("unexpected CallTool for %s", name)
}
func (s *stubCaller) CallToolWithTimeout(name string, args map[string]any, _ time.Duration) (json.RawMessage, error) {
	return s.CallTool(name, args)
}
func (s *stubCaller) CircuitOpen() bool { return false }
func (s *stubCaller) Close() error      { return nil }

func mcpTextResult(payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, payload))
}

func TestCodebaseStats(t *testing.T) {
	var gotArgs map[string]any
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_stats" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			gotArgs = args
			// Real fleet-mode response shape from HandleStats/aggregateStats.
			return mcpTextResult(`"{\"aggregate\":true,\"collection\":\"codebase_memory_v1\",\"total_chunks\":60794,\"by_language\":{\"go\":41000,\"typescript\":8000},\"by_chunk_type\":{\"function\":30000,\"method\":15000}}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	stats, err := b.CodebaseStats()
	if err != nil {
		t.Fatalf("CodebaseStats: %v", err)
	}
	// The bridge must NOT pass a repo_id — that is what makes the server
	// answer in fleet-aggregate mode instead of erroring.
	if _, ok := gotArgs["repo_id"]; ok {
		t.Fatalf("CodebaseStats must not send repo_id, got args %v", gotArgs)
	}
	if !stats.Aggregate {
		t.Fatalf("expected aggregate=true, got %+v", stats)
	}
	if stats.Collection != "codebase_memory_v1" {
		t.Fatalf("expected collection 'codebase_memory_v1', got %q", stats.Collection)
	}
	if stats.TotalChunks != 60794 {
		t.Fatalf("expected 60794 chunks, got %d", stats.TotalChunks)
	}
	if stats.ByLanguage["go"] != 41000 {
		t.Fatalf("expected 41000 go chunks, got %d", stats.ByLanguage["go"])
	}
	if stats.ByChunkType["function"] != 30000 {
		t.Fatalf("expected 30000 function chunks, got %d", stats.ByChunkType["function"])
	}
}

func TestCodebaseSearch(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_search" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			if q, _ := args["query"].(string); q != "AgentBridge" {
				t.Fatalf("expected query 'AgentBridge', got %q", q)
			}
			return mcpTextResult(`"{\"results\":[{\"file_path\":\"internal/hud/bridge/agent.go\",\"symbol\":\"AgentBridge\",\"kind\":\"struct\",\"line\":31,\"score\":0.95,\"snippet\":\"type AgentBridge struct\"}]}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	results, err := b.CodebaseSearch("AgentBridge", 10)
	if err != nil {
		t.Fatalf("CodebaseSearch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Symbol != "AgentBridge" {
		t.Fatalf("expected symbol 'AgentBridge', got %q", results[0].Symbol)
	}
	if results[0].Score != 0.95 {
		t.Fatalf("expected score 0.95, got %f", results[0].Score)
	}
}

func TestCodebaseTextSearch(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_text_search" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			if q, _ := args["query"].(string); q != "callTool" {
				t.Fatalf("expected query 'callTool', got %q", q)
			}
			return mcpTextResult(`"{\"results\":[{\"file_path\":\"internal/hud/bridge/agent.go\",\"symbol\":\"\",\"kind\":\"text\",\"line\":114,\"score\":1.0,\"snippet\":\"a.client.CallTool\"}]}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	results, err := b.CodebaseTextSearch("callTool", 5)
	if err != nil {
		t.Fatalf("CodebaseTextSearch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].FilePath != "internal/hud/bridge/agent.go" {
		t.Fatalf("expected file_path, got %q", results[0].FilePath)
	}
}

func TestCodebaseIndexStart(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_index_start" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			if p, _ := args["path"].(string); p != "/workspace/loom-core" {
				t.Fatalf("expected path '/workspace/loom-core', got %q", p)
			}
			return mcpTextResult(`"{\"job_id\":\"idx-42\",\"status\":\"running\"}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	job, err := b.CodebaseIndexStart("/workspace/loom-core")
	if err != nil {
		t.Fatalf("CodebaseIndexStart: %v", err)
	}
	if job.JobID != "idx-42" {
		t.Fatalf("expected job_id 'idx-42', got %q", job.JobID)
	}
	if job.Status != "running" {
		t.Fatalf("expected status 'running', got %q", job.Status)
	}
}

func TestCodebaseIndexPoll(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_index_poll" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			if jid, _ := args["job_id"].(string); jid != "idx-42" {
				t.Fatalf("expected job_id 'idx-42', got %q", jid)
			}
			return mcpTextResult(`"{\"job_id\":\"idx-42\",\"status\":\"completed\"}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	job, err := b.CodebaseIndexPoll("idx-42")
	if err != nil {
		t.Fatalf("CodebaseIndexPoll: %v", err)
	}
	if job.JobID != "idx-42" {
		t.Fatalf("expected job_id 'idx-42', got %q", job.JobID)
	}
	if job.Status != "completed" {
		t.Fatalf("expected status 'completed', got %q", job.Status)
	}
}

func TestCodebaseSearch_DefaultLimit(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(name string, args map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_search" {
				t.Fatalf("unexpected tool name: %s", name)
			}
			lim, _ := args["limit"].(int)
			if lim != 20 {
				t.Fatalf("expected default limit 20, got %v", args["limit"])
			}
			return mcpTextResult(`"{\"results\":[]}"`), nil
		},
	}
	b := NewAgentBridge(caller)
	results, err := b.CodebaseSearch("test", 0) // 0 should default to 20
	if err != nil {
		t.Fatalf("CodebaseSearch: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestCodebaseStats_Error(t *testing.T) {
	caller := &stubCaller{
		callToolFn: func(string, map[string]any) (json.RawMessage, error) {
			return nil, fmt.Errorf("connection refused")
		},
	}
	b := NewAgentBridge(caller)
	_, err := b.CodebaseStats()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
