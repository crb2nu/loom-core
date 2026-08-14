// Package policy contains small, deterministic policy primitives shared by
// higher-level Loom controllers.
package policy

import (
	"fmt"
	"math"
)

// FloatThreshold is a pointer-friendly helper for constructing
// AutonomyThresholds in policy loaders and tests.
func FloatThreshold(v float64) *float64 { return &v }

// IntThreshold is a pointer-friendly helper for constructing
// AutonomyThresholds in policy loaders and tests.
func IntThreshold(v int) *int { return &v }

// AutonomyThresholds captures the minimum evidence required before a council
// plan or pipeline action may run without human review.
//
// Pointer fields are deliberate: a missing threshold is different from a
// configured zero. Missing thresholds fail closed so autonomy cannot proceed
// under an underspecified policy.
type AutonomyThresholds struct {
	MinPlanConfidence      *float64
	MinExecutionConfidence *float64
	MaxWorkspaceSignalDebt *int
	MaxOpenFailureClusters *int
}

// AutonomyEvidence is the measured state for one autonomous decision.
type AutonomyEvidence struct {
	PlanConfidence      float64
	ExecutionConfidence float64
	WorkspaceSignalDebt int
	OpenFailureClusters int
}

// AutonomyDecision is the result of applying thresholds to evidence.
type AutonomyDecision struct {
	Allowed    bool
	FailClosed bool
	Reasons    []string
}

// DefaultAutonomyThresholds returns conservative defaults for full autonomy:
// both planning and execution evidence must be strong, and the workspace must
// have no unresolved signal debt or open failure clusters.
func DefaultAutonomyThresholds() AutonomyThresholds {
	return AutonomyThresholds{
		MinPlanConfidence:      FloatThreshold(0.70),
		MinExecutionConfidence: FloatThreshold(0.80),
		MaxWorkspaceSignalDebt: IntThreshold(0),
		MaxOpenFailureClusters: IntThreshold(0),
	}
}

// EvaluateAutonomy decides whether evidence satisfies the configured
// thresholds. Any missing threshold or invalid evidence blocks the action.
func EvaluateAutonomy(th AutonomyThresholds, ev AutonomyEvidence) AutonomyDecision {
	var reasons []string
	failClosed := false

	if th.MinPlanConfidence == nil {
		reasons = append(reasons, "missing min plan confidence threshold")
		failClosed = true
	} else if !validConfidence(ev.PlanConfidence) {
		reasons = append(reasons, fmt.Sprintf("plan confidence %.2f outside [0.00, 1.00]", ev.PlanConfidence))
		failClosed = true
	} else if ev.PlanConfidence < *th.MinPlanConfidence {
		reasons = append(reasons, fmt.Sprintf("plan confidence %.2f below %.2f", ev.PlanConfidence, *th.MinPlanConfidence))
	}

	if th.MinExecutionConfidence == nil {
		reasons = append(reasons, "missing min execution confidence threshold")
		failClosed = true
	} else if !validConfidence(ev.ExecutionConfidence) {
		reasons = append(reasons, fmt.Sprintf("execution confidence %.2f outside [0.00, 1.00]", ev.ExecutionConfidence))
		failClosed = true
	} else if ev.ExecutionConfidence < *th.MinExecutionConfidence {
		reasons = append(reasons, fmt.Sprintf("execution confidence %.2f below %.2f", ev.ExecutionConfidence, *th.MinExecutionConfidence))
	}

	if th.MaxWorkspaceSignalDebt == nil {
		reasons = append(reasons, "missing max workspace signal debt threshold")
		failClosed = true
	} else if ev.WorkspaceSignalDebt < 0 {
		reasons = append(reasons, "workspace signal debt cannot be negative")
		failClosed = true
	} else if ev.WorkspaceSignalDebt > *th.MaxWorkspaceSignalDebt {
		reasons = append(reasons, fmt.Sprintf("workspace signal debt %d above %d", ev.WorkspaceSignalDebt, *th.MaxWorkspaceSignalDebt))
	}

	if th.MaxOpenFailureClusters == nil {
		reasons = append(reasons, "missing max open failure clusters threshold")
		failClosed = true
	} else if ev.OpenFailureClusters < 0 {
		reasons = append(reasons, "open failure clusters cannot be negative")
		failClosed = true
	} else if ev.OpenFailureClusters > *th.MaxOpenFailureClusters {
		reasons = append(reasons, fmt.Sprintf("open failure clusters %d above %d", ev.OpenFailureClusters, *th.MaxOpenFailureClusters))
	}

	return AutonomyDecision{
		Allowed:    len(reasons) == 0,
		FailClosed: failClosed,
		Reasons:    reasons,
	}
}

func validConfidence(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1
}
