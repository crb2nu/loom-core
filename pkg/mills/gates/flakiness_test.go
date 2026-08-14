package gates

import (
	"testing"
	"time"
)

func TestEvaluateGateFlakiness_FlagsAlternatingFailures(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	runs := []GateRun{
		{GateName: "scope", Passed: true, At: now.Add(-5 * time.Minute)},
		{GateName: "scope", Passed: false, At: now.Add(-4 * time.Minute)},
		{GateName: "scope", Passed: true, At: now.Add(-3 * time.Minute)},
		{GateName: "scope", Passed: false, At: now.Add(-2 * time.Minute)},
		{GateName: "scope", Passed: true, At: now.Add(-time.Minute)},
	}
	got := EvaluateGateFlakiness(FlakinessPolicy{MinRuns: 5, MaxFailureRate: 0.2, MinTransitions: 2}, runs)
	if got.Pass || got.Code != FlakinessCodeFlakyGate || got.GateName != "scope" {
		t.Fatalf("flaky verdict = %+v", got)
	}
	if got.Metrics["scope.transitions"] != 4 {
		t.Fatalf("transition metric = %v", got.Metrics["scope.transitions"])
	}
}

func TestEvaluateGateFlakiness_DoesNotFlagConsistentFailure(t *testing.T) {
	runs := []GateRun{
		{GateName: "tests", Passed: false},
		{GateName: "tests", Passed: false},
		{GateName: "tests", Passed: false},
		{GateName: "tests", Passed: false},
		{GateName: "tests", Passed: false},
	}
	got := EvaluateGateFlakiness(FlakinessPolicy{MinRuns: 5, MaxFailureRate: 0.2, MinTransitions: 2}, runs)
	if !got.Pass || got.Code != FlakinessCodeOK {
		t.Fatalf("consistent failure should not be flaky: %+v", got)
	}
}

func TestEvaluateGateFlakiness_InsufficientDataPasses(t *testing.T) {
	got := EvaluateGateFlakiness(FlakinessPolicy{MinRuns: 3}, []GateRun{{GateName: "tests", Passed: false}})
	if !got.Pass || got.Code != FlakinessCodeInsufficientData {
		t.Fatalf("insufficient data verdict = %+v", got)
	}
}

func TestEvaluateGateFlakiness_InsufficientPerGateSamplesPasses(t *testing.T) {
	runs := []GateRun{
		{GateName: "scope", Passed: true},
		{GateName: "scope", Passed: false},
		{GateName: "tests", Passed: true},
		{GateName: "tests", Passed: false},
	}
	got := EvaluateGateFlakiness(FlakinessPolicy{MinRuns: 3}, runs)
	if !got.Pass || got.Code != FlakinessCodeInsufficientData {
		t.Fatalf("per-gate insufficient data verdict = %+v", got)
	}
	if got.Metrics["gate_count"] != 2 {
		t.Fatalf("gate_count metric = %v", got.Metrics["gate_count"])
	}
}
