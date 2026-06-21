package agentcontext

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"gitlab.flexinfer.ai/libs/mcp-go"
)

func newTestPlanSvc() *PlanSvc {
	// qdrant=nil exercises the in-memory cache path (Qdrant-first fetch falls
	// back to the cache). The cross-agent scoping invariant is independent of
	// the backend, so this is a faithful unit-level check.
	return NewPlanSvc(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func okJSON(t *testing.T, res *mcp.CallToolResult, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res != nil && res.IsError {
		text := ""
		if len(res.Content) > 0 {
			text = res.Content[0].Text
		}
		t.Fatalf("handler returned error result: %s", text)
	}
	return readResultJSON(t, res)
}

// TestPlan_CrossAgentRetrieval is the core regression for the entity's reason
// to exist: a plan created by agent A must be retrievable WITHOUT passing any
// agent_id — i.e. recall is scoped by plan_id, never by identity. This is what
// lets a fresh subagent in another worktree (or Codex, or a Mills pod) find it.
func TestPlan_CrossAgentRetrieval(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{
		"title":     "Loom Plan Store",
		"project":   "services/loom-core",
		"namespace": "loom-core/feat-plan-store",
		"agent_id":  "claude-A",
		"slices": []any{
			map[string]any{"name": "entity", "files": []any{"pkg/agentcontext/schema_plan.go"}},
			map[string]any{"name": "tools", "goal": "expose create/get"},
		},
	})
	created := okJSON(t, res, err)

	planID, _ := created["plan_id"].(string)
	if planID == "" {
		t.Fatalf("create returned no plan_id: %v", created)
	}
	if got := created["slice_count"]; got != float64(2) {
		t.Fatalf("slice_count = %v, want 2", got)
	}

	// Retrieve with NO agent_id at all — must succeed.
	res, err = ps.Get(ctx, map[string]any{"plan_id": planID})
	got := okJSON(t, res, err)
	plan, ok := got["plan"].(map[string]any)
	if !ok {
		t.Fatalf("get returned no plan: %v", got)
	}
	if plan["title"] != "Loom Plan Store" {
		t.Fatalf("title = %v", plan["title"])
	}
	if plan["created_by"] != "claude-A" {
		t.Fatalf("created_by attribution lost: %v", plan["created_by"])
	}

	// Retrieve again passing a DIFFERENT agent_id — must return the same plan
	// (identity must not scope reads).
	res, err = ps.Get(ctx, map[string]any{"plan_id": planID, "agent_id": "codex-B"})
	got2 := okJSON(t, res, err)
	plan2 := got2["plan"].(map[string]any)
	if plan2["id"] != planID {
		t.Fatalf("cross-agent get returned different plan: %v", plan2["id"])
	}
}

func TestPlan_ListByProject(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	for _, p := range []map[string]any{
		{"title": "A", "project": "proj/x"},
		{"title": "B", "project": "proj/x"},
		{"title": "C", "project": "proj/y"},
	} {
		res, err := ps.Create(ctx, p)
		okJSON(t, res, err)
	}

	res, err := ps.List(ctx, map[string]any{"project": "proj/x"})
	got := okJSON(t, res, err)
	if got["count"] != float64(2) {
		t.Fatalf("count = %v, want 2 (project filter, not agent filter)", got["count"])
	}
}

func TestPlan_PayloadRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	orig := &Plan{
		ID:        "plan-demo-abc123",
		Slug:      "demo",
		Title:     "Demo",
		Project:   "p/q",
		Namespace: "p/q/branch",
		Phase:     PlanPhaseInProgress,
		SpecDoc:   "# spec",
		CreatedBy: "agent-x",
		CreatedAt: now,
		UpdatedAt: now,
		Slices: []PlanSlice{
			{ID: "plan-demo-abc123#1", Order: 1, Name: "s1", Files: []string{"a.go", "b.go"}, Phase: SlicePhasePending},
		},
	}

	got := payloadToPlan(planToPayload(orig))
	if got == nil {
		t.Fatal("payloadToPlan returned nil")
	}
	if got.ID != orig.ID || got.Phase != orig.Phase || got.Title != orig.Title {
		t.Fatalf("scalar mismatch: %+v", got)
	}
	if len(got.Slices) != 1 || got.Slices[0].Name != "s1" || len(got.Slices[0].Files) != 2 {
		t.Fatalf("slice round-trip failed: %+v", got.Slices)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("created_at mismatch: %v vs %v", got.CreatedAt, orig.CreatedAt)
	}
}

func TestGeneratePlanID_StableAndSlugged(t *testing.T) {
	ts := time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	a := GeneratePlanID("Loom Plan Store!", "ns", ts)
	b := GeneratePlanID("Loom Plan Store!", "ns", ts)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if want := "plan-loom-plan-store-"; a[:len(want)] != want {
		t.Fatalf("unexpected id prefix: %q", a)
	}
}
