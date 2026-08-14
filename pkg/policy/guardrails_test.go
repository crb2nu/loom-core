package policy

import (
	"math"
	"testing"
)

func TestEvaluateAutonomy_AllowsWhenThresholdsSatisfied(t *testing.T) {
	got := EvaluateAutonomy(DefaultAutonomyThresholds(), AutonomyEvidence{
		PlanConfidence:      0.71,
		ExecutionConfidence: 0.91,
	})
	if !got.Allowed {
		t.Fatalf("allowed = false, reasons=%v", got.Reasons)
	}
	if got.FailClosed {
		t.Fatalf("failClosed = true, want false")
	}
}

func TestEvaluateAutonomy_FailsClosedWhenThresholdMissing(t *testing.T) {
	th := DefaultAutonomyThresholds()
	th.MinPlanConfidence = nil

	got := EvaluateAutonomy(th, AutonomyEvidence{
		PlanConfidence:      0.99,
		ExecutionConfidence: 0.99,
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if !got.FailClosed {
		t.Fatalf("failClosed = false, want true")
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "missing min plan confidence threshold" {
		t.Fatalf("reasons = %v", got.Reasons)
	}
}

func TestEvaluateAutonomy_BlocksUnsafeEvidence(t *testing.T) {
	got := EvaluateAutonomy(DefaultAutonomyThresholds(), AutonomyEvidence{
		PlanConfidence:      0.69,
		ExecutionConfidence: 0.90,
		WorkspaceSignalDebt: 2,
		OpenFailureClusters: 1,
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if got.FailClosed {
		t.Fatalf("failClosed = true, want ordinary threshold block")
	}
	if len(got.Reasons) != 3 {
		t.Fatalf("reasons = %v, want 3 threshold failures", got.Reasons)
	}
}

func TestEvaluateAutonomy_FailsClosedOnInvalidConfidenceEvidence(t *testing.T) {
	got := EvaluateAutonomy(DefaultAutonomyThresholds(), AutonomyEvidence{
		PlanConfidence:      1.01,
		ExecutionConfidence: -0.01,
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if !got.FailClosed {
		t.Fatalf("failClosed = false, want true")
	}
	if len(got.Reasons) != 2 {
		t.Fatalf("reasons = %v, want two invalid-confidence reasons", got.Reasons)
	}
}

func TestEvaluateAutonomy_FailsClosedOnNaNConfidence(t *testing.T) {
	got := EvaluateAutonomy(DefaultAutonomyThresholds(), AutonomyEvidence{
		PlanConfidence:      math.NaN(),
		ExecutionConfidence: 0.90,
	})
	if got.Allowed {
		t.Fatalf("allowed = true, want blocked")
	}
	if !got.FailClosed {
		t.Fatalf("failClosed = false, want true")
	}
}
