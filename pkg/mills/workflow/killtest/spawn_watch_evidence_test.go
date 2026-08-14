package killtest

import (
	"strings"
	"testing"
	"time"
)

func TestValidateSpawnPodWatchCoverageOrdersFullGateBeforeLaunch(t *testing.T) {
	closedAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	spawnID := "abc"
	ev := Evidence{
		GateBinding:                 GateBinding{GateID: "gate"},
		SpawnID:                     spawnID,
		SpawnPodName:                "spawn-abc",
		SpawnPodWatchStartedAt:      closedAt.Add(time.Second),
		CanaryLaunchRequestedAt:     closedAt.Add(2 * time.Second),
		SpawnPodWatchEndedAt:        closedAt.Add(10 * time.Second),
		SpawnPodWatchInitialRV:      "100",
		SpawnPodWatchContinuous:     true,
		CanaryHoldInitial:           CanaryHoldObservation{ObservedAt: closedAt.Add(3 * time.Second)},
		PostCrashProcessObservedEnd: closedAt.Add(9 * time.Second),
		TotalSpawnPodIncarnations:   []PodIdentity{{Name: "spawn-abc", UID: "uid-abc"}},
		SpawnPodWatchEvents: []SpawnPodWatchEvent{{
			Type: "ADDED", ResourceVersion: "101", ObservedAt: closedAt.Add(2500 * time.Millisecond),
			Pod: PodIdentity{Name: "spawn-abc", UID: "uid-abc"}, SpawnIDLabel: &spawnID,
		}},
	}
	ev.InitialPreflight.FluxSourcesEnd.GitRepositories.ObservedAt = closedAt
	if err := validateSpawnPodWatchCoverage(ev); err != nil {
		t.Fatalf("valid pre-launch ordering rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Evidence)
	}{
		{"missing launch timestamp", func(ev *Evidence) { ev.CanaryLaunchRequestedAt = time.Time{} }},
		{"watch before closing preflight", func(ev *Evidence) { ev.SpawnPodWatchStartedAt = closedAt.Add(-time.Nanosecond) }},
		{"launch before watch", func(ev *Evidence) { ev.CanaryLaunchRequestedAt = ev.SpawnPodWatchStartedAt.Add(-time.Nanosecond) }},
		{"hold before launch", func(ev *Evidence) { ev.CanaryHoldInitial.ObservedAt = ev.CanaryLaunchRequestedAt.Add(-time.Nanosecond) }},
		{"watch ends before process proof", func(ev *Evidence) { ev.SpawnPodWatchEndedAt = ev.PostCrashProcessObservedEnd.Add(-time.Nanosecond) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutated := ev
			tt.mutate(&mutated)
			if err := validateSpawnPodWatchCoverage(mutated); err == nil {
				t.Fatal("invalid watch ordering accepted")
			}
		})
	}
}

func TestValidateSpawnPodWatchCoveragePreservesRecoveryMode(t *testing.T) {
	startedAt := time.Now().UTC()
	ev := Evidence{
		SpawnPodWatchStartedAt:      startedAt,
		SpawnPodWatchEndedAt:        startedAt.Add(3 * time.Second),
		SpawnPodWatchInitialRV:      "200",
		SpawnPodWatchContinuous:     true,
		CanaryHoldInitial:           CanaryHoldObservation{ObservedAt: startedAt.Add(time.Second)},
		PostCrashProcessObservedEnd: startedAt.Add(2 * time.Second),
	}
	if err := validateSpawnPodWatchCoverage(ev); err != nil {
		t.Fatalf("recovery watch without launch timestamp rejected: %v", err)
	}
	ev.SpawnPodWatchStartedAt = ev.CanaryHoldInitial.ObservedAt.Add(time.Nanosecond)
	if err := validateSpawnPodWatchCoverage(ev); err == nil || !strings.Contains(err.Error(), "recovery") {
		t.Fatalf("late recovery observer error = %v", err)
	}
}
