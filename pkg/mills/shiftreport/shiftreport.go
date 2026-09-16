package shiftreport

import "time"

type MainRedExternalHold struct {
	Project     string    `json:"project"`
	Branch      string    `json:"branch"`
	ActivatedAt time.Time `json:"activated_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Escalated   bool      `json:"escalated"`
}

func (h *MainRedExternalHold) state(now time.Time) string {
	if h == nil {
		return "absent"
	}
	if h.Escalated {
		return "expired_escalated"
	}
	if !now.Before(h.ExpiresAt) {
		return "expired"
	}
	return "active"
}
