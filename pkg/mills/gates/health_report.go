package gates

import "time"

// HealthGateReport is the wire projection of a HealthDecision. HealthDecision
// itself carries no JSON tags — it is a decision object, not a payload — so
// the operator status endpoint and the HUD share this explicit shape instead
// of each spelling the keys by hand.
//
// internal/hud/monitor.HealthGateStatus decodes exactly these keys; the parity
// test there pins the two together so a field added here cannot silently stop
// reaching the HUD tile.
type HealthGateReport struct {
	Allowed      bool              `json:"allowed"`
	FailClosed   bool              `json:"fail_closed"`
	Status       string            `json:"status"`
	CheckedAt    time.Time         `json:"checked_at"`
	Reasons      []string          `json:"reasons,omitempty"`
	Remediations []string          `json:"remediations,omitempty"`
	Components   []HealthComponent `json:"components,omitempty"`
}

// NewHealthGateReport converts a decision into its wire projection.
func NewHealthGateReport(d HealthDecision) HealthGateReport {
	return HealthGateReport{
		Allowed:      d.Allowed,
		FailClosed:   d.FailClosed,
		Status:       d.Status,
		CheckedAt:    d.CheckedAt,
		Reasons:      append([]string(nil), d.Reasons...),
		Remediations: append([]string(nil), d.Remediations...),
		Components:   append([]HealthComponent(nil), d.Components...),
	}
}
