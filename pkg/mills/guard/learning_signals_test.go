package guard

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// The exporter is the reconciler's learning-signal sweep.
var _ mills.LearningSignalPublisher = (*LearningSignalExporter)(nil)

var signalNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

// resetLearningSignalGauges clears the process-global families so one case
// cannot read another's leftovers. These metrics live in the default registry,
// so the test cases must stay serial (no t.Parallel).
func resetLearningSignalGauges() {
	mills.JudgeCalibrationMeanScore.Reset()
	mills.JudgeCalibrationDiscrimination.Reset()
	mills.JudgeCalibrationGradedRuns.Reset()
	mills.PromotionEvidenceActions.Reset()
	mills.ConfigOutcomeMergeRate.Set(0)
	mills.ConfigOutcomeRuns.Set(0)
	mills.RegressionsWindowTotal.Set(0)
}

// promotionEvent builds one audited action under the overseer prefix.
func promotionEvent(at time.Time, actor, kind, subjectID string) *store.Event {
	return &store.Event{
		OccurredAt: at, Actor: actor, Kind: kind,
		SubjectKind: "backlog_item", SubjectID: subjectID,
	}
}

// discriminatingWindow is a factory whose judges still work: on code_review the
// merged runs score far above the escalated ones. docs_guard is the same window
// seen through a judge that has stopped discriminating — identical means on
// both sides of the outcome split.
func discriminatingWindow() ([]*store.Event, []*store.RunTerminalOutcome) {
	events := []*store.Event{
		verdictEvent(signalNow.Add(-1*time.Hour), "run-m1", "code_review", "sonnet", 0.90, true),
		verdictEvent(signalNow.Add(-2*time.Hour), "run-m2", "code_review", "sonnet", 0.94, true),
		verdictEvent(signalNow.Add(-3*time.Hour), "run-e1", "code_review", "sonnet", 0.40, false),
		verdictEvent(signalNow.Add(-4*time.Hour), "run-e2", "code_review", "sonnet", 0.30, false),
		verdictEvent(signalNow.Add(-1*time.Hour), "run-m1", "docs_guard", "sonnet", 0.70, true),
		verdictEvent(signalNow.Add(-3*time.Hour), "run-e1", "docs_guard", "sonnet", 0.70, true),

		promotionEvent(signalNow.Add(-1*time.Hour), "overseer.groomer", "overseer.groomer.dedup_close.dryrun", "A"),
		promotionEvent(signalNow.Add(-2*time.Hour), "overseer.groomer", "overseer.groomer.dedup_close.dryrun", "B"),
		promotionEvent(signalNow.Add(-3*time.Hour), "overseer.groomer", "overseer.groomer.dedup_close", "A"),
		promotionEvent(signalNow.Add(-2*time.Hour), "overseer.sentinel", "overseer.sentinel.incident_opened", "C"),
		// Outside the prefix: real evidence, but not evidence about a guarded
		// actor's promotion, so it must not become a series.
		promotionEvent(signalNow.Add(-2*time.Hour), "reconciler", "reconciler.tick", ""),

		provenanceEvent(signalNow.Add(-1*time.Hour), "run-m1", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(signalNow.Add(-2*time.Hour), "run-m2", "checksum-a", map[string]string{"implement": "sonnet"}),
		provenanceEvent(signalNow.Add(-3*time.Hour), "run-e1", "checksum-b", map[string]string{"implement": "opus"}),
		provenanceEvent(signalNow.Add(-4*time.Hour), "run-e2", "checksum-b", map[string]string{"implement": "opus"}),
		regressionEvent(signalNow.Add(-30*time.Minute), 42),
	}
	runs := []*store.RunTerminalOutcome{
		terminalRun("run-m1", store.PipelineDone, 1.5, iid(42)),
		terminalRun("run-m2", store.PipelineDone, 1.0, iid(43)),
		terminalRun("run-e1", store.PipelineEscalated, 0.5, nil),
		terminalRun("run-e2", store.PipelineEscalated, 0.5, nil),
	}
	return events, runs
}

func newExporter(events []*store.Event, runs []*store.RunTerminalOutcome) *LearningSignalExporter {
	return &LearningSignalExporter{
		Events:               &fakeEventLister{events: events},
		Runs:                 &fakeRunLister{runs: runs},
		PromotionActorPrefix: "overseer.",
	}
}

func gaugeValue(t *testing.T, name string, value float64, want float64) {
	t.Helper()
	if !nearly(value, want) {
		t.Errorf("%s = %v, want %v", name, value, want)
	}
}

// TestLearningSignalExporterPublishesGauges: the gauges carry the reports'
// numbers, and a judge that has stopped separating merged from escalated work
// shows up as discrimination 0 next to a judge that still does.
func TestLearningSignalExporterPublishesGauges(t *testing.T) {
	resetLearningSignalGauges()
	events, runs := discriminatingWindow()
	res, err := newExporter(events, runs).PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	gaugeValue(t, `mean_score{gate="code_review",outcome="merged"}`,
		testutil.ToFloat64(mills.JudgeCalibrationMeanScore.WithLabelValues("code_review", JudgeOutcomeMerged)), 0.92)
	gaugeValue(t, `mean_score{gate="code_review",outcome="escalated"}`,
		testutil.ToFloat64(mills.JudgeCalibrationMeanScore.WithLabelValues("code_review", JudgeOutcomeEscalated)), 0.35)
	gaugeValue(t, `discrimination{gate="code_review"}`,
		testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review")), 0.57)
	gaugeValue(t, `graded_runs{gate="code_review"}`,
		testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("code_review")), 4)

	// The converged judge: same mean on both sides, so the alert's headline
	// series reads exactly zero rather than "no data".
	gaugeValue(t, `discrimination{gate="docs_guard"}`,
		testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("docs_guard")), 0)
	gaugeValue(t, `graded_runs{gate="docs_guard"}`,
		testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("docs_guard")), 2)

	gaugeValue(t, `promotion_evidence_actions{actor="overseer.groomer"}`,
		testutil.ToFloat64(mills.PromotionEvidenceActions.WithLabelValues("overseer.groomer")), 3)
	gaugeValue(t, `promotion_evidence_actions{actor="overseer.sentinel"}`,
		testutil.ToFloat64(mills.PromotionEvidenceActions.WithLabelValues("overseer.sentinel")), 1)
	// Two actors only: the reconciler's own tick is not promotion evidence.
	if got := testutil.CollectAndCount(mills.PromotionEvidenceActions); got != 2 {
		t.Errorf("promotion actor series = %d, want 2", got)
	}

	gaugeValue(t, "config_outcome_runs", testutil.ToFloat64(mills.ConfigOutcomeRuns), 4)
	gaugeValue(t, "config_outcome_merge_rate", testutil.ToFloat64(mills.ConfigOutcomeMergeRate), 0.5)
	gaugeValue(t, "regressions_window_total", testutil.ToFloat64(mills.RegressionsWindowTotal), 1)

	want := mills.LearningSignalSweepResult{
		Gates: 2, JoinedVerdicts: 6, PromotionActions: 4, ConfigRuns: 4, Regressions: 1,
	}
	if res != want {
		t.Errorf("sweep result = %+v, want %+v", res, want)
	}
}

// TestLearningSignalExporterOneSidedGateReportsNaN: a gate whose window holds
// only merged verdicts has no discrimination to report. Exporting the merged
// mean as the difference would read as a perfectly calibrated judge.
func TestLearningSignalExporterOneSidedGateReportsNaN(t *testing.T) {
	resetLearningSignalGauges()
	events := []*store.Event{
		verdictEvent(signalNow.Add(-time.Hour), "run-m1", "code_review", "sonnet", 0.90, true),
	}
	runs := []*store.RunTerminalOutcome{terminalRun("run-m1", store.PipelineDone, 1.0, nil)}
	if _, err := newExporter(events, runs).PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow); err != nil {
		t.Fatalf("publish: %v", err)
	}

	gaugeValue(t, `mean_score{outcome="merged"}`,
		testutil.ToFloat64(mills.JudgeCalibrationMeanScore.WithLabelValues("code_review", JudgeOutcomeMerged)), 0.90)
	escalated := testutil.ToFloat64(mills.JudgeCalibrationMeanScore.WithLabelValues("code_review", JudgeOutcomeEscalated))
	if !math.IsNaN(escalated) {
		t.Errorf(`mean_score{outcome="escalated"} = %v, want NaN (no escalated verdicts)`, escalated)
	}
	discrimination := testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review"))
	if !math.IsNaN(discrimination) {
		t.Errorf("discrimination = %v, want NaN (one-sided window)", discrimination)
	}
	gaugeValue(t, "graded_runs", testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("code_review")), 1)
}

// TestLearningSignalExporterEmptyWindowClearsGauges: a window that recorded
// nothing must not leave the previous window's values standing. A stale
// discrimination series would keep a drift alert quiet through an outage that
// stopped the factory entirely.
func TestLearningSignalExporterEmptyWindowClearsGauges(t *testing.T) {
	resetLearningSignalGauges()
	events, runs := discriminatingWindow()
	exporter := newExporter(events, runs)
	if _, err := exporter.PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow); err != nil {
		t.Fatalf("seed publish: %v", err)
	}

	empty := newExporter(nil, nil)
	res, err := empty.PublishLearningSignals(context.Background(), signalNow.Add(-24*time.Hour), signalNow)
	if err != nil {
		t.Fatalf("empty publish: %v", err)
	}
	if res != (mills.LearningSignalSweepResult{}) {
		t.Errorf("empty-window result = %+v, want zero value", res)
	}

	if got := testutil.CollectAndCount(mills.JudgeCalibrationMeanScore); got != 0 {
		t.Errorf("mean_score series after empty window = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(mills.JudgeCalibrationDiscrimination); got != 0 {
		t.Errorf("discrimination series after empty window = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(mills.JudgeCalibrationGradedRuns); got != 0 {
		t.Errorf("graded_runs series after empty window = %d, want 0", got)
	}
	if got := testutil.CollectAndCount(mills.PromotionEvidenceActions); got != 0 {
		t.Errorf("promotion series after empty window = %d, want 0", got)
	}

	// Scalars have no series to drop, so they are written explicitly: zero
	// runs, zero regressions, and a merge rate of NaN because no runs is not
	// "no merges out of some".
	gaugeValue(t, "config_outcome_runs", testutil.ToFloat64(mills.ConfigOutcomeRuns), 0)
	gaugeValue(t, "regressions_window_total", testutil.ToFloat64(mills.RegressionsWindowTotal), 0)
	if rate := testutil.ToFloat64(mills.ConfigOutcomeMergeRate); !math.IsNaN(rate) {
		t.Errorf("config_outcome_merge_rate = %v, want NaN on an empty window", rate)
	}
}

// TestLearningSignalExporterFailureLeavesGaugesIntact: a failed pass publishes
// nothing at all. Half a window — this sweep's means beside the last sweep's
// sample sizes — is a worse input to an alert than a frozen one, and the error
// counter is what says the values are frozen.
func TestLearningSignalExporterFailureLeavesGaugesIntact(t *testing.T) {
	resetLearningSignalGauges()
	events, runs := discriminatingWindow()
	if _, err := newExporter(events, runs).PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow); err != nil {
		t.Fatalf("seed publish: %v", err)
	}

	boom := errors.New("events table unavailable")
	broken := &LearningSignalExporter{
		Events:               &fakeEventLister{err: boom},
		Runs:                 &fakeRunLister{runs: runs},
		PromotionActorPrefix: "overseer.",
	}
	before := testutil.ToFloat64(mills.LearningSignalExportErrorsTotal.WithLabelValues("judge_calibration"))
	if _, err := broken.PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow); !errors.Is(err, boom) {
		t.Fatalf("publish error = %v, want %v", err, boom)
	}
	if after := testutil.ToFloat64(mills.LearningSignalExportErrorsTotal.WithLabelValues("judge_calibration")); after != before+1 {
		t.Errorf("judge_calibration export errors = %v, want %v", after, before+1)
	}

	gaugeValue(t, `discrimination{gate="code_review"}`,
		testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review")), 0.57)
	gaugeValue(t, "config_outcome_runs", testutil.ToFloat64(mills.ConfigOutcomeRuns), 4)
	gaugeValue(t, "regressions_window_total", testutil.ToFloat64(mills.RegressionsWindowTotal), 1)
}

// TestLearningSignalExporterRefusesUnconfigured: an empty actor prefix would
// widen the promotion report to every writer in the events table, and every
// writer would become a metric series.
func TestLearningSignalExporterRefusesUnconfigured(t *testing.T) {
	resetLearningSignalGauges()
	cases := map[string]*LearningSignalExporter{
		"no events lister": {Runs: &fakeRunLister{}, PromotionActorPrefix: "overseer."},
		"no runs lister":   {Events: &fakeEventLister{}, PromotionActorPrefix: "overseer."},
		"no actor prefix":  {Events: &fakeEventLister{}, Runs: &fakeRunLister{}},
		"nil exporter":     nil,
	}
	for name, exporter := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := exporter.PublishLearningSignals(
				context.Background(), signalNow.Add(-24*time.Hour), signalNow); err == nil {
				t.Fatal("publish succeeded, want configuration error")
			}
		})
	}
}
