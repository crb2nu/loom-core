package guard

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
)

// LearningSignalExporter republishes the factory's learning-signal reports as
// Prometheus gauges.
//
// It reuses the same builders the operator's report endpoints serve — there is
// no second aggregation here, only a projection of the finished reports onto a
// closed label set. It satisfies mills.LearningSignalPublisher, so the
// reconciler schedules it like any other periodic sweep.
type LearningSignalExporter struct {
	Events EventLister
	Runs   RunOutcomeLister
	// PromotionActorPrefix scopes the promotion-evidence gauges to one guarded
	// family (the endpoints default to "overseer."). Required: an empty prefix
	// would widen the report to every writer in the events table, and every
	// such writer would become a metric series.
	PromotionActorPrefix string
}

// PublishLearningSignals builds the three reports over [since, now] and, only
// if all three succeed, overwrites the learning-signal gauge families.
//
// All-or-nothing by design. A half-published sweep would leave a gate's mean
// score from this window next to its sample size from the last one, and a drift
// alert reading the pair would be comparing two different realities. On failure
// nothing is written: the gauges keep their previous values and
// mills_learning_signal_export_errors_total marks them stale.
//
// Within a successful pass every Vec family is Reset before it is re-Set, so a
// window that lost a gate (or an actor) drops its series rather than freezing
// the last value it ever held. The scalar gauges are always written: an empty
// window sets the counts to 0 and the merge rate to NaN, because "no runs" is
// not "no merges out of some".
func (e *LearningSignalExporter) PublishLearningSignals(ctx context.Context, since, now time.Time) (mills.LearningSignalSweepResult, error) {
	res := mills.LearningSignalSweepResult{}
	if e == nil || e.Events == nil || e.Runs == nil {
		return res, errors.New("learning signals: exporter not configured")
	}
	if e.PromotionActorPrefix == "" {
		return res, errors.New("learning signals: promotion actor prefix required")
	}

	calibration, err := BuildJudgeCalibrationReport(ctx, e.Events, e.Runs, since, now)
	if err != nil {
		mills.LearningSignalExportErrorsTotal.WithLabelValues("judge_calibration").Inc()
		return res, err
	}
	promotion, err := BuildPromotionReport(ctx, e.Events, e.PromotionActorPrefix, since, now)
	if err != nil {
		mills.LearningSignalExportErrorsTotal.WithLabelValues("promotion").Inc()
		return res, err
	}
	outcomes, err := BuildConfigOutcomeReport(ctx, e.Events, e.Runs, since, now)
	if err != nil {
		mills.LearningSignalExportErrorsTotal.WithLabelValues("config_outcomes").Inc()
		return res, err
	}

	mills.JudgeCalibrationMeanScore.Reset()
	mills.JudgeCalibrationDiscrimination.Reset()
	mills.JudgeCalibrationGradedRuns.Reset()
	for _, gate := range calibration.PerGate {
		merged := meanOrNaN(gate.MeanScoreMerged, gate.MergedVerdicts)
		escalated := meanOrNaN(gate.MeanScoreEscalated, gate.EscalatedVerdicts)
		mills.JudgeCalibrationMeanScore.WithLabelValues(gate.Gate, JudgeOutcomeMerged).Set(merged)
		mills.JudgeCalibrationMeanScore.WithLabelValues(gate.Gate, JudgeOutcomeEscalated).Set(escalated)
		// NaN propagates through the subtraction, so a gate that has only ever
		// merged reports no discrimination rather than its merged mean.
		mills.JudgeCalibrationDiscrimination.WithLabelValues(gate.Gate).Set(merged - escalated)
		graded := gate.MergedVerdicts + gate.EscalatedVerdicts
		mills.JudgeCalibrationGradedRuns.WithLabelValues(gate.Gate).Set(float64(graded))
		res.Gates++
		res.JoinedVerdicts += graded
	}

	mills.PromotionEvidenceActions.Reset()
	for _, actor := range promotion.PerActor {
		total := 0
		for _, action := range actor.PerAction {
			total += action.DryRun + action.Executed
		}
		mills.PromotionEvidenceActions.WithLabelValues(actor.Actor).Set(float64(total))
		res.PromotionActions += total
	}

	mills.ConfigOutcomeRuns.Set(float64(outcomes.Totals.Runs))
	mills.ConfigOutcomeMergeRate.Set(meanOrNaN(outcomes.Totals.MergeRate, outcomes.Totals.Runs))
	mills.RegressionsWindowTotal.Set(float64(outcomes.Regressions.Total))
	res.ConfigRuns = outcomes.Totals.Runs
	res.Regressions = outcomes.Regressions.Total
	return res, nil
}

// meanOrNaN renders an average whose denominator may be zero. The builders
// return 0 for an empty average, which on a gauge is indistinguishable from a
// real zero — NaN says "no observations", and Prometheus comparisons against
// NaN are false, so a threshold alert stays quiet on an empty window instead of
// firing on a fabricated zero.
func meanOrNaN(mean float64, count int) float64 {
	if count <= 0 {
		return math.NaN()
	}
	return mean
}
