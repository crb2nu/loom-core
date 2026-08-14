package monitor

import (
	"encoding/json"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
)

// HealthGateStatus is the operator-facing HUD projection of Mills
// infrastructure health gates.
type HealthGateStatus struct {
	Allowed      bool                    `json:"allowed"`
	FailClosed   bool                    `json:"fail_closed"`
	Status       string                  `json:"status"`
	CheckedAt    time.Time               `json:"checked_at"`
	Reasons      []string                `json:"reasons,omitempty"`
	Remediations []string                `json:"remediations,omitempty"`
	Components   []gates.HealthComponent `json:"components,omitempty"`
}

// HealthGateStatusFromDecision converts the shared gate decision into the
// stable HUD JSON shape.
func HealthGateStatusFromDecision(d gates.HealthDecision) HealthGateStatus {
	return HealthGateStatus{
		Allowed: d.Allowed, FailClosed: d.FailClosed, Status: d.Status, CheckedAt: d.CheckedAt,
		Reasons: append([]string(nil), d.Reasons...), Remediations: append([]string(nil), d.Remediations...),
		Components: append([]gates.HealthComponent(nil), d.Components...),
	}
}

// HealthGateStatusFromMillsSnapshot extracts health_gates from the free-form
// operator status payload. Missing or malformed data returns a fail-closed view.
func HealthGateStatusFromMillsSnapshot(s MillsSnapshot, now time.Time) HealthGateStatus {
	raw, ok := s["health_gates"]
	if !ok {
		return HealthGateStatusFromDecision(gates.EvaluateHealthSnapshot(gates.HealthSnapshot{}, now))
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return HealthGateStatusFromDecision(gates.HealthDecision{Allowed: false, FailClosed: true, Status: "block", CheckedAt: now, Reasons: []string{"health gate status is not serializable"}})
	}
	var out HealthGateStatus
	if err := json.Unmarshal(b, &out); err != nil {
		return HealthGateStatusFromDecision(gates.HealthDecision{Allowed: false, FailClosed: true, Status: "block", CheckedAt: now, Reasons: []string{"health gate status is malformed"}})
	}
	if out.Status == "" {
		return HealthGateStatusFromDecision(gates.HealthDecision{Allowed: false, FailClosed: true, Status: "block", CheckedAt: now, Reasons: []string{"health gate status is incomplete"}})
	}
	return out
}
