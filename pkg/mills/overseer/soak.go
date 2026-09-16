package overseer

import (
	"sort"
	"time"
)

const (
	SoakFailureMalformedEvidence   = "malformed_evidence"
	SoakFailureWindowTooShort      = "window_too_short"
	SoakFailureNoProposedActions   = "no_proposed_actions"
	SoakFailureWouldHaveHarmed     = "would_have_harmed"
	SoakFailureQuarantinedRuns     = "quarantined_runs"
	SoakFailureConflictingOutcomes = "conflicting_outcomes"
)

// SoakOutcomeInterval is a half-open interval, [Start, End), during which an
// outcome was observed. Adjacent intervals do not overlap.
type SoakOutcomeInterval struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Outcome string    `json:"outcome"`
}

// SoakEvidence contains the closed, aggregate evidence for one S2 dry-run
// soak. Times and counters are supplied by the caller to keep evaluation pure.
type SoakEvidence struct {
	WindowStart            time.Time             `json:"window_start"`
	WindowEnd              time.Time             `json:"window_end"`
	ProposedActions        int                   `json:"proposed_actions"`
	WouldHaveHarmedActions int                   `json:"would_have_harmed_actions"`
	QuarantinedRuns        int                   `json:"quarantined_runs"`
	OutcomeIntervals       []SoakOutcomeInterval `json:"outcome_intervals"`
}

// SoakReport is the stable, machine-readable result of evaluating S2 soak
// evidence. FailureReasons follows the order declared by the constants above.
type SoakReport struct {
	Passed         bool          `json:"passed"`
	WindowDuration time.Duration `json:"window_duration"`
	FailureReasons []string      `json:"failure_reasons"`
	Evidence       SoakEvidence  `json:"evidence"`
}

// EvaluateSoak evaluates all S2 requirements without I/O or clock access.
// Invalid counters, timestamps, intervals, or outcome labels fail closed.
func EvaluateSoak(evidence SoakEvidence) SoakReport {
	report := SoakReport{Evidence: evidence, FailureReasons: []string{}}
	report.WindowDuration = evidence.WindowEnd.Sub(evidence.WindowStart)

	malformed := evidence.WindowStart.IsZero() || evidence.WindowEnd.IsZero() ||
		!evidence.WindowEnd.After(evidence.WindowStart) ||
		evidence.ProposedActions < 0 || evidence.WouldHaveHarmedActions < 0 ||
		evidence.QuarantinedRuns < 0 || evidence.WouldHaveHarmedActions > evidence.ProposedActions
	conflicting := false
	intervals := append([]SoakOutcomeInterval(nil), evidence.OutcomeIntervals...)
	for _, interval := range intervals {
		if interval.Start.IsZero() || interval.End.IsZero() || interval.Outcome == "" ||
			!interval.End.After(interval.Start) || interval.Start.Before(evidence.WindowStart) ||
			interval.End.After(evidence.WindowEnd) {
			malformed = true
		}
	}

	// Sort a copy, preserving the caller's evidence and making comparisons
	// independent of input order. Compare every active pair because intervals
	// may be nested rather than merely adjacent after sorting.
	sort.SliceStable(intervals, func(i, j int) bool {
		if intervals[i].Start.Equal(intervals[j].Start) {
			return intervals[i].End.Before(intervals[j].End)
		}
		return intervals[i].Start.Before(intervals[j].Start)
	})
	for i := range intervals {
		for j := i + 1; j < len(intervals) && intervals[j].Start.Before(intervals[i].End); j++ {
			if intervals[i].Outcome != intervals[j].Outcome {
				conflicting = true
			}
		}
	}

	if malformed {
		report.FailureReasons = append(report.FailureReasons, SoakFailureMalformedEvidence)
	}
	if report.WindowDuration < S2SoakMinimumDuration {
		report.FailureReasons = append(report.FailureReasons, SoakFailureWindowTooShort)
	}
	if evidence.ProposedActions < 1 {
		report.FailureReasons = append(report.FailureReasons, SoakFailureNoProposedActions)
	}
	if evidence.WouldHaveHarmedActions > 0 {
		report.FailureReasons = append(report.FailureReasons, SoakFailureWouldHaveHarmed)
	}
	if evidence.QuarantinedRuns > 0 {
		report.FailureReasons = append(report.FailureReasons, SoakFailureQuarantinedRuns)
	}
	if conflicting {
		report.FailureReasons = append(report.FailureReasons, SoakFailureConflictingOutcomes)
	}
	report.Passed = len(report.FailureReasons) == 0
	return report
}
