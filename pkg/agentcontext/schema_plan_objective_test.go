package agentcontext

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestPlanPayload_ObjectiveRoundTrip proves Plan.Objective survives the Qdrant
// payload marshal → unmarshal cycle (the additive optional field the producer
// fills). An absent key must read back as "" (older plans, no migration).
func TestPlanPayload_ObjectiveRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &Plan{
		ID:        "plan-obj-1",
		Title:     "FamilyForge",
		Objective: "Ship a web + iOS app so a couple can co-plan their household. Slice 1 lands the shared store the rest code against; later slices layer the API and both clients on top.",
		Phase:     PlanPhaseDraft,
		CreatedAt: now,
		UpdatedAt: now,
	}
	out := payloadToPlan(planToPayload(in))
	if out == nil {
		t.Fatal("payloadToPlan returned nil")
	}
	if out.Objective != in.Objective {
		t.Errorf("objective round-trip:\n got  %q\n want %q", out.Objective, in.Objective)
	}

	// Absent key → empty (a pre-objective plan payload).
	legacy := planToPayload(in)
	delete(legacy, "objective")
	if got := payloadToPlan(legacy); got.Objective != "" {
		t.Errorf("absent objective should read as empty, got %q", got.Objective)
	}
}

// TestPlanSlicePayload_TissueRoundTrip proves the connective-tissue fields
// (depends_on / interface_contracts / acceptance_criteria) round-trip through
// the slice payload.
func TestPlanSlicePayload_TissueRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	in := &PlanSlice{
		ID:                 "plan-obj-1#2",
		PlanID:             "plan-obj-1",
		Order:              2,
		Name:               "api",
		Goal:               "wire the API",
		DependsOn:          []string{"plan-obj-1#1"},
		InterfaceContracts: "consumes FamilyStore schema; exposes REST endpoints",
		AcceptanceCriteria: "GET /households returns 200 with seeded rows",
		Phase:              SlicePhasePending,
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	out := payloadToSlice(sliceToPayload(in))
	if out == nil {
		t.Fatal("payloadToSlice returned nil")
	}
	if len(out.DependsOn) != 1 || out.DependsOn[0] != "plan-obj-1#1" {
		t.Errorf("depends_on round-trip = %v", out.DependsOn)
	}
	if out.InterfaceContracts != in.InterfaceContracts {
		t.Errorf("interface_contracts = %q", out.InterfaceContracts)
	}
	if out.AcceptanceCriteria != in.AcceptanceCriteria {
		t.Errorf("acceptance_criteria = %q", out.AcceptanceCriteria)
	}
}

// TestParsePlanSlicesArg_ResolvesDependsByName proves the producer's key move:
// depends_on entries emitted by slice NAME (the only stable key the frame has)
// are rewritten to the sibling's minted slice_id. Self-edges and unknown names
// are dropped; entries already in <plan_id>#N form pass through.
func TestParsePlanSlicesArg_ResolvesDependsByName(t *testing.T) {
	now := time.Now().UTC()
	arr := []any{
		map[string]any{"name": "schema", "goal": "define store", "interface_contracts": "publishes FamilyStore schema"},
		map[string]any{"name": "api", "goal": "wire api", "depends_on": []any{"schema"}},
		map[string]any{"name": "ui", "goal": "build ui", "depends_on": []any{"API", "schema", "ui", "ghost"}},
		map[string]any{"name": "tests", "depends_on": []any{"plan-fam#2"}}, // already an id-shaped ref → passthrough
	}
	slices := parsePlanSlicesArg(arr, "plan-fam", now)
	if len(slices) != 4 {
		t.Fatalf("slices = %d, want 4", len(slices))
	}
	// api (#2) depends on schema (#1).
	if got := slices[1].DependsOn; len(got) != 1 || got[0] != "plan-fam#1" {
		t.Errorf("api.depends_on = %v, want [plan-fam#1]", got)
	}
	// ui (#3): "API" (case-insensitive) + "schema" resolve; self "ui" and unknown
	// "ghost" drop; order preserved, deduped.
	if got := slices[2].DependsOn; len(got) != 2 || got[0] != "plan-fam#2" || got[1] != "plan-fam#1" {
		t.Errorf("ui.depends_on = %v, want [plan-fam#2 plan-fam#1]", got)
	}
	// tests (#4): an already-id-shaped ref that matches no sibling name passes
	// through untouched (backward compatible with real slice_id callers).
	if got := slices[3].DependsOn; len(got) != 1 || got[0] != "plan-fam#2" {
		t.Errorf("tests.depends_on = %v, want [plan-fam#2] passthrough", got)
	}
	if slices[0].InterfaceContracts == "" {
		t.Error("schema.interface_contracts dropped")
	}
}

// TestPlanCreate_ObjectiveAndTissue_EndToEnd drives the MCP create handler
// (cache path, no Qdrant) and reads the plan + slices back, proving the tool
// accepts + stores the objective and the resolved slice DAG end to end.
func TestPlanCreate_ObjectiveAndTissue_EndToEnd(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	vs := 0
	ps := NewPlanSvc(nil, nil, nil, &vs, logger) // nil Qdrant → write-through cache
	ctx := context.Background()

	if _, err := ps.Create(ctx, map[string]any{
		"id":        "plan-fam-e2e",
		"title":     "FamilyForge",
		"objective": "Give a couple one shared source of truth for their household.",
		"slices": []any{
			map[string]any{"name": "schema", "goal": "define the store", "interface_contracts": "publishes FamilyStore schema"},
			map[string]any{"name": "api", "goal": "wire the API", "depends_on": []any{"schema"}, "acceptance_criteria": "endpoints 200"},
			map[string]any{"name": "ui", "goal": "build the UI", "depends_on": []any{"api", "schema"}},
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	plan, _ := ps.fetch(ctx, "plan-fam-e2e")
	if plan == nil {
		t.Fatal("plan not found after create")
	}
	if plan.Objective == "" {
		t.Error("objective not stored on plan")
	}

	slices := ps.slicesForPlan(ctx, "plan-fam-e2e")
	if len(slices) != 3 {
		t.Fatalf("slices = %d, want 3", len(slices))
	}
	if got := slices[1].DependsOn; len(got) != 1 || got[0] != "plan-fam-e2e#1" {
		t.Errorf("api.depends_on = %v, want [plan-fam-e2e#1]", got)
	}
	if got := slices[2].DependsOn; len(got) != 2 || got[0] != "plan-fam-e2e#2" || got[1] != "plan-fam-e2e#1" {
		t.Errorf("ui.depends_on = %v, want [plan-fam-e2e#2 plan-fam-e2e#1]", got)
	}
	if slices[0].InterfaceContracts == "" {
		t.Error("schema.interface_contracts not stored")
	}
	if slices[1].AcceptanceCriteria == "" {
		t.Error("api.acceptance_criteria not stored")
	}
}

// TestPlanUpdate_SetsObjective proves an operator can enrich a plan authored
// before it had an objective, in place.
func TestPlanUpdate_SetsObjective(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	vs := 0
	ps := NewPlanSvc(nil, nil, nil, &vs, logger)
	ctx := context.Background()
	if _, err := ps.Create(ctx, map[string]any{"id": "plan-up-1", "title": "Sparse plan"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ps.Update(ctx, map[string]any{"plan_id": "plan-up-1", "objective": "Now it has a through-line."}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	plan, _ := ps.fetch(ctx, "plan-up-1")
	if plan == nil || plan.Objective != "Now it has a through-line." {
		t.Errorf("objective not patched: %+v", plan)
	}
}
