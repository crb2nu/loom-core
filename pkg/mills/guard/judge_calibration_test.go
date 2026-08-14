package guard

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

type fakeEventLister struct {
	events []*store.Event
	err    error
}

func (f *fakeEventLister) ListSince(_ context.Context, since time.Time, limit int) ([]*store.Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]*store.Event, 0, len(f.events))
	for _, e := range f.events {
		if e.OccurredAt.Before(since) {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, e)
	}
	return out, nil
}

type fakeRunLister struct {
	runs []*store.RunTerminalOutcome
	err  error
}

func (f *fakeRunLister) ListTerminalOutcomesSince(_ context.Context, _ time.Time, limit int) ([]*store.RunTerminalOutcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.runs) > limit {
		return f.runs[:limit], nil
	}
	return f.runs, nil
}

func verdictEvent(at time.Time, runID, gate, model string, score float64, pass bool) *store.Event {
	return &store.Event{
		OccurredAt: at, Actor: "pipeline", Kind: store.JudgeVerdictEventKind,
		SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: runID,
		Payload: map[string]any{
			"run_id": runID, "backlog_id": "BL-" + runID, "gate": gate,
			"judge_model": model, "role": "primary",
			"score": score, "threshold": 0.8, "pass": pass, "attempt": 1,
		},
	}
}

// nearly compares means, which are float sums and cannot be asserted exactly.
func nearly(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func gateRow(t *testing.T, rep JudgeCalibrationReport, gate string) JudgeGate {
	t.Helper()
	for _, g := range rep.PerGate {
		if g.Gate == gate {
			return g
		}
	}
	t.Fatalf("gate %q missing from %+v", gate, rep.PerGate)
	return JudgeGate{}
}

func bucketRow(t *testing.T, row JudgeGate, bucket string) JudgeScoreBucket {
	t.Helper()
	for _, b := range row.Histogram {
		if b.Bucket == bucket {
			return b
		}
	}
	t.Fatalf("bucket %q missing from %+v", bucket, row.Histogram)
	return JudgeScoreBucket{}
}

// The whole point of the report: scores the judge gave to work that shipped
// versus work that escalated, never averaged together.
func TestBuildJudgeCalibrationReport_SplitsMergedFromEscalated(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		verdictEvent(now.Add(-time.Hour), "run-merged-1", "spec_conformance", "gemma", 0.90, true),
		verdictEvent(now.Add(-2*time.Hour), "run-merged-2", "spec_conformance", "gemma", 0.86, true),
		verdictEvent(now.Add(-3*time.Hour), "run-esc-1", "spec_conformance", "gemma", 0.82, true),
		verdictEvent(now.Add(-4*time.Hour), "run-esc-2", "spec_conformance", "gemma", 0.30, false),
		verdictEvent(now.Add(-5*time.Hour), "run-flight-1", "spec_conformance", "gemma", 0.60, false),
		verdictEvent(now.Add(-6*time.Hour), "run-merged-1", "pr_self_review", "gemma", 0.95, true),
		// Not a judge verdict: the window is full of other pipeline events.
		{OccurredAt: now.Add(-time.Hour), Actor: "pipeline", Kind: "pipeline.gate.eval", Payload: map[string]any{"pass": true}},
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		{RunID: "run-merged-1", BacklogID: "BL-1", State: store.PipelineDone},
		{RunID: "run-merged-2", BacklogID: "BL-2", State: store.PipelineDone},
		{RunID: "run-esc-1", BacklogID: "BL-3", State: store.PipelineEscalated},
		{RunID: "run-esc-2", BacklogID: "BL-4", State: store.PipelineEscalated},
		// run-flight-1 is still in flight and therefore absent.
	}}

	rep, err := BuildJudgeCalibrationReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.TotalVerdicts != 6 || rep.JoinedVerdicts != 5 {
		t.Fatalf("totals = %d verdicts / %d joined, want 6/5", rep.TotalVerdicts, rep.JoinedVerdicts)
	}
	if rep.ZeroEvidence {
		t.Error("zero_evidence set on a window holding six verdicts")
	}

	spec := gateRow(t, rep, "spec_conformance")
	if spec.Verdicts != 5 || spec.Passed != 3 {
		t.Fatalf("spec_conformance = %+v, want 5 verdicts / 3 passed", spec)
	}
	if spec.PassRate != 0.6 {
		t.Errorf("pass_rate = %v, want 0.6", spec.PassRate)
	}
	if spec.MergedVerdicts != 2 || spec.EscalatedVerdicts != 2 || spec.OtherVerdicts != 1 {
		t.Fatalf("outcome split = %+v", spec)
	}
	if got := spec.MeanScoreMerged; !nearly(got, 0.88) {
		t.Errorf("mean_score_merged = %v, want 0.88", got)
	}
	if got := spec.MeanScoreEscalated; !nearly(got, 0.56) {
		t.Errorf("mean_score_escalated = %v, want 0.56", got)
	}
	// The in-flight run must not be guessed into either column.
	if spec.MergedVerdicts+spec.EscalatedVerdicts+spec.OtherVerdicts != spec.Verdicts {
		t.Errorf("outcome split does not partition verdicts: %+v", spec)
	}
}

func TestBuildJudgeCalibrationReport_HistogramBucketsByOutcome(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		verdictEvent(now.Add(-time.Hour), "run-a", "spec_conformance", "gemma", 0.20, false),
		verdictEvent(now.Add(-time.Hour), "run-b", "spec_conformance", "gemma", 0.50, false),
		verdictEvent(now.Add(-time.Hour), "run-c", "spec_conformance", "gemma", 0.79, false),
		verdictEvent(now.Add(-time.Hour), "run-d", "spec_conformance", "gemma", 0.80, true),
		verdictEvent(now.Add(-time.Hour), "run-e", "spec_conformance", "gemma", 0.99, true),
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		{RunID: "run-a", State: store.PipelineEscalated},
		{RunID: "run-b", State: store.PipelineDone},
		{RunID: "run-c", State: store.PipelineEscalated},
		{RunID: "run-d", State: store.PipelineDone},
		// run-e paused: terminal, but neither merged nor escalated.
		{RunID: "run-e", State: store.PipelinePaused},
	}}

	rep, err := BuildJudgeCalibrationReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := gateRow(t, rep, "spec_conformance")
	if len(row.Histogram) != 3 {
		t.Fatalf("histogram = %+v, want all three buckets even when empty", row.Histogram)
	}
	if got := bucketRow(t, row, JudgeBucketLow); got.Escalated != 1 || got.Merged != 0 || got.Other != 0 {
		t.Errorf("%s = %+v", JudgeBucketLow, got)
	}
	// 0.5 and 0.79 both land in the middle bucket; 0.80 does not.
	if got := bucketRow(t, row, JudgeBucketMid); got.Merged != 1 || got.Escalated != 1 {
		t.Errorf("%s = %+v", JudgeBucketMid, got)
	}
	if got := bucketRow(t, row, JudgeBucketHigh); got.Merged != 1 || got.Other != 1 || got.Escalated != 0 {
		t.Errorf("%s = %+v", JudgeBucketHigh, got)
	}
	// A paused run is ground truth about nothing.
	if row.MergedVerdicts != 2 || row.EscalatedVerdicts != 2 || row.OtherVerdicts != 1 {
		t.Errorf("outcome split = %+v", row)
	}
}

func TestBuildJudgeCalibrationReport_ZeroEvidence(t *testing.T) {
	now := time.Now().UTC()
	rep, err := BuildJudgeCalibrationReport(context.Background(),
		&fakeEventLister{}, &fakeRunLister{}, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !rep.ZeroEvidence || rep.TotalVerdicts != 0 || len(rep.PerGate) != 0 {
		t.Fatalf("empty window = %+v, want a stated zero-evidence finding", rep)
	}
	if len(rep.Buckets) != 3 || len(rep.Outcomes) != 3 {
		t.Errorf("bucket/outcome labels must be present even on an empty report: %+v", rep)
	}
}

// A verdict that cannot be attributed to a run or a gate is dropped rather
// than bucketed under an empty key, where it would silently pad a pass rate.
func TestBuildJudgeCalibrationReport_DropsUnattributableVerdicts(t *testing.T) {
	now := time.Now().UTC()
	noGate := verdictEvent(now.Add(-time.Hour), "run-a", "", "gemma", 0.9, true)
	noGate.Payload["gate"] = ""
	noScore := verdictEvent(now.Add(-time.Hour), "run-b", "spec_conformance", "gemma", 0.9, true)
	delete(noScore.Payload, "score")

	rep, err := BuildJudgeCalibrationReport(context.Background(),
		&fakeEventLister{events: []*store.Event{noGate, noScore}},
		&fakeRunLister{}, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rep.TotalVerdicts != 0 || !rep.ZeroEvidence {
		t.Fatalf("report = %+v, want both malformed verdicts dropped", rep)
	}
}

func TestBuildJudgeCalibrationReport_RejectsBadInput(t *testing.T) {
	now := time.Now().UTC()
	if _, err := BuildJudgeCalibrationReport(context.Background(), nil, &fakeRunLister{}, now.Add(-time.Hour), now); err == nil {
		t.Error("nil events lister must be rejected")
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(), &fakeEventLister{}, nil, now.Add(-time.Hour), now); err == nil {
		t.Error("nil runs lister must be rejected")
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(), &fakeEventLister{}, &fakeRunLister{}, now, now); err == nil {
		t.Error("empty window must be rejected")
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(),
		&fakeEventLister{err: errors.New("boom")}, &fakeRunLister{}, now.Add(-time.Hour), now); err == nil {
		t.Error("events read error must surface")
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(),
		&fakeEventLister{}, &fakeRunLister{err: errors.New("boom")}, now.Add(-time.Hour), now); err == nil {
		t.Error("runs read error must surface")
	}
}

// A truncated scan reports a pass rate against a partial reality, so both
// saturations are errors rather than quiet under-counts.
func TestBuildJudgeCalibrationReport_SaturationIsAnError(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	runs := make([]*store.RunTerminalOutcome, judgeCalibrationRunLimit)
	for i := range runs {
		runs[i] = &store.RunTerminalOutcome{RunID: "run", State: store.PipelineDone}
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(), &fakeEventLister{}, &fakeRunLister{runs: runs}, since, now); err == nil {
		t.Error("saturated run scan must error rather than truncate")
	}

	events := make([]*store.Event, judgeCalibrationEventLimit)
	for i := range events {
		events[i] = verdictEvent(now.Add(-time.Hour), "run", "spec_conformance", "gemma", 0.9, true)
	}
	if _, err := BuildJudgeCalibrationReport(context.Background(), &fakeEventLister{events: events}, &fakeRunLister{}, since, now); err == nil {
		t.Error("saturated event scan must error rather than truncate")
	}
}
