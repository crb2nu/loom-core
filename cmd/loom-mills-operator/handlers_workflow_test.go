package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedWorkflowFixtures inserts two imperative runs into the operator's store:
//   - WF-LIVE: a running run with a live spawn step (effect_count=1, real cost)
//     and a replay/cache-hit step (success, effect_count=0).
//   - WF-QUAR: a quarantined run with one step.
//
// Returns the seeded run ids so callers can assert ordering / detail.
func seedWorkflowFixtures(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	dao := op.store.Workflow

	// Seed the backlog items the runs reference so the workflow_runs.backlog_id
	// FK to backlog_items resolves.
	for _, id := range []string{"MILLS-WF-1", "MILLS-WF-2"} {
		if err := op.store.Backlog.Put(ctx, &store.BacklogItem{
			ID: id, Title: id, State: store.BacklogQueued,
			Priority: store.P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", id, err)
		}
	}

	t0 := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t0.Add(2 * time.Minute)

	live := &store.WorkflowRun{
		ID: "WF-LIVE", BacklogID: "MILLS-WF-1",
		Engine: store.WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: "v0", InterpreterVersion: "h1",
		State: store.WorkflowRunRunning, StartedAt: &t0, CostUSD: 0.42,
	}
	if err := dao.PutWorkflowRun(ctx, live); err != nil {
		t.Fatalf("put live run: %v", err)
	}
	// A live spawn step: ran an effect this execution.
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-LIVE", StepKey: "spawn:0", EventType: store.WorkflowEventSpawnResult,
		CallHash: "h-spawn", Status: store.WorkflowStepSuccess, SpawnID: "spawn-abc",
		StartedAt: &t0, EndedAt: &t1, CostUSD: 0.42,
		CostSource: store.WorkflowCostReal, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append live step: %v", err)
	}
	// A cache-hit step: success but no live effect (replay short-circuit).
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-LIVE", StepKey: "gate:0", EventType: store.WorkflowEventGateEval,
		CallHash: "h-gate", Status: store.WorkflowStepSuccess,
		StartedAt: &t1, EndedAt: &t2, CostUSD: 0,
		CostSource: store.WorkflowCostUnavailable, EffectCount: 0,
	}); err != nil {
		t.Fatalf("append cache-hit step: %v", err)
	}

	quar := &store.WorkflowRun{
		ID: "WF-QUAR", BacklogID: "MILLS-WF-2",
		Engine: store.WorkflowEngineImperative, Template: "workflow-canary",
		TemplateVersion: "v0", InterpreterVersion: "h1",
		State: store.WorkflowRunQuarantined, StartedAt: &t1, CostUSD: 0.1,
	}
	if err := dao.PutWorkflowRun(ctx, quar); err != nil {
		t.Fatalf("put quarantined run: %v", err)
	}
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-QUAR", StepKey: "spawn:0", EventType: store.WorkflowEventSpawnRequested,
		CallHash: "h-q", Status: store.WorkflowStepSuccess,
		StartedAt: &t1, EndedAt: &t2, CostUSD: 0.1,
		CostSource: store.WorkflowCostReal, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append quar step: %v", err)
	}
}

// TestHandleWorkflowRunsList_Shape verifies the list endpoint returns the
// summary shape, newest-first, bounded by limit.
func TestHandleWorkflowRunsList_Shape(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	type runRow struct {
		ID        string  `json:"id"`
		BacklogID string  `json:"backlog_id"`
		Engine    string  `json:"engine"`
		Template  string  `json:"template"`
		State     string  `json:"state"`
		CostUSD   float64 `json:"cost_usd"`
		StepCount int     `json:"step_count"`
	}
	var body struct {
		Runs []runRow `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(body.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %s", len(body.Runs), rec.Body.String())
	}
	// Newest-first: WF-QUAR started at t1 > WF-LIVE at t0, so it sorts first.
	if body.Runs[0].ID != "WF-QUAR" {
		t.Errorf("ordering: want WF-QUAR first, got %q", body.Runs[0].ID)
	}
	byID := map[string]runRow{}
	for _, r := range body.Runs {
		byID[r.ID] = r
	}
	live, ok := byID["WF-LIVE"]
	if !ok {
		t.Fatal("WF-LIVE missing from list")
	}
	if live.Engine != "imperative" || live.Template != "workflow-canary" ||
		live.State != "running" || live.BacklogID != "MILLS-WF-1" || live.CostUSD != 0.42 {
		t.Errorf("WF-LIVE summary fields wrong: %+v", live)
	}
	// The list now carries a real per-run step_count (the backend follow-up to
	// the Work › Workflows fix), so the Mills table renders the count instead
	// of a permanent "—". WF-LIVE has 2 journaled steps; WF-QUAR has 1.
	if live.StepCount != 2 {
		t.Errorf("WF-LIVE step_count: want 2, got %d", live.StepCount)
	}
	if quar := byID["WF-QUAR"]; quar.StepCount != 1 {
		t.Errorf("WF-QUAR step_count: want 1, got %d", quar.StepCount)
	}
}

// TestHandleWorkflowRunsList_LimitValidation rejects a non-positive limit.
func TestHandleWorkflowRunsList_LimitValidation(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs?limit=0", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit=0: want 400, got %d", rec.Code)
	}
}

// TestHandleWorkflowRunGet_NestedStepsAndBadges verifies the detail endpoint
// returns the run + nested steps and the server-derived badge per step.
func TestHandleWorkflowRunGet_NestedStepsAndBadges(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/WF-LIVE", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			StepCount int    `json:"step_count"`
		} `json:"run"`
		Steps []struct {
			StepKey     string `json:"step_key"`
			Status      string `json:"status"`
			CostSource  string `json:"cost_source"`
			EffectCount int    `json:"effect_count"`
			Badge       string `json:"badge"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if body.Run.ID != "WF-LIVE" || body.Run.StepCount != 2 || len(body.Steps) != 2 {
		t.Fatalf("run/steps shape wrong: %+v", body)
	}
	byKey := map[string]string{} // step_key -> badge
	for _, s := range body.Steps {
		byKey[s.StepKey] = s.Badge
	}
	// spawn:0 ran a live effect (effect_count=1, success) → "live".
	if byKey["spawn:0"] != "live" {
		t.Errorf("spawn:0 badge: want live, got %q", byKey["spawn:0"])
	}
	// gate:0 succeeded with effect_count=0 → cache_hit (replay).
	if byKey["gate:0"] != "cache_hit" {
		t.Errorf("gate:0 badge: want cache_hit, got %q", byKey["gate:0"])
	}
}

// TestHandleWorkflowRunGet_QuarantinedBadge verifies that every step under a
// quarantined run is badged "quarantined" regardless of its own status.
func TestHandleWorkflowRunGet_QuarantinedBadge(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedWorkflowFixtures(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/WF-QUAR", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Steps []struct {
			Badge string `json:"badge"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Steps) != 1 {
		t.Fatalf("want 1 step, got %d", len(body.Steps))
	}
	// The step is success+effect_count=1 (would be "live"), but the run is
	// quarantined so the badge is "quarantined".
	if body.Steps[0].Badge != "quarantined" {
		t.Errorf("quarantined run step badge: want quarantined, got %q", body.Steps[0].Badge)
	}
}

// TestHandleWorkflowRunGet_NotFound returns 404 for an unknown run id.
func TestHandleWorkflowRunGet_NotFound(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/workflow/runs/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}
