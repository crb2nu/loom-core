package council

import (
	"strings"
	"time"
)

const (
	StalePlanCodeOK           = "ok"
	StalePlanCodePlanStale    = "plan_stale"
	StalePlanCodeSliceStale   = "slice_stale"
	StalePlanCodeSliceBacklog = "slice_backlog"
)

// PolicySeverity is the bounded severity vocabulary used by policy evaluators.
type PolicySeverity string

const (
	PolicySeverityOK       PolicySeverity = "ok"
	PolicySeverityWarning  PolicySeverity = "warning"
	PolicySeverityCritical PolicySeverity = "critical"
)

// PolicyAction is the bounded remediation vocabulary used by policy evaluators.
type PolicyAction string

const (
	PolicyActionNone     PolicyAction = "none"
	PolicyActionRefresh  PolicyAction = "refresh_plan"
	PolicyActionEscalate PolicyAction = "escalate"
)

// PolicyVerdict is the stable machine-readable output shared by council policy
// evaluators. Metrics are numeric so callers can index them without parsing
// reason text.
type PolicyVerdict struct {
	Pass     bool               `json:"pass"`
	Code     string             `json:"code"`
	Severity PolicySeverity     `json:"severity"`
	Action   PolicyAction       `json:"action"`
	Reasons  []string           `json:"reasons,omitempty"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

// StalePlanPolicy configures the council's deterministic stale-context check.
type StalePlanPolicy struct {
	// PlanStaleAfter is the maximum age of the plan's own UpdatedAt before the
	// planning context must be refreshed. Default: 14 days.
	PlanStaleAfter time.Duration
	// SliceStaleAfter is the maximum age of a non-terminal slice's UpdatedAt.
	// Default: 7 days.
	SliceStaleAfter time.Duration
	// MaxPendingSlices caps active/pending slice fan-out before the council
	// should stop adding more work and consolidate the plan. Default: 20.
	MaxPendingSlices int
	// Now is injectable for tests. Defaults to time.Now().UTC().
	Now func() time.Time
}

// PlanningContext is the council-facing subset of a plan needed to evaluate
// context freshness without depending on the agent-context store package.
type PlanningContext struct {
	PlanID    string
	Phase     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Slices    []PlanSliceContext
}

// PlanSliceContext is the policy-facing subset of one plan slice.
type PlanSliceContext struct {
	ID        string
	Phase     string
	UpdatedAt time.Time
}

// DefaultStalePlanPolicy returns the conservative thresholds used when policy
// YAML omits this evaluator's knobs.
func DefaultStalePlanPolicy() StalePlanPolicy {
	return StalePlanPolicy{
		PlanStaleAfter:   14 * 24 * time.Hour,
		SliceStaleAfter:  7 * 24 * time.Hour,
		MaxPendingSlices: 20,
	}
}

// EvaluateStalePlan returns a deterministic policy verdict for one plan's
// freshness and active-slice pressure.
func EvaluateStalePlan(policy StalePlanPolicy, ctx PlanningContext) PolicyVerdict {
	policy = normalizeStalePlanPolicy(policy)
	now := policy.now()

	updatedAt := ctx.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = ctx.CreatedAt
	}
	planAge := nonNegativeDuration(now.Sub(updatedAt))
	activeSlices, staleSlices := staleSliceCounts(now, policy.SliceStaleAfter, ctx.Slices)

	metrics := map[string]float64{
		"plan_age_hours":         planAge.Hours(),
		"active_slices":          float64(activeSlices),
		"stale_active_slices":    float64(staleSlices),
		"max_pending_slices":     float64(policy.MaxPendingSlices),
		"plan_stale_after_hours": policy.PlanStaleAfter.Hours(),
	}

	var reasons []string
	code := StalePlanCodeOK
	severity := PolicySeverityOK
	action := PolicyActionNone
	if planAge > policy.PlanStaleAfter && !planTerminal(ctx.Phase) {
		code = StalePlanCodePlanStale
		severity = PolicySeverityCritical
		action = PolicyActionRefresh
		reasons = append(reasons, "plan context exceeds stale age threshold")
	}
	if staleSlices > 0 {
		if severity != PolicySeverityCritical {
			code = StalePlanCodeSliceStale
			severity = PolicySeverityWarning
			action = PolicyActionRefresh
		}
		reasons = append(reasons, "one or more active slices exceed stale age threshold")
	}
	if activeSlices > policy.MaxPendingSlices {
		code = StalePlanCodeSliceBacklog
		severity = PolicySeverityCritical
		action = PolicyActionEscalate
		reasons = append(reasons, "active slice count exceeds pending slice threshold")
	}
	if len(reasons) == 0 {
		return PolicyVerdict{Pass: true, Code: code, Severity: severity, Action: action, Metrics: metrics}
	}
	return PolicyVerdict{Pass: false, Code: code, Severity: severity, Action: action, Reasons: reasons, Metrics: metrics}
}

func normalizeStalePlanPolicy(policy StalePlanPolicy) StalePlanPolicy {
	def := DefaultStalePlanPolicy()
	if policy.PlanStaleAfter <= 0 {
		policy.PlanStaleAfter = def.PlanStaleAfter
	}
	if policy.SliceStaleAfter <= 0 {
		policy.SliceStaleAfter = def.SliceStaleAfter
	}
	if policy.MaxPendingSlices <= 0 {
		policy.MaxPendingSlices = def.MaxPendingSlices
	}
	return policy
}

func (p StalePlanPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func staleSliceCounts(now time.Time, maxAge time.Duration, slices []PlanSliceContext) (active, stale int) {
	for _, s := range slices {
		if planTerminal(s.Phase) {
			continue
		}
		active++
		if s.UpdatedAt.IsZero() {
			stale++
			continue
		}
		if nonNegativeDuration(now.Sub(s.UpdatedAt)) > maxAge {
			stale++
		}
	}
	return active, stale
}

func planTerminal(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "merged", "deployed", "done", "abandoned", "completed", "closed":
		return true
	default:
		return false
	}
}

func nonNegativeDuration(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
