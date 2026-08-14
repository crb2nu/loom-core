package alerting

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/internal/hud/monitor"
)

func TestEvaluateHealthGateStatus_FiresCriticalOnFailClosedBlock(t *testing.T) {
	alerts := EvaluateHealthGateStatus(monitor.HealthGateStatus{
		Allowed: false, FailClosed: true, Reasons: []string{"gitlab down"},
	}, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if len(alerts) != 1 {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Severity != "critical" || alerts[0].Message != "gitlab down" {
		t.Fatalf("alert = %+v", alerts[0])
	}
}

func TestEvaluateHealthGateStatus_AllowsSilently(t *testing.T) {
	if got := EvaluateHealthGateStatus(monitor.HealthGateStatus{Allowed: true}, time.Now()); len(got) != 0 {
		t.Fatalf("alerts = %+v, want none", got)
	}
}
