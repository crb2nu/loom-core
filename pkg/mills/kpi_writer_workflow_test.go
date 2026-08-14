package mills

import (
	"context"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// TestKPIWriter_WorkflowCounters_BranchOnCostSource is the load-bearing test for
// the §S4a KPI requirement: workflow cost rollups MUST branch on CostSource —
// the average is over REAL-cost steps only, and estimated cost is surfaced
// separately, never blended into the average. It also pins the run/step
// counters.
//
// Seed:
//   - 1 running imperative run, 1 quarantined run.
//   - 3 success steps: two with real cost ($1.00, $3.00 over 2 real steps),
//     one with estimated cost ($10.00) that MUST NOT touch the average, plus an
//     unavailable-cost ($0) step.
//   - 1 error step + 1 gate_fail step (failed counter = 2).
//
// Expected:
//   - workflow_active_runs        = 1
//   - workflow_quarantined_runs   = 1
//   - workflow_completed_steps    = 4 (all success rows, incl. estimated/unavail)
//   - workflow_failed_steps       = 2 (error + gate_fail)
//   - workflow_avg_cost_per_step_usd = ($1+$3)/2 = $2.00  (REAL ONLY — the $10
//     estimated step is excluded from both numerator and denominator)
//   - workflow_estimated_cost_usd = $10.00 (surfaced separately)
func TestKPIWriter_WorkflowCounters_BranchOnCostSource(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now
	dao := env.store.Workflow

	started := now.Add(-time.Hour)
	ended := now.Add(-30 * time.Minute)

	mustRun := func(id string, state store.WorkflowRunState) {
		t.Helper()
		if err := dao.PutWorkflowRun(ctx, &store.WorkflowRun{
			ID: id, Engine: store.WorkflowEngineImperative, Template: "wf",
			TemplateVersion: "v0", InterpreterVersion: "h1",
			State: state, StartedAt: &started,
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
	}
	mustRun("WF-RUN", store.WorkflowRunRunning)
	mustRun("WF-Q", store.WorkflowRunQuarantined)

	mustStep := func(runID, key string, status store.WorkflowStepStatus, cost float64, src store.WorkflowCostSource) {
		t.Helper()
		if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
			RunID: runID, StepKey: key, EventType: store.WorkflowEventSpawnResult,
			CallHash: key, Status: status, StartedAt: &started, EndedAt: &ended,
			CostUSD: cost, CostSource: src, EffectCount: 1,
		}); err != nil {
			t.Fatalf("append step %s/%s: %v", runID, key, err)
		}
	}
	// Two real-cost success steps → average numerator/denominator.
	mustStep("WF-RUN", "s-real-1", store.WorkflowStepSuccess, 1.00, store.WorkflowCostReal)
	mustStep("WF-RUN", "s-real-2", store.WorkflowStepSuccess, 3.00, store.WorkflowCostReal)
	// An estimated-cost success step — MUST be excluded from the real average.
	mustStep("WF-RUN", "s-est", store.WorkflowStepSuccess, 10.00, store.WorkflowCostEstimated)
	// An unavailable-cost success step — $0, excluded from the real average.
	mustStep("WF-RUN", "s-unavail", store.WorkflowStepSuccess, 0, store.WorkflowCostUnavailable)
	// Failures.
	mustStep("WF-RUN", "s-err", store.WorkflowStepError, 0, store.WorkflowCostUnavailable)
	mustStep("WF-RUN", "s-gate", store.WorkflowStepGateFail, 0, store.WorkflowCostUnavailable)

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}

	assertMetric(t, snap.Metrics, "workflow_active_runs", float64(1))
	assertMetric(t, snap.Metrics, "workflow_quarantined_runs", float64(1))
	assertMetric(t, snap.Metrics, "workflow_completed_steps", float64(4))
	assertMetric(t, snap.Metrics, "workflow_failed_steps", float64(2))
	// THE branch assertion: average is real-only ($4 / 2 = $2), NOT
	// ($1+$3+$10+$0)/4 = $3.50 (blended) and NOT ($1+$3+$10)/3 (real+est).
	assertMetric(t, snap.Metrics, "workflow_avg_cost_per_step_usd", 2.00)
	// Estimated cost surfaced separately.
	assertMetric(t, snap.Metrics, "workflow_estimated_cost_usd", 10.00)
}

// TestKPIWriter_WorkflowAvgCost_OmittedWithoutRealSteps verifies the average is
// OMITTED (not a synthetic 0) when there are no real-cost steps — so the HUD
// renders "—" instead of a misleading "$0.00 / step".
func TestKPIWriter_WorkflowAvgCost_OmittedWithoutRealSteps(t *testing.T) {
	env := newRecEnv(t, nil)
	ctx := context.Background()
	now := env.now
	dao := env.store.Workflow

	started := now.Add(-time.Hour)
	ended := now.Add(-30 * time.Minute)
	if err := dao.PutWorkflowRun(ctx, &store.WorkflowRun{
		ID: "WF-EST", Engine: store.WorkflowEngineImperative, Template: "wf",
		TemplateVersion: "v0", InterpreterVersion: "h1",
		State: store.WorkflowRunRunning, StartedAt: &started,
	}); err != nil {
		t.Fatalf("put run: %v", err)
	}
	// Only estimated-cost steps; no real cost.
	if _, err := dao.AppendStep(ctx, &store.WorkflowStep{
		RunID: "WF-EST", StepKey: "s1", EventType: store.WorkflowEventSpawnResult,
		CallHash: "s1", Status: store.WorkflowStepSuccess, StartedAt: &started, EndedAt: &ended,
		CostUSD: 5.0, CostSource: store.WorkflowCostEstimated, EffectCount: 1,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	writer := NewKPIWriter(env.store, env.policy)
	writer.Clock = func() time.Time { return now }
	writer.Windows = []time.Duration{kpiWindow1d}
	if err := writer.Record(ctx); err != nil {
		t.Fatalf("record: %v", err)
	}
	snap, err := env.store.KPI.Latest(ctx, int(kpiWindow1d.Seconds()))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, ok := snap.Metrics["workflow_avg_cost_per_step_usd"]; ok {
		t.Errorf("workflow_avg_cost_per_step_usd must be omitted with no real steps, got %v",
			snap.Metrics["workflow_avg_cost_per_step_usd"])
	}
	// Estimated cost still surfaced.
	assertMetric(t, snap.Metrics, "workflow_estimated_cost_usd", 5.00)
}
