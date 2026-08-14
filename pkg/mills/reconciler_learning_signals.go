package mills

import (
	"context"
	"errors"
	"time"
)

const (
	// DefaultLearningSignalInterval is how often the export sweep runs when the
	// operator sets no interval. Thirty minutes: the reports it republishes are
	// two-week aggregates, so a faster cadence re-derives the same numbers at
	// real store cost, and a slower one would leave a judge-drift alert reading
	// evidence older than the incident it is meant to catch.
	DefaultLearningSignalInterval = 30 * time.Minute
	// defaultLearningSignalWindow matches the judge-calibration endpoint's
	// default: long enough that runs graded early in it have reached a terminal
	// state, which is the only way a verdict acquires ground truth.
	defaultLearningSignalWindow = 336 * time.Hour
	// learningSignalSweepTimeout reserves part of the tick budget for the
	// sweep's window scans over events and terminal runs.
	learningSignalSweepTimeout = 30 * time.Second
)

// LearningSignalSweepResult summarises one learning-signal export pass. The
// counts are the published gauges' own denominators, folded into the tick
// rollup so a window that exported nothing is visible in the ledger without
// scraping /metrics.
type LearningSignalSweepResult struct {
	// Gates is the number of gate rows published — the width of the judge
	// calibration families this pass.
	Gates int
	// JoinedVerdicts is how many verdicts in the window reached a terminal
	// outcome, summed across gates.
	JoinedVerdicts int
	// PromotionActions is the window's audited action volume across every
	// actor under the configured prefix.
	PromotionActions int
	// ConfigRuns is the window's provenance-stamped run count.
	ConfigRuns int
	// Regressions is the window's attributed post-merge regression count.
	Regressions int
}

// LearningSignalPublisher recomputes the factory's learning-signal reports over
// a window and publishes them as Prometheus gauges.
//
// It exists as an injected seam rather than a method here because the report
// builders live in pkg/mills/guard, which imports this package — the dependency
// runs the other way, so the reconciler cannot call them directly. The operator
// wires guard's exporter, which reuses the same builders the report endpoints
// serve; there is no second aggregation.
type LearningSignalPublisher interface {
	PublishLearningSignals(ctx context.Context, since, now time.Time) (LearningSignalSweepResult, error)
}

// SweepLearningSignals republishes the learning-signal gauges over the
// configured window.
//
// The reconciler owns the schedule and the clock; the publisher owns the
// aggregation and the gauge writes. A nil publisher disables the sweep, and a
// publisher failure is returned for the caller to log — it never wedges the
// tick, and it deliberately publishes nothing rather than a partial family, so
// the gauges hold their last good values while
// mills_learning_signal_export_errors_total says they are stale.
func (r *Reconciler) SweepLearningSignals(ctx context.Context) (LearningSignalSweepResult, error) {
	res := LearningSignalSweepResult{}
	if r == nil {
		return res, errors.New("reconciler: not configured")
	}
	if r.LearningSignals == nil {
		return res, nil
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	now := r.now()
	return r.LearningSignals.PublishLearningSignals(ctx, now.Add(-r.learningSignalWindow()), now)
}

func (r *Reconciler) learningSignalWindow() time.Duration {
	if r != nil && r.LearningSignalWindow > 0 {
		return r.LearningSignalWindow
	}
	return defaultLearningSignalWindow
}

func (r *Reconciler) learningSignalInterval() time.Duration {
	if r != nil && r.LearningSignalInterval > 0 {
		return r.LearningSignalInterval
	}
	return DefaultLearningSignalInterval
}

// learningSignalDue reports whether the interval has elapsed since the last
// attempt. The schedule is process-local (ticks are serial, so no locking): a
// restart merely runs one extra pass, which only overwrites gauges with the
// same values.
func (r *Reconciler) learningSignalDue(now time.Time) bool {
	if r == nil || r.LearningSignals == nil {
		return false
	}
	return !now.Before(r.nextLearningSignals)
}
