package planning

import (
	"testing"
	"time"
)

func TestEvaluateClusterResolution_RequiresThreeZeroWindows(t *testing.T) {
	now := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	got := EvaluateClusterResolution([]ClusterWindow{
		{ObservedAt: now.Add(-2 * time.Hour), Count: 0},
		{ObservedAt: now.Add(-1 * time.Hour), Count: 0},
	}, 0)
	if got.Resolved {
		t.Fatalf("resolved = true, want false")
	}
	if got.ConsecutiveZeroRuns != 2 || got.RequiredZeroRuns != 3 {
		t.Fatalf("result = %+v", got)
	}
}

func TestEvaluateClusterResolution_ResetsOnNonZeroWindow(t *testing.T) {
	now := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	got := EvaluateClusterResolution([]ClusterWindow{
		{ObservedAt: now.Add(-4 * time.Hour), Count: 0},
		{ObservedAt: now.Add(-3 * time.Hour), Count: 0},
		{ObservedAt: now.Add(-2 * time.Hour), Count: 1},
		{ObservedAt: now.Add(-1 * time.Hour), Count: 0},
	}, 0)
	if got.Resolved {
		t.Fatalf("resolved = true, want false")
	}
	if got.ConsecutiveZeroRuns != 1 {
		t.Fatalf("zero runs = %d, want 1", got.ConsecutiveZeroRuns)
	}
}

func TestEvaluateClusterResolution_ResolvesAfterThreeZeroWindows(t *testing.T) {
	now := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	got := EvaluateClusterResolution([]ClusterWindow{
		{ObservedAt: now.Add(-1 * time.Hour), Count: 0},
		{ObservedAt: now.Add(-3 * time.Hour), Count: 0},
		{ObservedAt: now.Add(-2 * time.Hour), Count: 0},
	}, 0)
	if !got.Resolved {
		t.Fatalf("resolved = false, result=%+v", got)
	}
	if got.ConsecutiveZeroRuns != 3 {
		t.Fatalf("zero runs = %d, want 3", got.ConsecutiveZeroRuns)
	}
}
