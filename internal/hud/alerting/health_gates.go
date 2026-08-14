package alerting

import (
	"fmt"
	"strings"
	"time"

	"github.com/crb2nu/loom/internal/hud/monitor"
)

// EvaluateHealthGateStatus emits a critical operator alert when Mills autonomy
// is blocked by infrastructure health gates.
func EvaluateHealthGateStatus(status monitor.HealthGateStatus, now time.Time) []Alert {
	if status.Allowed {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	msg := "Mills autonomy is blocked by infrastructure health gates."
	if len(status.Reasons) > 0 {
		msg = strings.Join(status.Reasons, "; ")
	}
	severity := "warning"
	if status.FailClosed {
		severity = "critical"
	}
	return []Alert{{
		ID:       fmt.Sprintf("health-gates-%d", now.UnixNano()),
		RuleID:   "mills-health-gates",
		RuleName: "Mills Health Gates",
		Severity: severity,
		Title:    "Mills health gates blocking autonomy",
		Message:  msg,
		FiredAt:  now,
	}}
}
