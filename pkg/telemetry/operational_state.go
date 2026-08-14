package telemetry

import (
	"sort"
	"strings"
)

// MillsOperationalState is the bounded operator state vocabulary exposed to
// Mills telemetry consumers.
type MillsOperationalState string

const (
	MillsOperationalStateActive      MillsOperationalState = "active"
	MillsOperationalStateIdleHealthy MillsOperationalState = "idle_healthy"
	MillsOperationalStateIdleBlocked MillsOperationalState = "idle_blocked"
	MillsOperationalStateDegraded    MillsOperationalState = "degraded"
)

// DependencyIncident identifies an active external dependency incident that
// should put Mills into degraded mode even when no local work is running.
type DependencyIncident struct {
	ID         string `json:"id,omitempty"`
	Dependency string `json:"dependency"`
	Summary    string `json:"summary,omitempty"`
}

// MillsOperationalStateInput is the pure input bundle for deriving the
// operator's current work/health state.
type MillsOperationalStateInput struct {
	ActiveWork                int
	AutonomyAllowed           bool
	AutonomyBlockers          []string
	HealthAllowed             bool
	HealthReasons             []string
	DegradedDependencies      []string
	ActiveDependencyIncidents []DependencyIncident
}

// MillsOperationalStateReport is the stable state-model output.
type MillsOperationalStateReport struct {
	State                MillsOperationalState `json:"state"`
	Reasons              []string              `json:"reasons,omitempty"`
	DegradedDependencies []string              `json:"degraded_dependencies,omitempty"`
	ActiveIncidents      []DependencyIncident  `json:"active_incidents,omitempty"`
}

// EvaluateMillsOperationalState distinguishes healthy idleness from false-idle
// blocked states and dependency degraded states.
func EvaluateMillsOperationalState(in MillsOperationalStateInput) MillsOperationalStateReport {
	report := MillsOperationalStateReport{
		DegradedDependencies: normalizeStrings(in.DegradedDependencies),
		ActiveIncidents:      normalizeDependencyIncidents(in.ActiveDependencyIncidents),
	}
	report.Reasons = append(report.Reasons, normalizeStrings(in.AutonomyBlockers)...)
	report.Reasons = append(report.Reasons, normalizeStrings(in.HealthReasons)...)

	if len(report.ActiveIncidents) > 0 || len(report.DegradedDependencies) > 0 {
		report.State = MillsOperationalStateDegraded
		if len(report.Reasons) == 0 {
			report.Reasons = []string{"dependency incident active"}
		}
		return report
	}
	if in.ActiveWork > 0 {
		report.State = MillsOperationalStateActive
		return report
	}
	if !in.AutonomyAllowed || !in.HealthAllowed || len(report.Reasons) > 0 {
		report.State = MillsOperationalStateIdleBlocked
		if len(report.Reasons) == 0 {
			report.Reasons = []string{"operator idle but autonomy is blocked"}
		}
		return report
	}
	report.State = MillsOperationalStateIdleHealthy
	return report
}

func normalizeStrings(values []string) []string {
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

func normalizeDependencyIncidents(values []DependencyIncident) []DependencyIncident {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]DependencyIncident, 0, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		value.Dependency = strings.TrimSpace(value.Dependency)
		value.Summary = strings.TrimSpace(value.Summary)
		if value.Dependency == "" && value.ID == "" {
			continue
		}
		key := value.ID + "\x00" + value.Dependency
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dependency == out[j].Dependency {
			return out[i].ID < out[j].ID
		}
		return out[i].Dependency < out[j].Dependency
	})
	return out
}
