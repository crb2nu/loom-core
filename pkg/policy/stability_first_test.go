package policy

import "testing"

func TestPrioritizeStabilityFirst_ScoresRemediationAboveFeature(t *testing.T) {
	feature := PrioritizeStabilityFirst(StabilityItem{Title: "Add feature", Priority: 80})
	remediation := PrioritizeStabilityFirst(StabilityItem{
		Title: "Repair health gates", Labels: []string{"remediation"}, Files: []string{"pkg/mills/pipeline/preflight.go"}, Priority: 1,
	})
	if !remediation.Remediation {
		t.Fatalf("remediation decision = %+v, want remediation", remediation)
	}
	if remediation.Score <= feature.Score {
		t.Fatalf("remediation score %d <= feature score %d", remediation.Score, feature.Score)
	}
}
