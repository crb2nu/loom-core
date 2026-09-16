package guard

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/gates"
	"github.com/crb2nu/loom/pkg/mills/store"
)

func TestJudgeRoleShadowMirrorsGates(t *testing.T) {
	if judgeRoleShadow != gates.JudgeRoleShadow {
		t.Fatalf("guard.judgeRoleShadow = %q, gates.JudgeRoleShadow = %q", judgeRoleShadow, gates.JudgeRoleShadow)
	}
}

func shadowVerdictEvent(at time.Time, runID, gate, model string, score float64, pass bool) *store.Event {
	e := verdictEvent(at, runID, gate, model, score, pass)
	e.Payload["role"] = gates.JudgeRoleShadow
	return e
}

// A shadow judge gets its own calibration row per gate: its scores never blend
// into the primary's means, and the two rows can be compared directly.
func TestBuildJudgeCalibrationReport_ShadowRowsSeparate(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	events := &fakeEventLister{events: []*store.Event{
		verdictEvent(now.Add(-1*time.Hour), "run-m", "spec_conformance", "luna", 0.90, true),
		shadowVerdictEvent(now.Add(-1*time.Hour), "run-m", "spec_conformance", "qwen38", 0.70, false),
		verdictEvent(now.Add(-2*time.Hour), "run-e", "spec_conformance", "luna", 0.30, false),
		shadowVerdictEvent(now.Add(-2*time.Hour), "run-e", "spec_conformance", "qwen38", 0.60, false),
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-m", store.PipelineDone, 1, nil),
		terminalRun("run-e", store.PipelineEscalated, 1, nil),
	}}
	rep, err := BuildJudgeCalibrationReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(rep.PerGate) != 2 {
		t.Fatalf("per_gate = %+v, want one primary row and one shadow row", rep.PerGate)
	}
	var primary, shadow JudgeGate
	for _, g := range rep.PerGate {
		switch g.Role {
		case "primary":
			primary = g
		case gates.JudgeRoleShadow:
			shadow = g
		}
	}
	if primary.Gate != "spec_conformance" || primary.Verdicts != 2 || !nearly(primary.MeanScoreMerged, 0.90) || !nearly(primary.MeanScoreEscalated, 0.30) {
		t.Errorf("primary row = %+v", primary)
	}
	if shadow.Gate != "spec_conformance" || shadow.Verdicts != 2 || !nearly(shadow.MeanScoreMerged, 0.70) || !nearly(shadow.MeanScoreEscalated, 0.60) {
		t.Errorf("shadow row = %+v", shadow)
	}
	if rep.TotalVerdicts != 4 || rep.JoinedVerdicts != 4 {
		t.Errorf("totals = %d/%d, want 4/4", rep.TotalVerdicts, rep.JoinedVerdicts)
	}
	models := map[string]int{}
	for _, m := range rep.Models {
		models[m.Model+"/"+m.Role] = m.Verdicts
	}
	if models["luna/primary"] != 2 || models["qwen38/shadow"] != 2 {
		t.Errorf("models = %+v", rep.Models)
	}
}

// The configuration report's mean_judge_score is the gate's real grading:
// shadow verdicts are excluded from the per-run rollup.
func TestBuildConfigOutcomeReport_ExcludesShadowVerdicts(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)
	models := map[string]string{"judge": "luna"}
	events := &fakeEventLister{events: []*store.Event{
		provenanceEvent(now.Add(-time.Hour), "run-merged", "checksum-a", models),
		verdictEvent(now.Add(-50*time.Minute), "run-merged", "spec_conformance", "luna", 0.90, true),
		shadowVerdictEvent(now.Add(-50*time.Minute), "run-merged", "spec_conformance", "qwen38", 0.10, false),
	}}
	runs := &fakeRunLister{runs: []*store.RunTerminalOutcome{
		terminalRun("run-merged", store.PipelineDone, 1.00, iid(101)),
	}}
	rep, err := BuildConfigOutcomeReport(context.Background(), events, runs, since, now)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	row := policyRow(t, rep, "checksum-a")
	if row.JudgeGradedRuns != 1 || !nearly(row.MeanJudgeScore, 0.90) || !nearly(row.JudgePassRate, 1) {
		t.Errorf("judge rollup = %+v, want the primary's 0.90 alone", row)
	}
}

// The gauges carry a role label so a shadow lane's calibration is a separate
// series the primary's alert never reads.
func TestLearningSignalExporterShadowRoleIsSeparateSeries(t *testing.T) {
	resetLearningSignalGauges()
	events, runs := discriminatingWindow()
	events = append(events,
		shadowVerdictEvent(signalNow.Add(-1*time.Hour), "run-m1", "code_review", "qwen38", 0.80, true),
		shadowVerdictEvent(signalNow.Add(-2*time.Hour), "run-m2", "code_review", "qwen38", 0.80, true),
		shadowVerdictEvent(signalNow.Add(-3*time.Hour), "run-e1", "code_review", "qwen38", 0.70, false),
		shadowVerdictEvent(signalNow.Add(-4*time.Hour), "run-e2", "code_review", "qwen38", 0.70, false),
	)
	res, err := newExporter(events, runs).PublishLearningSignals(
		context.Background(), signalNow.Add(-24*time.Hour), signalNow)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	gaugeValue(t, `discrimination{gate="code_review",role="primary"}`,
		testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review", "primary")), 0.57)
	gaugeValue(t, `discrimination{gate="code_review",role="shadow"}`,
		testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review", "shadow")), 0.10)
	gaugeValue(t, `graded_runs{gate="code_review",role="shadow"}`,
		testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("code_review", "shadow")), 4)
	gaugeValue(t, `graded_runs{gate="code_review",role="primary"}`,
		testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("code_review", "primary")), 4)
	// docs_guard never had a shadow: no phantom series.
	if v := testutil.ToFloat64(mills.JudgeCalibrationGradedRuns.WithLabelValues("docs_guard", "shadow")); v != 0 {
		t.Errorf("docs_guard shadow graded_runs = %v, want an absent (zero) series", v)
	}
	if math.IsNaN(testutil.ToFloat64(mills.JudgeCalibrationDiscrimination.WithLabelValues("code_review", "primary"))) {
		t.Error("primary discrimination became NaN once a shadow appeared")
	}
	if res.Gates != 3 || res.JoinedVerdicts != 10 {
		t.Errorf("sweep result = %+v, want 3 (gate,role) rows over 10 joined verdicts", res)
	}
}
