package monitor

import (
	"testing"
	"time"
)

func TestHealthGateStatusFromMillsSnapshot_MissingFailsClosed(t *testing.T) {
	got := HealthGateStatusFromMillsSnapshot(MillsSnapshot{}, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if got.Allowed || !got.FailClosed {
		t.Fatalf("status = %+v, want fail-closed", got)
	}
}

func TestHealthGateStatusFromMillsSnapshot_DecodesStatus(t *testing.T) {
	got := HealthGateStatusFromMillsSnapshot(MillsSnapshot{"health_gates": map[string]any{
		"allowed": false, "fail_closed": true, "status": "block", "reasons": []any{"gitlab down"},
	}}, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if got.Allowed || !got.FailClosed || len(got.Reasons) != 1 || got.Reasons[0] != "gitlab down" {
		t.Fatalf("status = %+v", got)
	}
}

func TestHealthGateStatusFromMillsSnapshot_IncompleteFailsClosed(t *testing.T) {
	got := HealthGateStatusFromMillsSnapshot(MillsSnapshot{"health_gates": map[string]any{}}, time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC))
	if got.Allowed || !got.FailClosed || got.Status != "block" {
		t.Fatalf("status = %+v, want fail-closed block", got)
	}
}
