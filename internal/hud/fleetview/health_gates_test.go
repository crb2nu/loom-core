package fleetview

import (
	"testing"
)

func TestHealthGateBadge(t *testing.T) {
	got := HealthGateBadge("block", false, true, []string{"a", "b"}, []string{"fix"})
	if !got.Blocked || !got.FailClosed || got.ReasonCount != 2 || got.RemediationN != 1 {
		t.Fatalf("badge = %+v", got)
	}
}
