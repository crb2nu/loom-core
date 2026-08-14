package mobile

import (
	"strings"
	"testing"

	"github.com/crb2nu/loom/internal/hud/coordination"
)

// One codex app in workspace 1713039686, seen as a presence-only scopeless twin
// and a scoped twin that holds the live session, must collapse to one roster row.
func TestMergeMobileWorkspaceAnchoredTwins_PresenceOnlyTwinFolds(t *testing.T) {
	agentMap := map[string]*unifiedAgent{
		"codex-1713039686": {
			AgentID:       "codex-1713039686",
			AgentType:     "codex",
			Status:        "active",
			Source:        "presence",
			HasPresence:   true,
			IsOrphan:      true,
			OrphanAgeSec:  120,
			LastHeartbeat: "2026-06-16T14:17:00Z",
		},
		"codex-1713039686-2004540290": {
			AgentID:        "codex-1713039686-2004540290",
			AgentType:      "codex",
			Status:         "active",
			Source:         "session_only",
			HasSession:     true,
			SessionID:      "s-scoped",
			Namespace:      "services/flexdeck/main",
			LastHeartbeat:  "2026-06-16T14:18:00Z",
			SessionStarted: "2026-06-16T14:18:00Z",
		},
		// A genuinely-separate codex in another workspace must survive.
		"codex-389747459-3485468849": {
			AgentID:       "codex-389747459-3485468849",
			AgentType:     "codex",
			Status:        "active",
			Source:        "session_only",
			HasSession:    true,
			SessionID:     "s-other",
			LastHeartbeat: "2026-06-16T14:10:00Z",
		},
		// A claude agent in the same workspace hash must not be touched.
		"claude-code-1713039686-999": {
			AgentID:       "claude-code-1713039686-999",
			AgentType:     "claude-code",
			Status:        "active",
			Source:        "presence",
			HasPresence:   true,
			LastHeartbeat: "2026-06-16T14:19:00Z",
		},
	}

	mergeMobileWorkspaceAnchoredTwins(agentMap)

	if len(agentMap) != 3 {
		t.Fatalf("want 3 agents after merge, got %d: %v", len(agentMap), keys(agentMap))
	}
	if _, ok := agentMap["codex-1713039686"]; ok {
		t.Error("scopeless presence-only twin should have been merged away")
	}
	merged, ok := agentMap["codex-1713039686-2004540290"]
	if !ok {
		t.Fatal("session-bearing twin should remain as the merged representative")
	}
	if !merged.HasPresence || !merged.HasSession {
		t.Errorf("merged row should carry both evidences: presence=%v session=%v", merged.HasPresence, merged.HasSession)
	}
	if merged.Source != "presence+session" {
		t.Errorf("merged source = %q, want presence+session", merged.Source)
	}
	if merged.IsOrphan {
		t.Error("merged row with a live session must not be an orphan")
	}
	if merged.Namespace != "services/flexdeck/main" {
		t.Errorf("merged namespace = %q, want the session's", merged.Namespace)
	}
	if _, ok := agentMap["codex-389747459-3485468849"]; !ok {
		t.Error("other-workspace codex should survive")
	}
	if _, ok := agentMap["claude-code-1713039686-999"]; !ok {
		t.Error("claude agent must not be merged")
	}
}

// The orphan attention lane for a codex twin must be suppressed when its sibling
// holds a live session.
func TestBuildMobileAttentionLanes_SuppressesCodexOrphanWithLiveSibling(t *testing.T) {
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{
				AgentID:          "codex-1713039686",
				NeedsAttention:   true,
				AttentionReasons: []string{"orphan without session"},
			},
			{
				AgentID:   "codex-1713039686-2004540290",
				SessionID: "s-scoped",
			},
		},
	}
	lanes := buildMobileAttentionLanes(snap)
	for _, lane := range lanes {
		if id, _ := lane["id"].(string); id == "codex-1713039686" {
			t.Fatalf("orphan lane for codex twin with a live sibling should be suppressed; got lane %v", lane)
		}
	}
}

// A real orphan with no live sibling must still surface its lane.
func TestBuildMobileAttentionLanes_KeepsGenuineOrphan(t *testing.T) {
	snap := coordination.Snapshot{
		Agents: []coordination.AgentSummary{
			{
				AgentID:          "codex-555000111",
				NeedsAttention:   true,
				AttentionReasons: []string{"orphan without session"},
			},
		},
	}
	lanes := buildMobileAttentionLanes(snap)
	found := false
	for _, lane := range lanes {
		if id, _ := lane["id"].(string); id == "codex-555000111" {
			found = true
		}
	}
	if !found {
		t.Fatal("genuine orphan (no live sibling) should still produce a lane")
	}
}

func keys(m map[string]*unifiedAgent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Split(strings.Join(out, ","), ",")
}
