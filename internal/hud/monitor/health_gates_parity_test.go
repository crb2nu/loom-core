package monitor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/gates"
)

// The operator publishes gates.HealthGateReport under the status payload's
// health_gates key and this package decodes it into HealthGateStatus. The two
// shapes are declared in different packages, so this pins them together: a
// field added on one side without the other silently stops reaching the HUD
// tile, which is how the tile came to render its fail-closed default while
// the gate chain was thought to be wired.
func TestHealthGateReport_RoundTripsIntoHealthGateStatus(t *testing.T) {
	checkedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	report := gates.NewHealthGateReport(gates.HealthDecision{
		Allowed:      false,
		FailClosed:   true,
		Status:       "block",
		CheckedAt:    checkedAt,
		Reasons:      []string{"critical dependency mills-store is down"},
		Remediations: []string{"free space on the Mills data volume"},
		Components: []gates.HealthComponent{{
			Name: "mills-store", State: gates.HealthStateDown, Critical: true, CheckedAt: checkedAt,
		}},
	})

	// Marshal exactly as the operator does, then decode through the real
	// snapshot path the monitor uses.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal to generic: %v", err)
	}

	got := HealthGateStatusFromMillsSnapshot(MillsSnapshot{"health_gates": generic}, checkedAt)

	if got.Allowed {
		t.Error("Allowed did not survive the round trip")
	}
	if !got.FailClosed {
		t.Error("FailClosed did not survive the round trip")
	}
	if got.Status != "block" {
		t.Errorf("Status = %q, want block", got.Status)
	}
	if !got.CheckedAt.Equal(checkedAt) {
		t.Errorf("CheckedAt = %v, want %v", got.CheckedAt, checkedAt)
	}
	if len(got.Reasons) != 1 || got.Reasons[0] != "critical dependency mills-store is down" {
		t.Errorf("Reasons = %v", got.Reasons)
	}
	if len(got.Remediations) != 1 {
		t.Errorf("Remediations = %v", got.Remediations)
	}
	if len(got.Components) != 1 || got.Components[0].Name != "mills-store" {
		t.Errorf("Components = %+v", got.Components)
	}
	if got.Components[0].State != gates.HealthStateDown {
		t.Errorf("component state = %q, want down", got.Components[0].State)
	}
}

// An allowing verdict must survive too — the tile should show green when the
// gates pass, not just render blocks correctly.
func TestHealthGateReport_AllowingVerdictRoundTrips(t *testing.T) {
	checkedAt := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	report := gates.NewHealthGateReport(gates.HealthDecision{
		Allowed: true, Status: "pass", CheckedAt: checkedAt,
		Components: []gates.HealthComponent{{
			Name: "mills-store", State: gates.HealthStateHealthy, Critical: true, CheckedAt: checkedAt,
		}},
	})

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal to generic: %v", err)
	}

	got := HealthGateStatusFromMillsSnapshot(MillsSnapshot{"health_gates": generic}, checkedAt)
	if !got.Allowed || got.Status != "pass" {
		t.Fatalf("allowing verdict decoded as %+v", got)
	}
}

// The JSON key set must match field for field in both directions.
func TestHealthGateReport_KeySetMatchesHealthGateStatus(t *testing.T) {
	full := gates.HealthGateReport{
		Allowed: true, FailClosed: true, Status: "pass", CheckedAt: time.Unix(0, 0).UTC(),
		Reasons: []string{"r"}, Remediations: []string{"m"},
		Components: []gates.HealthComponent{{Name: "c"}},
	}
	status := HealthGateStatus{
		Allowed: true, FailClosed: true, Status: "pass", CheckedAt: time.Unix(0, 0).UTC(),
		Reasons: []string{"r"}, Remediations: []string{"m"},
		Components: []gates.HealthComponent{{Name: "c"}},
	}

	reportJSON, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	statusJSON, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	if string(reportJSON) != string(statusJSON) {
		t.Fatalf("wire shapes diverged:\n report = %s\n status = %s", reportJSON, statusJSON)
	}
}
