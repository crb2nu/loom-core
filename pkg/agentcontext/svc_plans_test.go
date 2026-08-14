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

// TestPlan_UpdateProjectNamespace is the regression for the mis-scoping bug: a
// plan minted with a bare project ("loom-core") must be correctable in place via
// Update (project/namespace), instead of needing a destructive re-Create. After
// the fix the plan lists under the corrected project and no longer under the
// stale one — which is what un-fragments its HUD card.
func TestPlan_UpdateProjectNamespace(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	res, err := ps.Create(ctx, map[string]any{
		"title":     "Mis-scoped",
		"project":   "loom-core", // bare (wrong) — fragments the HUD project card
		"namespace": "loom-core/feat-x",
	})
	planID := okJSON(t, res, err)["plan_id"].(string)

	// Correct the scope without a destructive re-Create.
	res, err = ps.Update(ctx, map[string]any{
		"plan_id":   planID,
		"project":   "services/loom-core",
		"namespace": "services/loom-core/feat-x",
	})
	okJSON(t, res, err)

	res, err = ps.Get(ctx, map[string]any{"plan_id": planID})
	plan := okJSON(t, res, err)["plan"].(map[string]any)
	if plan["project"] != "services/loom-core" {
		t.Fatalf("project not updated: %v", plan["project"])
	}
	if plan["namespace"] != "services/loom-core/feat-x" {
		t.Fatalf("namespace not updated: %v", plan["namespace"])
	}

	// The corrected plan now lists under the canonical project, not the bare one.
	res, err = ps.List(ctx, map[string]any{"project": "services/loom-core"})
	if got := okJSON(t, res, err)["count"]; got != float64(1) {
		t.Fatalf("corrected project list count = %v, want 1", got)
	}
	res, err = ps.List(ctx, map[string]any{"project": "loom-core"})
	if got := okJSON(t, res, err)["count"]; got != float64(0) {
		t.Fatalf("stale bare-project list count = %v, want 0", got)
	}

	// An Update that omits project/namespace must leave them untouched.
	res, err = ps.Update(ctx, map[string]any{"plan_id": planID, "title": "Renamed"})
	okJSON(t, res, err)
	res, err = ps.Get(ctx, map[string]any{"plan_id": planID})
	plan = okJSON(t, res, err)["plan"].(map[string]any)
	if plan["project"] != "services/loom-core" || plan["namespace"] != "services/loom-core/feat-x" {
		t.Fatalf("omitted project/namespace were clobbered: %v / %v", plan["project"], plan["namespace"])
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
		Priority:           PlanPriorityP1,
		SpecDoc:            "# spec",
		CreatedBy:          "agent-x",
		RespunFrom:         "plan-old-sparse-9",
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
	if got.Priority != PlanPriorityP1 {
		t.Fatalf("priority round-trip failed: %q", got.Priority)
	}
	if got.RespunFrom != "plan-old-sparse-9" {
		t.Fatalf("respun_from round-trip failed: %q", got.RespunFrom)
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

// TestPlan_PriorityBeamOrder is the warp-beam contract: List returns plans in
// dispatch order — explicit priority buckets first (P0 highest), unset last —
// and priority is settable at create, updatable, clearable, and validated.
func TestPlan_PriorityBeamOrder(t *testing.T) {
	t.Setenv("LOOM_MCP_OUTPUT_FORMAT", "json")
	ps := newTestPlanSvc()
	ctx := context.Background()

	ids := map[string]string{}
	for _, p := range []struct{ title, priority string }{
		{"beam-unset", ""},
		{"beam-p2", "P2"},
		{"beam-p0", "p0"}, // lowercase must normalize
		{"beam-p1", "P1"},
	} {
		args := map[string]any{"title": p.title, "project": "proj/beam"}
		if p.priority != "" {
			args["priority"] = p.priority
		}
		res, err := ps.Create(ctx, args)
		created := okJSON(t, res, err)
		ids[p.title] = created["plan_id"].(string)
	}

	res, err := ps.List(ctx, map[string]any{"project": "proj/beam"})
	got := okJSON(t, res, err)
	plans, _ := got["plans"].([]any)
	if len(plans) != 4 {
		t.Fatalf("expected 4 plans, got %d", len(plans))
	}
	var order []string
	for _, raw := range plans {
		m := raw.(map[string]any)
		order = append(order, m["title"].(string))
	}
	want := []string{"beam-p0", "beam-p1", "beam-p2", "beam-unset"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("beam order = %v, want %v", order, want)
		}
	}

	// Reorder: promote beam-p2 to P0; it must now lead (stable sort puts the
	// newer-updated plan first within the P0 bucket).
	res, err = ps.Update(ctx, map[string]any{"plan_id": ids["beam-p2"], "priority": "P0"})
	okJSON(t, res, err)
	res, err = ps.List(ctx, map[string]any{"project": "proj/beam"})
	got = okJSON(t, res, err)
	first := got["plans"].([]any)[0].(map[string]any)
	if first["title"] != "beam-p2" {
		t.Fatalf("after promote, first = %v, want beam-p2", first["title"])
	}

	// Clear: empty string drops it to the unset tail.
	res, err = ps.Update(ctx, map[string]any{"plan_id": ids["beam-p2"], "priority": ""})
	okJSON(t, res, err)
	res, err = ps.Get(ctx, map[string]any{"plan_id": ids["beam-p2"]})
	plan := okJSON(t, res, err)["plan"].(map[string]any)
	if pr, ok := plan["priority"]; ok && pr != "" {
		t.Fatalf("priority not cleared: %v", pr)
	}

	// Validation: junk priority is rejected on create and update.
	res, err = ps.Create(ctx, map[string]any{"title": "bad", "priority": "urgent"})
	if err != nil {
		t.Fatalf("create returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("create with invalid priority must return an error result")
	}
	res, err = ps.Update(ctx, map[string]any{"plan_id": ids["beam-p0"], "priority": "high"})
	if err != nil {
		t.Fatalf("update returned transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("update with invalid priority must return an error result")
	}
}
