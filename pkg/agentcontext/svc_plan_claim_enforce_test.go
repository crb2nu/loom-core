package agentcontext

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func newTestClaimSvc() *ClaimSvc {
	return NewClaimSvc(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// TestFileClaim_EnforceRejects: advisory acquire (default) still acquires on
// conflict; enforce=true refuses instead.
func TestFileClaim_EnforceRejects(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	cs := newTestClaimSvc()
	ctx := context.Background()

	res, err := cs.Acquire(ctx, map[string]any{"agent_id": "A", "session_id": "s1", "file_path": "x.go"})
	if okJSON(t, res, err)["ok"] != true {
		t.Fatal("first claim should succeed")
	}

	// Advisory (default): acquires anyway but reports conflict.
	res, err = cs.Acquire(ctx, map[string]any{"agent_id": "B", "session_id": "s2", "file_path": "x.go"})
	adv := okJSON(t, res, err)
	if adv["ok"] != true || adv["has_conflicts"] != true {
		t.Fatalf("advisory claim should acquire with has_conflicts: %v", adv)
	}

	// Enforced: refuses.
	res, err = cs.Acquire(ctx, map[string]any{"agent_id": "C", "session_id": "s3", "file_path": "x.go", "enforce": true})
	enf := okJSON(t, res, err)
	if enf["ok"] != false || enf["rejected"] != true {
		t.Fatalf("enforced claim should be rejected: %v", enf)
	}

	// Same agent re-claiming a file only it holds, under enforce, is idempotent
	// success (use a clean file — x.go now has both A and B holders advisorily).
	res, err = cs.Acquire(ctx, map[string]any{"agent_id": "A", "session_id": "s1", "file_path": "y.go"})
	if okJSON(t, res, err)["ok"] != true {
		t.Fatal("A's first claim on y.go should succeed")
	}
	res, err = cs.Acquire(ctx, map[string]any{"agent_id": "A", "session_id": "s1", "file_path": "y.go", "enforce": true})
	if okJSON(t, res, err)["ok"] != true {
		t.Fatal("same-agent enforced re-claim should succeed")
	}
}

// TestSliceClaim_EnforcesFileBoundary: two slices sharing a file cannot both be
// claimed by different agents — the second slice claim is refused.
func TestSliceClaim_EnforcesFileBoundary(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ctx := context.Background()
	claims := newTestClaimSvc()
	ps := newTestPlanSvc()
	ps.claimFiles = func(ctx context.Context, agentID, sessionID, reason string, files []string) []string {
		return claims.AcquireEnforced(ctx, agentID, sessionID, reason, files)
	}

	res, err := ps.Create(ctx, map[string]any{
		"title": "Parallel", "project": "p/x",
		"slices": []any{
			map[string]any{"name": "s1", "files": []any{"shared.go", "a.go"}},
			map[string]any{"name": "s2", "files": []any{"shared.go", "b.go"}}, // overlaps shared.go
		},
	})
	planID := okJSON(t, res, err)["plan_id"].(string)
	s1 := planID + "#1"
	s2 := planID + "#2"

	// Agent A claims slice 1 → acquires shared.go + a.go.
	res, err = ps.SliceClaim(ctx, map[string]any{"slice_id": s1, "agent_id": "A"})
	if okJSON(t, res, err)["ok"] != true {
		t.Fatal("slice 1 claim should succeed")
	}

	// Agent B claims slice 2 → conflicts on shared.go → refused.
	res, err = ps.SliceClaim(ctx, map[string]any{"slice_id": s2, "agent_id": "B"})
	got := okJSON(t, res, err)
	if got["ok"] != false || got["conflict"] != true {
		t.Fatalf("slice 2 claim should be refused on file conflict: %v", got)
	}
	cf, _ := got["conflicting_files"].([]any)
	if len(cf) != 1 || cf[0] != "shared.go" {
		t.Fatalf("expected conflicting_files=[shared.go]: %v", got["conflicting_files"])
	}

	// Slice 2 stays unclaimed (no half-claim).
	res, err = ps.SliceGet(ctx, map[string]any{"slice_id": s2})
	slice := okJSON(t, res, err)["slice"].(map[string]any)
	if slice["assigned_agent_id"] != nil && slice["assigned_agent_id"] != "" {
		t.Fatalf("slice 2 must not be assigned after refused claim: %v", slice["assigned_agent_id"])
	}
}
