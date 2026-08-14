// Package planning contains deterministic planning guardrails used by the
// council and pipeline orchestration layers.
package planning

import "strings"

// SignalDebtLevel is a coarse risk class for recent workspace failures.
type SignalDebtLevel string

const (
	SignalDebtNone   SignalDebtLevel = "none"
	SignalDebtLow    SignalDebtLevel = "low"
	SignalDebtMedium SignalDebtLevel = "medium"
	SignalDebtHigh   SignalDebtLevel = "high"
)

// WorkspaceSignal is a normalized planning signal, typically derived from
// Loki, CI, or operator health observations.
type WorkspaceSignal struct {
	Source   string
	Service  string
	Count    int
	Sample   string
	Resolved bool
}

// SignalDebtThresholds defines the event and cluster ceilings for each level.
// Counts above the medium thresholds are classified as high debt.
type SignalDebtThresholds struct {
	LowEvents      int
	LowClusters    int
	MediumEvents   int
	MediumClusters int
}

// DefaultSignalDebtThresholds keeps a little noise as low debt, but makes
// sustained or broad failures block full autonomy.
func DefaultSignalDebtThresholds() SignalDebtThresholds {
	return SignalDebtThresholds{
		LowEvents:      5,
		LowClusters:    1,
		MediumEvents:   25,
		MediumClusters: 3,
	}
}

// SignalDebtSummary describes the unresolved workspace debt visible to a
// planning run.
type SignalDebtSummary struct {
	Level              SignalDebtLevel
	TotalEvents        int
	UnresolvedClusters int
	BlockingSignals    []WorkspaceSignal
	BlocksAutonomy     bool
}

// ClassifyWorkspaceSignalDebt reduces recent signals to a single debt class.
// Resolved signals are ignored. Invalid negative counts are treated as one
// event so malformed input cannot hide debt.
func ClassifyWorkspaceSignalDebt(signals []WorkspaceSignal, th SignalDebtThresholds) SignalDebtSummary {
	th = normalizeSignalDebtThresholds(th)

	var summary SignalDebtSummary
	for _, sig := range signals {
		if sig.Resolved {
			continue
		}
		count := sig.Count
		if count <= 0 {
			count = 1
		}
		sig.Count = count
		sig.Source = strings.TrimSpace(sig.Source)
		sig.Service = strings.TrimSpace(sig.Service)
		summary.TotalEvents += count
		summary.UnresolvedClusters++
		summary.BlockingSignals = append(summary.BlockingSignals, sig)
	}

	switch {
	case summary.UnresolvedClusters == 0:
		summary.Level = SignalDebtNone
	case summary.TotalEvents <= th.LowEvents && summary.UnresolvedClusters <= th.LowClusters:
		summary.Level = SignalDebtLow
	case summary.TotalEvents <= th.MediumEvents && summary.UnresolvedClusters <= th.MediumClusters:
		summary.Level = SignalDebtMedium
	default:
		summary.Level = SignalDebtHigh
	}
	summary.BlocksAutonomy = summary.Level == SignalDebtMedium || summary.Level == SignalDebtHigh
	return summary
}

func normalizeSignalDebtThresholds(th SignalDebtThresholds) SignalDebtThresholds {
	def := DefaultSignalDebtThresholds()
	if th.LowEvents <= 0 {
		th.LowEvents = def.LowEvents
	}
	if th.LowClusters <= 0 {
		th.LowClusters = def.LowClusters
	}
	if th.MediumEvents <= th.LowEvents {
		th.MediumEvents = def.MediumEvents
	}
	if th.MediumClusters <= th.LowClusters {
		th.MediumClusters = def.MediumClusters
	}
	return th
}
