package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/guard"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedConfigOutcomes writes two stamped runs under one policy revision — one
// merged and later reverted, one escalated — through the real DAOs, so the
// handler exercises the store-backed join and not just the aggregation.
func seedConfigOutcomes(t *testing.T, op *operator) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	mrIID := int64(4242)

	for _, item := range []*store.BacklogItem{
		{ID: "BL-CFG-1", Title: "merged work", State: store.BacklogMerged, Priority: store.P2},
		{ID: "BL-CFG-2", Title: "escalated work", State: store.BacklogEscalated, Priority: store.P2},
	} {
		if err := op.store.Backlog.Put(ctx, item); err != nil {
			t.Fatalf("seed backlog: %v", err)
		}
	}
	for _, r := range []*store.PipelineRun{
		{
			ID: "PIPE-CFG-MERGED", BacklogID: "BL-CFG-1", Template: "default",
			State: store.PipelineDone, StartedAt: now.Add(-3 * time.Hour), CostUSD: 3.50, MRIID: &mrIID,
		},
		{
			ID: "PIPE-CFG-ESC", BacklogID: "BL-CFG-2", Template: "default",
			State: store.PipelineEscalated, StartedAt: now.Add(-4 * time.Hour), CostUSD: 1.50,
		},
	} {
		if err := op.store.Pipeline.PutRun(ctx, r); err != nil {
			t.Fatalf("seed run: %v", err)
		}
	}
	for _, e := range []*store.Event{
		provenanceStamp(now.Add(-3*time.Hour), "PIPE-CFG-MERGED"),
		provenanceStamp(now.Add(-4*time.Hour), "PIPE-CFG-ESC"),
		judgeEvent(now.Add(-time.Hour), "PIPE-CFG-MERGED", "spec_conformance", 0.92, true),
		judgeEvent(now.Add(-2*time.Hour), "PIPE-CFG-ESC", "spec_conformance", 0.44, false),
		{
			OccurredAt: now.Add(-30 * time.Minute), Actor: mills.RegressionAttributionActor,
			Kind: mills.RegressionAttributedEventKind, SubjectKind: "merge_request", SubjectID: "4242",
			Payload: map[string]any{
				"regressed_mr_iid": mrIID, "merged_sha": "abcdef123456",
				"revert_sha": "fedcba654321", "revert_title": `Revert "feat: thing"`,
			},
		},
	} {
		if err := op.store.Events.Append(ctx, e); err != nil {
			t.Fatalf("seed event: %v", err)
		}
	}
}

func provenanceStamp(at time.Time, runID string) *store.Event {
	return &store.Event{
		OccurredAt: at, Actor: mills.RunProvenanceActor, Kind: mills.RunProvenanceEventKind,
		SubjectKind: "pipeline_run", SubjectID: runID,
		Payload: map[string]any{
			"run_id": runID, "backlog_id": "BL-" + runID, "lane": "pipeline",
			"policy_checksum": "checksum-a",
			"stage_models":    map[string]any{"implement": "sonnet"},
			"prompt_hashes":   map[string]any{},
			"outcome":         "ok",
		},
	}
}

func getConfigOutcomes(t *testing.T, op *operator, query string) (*httptest.ResponseRecorder, guard.ConfigOutcomeReport) {
	t.Helper()
	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/config-outcomes"+query, nil))
	var report guard.ConfigOutcomeReport
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode: %v body=%s", err, rec.Body.String())
		}
	}
	return rec, report
}

func TestHandleConfigOutcomes_Defaults(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedConfigOutcomes(t, op)

	rec, report := getConfigOutcomes(t, op, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := report.WindowEnd.Sub(report.WindowStart); got != configOutcomesDefaultWindow {
		t.Errorf("window = %s, want %s", got, configOutcomesDefaultWindow)
	}
	if report.StampedRuns != 2 || report.UncoveredRuns != 0 {
		t.Fatalf("coverage = %d stamped / %d uncovered, want 2/0", report.StampedRuns, report.UncoveredRuns)
	}
	if report.ZeroEvidence {
		t.Error("zero_evidence set on a window holding two stamps")
	}
	if len(report.PerPolicyChecksum) != 1 || report.PerPolicyChecksum[0].PolicyChecksum != "checksum-a" {
		t.Fatalf("per_policy_checksum = %+v", report.PerPolicyChecksum)
	}
	row := report.PerPolicyChecksum[0]
	if row.Runs != 2 || row.Merged != 1 || row.Escalated != 1 || row.MergeRate != 0.5 {
		t.Fatalf("checksum-a = %+v", row)
	}
	if row.TotalCostUSD != 5.00 || row.MeanCostUSD != 2.50 {
		t.Errorf("cost = %+v, want $5.00 total / $2.50 mean", row)
	}
	if row.Regressions != 1 || report.Regressions.Linked != 1 || report.Regressions.Unlinked != 0 {
		t.Errorf("regressions = %d group / %+v window", row.Regressions, report.Regressions)
	}
	if len(report.PerStageModel) != 1 || report.PerStageModel[0].Model != "sonnet" {
		t.Fatalf("per_stage_model = %+v", report.PerStageModel)
	}
}

func TestHandleConfigOutcomes_WindowParam(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	seedConfigOutcomes(t, op)

	rec, report := getConfigOutcomes(t, op, "?window=210m")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := report.WindowEnd.Sub(report.WindowStart); got != 210*time.Minute {
		t.Fatalf("window = %s, want 210m", got)
	}
	// Only the merged run's stamp falls inside; the escalated run started
	// outside and is neither stamped nor counted as uncovered here.
	if report.StampedRuns != 1 || report.Totals.Merged != 1 || report.Totals.Escalated != 0 {
		t.Fatalf("narrowed window = %+v", report.Totals)
	}

	rec, report = getConfigOutcomes(t, op, "?window=1s")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !report.ZeroEvidence || report.StampedRuns != 0 {
		t.Fatalf("narrow window = %+v, want zero evidence", report)
	}
}

func TestHandleConfigOutcomes_RejectsBadWindow(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	for _, q := range []string{"?window=fortnight", "?window=0s", "?window=-4h"} {
		rec, _ := getConfigOutcomes(t, op, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body=%s)", q, rec.Code, rec.Body.String())
		}
	}
}
