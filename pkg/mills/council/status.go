package council

import (
	"sort"
	"strings"

	"github.com/crb2nu/loom/pkg/telemetry"
)

const (
	CouncilStatusCodeOK                = "ok"
	CouncilStatusCodeTelemetryDegraded = "telemetry_degraded"

	DegradedReasonDependencyDegraded = "dependency_degraded"
	DegradedReasonIncidentActive     = "dependency_incident_active"
	DegradedReasonTelemetryReason    = "telemetry_reason"
)

// CouncilStatus is the council-facing operational status projection. It keeps
// degraded-mode reason codes machine-readable so automation and operators do
// not need to parse free-form telemetry reason text.
type CouncilStatus struct {
	OperationalState     telemetry.MillsOperationalState `json:"operational_state"`
	DegradedMode         bool                            `json:"degraded_mode"`
	DegradedReasons      []DegradedReason                `json:"degraded_reasons,omitempty"`
	Reasons              []string                        `json:"reasons,omitempty"`
	DegradedDependencies []string                        `json:"degraded_dependencies,omitempty"`
	ActiveIncidents      []telemetry.DependencyIncident  `json:"active_incidents,omitempty"`
	PolicyVerdict        PolicyVerdict                   `json:"policy_verdict"`
}

// DegradedReason is a stable degraded-mode reason. Code is bounded; Dependency,
// IncidentID, and Message carry the variable operator-facing detail.
type DegradedReason struct {
	Code       string `json:"code"`
	Dependency string `json:"dependency,omitempty"`
	IncidentID string `json:"incident_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

// CouncilStatusInput is the raw telemetry bundle used to derive a council
// status without coupling callers to the telemetry package's input type.
type CouncilStatusInput struct {
	ActiveWork                int
	AutonomyAllowed           bool
	AutonomyBlockers          []string
	HealthAllowed             bool
	HealthReasons             []string
	DegradedDependencies      []string
	ActiveDependencyIncidents []telemetry.DependencyIncident
}

// EvaluateCouncilStatus derives the shared Mills operational state and then
// surfaces degraded-mode reason codes for council policy consumers.
func EvaluateCouncilStatus(in CouncilStatusInput) CouncilStatus {
	report := telemetry.EvaluateMillsOperationalState(telemetry.MillsOperationalStateInput{
		ActiveWork:                in.ActiveWork,
		AutonomyAllowed:           in.AutonomyAllowed,
		AutonomyBlockers:          in.AutonomyBlockers,
		HealthAllowed:             in.HealthAllowed,
		HealthReasons:             in.HealthReasons,
		DegradedDependencies:      in.DegradedDependencies,
		ActiveDependencyIncidents: in.ActiveDependencyIncidents,
	})
	return CouncilStatusFromOperationalState(report)
}

// CouncilStatusFromOperationalState adapts an already-derived telemetry report
// into the council's stable output shape.
func CouncilStatusFromOperationalState(report telemetry.MillsOperationalStateReport) CouncilStatus {
	status := CouncilStatus{
		OperationalState:     report.State,
		Reasons:              append([]string(nil), report.Reasons...),
		DegradedDependencies: append([]string(nil), report.DegradedDependencies...),
		ActiveIncidents:      append([]telemetry.DependencyIncident(nil), report.ActiveIncidents...),
	}
	status.DegradedMode = report.State == telemetry.MillsOperationalStateDegraded
	status.DegradedReasons = degradedReasonsForReport(report)
	status.PolicyVerdict = councilStatusPolicyVerdict(status)
	return status
}

func degradedReasonsForReport(report telemetry.MillsOperationalStateReport) []DegradedReason {
	var reasons []DegradedReason
	for _, dep := range report.DegradedDependencies {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			continue
		}
		reasons = append(reasons, DegradedReason{
			Code:       DegradedReasonDependencyDegraded,
			Dependency: dep,
		})
	}
	for _, incident := range report.ActiveIncidents {
		dep := strings.TrimSpace(incident.Dependency)
		incidentID := strings.TrimSpace(incident.ID)
		if dep == "" && incidentID == "" {
			continue
		}
		reasons = append(reasons, DegradedReason{
			Code:       DegradedReasonIncidentActive,
			Dependency: dep,
			IncidentID: incidentID,
			Message:    strings.TrimSpace(incident.Summary),
		})
	}
	if report.State == telemetry.MillsOperationalStateDegraded && len(reasons) == 0 {
		for _, reason := range report.Reasons {
			reason = strings.TrimSpace(reason)
			if reason == "" {
				continue
			}
			reasons = append(reasons, DegradedReason{
				Code:    DegradedReasonTelemetryReason,
				Message: reason,
			})
		}
	}
	return normalizeDegradedReasons(reasons)
}

func councilStatusPolicyVerdict(status CouncilStatus) PolicyVerdict {
	if !status.DegradedMode {
		return PolicyVerdict{
			Pass:     true,
			Code:     CouncilStatusCodeOK,
			Severity: PolicySeverityOK,
			Action:   PolicyActionNone,
		}
	}
	return PolicyVerdict{
		Pass:     false,
		Code:     CouncilStatusCodeTelemetryDegraded,
		Severity: PolicySeverityWarning,
		Action:   PolicyActionEscalate,
		Reasons:  degradedReasonMessages(status.DegradedReasons, status.Reasons),
		Metrics: map[string]float64{
			"degraded_dependencies": float64(len(status.DegradedDependencies)),
			"active_incidents":      float64(len(status.ActiveIncidents)),
		},
	}
}

func degradedReasonMessages(degraded []DegradedReason, fallback []string) []string {
	var out []string
	for _, reason := range degraded {
		switch reason.Code {
		case DegradedReasonDependencyDegraded:
			out = append(out, "dependency degraded: "+reason.Dependency)
		case DegradedReasonIncidentActive:
			msg := "dependency incident active"
			if reason.Dependency != "" {
				msg += ": " + reason.Dependency
			}
			if reason.IncidentID != "" {
				msg += " (" + reason.IncidentID + ")"
			}
			if reason.Message != "" {
				msg += ": " + reason.Message
			}
			out = append(out, msg)
		case DegradedReasonTelemetryReason:
			if reason.Message != "" {
				out = append(out, reason.Message)
			}
		}
	}
	if len(out) == 0 {
		out = append(out, fallback...)
	}
	return normalizeStringsLocal(out)
}

func normalizeDegradedReasons(reasons []DegradedReason) []DegradedReason {
	if len(reasons) == 0 {
		return nil
	}
	seen := make(map[DegradedReason]struct{}, len(reasons))
	out := make([]DegradedReason, 0, len(reasons))
	for _, reason := range reasons {
		reason.Code = strings.TrimSpace(reason.Code)
		reason.Dependency = strings.TrimSpace(reason.Dependency)
		reason.IncidentID = strings.TrimSpace(reason.IncidentID)
		reason.Message = strings.TrimSpace(reason.Message)
		if reason.Code == "" {
			continue
		}
		if _, ok := seen[reason]; ok {
			continue
		}
		seen[reason] = struct{}{}
		out = append(out, reason)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		if out[i].Dependency != out[j].Dependency {
			return out[i].Dependency < out[j].Dependency
		}
		if out[i].IncidentID != out[j].IncidentID {
			return out[i].IncidentID < out[j].IncidentID
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func normalizeStringsLocal(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
