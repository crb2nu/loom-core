package mobile

import (
	"testing"

	"github.com/crb2nu/loom/internal/hud/coordination"
)

// TestBuildMobileAttentionLanes_TypeContract pins the closed set of attention
// lane `type` values the mobile dashboard emits. The iOS companion's
// AttentionLaneKind
// (apps/loom-companion-ios/Sources/LoomCompanionKit/Models/AttentionLaneKind.swift)
// authors its icon/title table against exactly this set, so a new type emitted
// here without a matching Kit case would render as a generic flag on the hero
// and queue cards. The existing golden contract
// (internal/contracts/testdata/mobile_attention_lanes.golden) freezes the wire
// shape but hand-builds the lanes; this exercises the real producer.
func TestBuildMobileAttentionLanes_TypeContract(t *testing.T) {
	snapshot := coordination.Snapshot{
		Summary: coordination.Summary{
			MergeReadyBranches: 2, // -> "merge" lane
			ConflictFiles:      1, // -> "conflict" lane
		},
		Agents: []coordination.AgentSummary{
			{
				AgentID:          "claude-code-1",
				SessionID:        "sess_abc",
				Namespace:        "services/loom-core",
				NeedsAttention:   true,
				AttentionReasons: []string{"blocked on review"},
			},
		},
		Namespaces: []coordination.NamespaceSummary{
			{
				Namespace:        "services/loom-core/mobile",
				TaskCount:        3,
				BlockedTasks:     2,
				NeedsAttention:   true,
				AttentionReasons: []string{"blocked tasks"},
			},
		},
	}

	lanes := buildMobileAttentionLanes(snapshot)

	allowed := map[string]bool{
		"agent":     true,
		"namespace": true,
		"merge":     true,
		"conflict":  true,
	}
	seen := make(map[string]bool, len(allowed))
	for i, lane := range lanes {
		typ, _ := lane["type"].(string)
		if !allowed[typ] {
			t.Errorf("lane[%d] type %q is outside the mobile contract {agent, namespace, merge, conflict}; add a matching AttentionLaneKind case in iOS before emitting it", i, typ)
		}
		seen[typ] = true
	}

	for typ := range allowed {
		if !seen[typ] {
			t.Errorf("expected the snapshot to produce a %q lane but it did not (lane builder drift?)", typ)
		}
	}
}
