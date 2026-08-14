package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/council"
	"github.com/crb2nu/loom/pkg/mills/store"
	"github.com/crb2nu/loom/pkg/telemetry"
)

type fakeBreakerPolicy struct {
	p *mills.Policy
}

func (f fakeBreakerPolicy) Current() *mills.Policy { return f.p }

type fakeKPIReader struct {
	snap *store.KPISnapshot
	err  error
}

func (f fakeKPIReader) Latest(context.Context, int) (*store.KPISnapshot, error) {
	return f.snap, f.err
}

func TestBreakerEvaluatorAllowsHealthyPolicyKPIAndInfrastructure(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	eval := BreakerEvaluator{
		Policy: fakeBreakerPolicy{p: &mills.Policy{}},
		KPI: fakeKPIReader{snap: &store.KPISnapshot{
			SnapshotAt:    now.Add(-time.Minute),
			WindowSeconds: 86400,
			Metrics: map[string]any{
				"policy_enabled":       true,
				"pipeline_merged_real": 2,
				"regression_rate":      0.01,
				"gate_pass_rate":       0.97,
			},
		}},
		Infrastructure: InfrastructureHealthFunc(func(context.Context) ([]InfrastructureHealth, error) {
			return []InfrastructureHealth{
				{ID: "hud_spawn", Status: "green", Mode: "real", RequiredForAutonomy: true},
				{ID: "kpi_writer", Status: "yellow", Mode: "disabled", RequiredForAutonomy: false},
			}, nil
		}),
		Thresholds: BreakerThresholds{
			MinRealMerges:     1,
			MaxRegressionRate: 0.05,
			MinGatePassRate:   0.95,
		},
		Clock: func() time.Time { return now },
	}

	got := eval.CheckAutonomy(context.Background())
	if !got.Allowed {
		t.Fatalf("Allowed = false, blockers=%v", got.Blockers)
	}
}

func TestBreakerEvaluatorFailsClosedWhenPolicyDisabled(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	enabled := false
	eval := BreakerEvaluator{
		Policy: fakeBreakerPolicy{p: &mills.Policy{Enabled: &enabled}},
		KPI: fakeKPIReader{snap: &store.KPISnapshot{
			SnapshotAt: now.Add(-time.Minute),
			Metrics:    map[string]any{"policy_enabled": true},
		}},
		Clock: func() time.Time { return now },
	}

	got := eval.CheckAutonomy(context.Background())
	if got.Allowed {
		t.Fatal("Allowed = true, want blocked")
	}
	if got.Code != "policy_disabled" {
		t.Fatalf("Code = %q, want policy_disabled", got.Code)
	}
	if !containsBlocker(got.Blockers, "policy.enabled=false") {
		t.Fatalf("Blockers = %v, want policy.enabled=false", got.Blockers)
	}
}

func TestBreakerEvaluatorFailsClosedOnMissingOrStaleKPI(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		kpi  fakeKPIReader
		want string
	}{
		{
			name: "read error",
			kpi:  fakeKPIReader{err: errors.New("store down")},
			want: "kpi_unavailable",
		},
		{
			name: "nil snapshot",
			kpi:  fakeKPIReader{},
			want: "kpi_unavailable",
		},
		{
			name: "stale snapshot",
			kpi: fakeKPIReader{snap: &store.KPISnapshot{
				SnapshotAt: now.Add(-3 * time.Hour),
				Metrics:    map[string]any{"policy_enabled": true},
			}},
			want: "kpi_stale",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := BreakerEvaluator{
				Policy: fakeBreakerPolicy{p: &mills.Policy{}},
				KPI:    tt.kpi,
				Clock:  func() time.Time { return now },
			}
			got := eval.CheckAutonomy(context.Background())
			if got.Allowed {
				t.Fatal("Allowed = true, want blocked")
			}
			if got.Code != tt.want {
				t.Fatalf("Code = %q, want %q; blockers=%v", got.Code, tt.want, got.Blockers)
			}
		})
	}
}

func TestBreakerEvaluatorBlocksOnKPIThresholds(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	eval := BreakerEvaluator{
		Policy: fakeBreakerPolicy{p: &mills.Policy{}},
		KPI: fakeKPIReader{snap: &store.KPISnapshot{
			SnapshotAt: now.Add(-time.Minute),
			Metrics: map[string]any{
				"policy_enabled":       true,
				"pipeline_merged_real": 0,
				"regression_rate":      0.12,
				"gate_pass_rate":       0.75,
				"active_pipeline_runs": 5,
				"queue_depth":          42,
			},
		}},
		Thresholds: BreakerThresholds{
			MinRealMerges:         1,
			MaxRegressionRate:     0.05,
			MinGatePassRate:       0.90,
			MaxActivePipelineRuns: 3,
			MaxQueueDepth:         20,
		},
		Clock: func() time.Time { return now },
	}

	got := eval.CheckAutonomy(context.Background())
	if got.Allowed {
		t.Fatal("Allowed = true, want blocked")
	}
	if got.Code != "kpi_unhealthy" {
		t.Fatalf("Code = %q, want kpi_unhealthy", got.Code)
	}
	for _, want := range []string{
		"pipeline_merged_real below threshold",
		"regression_rate above threshold",
		"gate_pass_rate below threshold",
		"active_pipeline_runs above threshold",
		"queue_depth above threshold",
	} {
		if !containsBlocker(got.Blockers, want) {
			t.Fatalf("Blockers = %v, want substring %q", got.Blockers, want)
		}
	}
}

func TestBreakerEvaluatorBlocksOnInfrastructureHealth(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	eval := BreakerEvaluator{
		Policy: fakeBreakerPolicy{p: &mills.Policy{}},
		KPI: fakeKPIReader{snap: &store.KPISnapshot{
			SnapshotAt: now.Add(-time.Minute),
			Metrics:    map[string]any{"policy_enabled": true},
		}},
		Infrastructure: InfrastructureHealthFunc(func(context.Context) ([]InfrastructureHealth, error) {
			return []InfrastructureHealth{
				{ID: "gitlab", Status: "red", Mode: "not_configured", Message: "GitLab client config is incomplete", RequiredForAutonomy: true},
				{ID: "kpi_writer", Status: "yellow", Mode: "disabled", Message: "advisory", RequiredForAutonomy: false},
			}, nil
		}),
		Clock: func() time.Time { return now },
	}

	got := eval.CheckAutonomy(context.Background())
	if got.Allowed {
		t.Fatal("Allowed = true, want blocked")
	}
	if got.Code != "infrastructure_red" {
		t.Fatalf("Code = %q, want infrastructure_red", got.Code)
	}
	if !containsBlocker(got.Blockers, "gitlab: GitLab client config is incomplete") {
		t.Fatalf("Blockers = %v, want gitlab blocker", got.Blockers)
	}
}

func TestEvaluateBreakerOperationalStateDistinguishesIdleBlocked(t *testing.T) {
	got := EvaluateBreakerOperationalState(BreakerOperationalStateInput{
		Decision: councilDecision(false, "policy_disabled", []string{"policy.enabled=false"}),
	})
	if got.State != telemetry.MillsOperationalStateIdleBlocked {
		t.Fatalf("State = %q, want idle_blocked; report=%+v", got.State, got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "policy.enabled=false" {
		t.Fatalf("Reasons = %v, want policy blocker", got.Reasons)
	}
}

func TestEvaluateBreakerOperationalStateSurfacesDependencyIncidentAsDegraded(t *testing.T) {
	got := EvaluateBreakerOperationalState(BreakerOperationalStateInput{
		Decision:   councilDecision(true, "", nil),
		ActiveWork: 1,
		Infrastructure: []InfrastructureHealth{{
			ID: "gitlab", Status: "yellow", Mode: "degraded", Message: "api 503",
			RequiredForAutonomy: true, IncidentID: "gitlab-503", IncidentActive: true,
		}},
	})
	if got.State != telemetry.MillsOperationalStateDegraded {
		t.Fatalf("State = %q, want degraded; report=%+v", got.State, got)
	}
	if len(got.DegradedDependencies) != 1 || got.DegradedDependencies[0] != "gitlab" {
		t.Fatalf("DegradedDependencies = %v, want gitlab", got.DegradedDependencies)
	}
	if len(got.ActiveIncidents) != 1 || got.ActiveIncidents[0].ID != "gitlab-503" {
		t.Fatalf("ActiveIncidents = %+v, want gitlab-503", got.ActiveIncidents)
	}
}

func containsBlocker(blockers []string, sub string) bool {
	for _, b := range blockers {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return false
}

func councilDecision(allowed bool, code string, blockers []string) council.AutonomyGateDecision {
	return council.AutonomyGateDecision{Allowed: allowed, Code: code, Blockers: blockers}
}
