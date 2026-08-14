package audit

import "testing"

func TestEvaluateOperationalThresholds_PassesHealthySnapshot(t *testing.T) {
	got := EvaluateOperationalThresholds(ThresholdPolicy{}, OperationalSnapshot{
		OpenEscalations:     1,
		AuditQueueDepth:     2,
		ConsecutiveFailures: 0,
		AuditSurvivalRate:   0.95,
		GateFlakeRate:       0.01,
	})
	if !got.Pass || got.Code != ThresholdCodeOK || got.Severity != ThresholdSeverityOK {
		t.Fatalf("healthy verdict = %+v", got)
	}
	if len(got.Checks) != 5 {
		t.Fatalf("checks = %d, want 5", len(got.Checks))
	}
	for _, check := range got.Checks {
		if check.Reason != "" {
			t.Fatalf("passing check %q included failure reason %q", check.Name, check.Reason)
		}
	}
}

func TestEvaluateOperationalThresholds_FailsCriticalEscalationLoad(t *testing.T) {
	got := EvaluateOperationalThresholds(ThresholdPolicy{MaxOpenEscalations: 2}, OperationalSnapshot{
		OpenEscalations:   3,
		AuditSurvivalRate: 1,
	})
	if got.Pass || got.Code != ThresholdCodeEscalationLoad || got.Severity != ThresholdSeverityCritical {
		t.Fatalf("critical escalation verdict = %+v", got)
	}
}

func TestEvaluateOperationalThresholds_FailsAuditQualityWarning(t *testing.T) {
	got := EvaluateOperationalThresholds(ThresholdPolicy{MinAuditSurvivalRate: 0.9}, OperationalSnapshot{
		AuditSurvivalRate: 0.7,
	})
	if got.Pass || got.Code != ThresholdCodeAuditQuality || got.Severity != ThresholdSeverityWarning {
		t.Fatalf("audit quality verdict = %+v", got)
	}
}

func TestEvaluateOperationalThresholds_FailsGateFlakinessWarning(t *testing.T) {
	got := EvaluateOperationalThresholds(ThresholdPolicy{MaxGateFlakeRate: 0.1}, OperationalSnapshot{
		AuditSurvivalRate: 1,
		GateFlakeRate:     0.2,
	})
	if got.Pass || got.Code != ThresholdCodeGateFlakiness || got.Severity != ThresholdSeverityWarning {
		t.Fatalf("gate flakiness verdict = %+v", got)
	}
}
