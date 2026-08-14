package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills"
	"github.com/crb2nu/loom/pkg/mills/store"
)

// seedRunWithEvidence creates a run plus the three ground-truth event kinds
// the evidence block assembles, returning the run ID and MR iid.
func seedRunWithEvidence(t *testing.T, o *operator) (string, int64) {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{ID: "BL-EV", Title: "evidence item", State: store.BacklogQueued}
	if err := o.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{ID: "run-ev-1", BacklogID: "BL-EV", Template: "t", State: "running"}
	if err := o.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	mriid := int64(777)
	for _, ev := range []*store.Event{
		{Actor: "pipeline", Kind: store.JudgeVerdictEventKind, SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: run.ID,
			Payload: map[string]any{"gate": "pr_self_review", "role": "primary", "judge_model": "m1", "score": 0.91, "threshold": 0.7, "pass": true, "attempt": 1}},
		{Actor: "pipeline", Kind: store.JudgeVerdictEventKind, SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: run.ID,
			Payload: map[string]any{"gate": "spec_conformance", "role": "primary", "judge_model": "m1", "score": 0.55, "threshold": 0.7, "pass": false, "attempt": 1}},
		{Actor: mills.RunProvenanceActor, Kind: mills.RunProvenanceEventKind, SubjectKind: store.JudgeVerdictSubjectKind, SubjectID: run.ID,
			Payload: map[string]any{"lane": "pipeline", "policy_checksum": "abc123def456", "stage_models": map[string]any{"implement": "sol"}}},
		{Actor: mills.RegressionAttributionActor, Kind: mills.RegressionAttributedEventKind, SubjectKind: "merge_request", SubjectID: "777",
			Payload: map[string]any{"regressed_mr_iid": mriid, "merged_sha": "aaa", "revert_sha": "bbb", "revert_title": "Revert \"evidence item\""}},
	} {
		if err := o.store.Events.Append(ctx, ev); err != nil {
			t.Fatalf("seed event %s: %v", ev.Kind, err)
		}
	}
	return run.ID, mriid
}

func TestPipelineRunGet_EvidenceBlock(t *testing.T) {
	o, done := newTestOperator(t)
	defer done()
	runID, mriid := seedRunWithEvidence(t, o)

	// Link the run to its MR so the regression join has a key.
	run, err := o.store.Pipeline.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	run.MRIID = &mriid
	if err := o.store.Pipeline.PutRun(context.Background(), run); err != nil {
		t.Fatalf("update run mriid: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/"+runID, nil)
	req.SetPathValue("id", runID)
	rec := httptest.NewRecorder()
	o.handlePipelineRunGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Evidence struct {
			Verdicts   []map[string]any `json:"verdicts"`
			Provenance map[string]any   `json:"provenance"`
			Regression map[string]any   `json:"regression"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Evidence.Verdicts) != 2 {
		t.Fatalf("verdicts = %d, want 2", len(resp.Evidence.Verdicts))
	}
	// Oldest-first chronology: the passing pr_self_review verdict was appended first.
	if got := resp.Evidence.Verdicts[0]["gate"]; got != "pr_self_review" {
		t.Errorf("first verdict gate = %v, want pr_self_review (oldest-first)", got)
	}
	if resp.Evidence.Provenance == nil || resp.Evidence.Provenance["policy_checksum"] != "abc123def456" {
		t.Errorf("provenance = %v, want policy_checksum abc123def456", resp.Evidence.Provenance)
	}
	if resp.Evidence.Regression == nil || resp.Evidence.Regression["revert_sha"] != "bbb" {
		t.Errorf("regression = %v, want revert_sha bbb", resp.Evidence.Regression)
	}
	for _, v := range resp.Evidence.Verdicts {
		if _, ok := v["occurred_at"]; !ok {
			t.Errorf("verdict missing occurred_at: %v", v)
		}
	}
}

func TestPipelineRunGet_EvidenceEmptyForBareRun(t *testing.T) {
	o, done := newTestOperator(t)
	defer done()
	ctx := context.Background()
	if err := o.store.Backlog.Put(ctx, &store.BacklogItem{ID: "BL-B", Title: "bare", State: store.BacklogQueued}); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	if err := o.store.Pipeline.PutRun(ctx, &store.PipelineRun{ID: "run-bare", BacklogID: "BL-B", Template: "t", State: "running"}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/run-bare", nil)
	req.SetPathValue("id", "run-bare")
	rec := httptest.NewRecorder()
	o.handlePipelineRunGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Evidence struct {
			Verdicts   []map[string]any `json:"verdicts"`
			Provenance any              `json:"provenance"`
			Regression any              `json:"regression"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Empty shape, not null shape: verdicts [] and nil singletons — a live
	// run with no evidence must not crash or omit the block.
	if resp.Evidence.Verdicts == nil || len(resp.Evidence.Verdicts) != 0 {
		t.Errorf("verdicts = %v, want []", resp.Evidence.Verdicts)
	}
	if resp.Evidence.Provenance != nil || resp.Evidence.Regression != nil {
		t.Errorf("provenance/regression = %v/%v, want null/null", resp.Evidence.Provenance, resp.Evidence.Regression)
	}
}

func TestDemandLog_SuppressionsOnlyWindowedNewestFirst(t *testing.T) {
	o, done := newTestOperator(t)
	defer done()
	ctx := context.Background()
	append := func(kind string, when time.Time, title string) {
		t.Helper()
		if err := o.store.Events.Append(ctx, &store.Event{
			OccurredAt: when, Actor: demandLogActor, Kind: kind,
			SubjectKind: "merge_request", SubjectID: "!42",
			Payload: map[string]any{"proposal_title": title, "merged_title": "shipped: " + title, "merged_url": "https://x/!42", "score": 0.83, "basis": "hard"},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	now := time.Now().UTC()
	append(demandLogKindPrefix, now.Add(-2*time.Hour), "older suppression")
	append(demandLogKindPrefix, now.Add(-1*time.Hour), "newer suppression")
	append(demandLogKindPrefix+".dryrun", now.Add(-30*time.Minute), "dryrun suppression")
	append(demandLogActor+".dedup_skip", now.Add(-10*time.Minute), "not a merged-work skip")
	append(demandLogKindPrefix, now.Add(-48*time.Hour), "outside window")

	req := httptest.NewRequest(http.MethodGet, "/api/mills/demand-log", nil)
	rec := httptest.NewRecorder()
	o.handleDemandLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp demandLogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 3 {
		t.Fatalf("count = %d, want 3 (dedup_skip and out-of-window excluded)", resp.Count)
	}
	if resp.Rows[0].ProposalTitle != "dryrun suppression" || !resp.Rows[0].DryRun {
		t.Errorf("row0 = %+v, want newest dryrun suppression flagged dry_run", resp.Rows[0])
	}
	if resp.Rows[2].ProposalTitle != "older suppression" {
		t.Errorf("row2 = %+v, want older suppression last", resp.Rows[2])
	}
	if resp.Rows[1].Score != 0.83 || resp.Rows[1].Basis != "hard" || resp.Rows[1].MergedRef != "!42" {
		t.Errorf("row1 payload projection = %+v", resp.Rows[1])
	}
}

func TestDemandLog_BadWindowAndMissingStore(t *testing.T) {
	o, done := newTestOperator(t)
	defer done()
	req := httptest.NewRequest(http.MethodGet, "/api/mills/demand-log?window=yesterday", nil)
	rec := httptest.NewRecorder()
	o.handleDemandLog(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad window status = %d, want 400", rec.Code)
	}
}
