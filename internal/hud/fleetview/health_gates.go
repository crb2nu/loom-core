package fleetview

// HealthGateFleetBadge is the compact health-gate summary shown alongside the
// fleet autonomy state.
type HealthGateFleetBadge struct {
	Status       string `json:"status"`
	Blocked      bool   `json:"blocked"`
	FailClosed   bool   `json:"fail_closed"`
	ReasonCount  int    `json:"reason_count"`
	RemediationN int    `json:"remediation_count"`
}

func HealthGateBadge(status string, allowed bool, failClosed bool, reasons, remediations []string) HealthGateFleetBadge {
	return HealthGateFleetBadge{
		Status: status, Blocked: !allowed, FailClosed: failClosed,
		ReasonCount: len(reasons), RemediationN: len(remediations),
	}
}
