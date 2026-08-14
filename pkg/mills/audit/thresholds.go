package audit

const (
	ThresholdCodeOK             = "ok"
	ThresholdCodeEscalationLoad = "escalation_load"
	ThresholdCodeAuditQuality   = "audit_quality"
	ThresholdCodeGateFlakiness  = "gate_flakiness"
	ThresholdCodePipelineHealth = "pipeline_health"
)

// ThresholdSeverity is the bounded severity vocabulary for operational
// threshold policy output.
type ThresholdSeverity string

const (
	ThresholdSeverityOK       ThresholdSeverity = "ok"
	ThresholdSeverityWarning  ThresholdSeverity = "warning"
	ThresholdSeverityCritical ThresholdSeverity = "critical"
)

// ThresholdPolicy configures deterministic operational guardrails for Mills.
type ThresholdPolicy struct {
	MaxOpenEscalations     int
	MaxAuditQueueDepth     int
	MaxConsecutiveFailures int
	MinAuditSurvivalRate   float64
	MaxGateFlakeRate       float64
}

// OperationalSnapshot is the metrics bundle evaluated against ThresholdPolicy.
type OperationalSnapshot struct {
	OpenEscalations     int
	AuditQueueDepth     int
	ConsecutiveFailures int
	AuditSurvivalRate   float64
	GateFlakeRate       float64
}

// ThresholdCheck reports one checked metric and whether it crossed policy.
type ThresholdCheck struct {
	Name      string            `json:"name"`
	Pass      bool              `json:"pass"`
	Value     float64           `json:"value"`
	Threshold float64           `json:"threshold"`
	Severity  ThresholdSeverity `json:"severity"`
	Reason    string            `json:"reason,omitempty"`
}

// ThresholdVerdict is the stable machine-readable policy output.
type ThresholdVerdict struct {
	Pass     bool              `json:"pass"`
	Code     string            `json:"code"`
	Severity ThresholdSeverity `json:"severity"`
	Reasons  []string          `json:"reasons,omitempty"`
	Checks   []ThresholdCheck  `json:"checks"`
}

// DefaultThresholdPolicy returns conservative operator-facing thresholds.
func DefaultThresholdPolicy() ThresholdPolicy {
	return ThresholdPolicy{
		MaxOpenEscalations:     10,
		MaxAuditQueueDepth:     25,
		MaxConsecutiveFailures: 3,
		MinAuditSurvivalRate:   0.85,
		MaxGateFlakeRate:       0.10,
	}
}

// EvaluateOperationalThresholds checks the supplied metrics against policy and
// returns a machine-readable verdict suitable for HUD/API payloads.
func EvaluateOperationalThresholds(policy ThresholdPolicy, snapshot OperationalSnapshot) ThresholdVerdict {
	policy = normalizeThresholdPolicy(policy)
	checks := []ThresholdCheck{
		maxCheck("open_escalations", float64(snapshot.OpenEscalations), float64(policy.MaxOpenEscalations), ThresholdSeverityCritical, "open escalations exceed threshold"),
		maxCheck("audit_queue_depth", float64(snapshot.AuditQueueDepth), float64(policy.MaxAuditQueueDepth), ThresholdSeverityWarning, "audit queue depth exceeds threshold"),
		maxCheck("consecutive_failures", float64(snapshot.ConsecutiveFailures), float64(policy.MaxConsecutiveFailures), ThresholdSeverityCritical, "consecutive pipeline failures exceed threshold"),
		minCheck("audit_survival_rate", snapshot.AuditSurvivalRate, policy.MinAuditSurvivalRate, ThresholdSeverityWarning, "audit survival rate below threshold"),
		maxCheck("gate_flake_rate", snapshot.GateFlakeRate, policy.MaxGateFlakeRate, ThresholdSeverityWarning, "gate flake rate exceeds threshold"),
	}

	verdict := ThresholdVerdict{Pass: true, Code: ThresholdCodeOK, Severity: ThresholdSeverityOK, Checks: checks}
	for _, c := range checks {
		if c.Pass {
			continue
		}
		verdict.Pass = false
		verdict.Reasons = append(verdict.Reasons, c.Reason)
		if c.Severity == ThresholdSeverityCritical {
			verdict.Severity = ThresholdSeverityCritical
		} else if verdict.Severity == ThresholdSeverityOK {
			verdict.Severity = ThresholdSeverityWarning
		}
		if verdict.Code == ThresholdCodeOK || c.Severity == ThresholdSeverityCritical {
			verdict.Code = thresholdCodeFor(c.Name)
		}
	}
	return verdict
}

func normalizeThresholdPolicy(policy ThresholdPolicy) ThresholdPolicy {
	def := DefaultThresholdPolicy()
	if policy.MaxOpenEscalations <= 0 {
		policy.MaxOpenEscalations = def.MaxOpenEscalations
	}
	if policy.MaxAuditQueueDepth <= 0 {
		policy.MaxAuditQueueDepth = def.MaxAuditQueueDepth
	}
	if policy.MaxConsecutiveFailures <= 0 {
		policy.MaxConsecutiveFailures = def.MaxConsecutiveFailures
	}
	if policy.MinAuditSurvivalRate <= 0 || policy.MinAuditSurvivalRate > 1 {
		policy.MinAuditSurvivalRate = def.MinAuditSurvivalRate
	}
	if policy.MaxGateFlakeRate <= 0 || policy.MaxGateFlakeRate >= 1 {
		policy.MaxGateFlakeRate = def.MaxGateFlakeRate
	}
	return policy
}

func maxCheck(name string, value, threshold float64, severity ThresholdSeverity, reason string) ThresholdCheck {
	check := ThresholdCheck{Name: name, Pass: value <= threshold, Value: value, Threshold: threshold, Severity: severity}
	if !check.Pass {
		check.Reason = reason
	}
	return check
}

func minCheck(name string, value, threshold float64, severity ThresholdSeverity, reason string) ThresholdCheck {
	check := ThresholdCheck{Name: name, Pass: value >= threshold, Value: value, Threshold: threshold, Severity: severity}
	if !check.Pass {
		check.Reason = reason
	}
	return check
}

func thresholdCodeFor(name string) string {
	switch name {
	case "open_escalations":
		return ThresholdCodeEscalationLoad
	case "audit_survival_rate":
		return ThresholdCodeAuditQuality
	case "gate_flake_rate":
		return ThresholdCodeGateFlakiness
	case "consecutive_failures":
		return ThresholdCodePipelineHealth
	default:
		return ThresholdCodePipelineHealth
	}
}
