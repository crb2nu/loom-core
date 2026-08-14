package overseer

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestEvaluateSoakGate(t *testing.T) {
	valid := SoakGateTelemetry{Window: 168 * time.Hour, ReviewedDecisions: 100, Disagreements: 4}
	tests := []struct {
		name string
		in   *SoakGateTelemetry
		pass bool
	}{
		{name: "exact duration and below five percent", in: &valid, pass: true},
		{name: "window too short", in: &SoakGateTelemetry{Window: 168*time.Hour - time.Nanosecond, ReviewedDecisions: 100}},
		{name: "regression", in: &SoakGateTelemetry{Window: 168 * time.Hour, Regressions: 1, ReviewedDecisions: 100}},
		{name: "exactly five percent", in: &SoakGateTelemetry{Window: 168 * time.Hour, ReviewedDecisions: 100, Disagreements: 5}},
		{name: "missing", in: nil},
		{name: "negative", in: &SoakGateTelemetry{Window: 168 * time.Hour, ReviewedDecisions: -1}},
		{name: "zero decisions", in: &SoakGateTelemetry{Window: 168 * time.Hour}},
		{name: "inconsistent", in: &SoakGateTelemetry{Window: 168 * time.Hour, ReviewedDecisions: 2, Disagreements: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NewSoakGate().Evaluate(tc.in)
			if got.Pass != tc.pass || got.MetricPass != boolMetric(tc.pass) {
				t.Fatalf("verdict = %+v, want pass %v", got, tc.pass)
			}
			if !tc.pass && len(got.FailureReasons) == 0 {
				t.Fatal("failed verdict has no reason")
			}
		})
	}
}

func TestSoakGateVerdictStableJSONAndPureEvaluation(t *testing.T) {
	in := &SoakGateTelemetry{Window: 168 * time.Hour, ReviewedDecisions: 40, Disagreements: 1}
	before := *in
	got := EvaluateSoakGate(in)
	if !reflect.DeepEqual(*in, before) {
		t.Fatalf("evaluation mutated telemetry: before %+v, after %+v", before, *in)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"pass":true,"decision_disagreement_rate":0.025,"failure_reasons":[],"mills_overseer_s2_soak_gate_pass":1}`
	if string(b) != want {
		t.Fatalf("JSON = %s, want %s", b, want)
	}
}

func boolMetric(pass bool) int {
	if pass {
		return 1
	}
	return 0
}
