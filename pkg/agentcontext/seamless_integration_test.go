package agentcontext

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// M1: Session Lifecycle Hardening
// ---------------------------------------------------------------------------

func TestTokenBudgetForPlatform(t *testing.T) {
	t.Parallel()

	const fallback = 4000

	tests := []struct {
		name            string
		platformBudgets map[string]int
		agentType       string
		want            int
	}{
		{
			name:            "known platform returns configured budget",
			platformBudgets: map[string]int{"claude-code": 8000, "gemini": 6000},
			agentType:       "claude-code",
			want:            8000,
		},
		{
			name:            "unknown platform falls back to DefaultTokenBudget",
			platformBudgets: map[string]int{"claude-code": 8000},
			agentType:       "unknown-agent",
			want:            fallback,
		},
		{
			name:            "empty agentType falls back to DefaultTokenBudget",
			platformBudgets: map[string]int{"claude-code": 8000},
			agentType:       "",
			want:            fallback,
		},
		{
			name:            "nil PlatformBudgets falls back to DefaultTokenBudget",
			platformBudgets: nil,
			agentType:       "claude-code",
			want:            fallback,
		},
		{
			name:            "empty PlatformBudgets falls back to DefaultTokenBudget",
			platformBudgets: map[string]int{},
			agentType:       "claude-code",
			want:            fallback,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{
				DefaultTokenBudget: fallback,
				PlatformBudgets:    tc.platformBudgets,
			}
			got := cfg.TokenBudgetForPlatform(tc.agentType)
			if got != tc.want {
				t.Errorf("TokenBudgetForPlatform(%q) = %d, want %d", tc.agentType, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// M3: Workflow Reliability — Graph BFS depth limiting
// ---------------------------------------------------------------------------

// buildEntityChain creates a linear chain of n entities (A, B, C, ...) and
// connects each adjacent pair with a RelationDependsOn relation.
func buildEntityChain(t *testing.T, g *KnowledgeGraph, n int) []string {
	t.Helper()

	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := string(rune('A' + i))
		if i >= 26 {
			id = string(rune('A'+i/26-1)) + string(rune('A'+i%26))
		}
		ids[i] = id
		if err := g.AddEntity(&Entity{
			ID:   id,
			Name: "entity-" + id,
			Type: EntityTypeFile,
		}); err != nil {
			t.Fatalf("AddEntity(%s): %v", id, err)
		}
	}

	for i := 0; i < n-1; i++ {
		if err := g.AddRelation(&Relation{
			ID:       fmt.Sprintf("rel-%s-%s", ids[i], ids[i+1]),
			SourceID: ids[i],
			TargetID: ids[i+1],
			Type:     RelationDependsOn,
		}); err != nil {
			t.Fatalf("AddRelation(%s→%s): %v", ids[i], ids[i+1], err)
		}
	}

	return ids
}

func TestGraphQuery_DepthLimit(t *testing.T) {
	t.Parallel()

	g := NewKnowledgeGraph()
	ids := buildEntityChain(t, g, 12) // A through L

	// Query from A with maxDepth=100 — should cap at 10.
	result, err := g.Query(GraphQuery{
		EntityID: ids[0],
		MaxDepth: 100,
	})
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}

	// Expect at most 11 entities (start + 10 hops).
	if len(result.Entities) > 11 {
		t.Errorf("expected at most 11 entities (depth cap 10), got %d", len(result.Entities))
	}
	if !result.Truncated {
		t.Error("expected Truncated=true when chain exceeds depth cap")
	}

	// Query with maxDepth=2 — should return exactly 3 entities (A, B, C).
	result2, err := g.Query(GraphQuery{
		EntityID: ids[0],
		MaxDepth: 2,
	})
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}

	if len(result2.Entities) != 3 {
		t.Errorf("expected 3 entities with maxDepth=2, got %d", len(result2.Entities))
	}
}

func TestGraphQuery_DepthLimitSetsTruncated(t *testing.T) {
	t.Parallel()

	g := NewKnowledgeGraph()
	buildEntityChain(t, g, 5) // A through E

	result, err := g.Query(GraphQuery{
		EntityID: "A",
		MaxDepth: 2,
	})
	if err != nil {
		t.Fatalf("Query returned error: %v", err)
	}

	// With 5 entities and maxDepth=2, only A/B/C are returned — D/E are beyond.
	if !result.Truncated {
		t.Error("expected Truncated=true when nodes remain beyond maxDepth")
	}
}

func TestFindPath_DepthLimit(t *testing.T) {
	t.Parallel()

	g := NewKnowledgeGraph()
	ids := buildEntityChain(t, g, 12) // A through L

	// FindPath from A to L with maxDepth=100 — should cap at 10.
	// Since L is 11 hops away (exceeds cap of 10), path should not be found.
	path, err := g.FindPath(ids[0], ids[11], 100, nil)
	if err == nil && path != nil {
		// If a path was found despite the cap, it should not exceed 11 nodes.
		if len(path.Nodes) > 11 {
			t.Errorf("FindPath exceeded depth cap: got %d nodes", len(path.Nodes))
		}
	}
	// It is acceptable for err to be non-nil ("no path found") due to depth cap.

	// FindPath from A to E (4 hops) with maxDepth=4 — should succeed.
	path2, err := g.FindPath(ids[0], ids[4], 4, nil)
	if err != nil {
		t.Fatalf("FindPath(A→E, maxDepth=4) returned error: %v", err)
	}
	if path2 == nil {
		t.Fatal("FindPath(A→E, maxDepth=4) returned nil path")
	}
	if len(path2.Nodes) != 5 {
		t.Errorf("expected 5 nodes in path A→E, got %d", len(path2.Nodes))
	}
}

func TestFindPath_SelfPath(t *testing.T) {
	t.Parallel()

	g := NewKnowledgeGraph()
	if err := g.AddEntity(&Entity{
		ID:   "self",
		Name: "self-entity",
		Type: EntityTypeFile,
	}); err != nil {
		t.Fatalf("AddEntity: %v", err)
	}

	path, err := g.FindPath("self", "self", 10, nil)
	if err != nil {
		t.Fatalf("FindPath(self→self) returned error: %v", err)
	}
	if path == nil {
		t.Fatal("FindPath(self→self) returned nil path")
	}
	if len(path.Nodes) != 1 {
		t.Errorf("expected 1 node in self-path, got %d", len(path.Nodes))
	}
	if len(path.Edges) != 0 {
		t.Errorf("expected 0 edges in self-path, got %d", len(path.Edges))
	}
	if path.Length != 0 {
		t.Errorf("expected length 0 for self-path, got %d", path.Length)
	}
}

// ---------------------------------------------------------------------------
// M4: Memory & Compaction Polish
// ---------------------------------------------------------------------------

func TestMemoryHierarchy_PromoteItem(t *testing.T) {
	t.Parallel()

	h := NewMemoryHierarchy()
	if err := h.AddItem(&MemoryItem{
		ID:         "promote-test",
		Title:      "promote test",
		Content:    "test content",
		Tier:       MemoryTierWorking,
		Status:     MemoryItemStatusActive,
		Importance: ImportanceLevelMedium,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	// First promotion: working → short-term.
	if err := h.PromoteItem("promote-test"); err != nil {
		t.Fatalf("first PromoteItem returned error: %v", err)
	}
	got, err := h.GetItem("promote-test")
	if err != nil {
		t.Fatalf("GetItem after first promotion: %v", err)
	}
	if got.Tier != MemoryTierShortTerm {
		t.Errorf("expected tier %s after first promotion, got %s", MemoryTierShortTerm, got.Tier)
	}

	// Second promotion: short-term → long-term.
	if err := h.PromoteItem("promote-test"); err != nil {
		t.Fatalf("second PromoteItem returned error: %v", err)
	}
	got, err = h.GetItem("promote-test")
	if err != nil {
		t.Fatalf("GetItem after second promotion: %v", err)
	}
	if got.Tier != MemoryTierLongTerm {
		t.Errorf("expected tier %s after second promotion, got %s", MemoryTierLongTerm, got.Tier)
	}

	// Third promotion: already in long-term → error.
	err = h.PromoteItem("promote-test")
	if err == nil {
		t.Error("expected error when promoting from long-term, got nil")
	} else if !strings.Contains(err.Error(), "long-term") && !strings.Contains(err.Error(), "long_term") {
		t.Errorf("expected error mentioning long-term tier, got: %v", err)
	}
}

func TestMemoryHierarchy_DemoteItem(t *testing.T) {
	t.Parallel()

	h := NewMemoryHierarchy()
	if err := h.AddItem(&MemoryItem{
		ID:         "demote-test",
		Title:      "demote test",
		Content:    "test content",
		Tier:       MemoryTierLongTerm,
		Status:     MemoryItemStatusActive,
		Importance: ImportanceLevelMedium,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}

	// First demotion: long-term → short-term.
	if err := h.DemoteItem("demote-test"); err != nil {
		t.Fatalf("first DemoteItem returned error: %v", err)
	}
	got, err := h.GetItem("demote-test")
	if err != nil {
		t.Fatalf("GetItem after first demotion: %v", err)
	}
	if got.Tier != MemoryTierShortTerm {
		t.Errorf("expected tier %s after first demotion, got %s", MemoryTierShortTerm, got.Tier)
	}

	// Second demotion: short-term → working.
	if err := h.DemoteItem("demote-test"); err != nil {
		t.Fatalf("second DemoteItem returned error: %v", err)
	}
	got, err = h.GetItem("demote-test")
	if err != nil {
		t.Fatalf("GetItem after second demotion: %v", err)
	}
	if got.Tier != MemoryTierWorking {
		t.Errorf("expected tier %s after second demotion, got %s", MemoryTierWorking, got.Tier)
	}

	// Third demotion: already in working → error.
	err = h.DemoteItem("demote-test")
	if err == nil {
		t.Error("expected error when demoting from working, got nil")
	} else if !strings.Contains(err.Error(), "working") {
		t.Errorf("expected error mentioning 'working', got: %v", err)
	}
}

func TestMemoryHierarchy_CompactingFlag(t *testing.T) {
	t.Parallel()

	h := NewMemoryHierarchy()

	if h.IsCompacting() {
		t.Error("expected IsCompacting()=false on fresh hierarchy")
	}

	// RunCompression on an empty tier runs synchronously and should leave
	// the compacting flag as false when done.
	_, _ = h.RunCompression(MemoryTierWorking)

	if h.IsCompacting() {
		t.Error("expected IsCompacting()=false after synchronous RunCompression on empty tier")
	}
}

// ---------------------------------------------------------------------------
// Presence Reliability: auto-register on heartbeat
// ---------------------------------------------------------------------------

func newTestPresenceSvc(t *testing.T) *PresenceSvc {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewPresenceSvc(nil, Config{PresenceHeartbeatTTL: 120}, logger, nil)
}

func TestPresenceHeartbeat_AutoRegistersUnknownAgent(t *testing.T) {
	t.Parallel()

	svc := newTestPresenceSvc(t)

	// Heartbeat for an unregistered agent should auto-register instead of failing.
	result, err := svc.Heartbeat(context.Background(), map[string]any{
		"agent_id":   "auto-reg-test",
		"agent_type": "claude-code",
		"status":     "active",
	})
	if err != nil {
		t.Fatalf("Heartbeat returned unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.IsError {
		t.Errorf("expected success, got error: %s", result.Content[0].Text)
	}

	// Verify the agent is now registered.
	presence := svc.Get("auto-reg-test")
	if presence == nil {
		t.Fatal("expected agent to be auto-registered after heartbeat")
	}
	if presence.AgentType != "claude-code" {
		t.Errorf("expected agent_type 'claude-code', got %q", presence.AgentType)
	}
	if presence.Status != PresenceStatusActive {
		t.Errorf("expected status 'active', got %q", presence.Status)
	}
}

func TestPresenceHeartbeat_AutoRegisteredFlag(t *testing.T) {
	t.Parallel()

	svc := newTestPresenceSvc(t)

	// Heartbeat for an unregistered agent should include auto_registered=true.
	result, err := svc.Heartbeat(context.Background(), map[string]any{
		"agent_id": "flag-test",
		"status":   "active",
	})
	if err != nil {
		t.Fatalf("Heartbeat returned unexpected error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatal("expected success result")
	}

	// Check auto_registered flag in result text (may be YAML or JSON formatted).
	resultText := result.Content[0].Text
	if !strings.Contains(resultText, "auto_registered") {
		t.Errorf("expected auto_registered in result, got: %s", resultText)
	}
	if !strings.Contains(resultText, "true") {
		t.Errorf("expected auto_registered=true in result, got: %s", resultText)
	}
}

func TestPresenceHeartbeat_PreRegisteredAgent_NoAutoRegister(t *testing.T) {
	t.Parallel()

	svc := newTestPresenceSvc(t)

	// Register the agent first.
	_, err := svc.Register(context.Background(), map[string]any{
		"agent_id":   "pre-reg-test",
		"agent_type": "codex",
	})
	if err != nil {
		t.Fatalf("Register returned unexpected error: %v", err)
	}

	// Heartbeat for a registered agent should succeed without auto-register.
	result, err := svc.Heartbeat(context.Background(), map[string]any{
		"agent_id": "pre-reg-test",
		"status":   "active",
	})
	if err != nil {
		t.Fatalf("Heartbeat returned unexpected error: %v", err)
	}
	if result == nil || result.IsError {
		t.Fatal("expected success result")
	}

	// Should NOT contain auto_registered flag.
	resultText := result.Content[0].Text
	if strings.Contains(resultText, "auto_registered") {
		t.Errorf("expected no auto_registered flag for pre-registered agent, got: %s", resultText)
	}
}

func TestPresenceHeartbeat_AutoRegisterUpdatesTypeOnExisting(t *testing.T) {
	t.Parallel()

	svc := newTestPresenceSvc(t)

	// Register without agent_type.
	_, _ = svc.Register(context.Background(), map[string]any{
		"agent_id": "type-update-test",
	})

	presence := svc.Get("type-update-test")
	if presence == nil {
		t.Fatal("expected agent to be registered")
	}
	if presence.AgentType != "" {
		t.Errorf("expected empty agent_type initially, got %q", presence.AgentType)
	}

	// Heartbeat with agent_type should backfill the type on existing presence.
	_, err := svc.Heartbeat(context.Background(), map[string]any{
		"agent_id":   "type-update-test",
		"agent_type": "gemini",
		"status":     "active",
	})
	if err != nil {
		t.Fatalf("Heartbeat returned error: %v", err)
	}

	presence = svc.Get("type-update-test")
	if presence.AgentType != "gemini" {
		t.Errorf("expected agent_type 'gemini' after backfill, got %q", presence.AgentType)
	}
}
