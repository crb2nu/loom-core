package gates

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/telemetry"
)

// HealthState is the normalized state for one infrastructure dependency that
// Mills autonomy relies on before starting a pipeline run.
type HealthState string

const (
	HealthStateHealthy  HealthState = "healthy"
	HealthStateDegraded HealthState = "degraded"
	HealthStateDown     HealthState = "down"
	HealthStateUnknown  HealthState = "unknown"
)

// HealthComponent is one infrastructure dependency observed by the operator.
// Critical components fail the aggregate gate unless they are healthy and fresh.
type HealthComponent struct {
	Name           string      `json:"name"`
	State          HealthState `json:"state"`
	Critical       bool        `json:"critical"`
	CheckedAt      time.Time   `json:"checked_at"`
	Error          string      `json:"error,omitempty"`
	Remediation    string      `json:"remediation,omitempty"`
	IncidentID     string      `json:"incident_id,omitempty"`
	IncidentActive bool        `json:"incident_active,omitempty"`
}

// HealthSnapshot is the evidence bundle evaluated by the autonomy gate.
type HealthSnapshot struct {
	Components []HealthComponent
	ObservedAt time.Time
	MaxAge     time.Duration
}

// HealthDecision is the fail-closed aggregate verdict for infrastructure gates.
type HealthDecision struct {
	Allowed              bool
	FailClosed           bool
	Status               string
	CheckedAt            time.Time
	OperationalState     telemetry.MillsOperationalState
	Reasons              []string
	Remediations         []string
	DegradedDependencies []string
	ActiveIncidents      []telemetry.DependencyIncident
	Components           []HealthComponent
}

// EvaluateHealthSnapshot returns an autonomy decision for the supplied
// infrastructure evidence. Missing evidence, stale evidence, missing critical
// dependencies, and any non-healthy critical dependency all block.
func EvaluateHealthSnapshot(s HealthSnapshot, now time.Time) HealthDecision {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if s.ObservedAt.IsZero() {
		s.ObservedAt = newestHealthCheck(s.Components)
	}
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}

	d := HealthDecision{
		Allowed:          true,
		Status:           "pass",
		CheckedAt:        now,
		OperationalState: telemetry.MillsOperationalStateIdleHealthy,
		Components:       append([]HealthComponent(nil), s.Components...),
	}
	sort.SliceStable(d.Components, func(i, j int) bool {
		return d.Components[i].Name < d.Components[j].Name
	})

	if len(s.Components) == 0 {
		return failHealthDecision(d, true, "no infrastructure health evidence available")
	}
	if s.ObservedAt.IsZero() {
		return failHealthDecision(d, true, "infrastructure health evidence has no timestamp")
	}
	if age := now.Sub(s.ObservedAt); age < 0 || age > maxAge {
		return failHealthDecision(d, true, fmt.Sprintf("infrastructure health evidence is stale: age %s exceeds %s", age.Truncate(time.Second), maxAge))
	}

	criticalSeen := false
	for _, c := range d.Components {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			d.Allowed = false
			d.FailClosed = true
			d.Reasons = append(d.Reasons, "health component missing name")
			continue
		}
		state := normalizeHealthState(c.State)
		if state == HealthStateDegraded {
			d.DegradedDependencies = append(d.DegradedDependencies, name)
		}
		if c.IncidentActive {
			d.ActiveIncidents = append(d.ActiveIncidents, telemetry.DependencyIncident{
				ID:         c.IncidentID,
				Dependency: name,
				Summary:    c.Error,
			})
		}
		if !c.Critical {
			continue
		}
		criticalSeen = true
		if state != HealthStateHealthy {
			d.Allowed = false
			if state == HealthStateUnknown {
				d.FailClosed = true
			}
			msg := fmt.Sprintf("critical dependency %s is %s", name, state)
			if c.Error != "" {
				msg += ": " + c.Error
			}
			d.Reasons = append(d.Reasons, msg)
			if strings.TrimSpace(c.Remediation) != "" {
				d.Remediations = append(d.Remediations, strings.TrimSpace(c.Remediation))
			}
		}
		if c.CheckedAt.IsZero() {
			d.Allowed = false
			d.FailClosed = true
			d.Reasons = append(d.Reasons, fmt.Sprintf("critical dependency %s has no check timestamp", name))
		} else if age := now.Sub(c.CheckedAt); age < 0 || age > maxAge {
			d.Allowed = false
			d.FailClosed = true
			d.Reasons = append(d.Reasons, fmt.Sprintf("critical dependency %s health check is stale: age %s exceeds %s", name, age.Truncate(time.Second), maxAge))
		}
	}
	if !criticalSeen {
		return failHealthDecision(d, true, "no critical infrastructure dependencies declared")
	}
	if !d.Allowed {
		d.Status = "block"
	}
	d.applyOperationalState()
	return d
}

func failHealthDecision(d HealthDecision, failClosed bool, reason string) HealthDecision {
	d.Allowed = false
	d.FailClosed = failClosed
	d.Status = "block"
	d.Reasons = append(d.Reasons, reason)
	d.applyOperationalState()
	return d
}

func (d *HealthDecision) applyOperationalState() {
	report := telemetry.EvaluateMillsOperationalState(telemetry.MillsOperationalStateInput{
		AutonomyAllowed:           true,
		HealthAllowed:             d.Allowed,
		HealthReasons:             d.Reasons,
		DegradedDependencies:      d.DegradedDependencies,
		ActiveDependencyIncidents: d.ActiveIncidents,
	})
	d.OperationalState = report.State
	d.DegradedDependencies = report.DegradedDependencies
	d.ActiveIncidents = report.ActiveIncidents
}

func newestHealthCheck(components []HealthComponent) time.Time {
	var newest time.Time
	for _, c := range components {
		if c.CheckedAt.After(newest) {
			newest = c.CheckedAt
		}
	}
	return newest
}

func normalizeHealthState(s HealthState) HealthState {
	switch HealthState(strings.ToLower(strings.TrimSpace(string(s)))) {
	case HealthStateHealthy:
		return HealthStateHealthy
	case HealthStateDegraded:
		return HealthStateDegraded
	case HealthStateDown:
		return HealthStateDown
	default:
		return HealthStateUnknown
	}
}
