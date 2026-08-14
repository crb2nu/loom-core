package council

import (
	"testing"
	"time"
)

func TestEvaluateStalePlan_PassesFreshPlan(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	got := EvaluateStalePlan(StalePlanPolicy{Now: func() time.Time { return now }}, PlanningContext{
		PlanID:    "plan-1",
		Phase:     "planned",
		UpdatedAt: now.Add(-2 * time.Hour),
		Slices: []PlanSliceContext{
			{ID: "s1", Phase: "pending", UpdatedAt: now.Add(-time.Hour)},
			{ID: "s2", Phase: "merged", UpdatedAt: now.Add(-30 * 24 * time.Hour)},
		},
	})
	if !got.Pass || got.Code != StalePlanCodeOK || got.Action != PolicyActionNone {
		t.Fatalf("fresh plan verdict = %+v", got)
	}
	if got.Metrics["active_slices"] != 1 {
		t.Fatalf("active_slices metric = %v", got.Metrics["active_slices"])
	}
}

func TestEvaluateStalePlan_FailsStalePlanContext(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	got := EvaluateStalePlan(StalePlanPolicy{Now: func() time.Time { return now }}, PlanningContext{
		PlanID:    "plan-1",
		Phase:     "planned",
		UpdatedAt: now.Add(-15 * 24 * time.Hour),
	})
	if got.Pass {
		t.Fatalf("stale plan should fail: %+v", got)
	}
	if got.Code != StalePlanCodePlanStale || got.Severity != PolicySeverityCritical || got.Action != PolicyActionRefresh {
		t.Fatalf("unexpected stale plan verdict: %+v", got)
	}
}

func TestEvaluateStalePlan_FailsSliceBacklog(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	got := EvaluateStalePlan(StalePlanPolicy{
		Now:              func() time.Time { return now },
		MaxPendingSlices: 1,
	}, PlanningContext{
		Phase:     "planned",
		UpdatedAt: now,
		Slices: []PlanSliceContext{
			{ID: "s1", Phase: "pending", UpdatedAt: now},
			{ID: "s2", Phase: "in_progress", UpdatedAt: now},
		},
	})
	if got.Code != StalePlanCodeSliceBacklog || got.Action != PolicyActionEscalate {
		t.Fatalf("slice backlog verdict = %+v", got)
	}
}
