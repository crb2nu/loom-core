package agentcontext

import (
	"testing"
	"time"
)

// TestHandoff_PlanLinkageRoundTrip verifies a plan-aware handoff carries
// plan_id/slice_id through the Qdrant payload, so the receiver can resume a
// known plan scope by id instead of rebuilding from entry_ids.
func TestHandoff_PlanLinkageRoundTrip(t *testing.T) {
	now := time.Now()
	h := Handoff{
		ID:            "handoff-plan-1",
		SourceAgentID: "claude-A",
		TargetAgentID: "codex-B",
		HandoffType:   HandoffTypeSummaryOnly,
		Status:        HandoffStatusPending,
		Instructions:  "take over slice 2",
		PlanID:        "plan-demo-abc123",
		SliceID:       "plan-demo-abc123#2",
		CreatedAt:     now,
	}

	payload := handoffToPayload(h)
	if payload["plan_id"] != "plan-demo-abc123" || payload["slice_id"] != "plan-demo-abc123#2" {
		t.Fatalf("plan linkage not in payload: %v / %v", payload["plan_id"], payload["slice_id"])
	}

	got, err := payloadToHandoff(payload)
	if err != nil {
		t.Fatalf("payloadToHandoff: %v", err)
	}
	if got.PlanID != h.PlanID || got.SliceID != h.SliceID {
		t.Fatalf("plan linkage lost: plan_id=%q slice_id=%q", got.PlanID, got.SliceID)
	}
}

// TestHandoff_NoPlanLinkage: a plain handoff round-trips with empty plan fields
// (backward compatible).
func TestHandoff_NoPlanLinkage(t *testing.T) {
	got, err := payloadToHandoff(map[string]any{
		"id":              "h1",
		"source_agent_id": "a",
		"target_agent_id": "b",
		"handoff_type":    "full",
		"status":          "pending",
		"created_at":      time.Now().Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanID != "" || got.SliceID != "" {
		t.Fatalf("expected empty plan linkage, got %q/%q", got.PlanID, got.SliceID)
	}
}
