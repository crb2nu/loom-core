package store

import (
	"context"
	"testing"
	"time"
)

// TestListWorkflowRuns_NewestFirstBounded verifies the list method orders
// newest-first by started_at and respects the limit fallback.
func TestListWorkflowRuns_NewestFirstBounded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 6, 9, 0, 0, 0, time.UTC)

	mk := func(id string, offset time.Duration) {
		started := base.Add(offset)
		if err := st.Workflow.PutWorkflowRun(ctx, &WorkflowRun{
			ID: id, Engine: WorkflowEngineImperative, Template: "wf",
			TemplateVersion: "v1", InterpreterVersion: "h1",
			State: WorkflowRunRunning, StartedAt: &started,
		}); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	mk("WF-A", 0)
	mk("WF-B", time.Minute)
	mk("WF-C", 2*time.Minute)

	all, err := st.Workflow.ListWorkflowRuns(ctx, 0) // 0 → default limit
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	if all[0].ID != "WF-C" || all[2].ID != "WF-A" {
		t.Fatalf("ordering wrong: %s..%s", all[0].ID, all[2].ID)
	}

	limited, err := st.Workflow.ListWorkflowRuns(ctx, 1)
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(limited) != 1 || limited[0].ID != "WF-C" {
		t.Fatalf("limit=1 wrong: %+v", limited)
	}
}

// TestStepCostRollupSince_BranchesOnCostSource verifies the rollup buckets cost
// by source and never blends real with estimated/unavailable.
func TestStepCostRollupSince_BranchesOnCostSource(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	seedWorkflowRun(t, st, "WF-COST")
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	stepTime := now.Add(-30 * time.Minute)

	add := func(key string, cost float64, src WorkflowCostSource) {
		if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
			RunID: "WF-COST", StepKey: key, EventType: WorkflowEventSpawnResult,
			CallHash: key, Status: WorkflowStepSuccess, StartedAt: &stepTime, EndedAt: &stepTime,
			CostUSD: cost, CostSource: src, EffectCount: 1,
		}); err != nil {
			t.Fatalf("append %s: %v", key, err)
		}
	}
	add("r1", 1.5, WorkflowCostReal)
	add("r2", 2.5, WorkflowCostReal)
	add("e1", 9.0, WorkflowCostEstimated)
	add("u1", 0, WorkflowCostUnavailable)

	r, err := st.Workflow.StepCostRollupSince(ctx, since)
	if err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if r.RealCostUSD != 4.0 || r.RealSteps != 2 {
		t.Errorf("real bucket: got cost=%v steps=%d want 4.0/2", r.RealCostUSD, r.RealSteps)
	}
	if r.EstimatedCostUSD != 9.0 || r.EstimatedSteps != 1 {
		t.Errorf("estimated bucket: got cost=%v steps=%d want 9.0/1", r.EstimatedCostUSD, r.EstimatedSteps)
	}
	if r.UnavailableSteps != 1 {
		t.Errorf("unavailable steps: got %d want 1", r.UnavailableSteps)
	}
}

// TestCountStepsByStatusSince_AndCountRunsByState pins the counter helpers used
// by the KPI snapshot.
func TestCountStepsByStatusSince_AndCountRunsByState(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)
	stepTime := now.Add(-30 * time.Minute)

	mkRun := func(id string, state WorkflowRunState) {
		started := now.Add(-time.Hour)
		if err := st.Workflow.PutWorkflowRun(ctx, &WorkflowRun{
			ID: id, Engine: WorkflowEngineImperative, Template: "wf",
			TemplateVersion: "v1", InterpreterVersion: "h1",
			State: state, StartedAt: &started,
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
	}
	mkRun("R-RUN", WorkflowRunRunning)
	mkRun("R-Q1", WorkflowRunQuarantined)
	mkRun("R-Q2", WorkflowRunQuarantined)

	add := func(key string, status WorkflowStepStatus) {
		if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
			RunID: "R-RUN", StepKey: key, EventType: WorkflowEventSpawnResult,
			CallHash: key, Status: status, StartedAt: &stepTime, EndedAt: &stepTime,
			CostSource: WorkflowCostReal,
		}); err != nil {
			t.Fatalf("append %s: %v", key, err)
		}
	}
	add("ok1", WorkflowStepSuccess)
	add("ok2", WorkflowStepSuccess)
	add("err1", WorkflowStepError)

	success, err := st.Workflow.CountStepsByStatusSince(ctx, WorkflowStepSuccess, since)
	if err != nil {
		t.Fatalf("count success: %v", err)
	}
	if success != 2 {
		t.Errorf("success steps: got %d want 2", success)
	}
	errs, err := st.Workflow.CountStepsByStatusSince(ctx, WorkflowStepError, since)
	if err != nil {
		t.Fatalf("count error: %v", err)
	}
	if errs != 1 {
		t.Errorf("error steps: got %d want 1", errs)
	}
	quar, err := st.Workflow.CountRunsByState(ctx, WorkflowRunQuarantined)
	if err != nil {
		t.Fatalf("count quarantined: %v", err)
	}
	if quar != 2 {
		t.Errorf("quarantined runs: got %d want 2", quar)
	}
}
