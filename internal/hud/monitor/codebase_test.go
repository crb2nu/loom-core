package monitor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/bridge"
)

// codebaseStubCaller implements bridge.Caller for codebase monitor tests.
type codebaseStubCaller struct {
	callToolFn func(name string, args map[string]any) (json.RawMessage, error)
}

func (s *codebaseStubCaller) Call(string, any) (json.RawMessage, error) { return nil, nil }
func (s *codebaseStubCaller) CallWithTimeout(string, any, time.Duration) (json.RawMessage, error) {
	return nil, nil
}
func (s *codebaseStubCaller) CallTool(name string, args map[string]any) (json.RawMessage, error) {
	if s.callToolFn != nil {
		return s.callToolFn(name, args)
	}
	return nil, fmt.Errorf("unexpected CallTool for %s", name)
}
func (s *codebaseStubCaller) CallToolWithTimeout(name string, args map[string]any, _ time.Duration) (json.RawMessage, error) {
	return s.CallTool(name, args)
}
func (s *codebaseStubCaller) CircuitOpen() bool { return false }
func (s *codebaseStubCaller) Close() error      { return nil }

func codebaseMCPResult(payload string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"content":[{"type":"text","text":%s}]}`, payload))
}

func TestCodebaseMonitor_Refresh(t *testing.T) {
	caller := &codebaseStubCaller{
		callToolFn: func(name string, _ map[string]any) (json.RawMessage, error) {
			if name != "codebase_memory__codebase_stats" {
				t.Fatalf("unexpected tool: %s", name)
			}
			return codebaseMCPResult(`"{\"aggregate\":true,\"collection\":\"codebase_memory_v1\",\"total_chunks\":5000,\"by_language\":{\"go\":4000,\"typescript\":1000},\"by_chunk_type\":{\"function\":3000}}"`), nil
		},
	}

	agent := bridge.NewAgentBridge(caller)
	m := NewCodebaseMonitor(agent, slog.Default())

	if err := m.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := m.Status()
	if !snap.Aggregate {
		t.Fatalf("expected aggregate=true, got %+v", snap)
	}
	if snap.Collection != "codebase_memory_v1" {
		t.Fatalf("expected collection 'codebase_memory_v1', got %q", snap.Collection)
	}
	if snap.TotalChunks != 5000 {
		t.Fatalf("expected 5000 chunks, got %d", snap.TotalChunks)
	}
	if snap.ByLanguage["go"] != 4000 {
		t.Fatalf("expected 4000 go chunks, got %d", snap.ByLanguage["go"])
	}
	if snap.ByChunkType["function"] != 3000 {
		t.Fatalf("expected 3000 function chunks, got %d", snap.ByChunkType["function"])
	}
	if snap.UpdatedAt.IsZero() {
		t.Fatal("expected non-zero UpdatedAt")
	}
}

func TestCodebaseMonitor_RefreshError(t *testing.T) {
	caller := &codebaseStubCaller{
		callToolFn: func(string, map[string]any) (json.RawMessage, error) {
			return nil, fmt.Errorf("transport closed")
		},
	}

	agent := bridge.NewAgentBridge(caller)
	m := NewCodebaseMonitor(agent, slog.Default())

	err := m.Refresh()
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Snapshot should remain zero-valued.
	snap := m.Status()
	if snap.TotalChunks != 0 {
		t.Fatalf("expected 0 chunks after error, got %d", snap.TotalChunks)
	}
}

func TestCodebaseMonitor_OnRefreshCallback(t *testing.T) {
	caller := &codebaseStubCaller{
		callToolFn: func(string, map[string]any) (json.RawMessage, error) {
			return codebaseMCPResult(`"{\"repo_id\":\"loom-core\",\"collection\":\"codebase_memory_v1\",\"total_chunks\":100,\"by_language\":{\"go\":100},\"by_chunk_type\":{\"function\":60}}"`), nil
		},
	}

	agent := bridge.NewAgentBridge(caller)
	m := NewCodebaseMonitor(agent, slog.Default())

	called := false
	m.OnRefresh(func(snap CodebaseSnapshot) {
		called = true
		if snap.RepoID != "loom-core" {
			t.Errorf("expected repo_id 'loom-core', got %q", snap.RepoID)
		}
		if snap.Aggregate {
			t.Errorf("expected aggregate=false for single-repo response, got true")
		}
	})

	if err := m.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !called {
		t.Fatal("OnRefresh callback was not invoked")
	}
}
