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
	// nil Qdrant/embedder exercises the in-memory cache path (Qdrant-first fetch
	// falls back to the cache). The cross-agent scoping invariant is independent
	// of the backend, so this is a faithful unit-level check.
	return NewPlanSvc(nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// TestPlan_ListSliceSummary covers the board slice-progress enrichment: List
// must attach a per-plan slice_summary (phase->count) so the HUD can draw a
// progress bar without a detail fetch per card.
func TestPlan_ListSliceSummary(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{
		"title":   "Sliced",
		"project": "proj/s",
		"slices": []any{
			map[string]any{"name": "s1", "files": []any{"a.go"}},
			map[string]any{"name": "s2", "files": []any{"b.go"}},
		},
	})
	okJSON(t, res, err)
	// A second, slice-less plan must NOT get a summary (omitempty).
	res, err = ps.Create(ctx, map[string]any{"title": "Bare", "project": "proj/s"})
	okJSON(t, res, err)

	res, err = ps.List(ctx, map[string]any{"project": "proj/s"})
	got := okJSON(t, res, err)
	plans, _ := got["plans"].([]any)
	if len(plans) != 2 {
		t.Fatalf("want 2 plans, got %d", len(plans))
	}
	var summarized int
	for _, raw := range plans {
		p := raw.(map[string]any)
		ss, ok := p["slice_summary"].(map[string]any)
		if !ok {
			continue
		}
		summarized++
		total := 0
		for _, v := range ss {
			total += int(v.(float64))
		}
		if total != 2 {
			t.Fatalf("plan %v slice_summary total = %d, want 2 (%v)", p["title"], total, ss)
		}
	}
	if summarized != 1 {
		t.Fatalf("exactly one plan should carry a slice_summary, got %d", summarized)
	}
}

func TestPlan_PayloadRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	orig := &Plan{
		ID:                 "plan-demo-abc123",
		Slug:               "demo",
		Title:              "Demo",
		Project:            "p/q",
		Namespace:          "p/q/branch",
		Phase:              PlanPhaseInProgress,
		SpecDoc:            "# spec",
		CreatedBy:          "agent-x",
		RiskiestAssumption: "qdrant reachable from pods",
		KillTestStatus:     "passed",
		Dependencies:       []string{"plan-other-1"},
		MRRefs:             []string{"!747"},
		Success:            &SuccessCriteria{Tests: []string{"go test ./..."}, ManualCheck: "open HUD"},
		PhaseHistory:       []PhaseTransition{{From: PlanPhaseDraft, To: PlanPhaseInProgress, At: now}},
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	got := payloadToPlan(planToPayload(orig))
	if got == nil {
		t.Fatal("payloadToPlan returned nil")
	}
	if got.ID != orig.ID || got.Phase != orig.Phase || got.Title != orig.Title {
		t.Fatalf("scalar mismatch: %+v", got)
	}
	if got.Success == nil || len(got.Success.Tests) != 1 || got.Success.ManualCheck != "open HUD" {
		t.Fatalf("success round-trip failed: %+v", got.Success)
	}
	if len(got.MRRefs) != 1 || got.MRRefs[0] != "!747" {
		t.Fatalf("mr_refs round-trip failed: %+v", got.MRRefs)
	}
	if len(got.PhaseHistory) != 1 || got.PhaseHistory[0].To != PlanPhaseInProgress {
		t.Fatalf("phase_history round-trip failed: %+v", got.PhaseHistory)
	}
	if !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("created_at mismatch: %v vs %v", got.CreatedAt, orig.CreatedAt)
	}
}

func TestSlice_PayloadRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	orig := &PlanSlice{
		ID: "plan-demo-abc123#1", PlanID: "plan-demo-abc123", Order: 1,
		Name: "s1", Files: []string{"a.go", "b.go"}, Phase: SlicePhaseClaimed,
		AssignedAgentID: "claude-A", DependsOn: []string{"plan-demo-abc123#0"},
		Decisions: []string{"stubbed iface X"}, CreatedAt: now, UpdatedAt: now,
	}
	got := payloadToSlice(sliceToPayload(orig))
	if got == nil || got.ID != orig.ID || got.PlanID != orig.PlanID {
		t.Fatalf("slice scalar mismatch: %+v", got)
	}
	if len(got.Files) != 2 || got.Phase != SlicePhaseClaimed || got.AssignedAgentID != "claude-A" {
		t.Fatalf("slice round-trip failed: %+v", got)
	}
	if len(got.Decisions) != 1 || got.Decisions[0] != "stubbed iface X" {
		t.Fatalf("decisions round-trip failed: %+v", got.Decisions)
	}
}

func TestPlan_LifecycleAdvance(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{"title": "LC", "phase": "draft"})
	planID := okJSON(t, res, err)["plan_id"].(string)

	// Legal: draft -> in_progress
	res, err = ps.LifecycleAdvance(ctx, map[string]any{"plan_id": planID, "to_phase": "in_progress", "note": "go"})
	got := okJSON(t, res, err)
	if got["to_phase"] != "in_progress" || got["from_phase"] != "draft" {
		t.Fatalf("transition result wrong: %v", got)
	}

	// Illegal: in_progress -> merged (must go through review/merging)
	res, err = ps.LifecycleAdvance(ctx, map[string]any{"plan_id": planID, "to_phase": "merged"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected illegal-transition error, got %v", res)
	}

	// History recorded.
	res, err = ps.Get(ctx, map[string]any{"plan_id": planID})
	plan := okJSON(t, res, err)["plan"].(map[string]any)
	hist, _ := plan["phase_history"].([]any)
	if len(hist) != 1 {
		t.Fatalf("phase_history len = %d, want 1", len(hist))
	}
}

func TestPlanSlice_AddClaimUpdate(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{"title": "Parallel", "project": "p/x"})
	planID := okJSON(t, res, err)["plan_id"].(string)

	res, err = ps.SliceAdd(ctx, map[string]any{"plan_id": planID, "name": "slice-1", "files": []any{"a.go"}})
	sliceID := okJSON(t, res, err)["slice_id"].(string)

	// Fresh "implementer" claims it.
	res, err = ps.SliceClaim(ctx, map[string]any{"slice_id": sliceID, "agent_id": "impl-A", "worktree_id": "wt-1"})
	claimed := okJSON(t, res, err)
	if claimed["ok"] != true || claimed["phase"] != "claimed" {
		t.Fatalf("claim failed: %v", claimed)
	}

	// A different agent is rejected without force.
	res, err = ps.SliceClaim(ctx, map[string]any{"slice_id": sliceID, "agent_id": "impl-B"})
	conflict := okJSON(t, res, err)
	if conflict["conflict"] != true || conflict["held_by"] != "impl-A" {
		t.Fatalf("expected conflict held_by impl-A: %v", conflict)
	}

	// Implementer records a decision + advances phase.
	res, err = ps.SliceUpdate(ctx, map[string]any{"slice_id": sliceID, "phase": "implemented", "add_decision": "used iface stub"})
	okJSON(t, res, err)

	res, err = ps.SliceGet(ctx, map[string]any{"slice_id": sliceID})
	slice := okJSON(t, res, err)["slice"].(map[string]any)
	if slice["phase"] != "implemented" {
		t.Fatalf("phase not persisted: %v", slice["phase"])
	}
	decisions, _ := slice["decisions"].([]any)
	if len(decisions) != 1 {
		t.Fatalf("decision not anchored to slice: %v", slice["decisions"])
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
