package telemetry

import "testing"

func TestEvaluateMillsOperationalState_IdleHealthy(t *testing.T) {
	got := EvaluateMillsOperationalState(MillsOperationalStateInput{
		AutonomyAllowed: true,
		HealthAllowed:   true,
	})
	if got.State != MillsOperationalStateIdleHealthy {
		t.Fatalf("State = %q, want %q; report=%+v", got.State, MillsOperationalStateIdleHealthy, got)
	}
	if len(got.Reasons) != 0 {
		t.Fatalf("Reasons = %v, want empty", got.Reasons)
	}
}

func TestEvaluateMillsOperationalState_IdleBlocked(t *testing.T) {
	got := EvaluateMillsOperationalState(MillsOperationalStateInput{
		AutonomyAllowed:  false,
		HealthAllowed:    true,
		AutonomyBlockers: []string{"policy.enabled=false"},
	})
	if got.State != MillsOperationalStateIdleBlocked {
		t.Fatalf("State = %q, want %q; report=%+v", got.State, MillsOperationalStateIdleBlocked, got)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "policy.enabled=false" {
		t.Fatalf("Reasons = %v, want policy blocker", got.Reasons)
	}
}

func TestEvaluateMillsOperationalState_DependencyIncidentDegraded(t *testing.T) {
	got := EvaluateMillsOperationalState(MillsOperationalStateInput{
		ActiveWork:      1,
		AutonomyAllowed: true,
		HealthAllowed:   true,
		HealthReasons:   []string{"critical dependency gitlab is degraded"},
		DegradedDependencies: []string{
			"gitlab",
			" gitlab ",
		},
		ActiveDependencyIncidents: []DependencyIncident{
			{ID: "gitlab-503", Dependency: "gitlab", Summary: "503 from API"},
			{ID: "gitlab-503", Dependency: "gitlab", Summary: "duplicate"},
		},
	})
	if got.State != MillsOperationalStateDegraded {
		t.Fatalf("State = %q, want %q; report=%+v", got.State, MillsOperationalStateDegraded, got)
	}
	if len(got.DegradedDependencies) != 1 || got.DegradedDependencies[0] != "gitlab" {
		t.Fatalf("DegradedDependencies = %v, want gitlab once", got.DegradedDependencies)
	}
	if len(got.ActiveIncidents) != 1 || got.ActiveIncidents[0].ID != "gitlab-503" {
		t.Fatalf("ActiveIncidents = %+v, want deduped gitlab incident", got.ActiveIncidents)
	}
}
