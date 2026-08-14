package clients

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/spin"
)

func TestPlanClient_AuthorDraftPlan_HappyPath(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffStub(t, map[string]any{"ok": true, "plan_id": "plan-spun-xyz"}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "")

	planID, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{
		Title:       "Harden the importer",
		Project:     "services/loom-core",
		Namespace:   "mills/spun",
		Priority:    "p0",
		Frame:       "opus",
		Model:       "claude-opus",
		Backend:     "flexinfer",
		Brief:       "Retry on 5xx",
		Competitors: []string{"ring", "mule"},
		Slices: []council.PlanSliceSpec{
			{Name: "client", Goal: "retry", Files: []string{"pkg/x/client.go"}},
			{Name: "", Goal: "dropped"}, // unnamed slice filtered out
		},
	})
	if err != nil {
		t.Fatalf("AuthorDraftPlan: %v", err)
	}
	if planID != "plan-spun-xyz" {
		t.Errorf("planID = %q", planID)
	}

	var params mcp.CallToolParams
	for _, m := range ft.sentMessages() {
		if m.Method == "tools/call" {
			_ = json.Unmarshal(m.Params, &params)
		}
	}
	if params.Name != "agent_plan_create" {
		t.Fatalf("tool = %q, want agent_plan_create", params.Name)
	}
	if params.Arguments["phase"] != "draft" {
		t.Errorf("phase = %v, want draft", params.Arguments["phase"])
	}
	if params.Arguments["priority"] != "P0" {
		t.Errorf("priority = %v, want normalized P0", params.Arguments["priority"])
	}
	if params.Arguments["project"] != "services/loom-core" {
		t.Errorf("project = %v", params.Arguments["project"])
	}
	if params.Arguments["namespace"] != "mills/spun" {
		t.Errorf("namespace = %v", params.Arguments["namespace"])
	}
	if params.Arguments["agent_id"] != "mills:spinning-room" {
		t.Errorf("agent_id = %v, want mills:spinning-room attribution", params.Arguments["agent_id"])
	}
	// A fresh id must NOT be sent — the store mints one so re-spins don't clobber.
	if _, ok := params.Arguments["id"]; ok {
		t.Errorf("id should be omitted so each spin is a fresh draft; got %v", params.Arguments["id"])
	}
	// Only the named slice survives.
	slices, ok := params.Arguments["slices"].([]any)
	if !ok || len(slices) != 1 {
		t.Fatalf("slices = %v, want 1 named slice", params.Arguments["slices"])
	}
	// spec_doc records the audit trail (frame + brief + competitive siblings).
	spec, _ := params.Arguments["spec_doc"].(string)
	if spec == "" || !containsAll(spec, "opus", "Retry on 5xx", "Spinning Room") {
		t.Errorf("spec_doc missing audit trail: %q", spec)
	}
	if !containsAll(spec, "Competing frames", "ring, mule") {
		t.Errorf("spec_doc missing competitive-spin siblings: %q", spec)
	}
}

// TestPlanClient_AuthorDraftPlan_ObjectiveAndTissue proves the author forwards
// the plan objective and each slice's connective tissue into the
// agent_plan_create args (depends_on emitted by NAME for the store to resolve).
func TestPlanClient_AuthorDraftPlan_ObjectiveAndTissue(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffStub(t, map[string]any{"ok": true, "plan_id": "plan-fam-1"}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "")

	if _, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{
		Title:     "FamilyForge",
		Objective: "One shared source of truth for the household.",
		Slices: []council.PlanSliceSpec{
			{Name: "schema", Goal: "define store", InterfaceContracts: "publishes FamilyStore schema"},
			{Name: "api", Goal: "wire api", DependsOn: []string{"schema"}, AcceptanceCriteria: "endpoints 200"},
		},
	}); err != nil {
		t.Fatalf("AuthorDraftPlan: %v", err)
	}

	var params mcp.CallToolParams
	for _, m := range ft.sentMessages() {
		if m.Method == "tools/call" {
			_ = json.Unmarshal(m.Params, &params)
		}
	}
	if params.Arguments["objective"] != "One shared source of truth for the household." {
		t.Errorf("objective arg = %v", params.Arguments["objective"])
	}
	slices, ok := params.Arguments["slices"].([]any)
	if !ok || len(slices) != 2 {
		t.Fatalf("slices = %v, want 2", params.Arguments["slices"])
	}
	schema, _ := slices[0].(map[string]any)
	if schema["interface_contracts"] != "publishes FamilyStore schema" {
		t.Errorf("schema.interface_contracts = %v", schema["interface_contracts"])
	}
	api, _ := slices[1].(map[string]any)
	// depends_on survives the JSON round-trip as []any.
	deps, _ := api["depends_on"].([]any)
	if len(deps) != 1 || deps[0] != "schema" {
		t.Errorf("api.depends_on = %v, want [schema]", api["depends_on"])
	}
	if api["acceptance_criteria"] != "endpoints 200" {
		t.Errorf("api.acceptance_criteria = %v", api["acceptance_criteria"])
	}
}

func TestPlanClient_AuthorDraftPlan_Validation(t *testing.T) {
	hub := newTestHubClient(t, &fakeTransport{})
	pc := NewPlanClient(hub, "")

	if _, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{Slices: []council.PlanSliceSpec{{Name: "s"}}}); err == nil {
		t.Error("expected error when title empty")
	}
	if _, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{Title: "T"}); err == nil {
		t.Error("expected error when no slices")
	}
	if _, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{Title: "T", Slices: []council.PlanSliceSpec{{Name: "  "}}}); err == nil {
		t.Error("expected error when no named slices")
	}
}

func TestPlanClient_AuthorDraftPlan_ServiceFailureBubbles(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffStub(t, map[string]any{"ok": false, "plan_id": ""}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "")
	_, err := pc.AuthorDraftPlan(context.Background(), spin.DraftPlanInput{
		Title:  "T",
		Slices: []council.PlanSliceSpec{{Name: "s"}},
	})
	if err == nil {
		t.Error("expected error when service reports ok=false with no plan_id")
	}
}

// compile-time proof the client satisfies the spinner's author interface.
var _ spin.DraftPlanAuthor = (*PlanClient)(nil)

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
