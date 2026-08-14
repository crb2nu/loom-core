package agentcontext

import (
	"context"
	"testing"
)

func addCandidate(t *testing.T, ps *PatternSvc, ctx context.Context, name string) string {
	t.Helper()
	r, e := ps.Add(ctx, map[string]any{"name": name, "makes": "svc"})
	got := okJSON(t, r, e)
	if got["status"] != "candidate" {
		t.Fatalf("new pattern should default to candidate, got %v", got["status"])
	}
	return got["pattern_id"].(string)
}

func recordInstance(t *testing.T, ps *PatternSvc, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	r, e := ps.RecordInstance(ctx, args)
	return okJSON(t, r, e)
}

func promotePattern(t *testing.T, ps *PatternSvc, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	r, e := ps.Promote(ctx, args)
	return okJSON(t, r, e)
}

// TestTaste_RecordInstanceAutoPromotes: a candidate auto-promotes to approved
// once it ships its first green instance (threshold = 1).
func TestTaste_RecordInstanceAutoPromotes(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()
	id := addCandidate(t, ps, ctx, "auto promote svc")

	got := recordInstance(t, ps, ctx, map[string]any{"pattern_id": id, "mr_ref": "!999"})
	if got["instances_shipped_green"] != float64(1) {
		t.Fatalf("instances_shipped_green = %v, want 1", got["instances_shipped_green"])
	}
	if got["status"] != "approved" || got["promoted"] != true {
		t.Fatalf("expected auto-promotion to approved; got status=%v promoted=%v", got["status"], got["promoted"])
	}
}

// TestTaste_PromoteBelowThresholdNeedsForce: explicit promotion of a candidate
// with no green instances is rejected unless force=true (human curation).
func TestTaste_PromoteBelowThresholdNeedsForce(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()
	id := addCandidate(t, ps, ctx, "force approve svc")

	// Auto-promote with no green instances → error.
	res, _ := ps.Promote(ctx, map[string]any{"pattern_id": id})
	if res == nil || !res.IsError {
		t.Fatalf("expected error promoting a candidate with 0 green instances, got %v", res)
	}

	// Human curation: force approve.
	got := promotePattern(t, ps, ctx, map[string]any{"pattern_id": id, "to_status": "approved", "force": true})
	if got["status"] != "approved" {
		t.Fatalf("force approve failed: %v", got["status"])
	}
}

// TestTaste_DeprecateAndRecordOnApproved: deprecating doesn't need force, and
// recording an instance on an already-approved pattern just increments.
func TestTaste_DeprecateAndRecordOnApproved(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPatternSvc()
	ctx := context.Background()
	id := addCandidate(t, ps, ctx, "lifecycle svc")

	// First green instance → approved.
	recordInstance(t, ps, ctx, map[string]any{"pattern_id": id})
	// A second instance on an approved pattern: count climbs, no re-promotion.
	got := recordInstance(t, ps, ctx, map[string]any{"pattern_id": id})
	if got["instances_shipped_green"] != float64(2) || got["status"] != "approved" || got["promoted"] != false {
		t.Fatalf("approved pattern record: count=%v status=%v promoted=%v", got["instances_shipped_green"], got["status"], got["promoted"])
	}

	// Deprecate (no force needed for a non-approval transition).
	dep := promotePattern(t, ps, ctx, map[string]any{"pattern_id": id, "to_status": "deprecated"})
	if dep["status"] != "deprecated" {
		t.Fatalf("deprecate failed: %v", dep["status"])
	}
}
