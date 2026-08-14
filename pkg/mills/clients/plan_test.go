package clients

import (
	"context"
	"encoding/json"
	"testing"

	mcp "gitlab.flexinfer.ai/libs/mcp-go"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func i64(v int64) *int64 { return &v }

func TestPlanClient_AuthorPlan_HappyPath(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffStub(t, map[string]any{"ok": true, "plan_id": "plan-mills-gl-47-123"}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "")

	item := &store.BacklogItem{
		ID:             "gl-47-123",
		GitLabIssueIID: i64(123),
		Title:          "Add retry to importer",
		State:          store.BacklogQueued,
		SpecDoc:        "## Goal\nRetry on 5xx.",
		Slices: []store.Slice{
			{Name: "client", Files: []string{"pkg/x/client.go"}},
		},
		Dependencies: []string{"gl-47-100"},
	}
	planID, err := pc.AuthorPlan(context.Background(), item, "services/loom-core")
	if err != nil {
		t.Fatalf("AuthorPlan: %v", err)
	}
	if planID != "plan-mills-gl-47-123" {
		t.Errorf("planID = %q", planID)
	}

	var params mcp.CallToolParams
	for _, m := range ft.sentMessages() {
		if m.Method == "tools/call" {
			_ = json.Unmarshal(m.Params, &params)
		}
	}
	if params.Name != "agent_plan_create" {
		t.Errorf("tool = %q, want agent_plan_create", params.Name)
	}
	if params.Arguments["title"] != "Add retry to importer" {
		t.Errorf("title = %v", params.Arguments["title"])
	}
	if params.Arguments["id"] != "plan-mills-gl-47-123" {
		t.Errorf("id = %v, want deterministic plan-mills-gl-47-123", params.Arguments["id"])
	}
	if params.Arguments["mills_backlog_id"] != "gl-47-123" {
		t.Errorf("mills_backlog_id = %v", params.Arguments["mills_backlog_id"])
	}
	if params.Arguments["project"] != "services/loom-core" {
		t.Errorf("project = %v", params.Arguments["project"])
	}
	if params.Arguments["phase"] != "planned" {
		t.Errorf("phase = %v, want planned for queued", params.Arguments["phase"])
	}
	// gitlab_issue_iid round-trips through JSON as a float64.
	if iid, _ := params.Arguments["gitlab_issue_iid"].(float64); iid != 123 {
		t.Errorf("gitlab_issue_iid = %v, want 123", params.Arguments["gitlab_issue_iid"])
	}
	if _, ok := params.Arguments["slices"].([]any); !ok {
		t.Errorf("slices not emitted as array: %T", params.Arguments["slices"])
	}
}

func TestPlanClient_AuthorPlan_AcceptsYAMLBody(t *testing.T) {
	yamlBody := "ok: true\nplan_id: plan-mills-bl-x\n"
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffRawStub(t, yamlBody),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "mills:test")
	planID, err := pc.AuthorPlan(context.Background(), &store.BacklogItem{ID: "bl-x", Title: "T"}, "")
	if err != nil {
		t.Fatalf("AuthorPlan YAML: %v", err)
	}
	if planID != "plan-mills-bl-x" {
		t.Errorf("planID = %q, want plan-mills-bl-x", planID)
	}
}

func TestPlanClient_AuthorPlan_RequiresTitle(t *testing.T) {
	hub := newTestHubClient(t, &fakeTransport{})
	pc := NewPlanClient(hub, "")
	if _, err := pc.AuthorPlan(context.Background(), &store.BacklogItem{ID: "x"}, ""); err == nil {
		t.Error("expected error when title empty")
	}
}

func TestPlanClient_AuthorPlan_ServiceFailureBubbles(t *testing.T) {
	ft := &fakeTransport{
		responses: map[string][]byte{
			"initialize": []byte(`{}`),
			"tools/call": handoffStub(t, map[string]any{"ok": false, "plan_id": ""}),
		},
	}
	hub := newTestHubClient(t, ft)
	pc := NewPlanClient(hub, "")
	if _, err := pc.AuthorPlan(context.Background(), &store.BacklogItem{ID: "x", Title: "T"}, ""); err == nil {
		t.Error("expected error when service reports ok=false with no plan_id")
	}
}

func TestBacklogItemToPlanArgs(t *testing.T) {
	item := &store.BacklogItem{
		ID:      "gl-47-9",
		Title:   "Thing",
		State:   store.BacklogRunning,
		SpecDoc: "body",
	}
	args := backlogItemToPlanArgs(item, "", "mills:x")
	if args["id"] != "plan-mills-gl-47-9" {
		t.Errorf("id = %v", args["id"])
	}
	if args["phase"] != "in_progress" {
		t.Errorf("phase = %v, want in_progress for running", args["phase"])
	}
	// project omitted when empty so it doesn't overwrite list scoping.
	if _, ok := args["project"]; ok {
		t.Errorf("project should be omitted when empty")
	}
	// gitlab_issue_iid omitted when nil.
	if _, ok := args["gitlab_issue_iid"]; ok {
		t.Errorf("gitlab_issue_iid should be omitted when nil")
	}
	// slices omitted when empty.
	if _, ok := args["slices"]; ok {
		t.Errorf("slices should be omitted when empty")
	}
}

func TestPlanIDForBacklog(t *testing.T) {
	cases := map[string]string{
		"gl-47-123":  "plan-mills-gl-47-123",
		"GL_47_123":  "plan-mills-gl-47-123",
		"  spaced  ": "plan-mills-spaced",
		"a/b:c#d":    "plan-mills-a-b-c-d",
		"":           "plan-mills-item",
		"---":        "plan-mills-item",
	}
	for in, want := range cases {
		if got := PlanIDForBacklog(in); got != want {
			t.Errorf("PlanIDForBacklog(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanPhaseForBacklogState(t *testing.T) {
	cases := map[store.BacklogState]string{
		store.BacklogQueued:    "planned",
		store.BacklogPaused:    "planned",
		store.BacklogRunning:   "in_progress",
		store.BacklogEscalated: "in_progress",
		store.BacklogMerged:    "merged",
	}
	for st, want := range cases {
		if got := planPhaseForBacklogState(st); got != want {
			t.Errorf("planPhaseForBacklogState(%q) = %q, want %q", st, got, want)
		}
	}
}

func TestDecodePlanCreateResponse(t *testing.T) {
	if p, err := decodePlanCreateResponse(`{"ok":true,"plan_id":"p1"}`); err != nil || !p.OK || p.PlanID != "p1" {
		t.Errorf("JSON decode: %+v err=%v", p, err)
	}
	if p, err := decodePlanCreateResponse("ok: true\nplan_id: p2\n"); err != nil || !p.OK || p.PlanID != "p2" {
		t.Errorf("YAML decode: %+v err=%v", p, err)
	}
	if _, err := decodePlanCreateResponse("   "); err == nil {
		t.Error("expected error for empty body")
	}
}
