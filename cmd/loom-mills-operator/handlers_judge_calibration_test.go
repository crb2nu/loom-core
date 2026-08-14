package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedJudgeCalibration writes two graded runs — one merged, one escalated —
// through the real DAOs, so the handler exercises the store-backed join and
// not just the aggregation.
func seedJudgeCalibration(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()

	for _, item := range []*store.BacklogItem{
		{ID: "BL-CAL-1", Title: "merged work", State: store.BacklogMerged, Priority: store.P2},
		{ID: "BL-CAL-2", Title: "escalated work", State: store.BacklogEscalated, Priority: store.P2},
	} {
		if err := op.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed backlog: %v", err)
		}
	}
	for _, r := range []*store.PipelineRun{
		{ID: "PIPE-CAL-MERGED", BacklogID: "BL-CAL-1", Template: "default", State: store.PipelineDone, StartedAt: now.Add(-3 * time.Hour)},
		{ID: "PIPE-CAL-ESC", BacklogID: "BL-CAL-2", Template: "default", State: store.PipelineEscalated, StartedAt: now.Add(-4 * time.Hour)},
	} {
		if err := op.store.Pipeline.PutRun(ctx, r); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
	for _, e := range []*store.Event{
		judgeEvent(now.Add(-time.Hour), "PIPE-CAL-MERGED", "spec_conformance", 0.92, true),
		judgeEvent(now.Add(-2*time.Hour), "PIPE-CAL-ESC", "spec_conformance", 0.44, false),
	} {
		if err := op.store.Events.Append(ctx, e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func judgeEvent(at time.Time, runID, gate string, score float64, pass bool) *store.Event {
	return &store.Event{
		OccurredAt: at, Actor: "pipeline", Kind: store.JudgeVerdictEventKind,
		SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: runID,
		Payload: map[string]any{
			"run_id": runID, "backlog_id": "BL-" + runID, "gate": gate,
			"judge_model": "gemma", "role": "primary",
			"score": score, "threshold": 0.8, "pass": pass, "attempt": 1,
		},
	}
}

func getJudgeCalibration(t *testing.T, op *operator, query string) (*httptest.ResponseRecorder, guard.JudgeCalibrationReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/judge-calibration"+query, nil))
	var report guard.JudgeCalibrationReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, report
}

func TestHandleJudgeCalibration_Defaults(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedJudgeCalibration(t, op)

	rec, report := getJudgeCalibration(t, op, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := report.WindowEnd.Sub(report.WindowStart); got != judgeCalibrationDefaultWindow {
		t.Errorf("window = %s, want %s", got, judgeCalibrationDefaultWindow)
	}
	if report.TotalVerdicts != 2 || report.JoinedVerdicts != 2 {
		t.Fatalf("totals = %d verdicts / %d joined, want 2/2", report.TotalVerdicts, report.JoinedVerdicts)
	}
	if report.ZeroEvidence {
		t.Error("zero_evidence set on a window holding two verdicts")
	}
	if len(report.PerGate) != 1 || report.PerGate[0].Gate != "spec_conformance" {
		t.Fatalf("per_gate = %+v", report.PerGate)
	}
	row := report.PerGate[0]
	if row.MergedVerdicts != 1 || row.EscalatedVerdicts != 1 {
		t.Fatalf("outcome split = %+v", row)
	}
	if row.MeanScoreMerged <= row.MeanScoreEscalated {
		t.Errorf("merged mean %v should exceed escalated mean %v in this fixture",
			row.MeanScoreMerged, row.MeanScoreEscalated)
	}
}

func TestHandleJudgeCalibration_WindowParam(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedJudgeCalibration(t, op)

	rec, report := getJudgeCalibration(t, op, "?window=90m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := report.WindowEnd.Sub(report.WindowStart); got != 90*time.Minute {
		t.Fatalf("window = %s, want 90m", got)
	}
	// Only the newest verdict falls inside; its run started outside, so the
	// join has no ground truth and must say "other" rather than guess.
	if report.TotalVerdicts != 1 || report.JoinedVerdicts != 0 {
		t.Fatalf("totals = %d verdicts / %d joined, want 1/0", report.TotalVerdicts, report.JoinedVerdicts)
	}
	if report.PerGate[0].OtherVerdicts != 1 {
		t.Fatalf("per_gate = %+v, want the unjoined verdict counted as other", report.PerGate[0])
	}

	rec, report = getJudgeCalibration(t, op, "?window=1s")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !report.ZeroEvidence || report.TotalVerdicts != 0 {
		t.Fatalf("narrow window = %+v, want zero evidence", report)
	}
}

func TestHandleJudgeCalibration_RejectsBadWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, q := range []string{"?window=fortnight", "?window=0s", "?window=-4h"} {
		rec, _ := getJudgeCalibration(t, op, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body=%s)", q, rec.Code, rec.Body.String())
		}
	}
}
