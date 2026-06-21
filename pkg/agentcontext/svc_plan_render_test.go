package agentcontext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenderPlanMarkdown_Sections(t *testing.T) {
	now := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)
	p := &Plan{
		ID: "plan-demo-abc123", Slug: "demo", Title: "Demo Plan",
		Project: "services/loom-core", Namespace: "loom-core/feat", Phase: PlanPhaseInProgress,
		CreatedBy: "claude-A", CreatedAt: now, UpdatedAt: now,
		RiskiestAssumption: "qdrant reachable from pods", KillTest: "create+get cross-process", KillTestStatus: "passed",
		Success:      &SuccessCriteria{Tests: []string{"go test ./..."}, ManualCheck: "open HUD"},
		Dependencies: []string{"plan-other-1"},
		MRRefs:       []string{"!748"},
		PhaseHistory: []PhaseTransition{{From: PlanPhaseDraft, To: PlanPhaseInProgress, At: now, Note: "go\nnow"}},
		SpecDoc:      "# heading\nbody text",
		Slices: []PlanSlice{
			{ID: "plan-demo-abc123#1", Order: 1, Name: "entity", Phase: SlicePhaseClaimed,
				Files: []string{"a.go"}, AssignedAgentID: "impl-A", Decisions: []string{"stubbed X"}},
		},
	}
	md := renderPlanMarkdown(p)

	for _, want := range []string{
		"# Demo Plan",
		"`plan-demo-abc123`",
		"**Phase**: in_progress",
		"## Riskiest assumption + kill-test",
		"**Status**: passed",
		"## Success criteria",
		"- Test: go test ./...",
		"## Dependencies",
		"## Lifecycle refs",
		"MRs: !748",
		"## Phase history",
		"| draft | in_progress |",
		"## Spec",
		"body text",
		"## Slices",
		"### 1. entity — `claimed`",
		"**Assignee**: impl-A",
		"**Decision**: stubbed X",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q\n---\n%s", want, md)
		}
	}
	// Note newline must be collapsed inside the table cell.
	if strings.Contains(md, "go\nnow |") {
		t.Errorf("phase-history note newline not collapsed")
	}
}

func TestRenderPlanMarkdown_MinimalPlan(t *testing.T) {
	md := renderPlanMarkdown(&Plan{ID: "plan-x-1", Slug: "x", Title: "X", Phase: PlanPhaseDraft})
	if !strings.HasPrefix(md, "# X\n") {
		t.Fatalf("unexpected header: %q", md[:20])
	}
	// Optional sections absent.
	for _, absent := range []string{"## Slices", "## Spec", "## Success criteria", "## Phase history"} {
		if strings.Contains(md, absent) {
			t.Errorf("minimal plan should not render %q", absent)
		}
	}
}

func TestPlanRender_AtomicWriteAndMirrorPath(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{"title": "Mirror Me", "project": "p/x", "spec_doc": "the body"})
	planID := okJSON(t, res, err)["plan_id"].(string)

	dir := t.TempDir()
	path := filepath.Join(dir, ".loom", "99-plan-mirror-me.md")

	res, err = ps.Render(ctx, map[string]any{"plan_id": planID, "path": path})
	out := okJSON(t, res, err)
	if out["path"] != path {
		t.Fatalf("path not echoed: %v", out["path"])
	}

	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("mirror not written: %v", rerr)
	}
	if !strings.Contains(string(data), "# Mirror Me") || !strings.Contains(string(data), "the body") {
		t.Fatalf("mirror content wrong:\n%s", data)
	}

	// mirror_path recorded on the plan.
	res, err = ps.Get(ctx, map[string]any{"plan_id": planID})
	plan := okJSON(t, res, err)["plan"].(map[string]any)
	if plan["mirror_path"] != path {
		t.Fatalf("mirror_path not recorded: %v", plan["mirror_path"])
	}

	// No leftover temp files in the dir.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".plan-") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}
