package council

import "testing"

func TestStabilityFirstOrder_PrioritizesRemediation(t *testing.T) {
	got := StabilityFirstOrder([]PlanningCandidate{
		{ID: "feature", Title: "Add new dashboard filter", Priority: 90},
		{ID: "infra", Title: "Repair health gate telemetry", Labels: []string{"remediation"}, Files: []string{"pkg/mills/gates/health.go"}, Priority: 1},
	})
	if got[0].ID != "infra" {
		t.Fatalf("first = %s, want infra", got[0].ID)
	}
}
