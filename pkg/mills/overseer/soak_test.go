package overseer

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestEvaluateSoak(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	valid := SoakEvidence{
		WindowStart: start, WindowEnd: start.Add(S2SoakMinimumDuration), ProposedActions: 1,
		OutcomeIntervals: []SoakOutcomeInterval{{Start: start, End: start.Add(time.Hour), Outcome: "propose"}},
	}
	conflict := valid
	conflict.OutcomeIntervals = []SoakOutcomeInterval{
		{Start: start, End: start.Add(2 * time.Hour), Outcome: "propose"},
		{Start: start.Add(time.Hour), End: start.Add(3 * time.Hour), Outcome: "suppress"},
	}

	tests := []struct {
		name string
		in   SoakEvidence
		want []string
	}{
		{name: "exact boundary passes", in: valid},
		{name: "short window", in: alterSoak(valid, func(e *SoakEvidence) { e.WindowEnd = start.Add(S2SoakMinimumDuration - time.Nanosecond) }), want: []string{SoakFailureWindowTooShort}},
		{name: "no proposals", in: alterSoak(valid, func(e *SoakEvidence) { e.ProposedActions = 0 }), want: []string{SoakFailureNoProposedActions}},
		{name: "would harm", in: alterSoak(valid, func(e *SoakEvidence) { e.WouldHaveHarmedActions = 1 }), want: []string{SoakFailureWouldHaveHarmed}},
		{name: "quarantine", in: alterSoak(valid, func(e *SoakEvidence) { e.QuarantinedRuns = 1 }), want: []string{SoakFailureQuarantinedRuns}},
		{name: "conflicting overlap", in: conflict, want: []string{SoakFailureConflictingOutcomes}},
		{name: "touching outcomes do not overlap", in: alterSoak(valid, func(e *SoakEvidence) {
			e.OutcomeIntervals = []SoakOutcomeInterval{{Start: start, End: start.Add(time.Hour), Outcome: "a"}, {Start: start.Add(time.Hour), End: start.Add(2 * time.Hour), Outcome: "b"}}
		})},
		{name: "negative counter fails closed", in: alterSoak(valid, func(e *SoakEvidence) { e.QuarantinedRuns = -1 }), want: []string{SoakFailureMalformedEvidence}},
		{name: "inverted interval fails closed", in: alterSoak(valid, func(e *SoakEvidence) { e.OutcomeIntervals[0].End = e.OutcomeIntervals[0].Start }), want: []string{SoakFailureMalformedEvidence}},
		{name: "multiple failures are ordered", in: alterSoak(conflict, func(e *SoakEvidence) {
			e.WindowEnd = start.Add(6 * 24 * time.Hour)
			e.ProposedActions = 0
			e.WouldHaveHarmedActions = 2
			e.QuarantinedRuns = 1
		}), want: []string{SoakFailureMalformedEvidence, SoakFailureWindowTooShort, SoakFailureNoProposedActions, SoakFailureWouldHaveHarmed, SoakFailureQuarantinedRuns, SoakFailureConflictingOutcomes}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateSoak(tc.in)
			if got.Passed != (len(tc.want) == 0) {
				t.Fatalf("Passed = %v, reasons = %v", got.Passed, got.FailureReasons)
			}
			if !slices.Equal(got.FailureReasons, tc.want) {
				t.Fatalf("reasons = %v, want %v", got.FailureReasons, tc.want)
			}
		})
	}
}

func TestSoakReportStableJSON(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	report := EvaluateSoak(SoakEvidence{WindowStart: start, WindowEnd: start.Add(S2SoakMinimumDuration), ProposedActions: 1, OutcomeIntervals: []SoakOutcomeInterval{}})
	got, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"passed":true,"window_duration":604800000000000,"failure_reasons":[],"evidence":{"window_start":"2026-08-01T00:00:00Z","window_end":"2026-08-08T00:00:00Z","proposed_actions":1,"would_have_harmed_actions":0,"quarantined_runs":0,"outcome_intervals":[]}}`
	if string(got) != want {
		t.Fatalf("JSON contract changed:\n got %s\nwant %s", got, want)
	}
}

func alterSoak(in SoakEvidence, fn func(*SoakEvidence)) SoakEvidence {
	in.OutcomeIntervals = append([]SoakOutcomeInterval(nil), in.OutcomeIntervals...)
	fn(&in)
	return in
}
