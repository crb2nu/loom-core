package council

// EscalationAction is the closed set of decisions the council-facing policy can
// make when a pipeline failure reaches an escalation boundary.
type EscalationAction string

const (
	EscalationActionRetry         EscalationAction = "retry"
	EscalationActionAutoRetryRun  EscalationAction = "auto_retry_run"
	EscalationActionHumanEscalate EscalationAction = "human_escalate"
)

// EscalationPolicy configures retry-aware escalation decisions.
type EscalationPolicy struct {
	MaxAttempts            int
	TransientRetryCap      int
	EscalationAutoRetryCap int
}

// EscalationFailureClass is the policy-facing string form of
// pipeline.FailureClass. It is intentionally local to council to avoid a
// package cycle: pipeline already depends on council for autonomy gates.
type EscalationFailureClass string

const (
	EscalationFailureTransient      EscalationFailureClass = "transient"
	EscalationFailureTransientQuota EscalationFailureClass = "transient_quota"
	EscalationFailureInfrastructure EscalationFailureClass = "infrastructure"
	EscalationFailureCode           EscalationFailureClass = "code"
	EscalationFailureConfiguration  EscalationFailureClass = "configuration"
)

// EscalationContext describes the retry history at the point of failure.
type EscalationContext struct {
	FailureClass       EscalationFailureClass
	Attempts           int
	EffectiveAttempts  int
	PriorAutoRetryRuns int
	FailureDescription string
}

// EscalationDecision is the policy verdict for a failed stage/run.
type EscalationDecision struct {
	Action EscalationAction
	Class  EscalationFailureClass
	Reason string
}

// DefaultEscalationPolicy mirrors the pipeline runner defaults.
func DefaultEscalationPolicy() EscalationPolicy {
	return EscalationPolicy{
		MaxAttempts:            3,
		TransientRetryCap:      5,
		EscalationAutoRetryCap: 0,
	}
}

// DecideEscalation applies retry-aware escalation policy to a classified
// failure. Unknown classes are treated as code failures so the system fails
// closed to human review after the normal attempt budget.
func DecideEscalation(policy EscalationPolicy, ctx EscalationContext) EscalationDecision {
	policy = normalizeEscalationPolicy(policy)
	class := normalizeEscalationFailureClass(ctx.FailureClass)

	decision := EscalationDecision{Class: class}
	if class == EscalationFailureConfiguration {
		decision.Action = EscalationActionHumanEscalate
		decision.Reason = "terminal configuration failure"
		return decision
	}

	if isEscalationFreeRetry(class) {
		totalCap := policy.MaxAttempts + policy.TransientRetryCap
		if ctx.Attempts < totalCap {
			decision.Action = EscalationActionRetry
			decision.Reason = "transient retry budget available"
			return decision
		}
		if ctx.PriorAutoRetryRuns < policy.EscalationAutoRetryCap {
			decision.Action = EscalationActionAutoRetryRun
			decision.Reason = "transient cap reached; auto-retry run budget available"
			return decision
		}
		decision.Action = EscalationActionHumanEscalate
		decision.Reason = "transient retry and auto-retry budgets exhausted"
		return decision
	}

	if ctx.EffectiveAttempts < policy.MaxAttempts {
		decision.Action = EscalationActionRetry
		decision.Reason = "attempt budget available"
		return decision
	}
	decision.Action = EscalationActionHumanEscalate
	decision.Reason = "attempt budget exhausted"
	return decision
}

func normalizeEscalationPolicy(policy EscalationPolicy) EscalationPolicy {
	def := DefaultEscalationPolicy()
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = def.MaxAttempts
	}
	if policy.TransientRetryCap < 0 {
		policy.TransientRetryCap = 0
	}
	if policy.TransientRetryCap == 0 {
		policy.TransientRetryCap = def.TransientRetryCap
	}
	if policy.EscalationAutoRetryCap < 0 {
		policy.EscalationAutoRetryCap = 0
	}
	return policy
}

func normalizeEscalationFailureClass(class EscalationFailureClass) EscalationFailureClass {
	switch class {
	case EscalationFailureTransient,
		EscalationFailureTransientQuota,
		EscalationFailureInfrastructure,
		EscalationFailureCode,
		EscalationFailureConfiguration:
		return class
	default:
		return EscalationFailureCode
	}
}

func isEscalationFreeRetry(class EscalationFailureClass) bool {
	return class == EscalationFailureTransient || class == EscalationFailureTransientQuota
}
