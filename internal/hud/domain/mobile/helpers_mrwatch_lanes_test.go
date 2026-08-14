package mobile

import "testing"

// TestBuildMRWatchAttentionLanes_TypeContract pins that the mrwatch M5 lanes
// stay inside the closed mobile attention-lane contract {agent, namespace,
// merge, conflict} (project_mobile_attention_lane_contract). A conflicted MR
// maps to the dedicated `conflict` lane; every other unhealthy state collapses
// to `merge` — classify by TYPE first, so the iOS AttentionLaneKind table
// renders them and the `merge` lane doesn't leak into the generic work bucket.
func TestBuildMRWatchAttentionLanes_TypeContract(t *testing.T) {
	items := []MergeAttentionItem{
		{Repo: "services/loom-core", IID: 42, Branch: "feat/x", State: "conflict", Lane: "conflict", Reason: "merge_conflict", WebURL: "https://gl/mr/42", Severity: "critical"},
		{Repo: "services/loom-core", IID: 43, Branch: "feat/y", State: "ci_failed_flaky", Lane: "merge", Reason: "flaky", WebURL: "https://gl/mr/43", Severity: "warning"},
		{Repo: "services/loom-core", IID: 44, Branch: "feat/z", State: "automerge_unarmed", Lane: "merge", Severity: "warning"},
	}

	lanes := buildMRWatchAttentionLanes(items)
	if len(lanes) != 3 {
		t.Fatalf("lanes = %d, want 3", len(lanes))
	}

	allowed := map[string]bool{"agent": true, "namespace": true, "merge": true, "conflict": true}
	for i, lane := range lanes {
		typ, _ := lane["type"].(string)
		if !allowed[typ] {
			t.Errorf("lane[%d] type %q outside the mobile contract {agent, namespace, merge, conflict}", i, typ)
		}
		// Required wire fields the iOS card renderer reads.
		for _, key := range []string{"id", "label", "route", "summary", "severity", "target_kind", "deep_link"} {
			if _, ok := lane[key]; !ok {
				t.Errorf("lane[%d] (%s) missing required field %q", i, typ, key)
			}
		}
		if fr, ok := lane["freshness"].(map[string]any); !ok || fr["source"] != "mrwatch" {
			t.Errorf("lane[%d] freshness.source = %v, want mrwatch", i, lane["freshness"])
		}
	}

	// The conflicted MR must land in the conflict lane; deep link carries the URL.
	if lanes[0]["type"] != "conflict" {
		t.Errorf("lane[0] type = %v, want conflict", lanes[0]["type"])
	}
	if lanes[0]["deep_link"] != "https://gl/mr/42" {
		t.Errorf("lane[0] deep_link = %v, want the MR web URL", lanes[0]["deep_link"])
	}
	if lanes[0]["id"] != "mr:services/loom-core!42" {
		t.Errorf("lane[0] id = %v, want mr:services/loom-core!42", lanes[0]["id"])
	}
}

// TestBuildMRWatchAttentionLanes_Empty verifies an empty input yields a
// non-nil, empty slice (so the dashboard append is safe and JSON-clean).
func TestBuildMRWatchAttentionLanes_Empty(t *testing.T) {
	lanes := buildMRWatchAttentionLanes(nil)
	if lanes == nil {
		t.Fatal("lanes must be non-nil")
	}
	if len(lanes) != 0 {
		t.Fatalf("lanes = %d, want 0", len(lanes))
	}
}

// TestBuildMRWatchAttentionLanes_DefensiveType ensures an unclassified/blank
// lane type collapses to `merge` rather than leaking outside the contract.
func TestBuildMRWatchAttentionLanes_DefensiveType(t *testing.T) {
	lanes := buildMRWatchAttentionLanes([]MergeAttentionItem{
		{Repo: "r", IID: 1, State: "stale_branch", Lane: "", Severity: ""},
	})
	if len(lanes) != 1 {
		t.Fatalf("lanes = %d, want 1", len(lanes))
	}
	if lanes[0]["type"] != "merge" {
		t.Errorf("blank lane type = %v, want merge (defensive collapse)", lanes[0]["type"])
	}
	if lanes[0]["severity"] != "warning" {
		t.Errorf("blank severity = %v, want warning (default)", lanes[0]["severity"])
	}
}
