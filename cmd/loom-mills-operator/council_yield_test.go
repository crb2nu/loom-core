package main

import (
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestComputeCouncilYield(t *testing.T) {
	base := time.Date(2026, 9, 1, 18, 0, 0, 0, time.UTC)
	at := func(hoursAgo int) time.Time { return base.Add(-time.Duration(hoursAgo) * time.Hour) }
	run := func(id string, hoursAgo int, outcome store.CouncilOutcome, cost float64, deltas store.BacklogDeltas) *store.CouncilRun {
		return &store.CouncilRun{ID: id, StartedAt: at(hoursAgo), Outcome: outcome, CostFrontierUSD: cost, BacklogDeltas: deltas}
	}
	yielded := store.BacklogDeltas{Created: []string{"bl-x"}}

	t.Run("dry spell counts finished runs back to the last delta", func(t *testing.T) {
		runs := []*store.CouncilRun{
			run("C-now", 0, store.CouncilOutcomeRunning, 0, store.BacklogDeltas{}), // in flight: skipped
			run("C-1", 6, store.CouncilOutcomeSuccess, 0.70, store.BacklogDeltas{}),
			run("C-2", 12, store.CouncilOutcomeError, 0, store.BacklogDeltas{}), // orphaned: counts
			run("C-3", 18, store.CouncilOutcomeSuccess, 0.65, store.BacklogDeltas{}),
			run("C-4", 24, store.CouncilOutcomeSuccess, 0.80, yielded),
			run("C-5", 30, store.CouncilOutcomeSuccess, 0.80, store.BacklogDeltas{}),
		}
		y := computeCouncilYield(runs)
		if y.RunsSinceLastDelta != 3 {
			t.Errorf("runs_since_last_delta = %d, want 3", y.RunsSinceLastDelta)
		}
		if y.CostSinceLastDeltaUSD != 1.35 {
			t.Errorf("cost_since_last_delta_usd = %v, want 1.35", y.CostSinceLastDeltaUSD)
		}
		if y.LastDeltaAt == nil || !y.LastDeltaAt.Equal(at(24)) || y.LastDeltaRunID != "C-4" {
			t.Errorf("last delta = %v/%q, want %v/C-4", y.LastDeltaAt, y.LastDeltaRunID, at(24))
		}
		if y.SampleSize != 4 {
			t.Errorf("sample_size = %d, want 4 (stops at the yielding run)", y.SampleSize)
		}
	})

	t.Run("latest finished run yielded", func(t *testing.T) {
		y := computeCouncilYield([]*store.CouncilRun{run("C-1", 6, store.CouncilOutcomeSuccess, 0.7, yielded)})
		if y.RunsSinceLastDelta != 0 || y.CostSinceLastDeltaUSD != 0 || y.LastDeltaRunID != "C-1" {
			t.Errorf("yield = %+v, want 0 runs / $0 / C-1", y)
		}
	})

	t.Run("sample exhausted without a delta leaves last_delta nil", func(t *testing.T) {
		y := computeCouncilYield([]*store.CouncilRun{
			run("C-1", 6, store.CouncilOutcomeSuccess, 0.7, store.BacklogDeltas{}),
			run("C-2", 12, store.CouncilOutcomeSuccess, 0.7, store.BacklogDeltas{Updated: []string{}}),
		})
		if y.RunsSinceLastDelta != 2 || y.SampleSize != 2 || y.LastDeltaAt != nil || y.LastDeltaRunID != "" {
			t.Errorf("yield = %+v, want 2/2 with nil last delta", y)
		}
	})

	t.Run("empty and nil input", func(t *testing.T) {
		for _, runs := range [][]*store.CouncilRun{nil, {}, {nil}} {
			y := computeCouncilYield(runs)
			if y.RunsSinceLastDelta != 0 || y.SampleSize != 0 || y.LastDeltaAt != nil {
				t.Errorf("yield(%v) = %+v, want zero", runs, y)
			}
		}
	})
}
