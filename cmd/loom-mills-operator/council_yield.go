package main

import (
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// councilYieldSample bounds how many recent council runs the status handler
// examines when computing the yield block. Fifty is two days of the 6h cron
// cadence plus manual replays — enough to spot a dry spell, cheap enough to
// scan on every status poll.
const councilYieldSample = 50

// councilYield tells an operator whether council deliberation is still
// producing backlog work. Live 2026-09-01: five consecutive successful cron
// runs (~$0.70 each) reported `BacklogDeltas: {}` — every proposal was
// dropped as external-only or merely labeled — and nothing on the status
// surface said so; `last_council_at` kept advancing, `council_roi` stayed
// positive off merges the council had not minted. This block makes that dry
// spell a first-class signal.
type councilYield struct {
	// RunsSinceLastDelta counts finished runs, newest first, before the most
	// recent run that intended any backlog mutation. Zero means the latest
	// finished run yielded.
	RunsSinceLastDelta int `json:"runs_since_last_delta"`
	// CostSinceLastDeltaUSD is the frontier + local spend of those runs.
	CostSinceLastDeltaUSD float64 `json:"cost_since_last_delta_usd"`
	// LastDeltaAt is the start time of the most recent yielding run, nil when
	// no run in the sample yielded.
	LastDeltaAt *time.Time `json:"last_delta_at"`
	// LastDeltaRunID names that run so an operator can jump straight to it.
	LastDeltaRunID string `json:"last_delta_run_id,omitempty"`
	// SampleSize is how many finished runs were examined. When
	// RunsSinceLastDelta == SampleSize and LastDeltaAt is nil the dry spell is
	// at least the whole sample — the true length is unknown, not zero.
	SampleSize int `json:"sample_size"`
}

// computeCouncilYield folds a newest-first council run list into the yield
// block. Runs still in flight are skipped: they have not had the chance to
// yield yet. Errored and orphaned runs count as non-yielding — they spent the
// slot and produced nothing, which is exactly what the block measures.
func computeCouncilYield(runs []*store.CouncilRun) councilYield {
	var y councilYield
	for _, r := range runs {
		if r == nil || r.Outcome == store.CouncilOutcomeRunning {
			continue
		}
		y.SampleSize++
		if !r.BacklogDeltas.IsEmpty() {
			t := r.StartedAt
			y.LastDeltaAt = &t
			y.LastDeltaRunID = r.ID
			return y
		}
		y.RunsSinceLastDelta++
		y.CostSinceLastDeltaUSD += r.CostFrontierUSD + r.CostLocalUSD
	}
	return y
}
