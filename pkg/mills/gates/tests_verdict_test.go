package gates

import (
	"context"
	"strings"
	"testing"
)

func TestTestsVerdictGate(t *testing.T) {
	gate := &TestsVerdictGate{}
	for _, input := range []StageInput{{}, {TestsVerdict: &TestsVerdict{Passed: true}}} {
		got, err := gate.Evaluate(context.Background(), input)
		if err != nil || !got.Pass {
			t.Fatalf("pass case = %+v, %v", got, err)
		}
	}
	got, err := gate.Evaluate(context.Background(), StageInput{TestsVerdict: &TestsVerdict{FailedChecks: []FailedCheck{
		{Name: "lint", ExitCode: 1, Output: "lint tail"}, {Name: "test", ExitCode: 2, Output: "test tail"},
	}}})
	if err != nil || got.Pass || len(got.Reasons) != 2 {
		t.Fatalf("failed verdict = %+v, %v", got, err)
	}
	for i, want := range []string{"lint[exit=1]: lint tail", "test[exit=2]: test tail"} {
		if !strings.Contains(got.Reasons[i], want) {
			t.Errorf("reason %d = %q, want %q", i, got.Reasons[i], want)
		}
	}
}
