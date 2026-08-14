package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

func seedTransitionRun(t *testing.T, op *operator) string {
	t.Helper()
	ctx := context.Background()
	item := &store.BacklogItem{
		ID:       "BL-TRANSITIONS",
		Title:    "head transitions read api",
		State:    store.BacklogRunning,
		Priority: store.P2,
	}
	if err := op.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{
		ID:        "PIPE-TRANSITIONS-0",
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     store.PipelineMerging,
		Attempts:  1,
		StartedAt: time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC),
	}
	if err := op.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run.ID
}

// A run that never moved its head must return an empty ARRAY. A bare null
// forces every client to special-case it — the same defect that broke the HUD
// drawer for in-flight runs.
func TestHandlePipelineRunTransitions_EmptyIsJSONArrayNotNull(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	runID := seedTransitionRun(t, op)

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/"+runID+"/transitions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunID       string                    `json:"run_id"`
		Transitions []*store.MRHeadTransition `json:"transitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RunID != runID {
		t.Errorf("run_id = %q, want %q", body.RunID, runID)
	}
	if body.Transitions == nil {
		t.Error("transitions encoded as null; must be []")
	}
	if len(body.Transitions) != 0 {
		t.Errorf("transitions = %d, want 0", len(body.Transitions))
	}
}

func TestHandlePipelineRunTransitions_ReturnsLedgerNewestFirst(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()
	runID := seedTransitionRun(t, op)

	for _, pair := range [][2]string{{"sha-a", "sha-b"}, {"sha-b", "sha-c"}} {
		if _, err := op.store.MRHeadTransitions.Open(ctx, &store.MRHeadTransition{
			PipelineRunID: runID,
			Project:       "services/loom-core",
			MRIID:         77,
			SourceBranch:  "feat/x",
			TargetBranch:  "main",
			ReviewedSHA:   pair[0],
			SuccessorSHA:  pair[1],
			Trigger:       store.MRHeadTriggerExternal,
			State:         store.MRHeadTransitionAmbiguous,
			Provenance:    map[string]any{"reason": "external push observed at merge"},
		}); err != nil {
			t.Fatalf("seed transition %v: %v", pair, err)
		}
	}

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/"+runID+"/transitions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Transitions []*store.MRHeadTransition `json:"transitions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Transitions) != 2 {
		t.Fatalf("transitions = %d, want 2", len(body.Transitions))
	}
	if body.Transitions[0].Seq != 2 || body.Transitions[1].Seq != 1 {
		t.Errorf("ledger must be newest-first: %d, %d", body.Transitions[0].Seq, body.Transitions[1].Seq)
	}
	// The evidence bundle is the whole point of the endpoint: an operator
	// diagnosing a fail-closed merge must see both SHAs without shelling into
	// the pod.
	newest := body.Transitions[0]
	if newest.ReviewedSHA != "sha-b" || newest.SuccessorSHA != "sha-c" {
		t.Errorf("newest row = %+v", newest)
	}
	if newest.Provenance["reason"] != "external push observed at merge" {
		t.Errorf("provenance = %v", newest.Provenance)
	}
}

func TestHandlePipelineRunTransitions_UnknownRunIs404(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()

	rec := httptest.NewRecorder()
	op.httpMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mills/pipeline/runs/PIPE-NOPE/transitions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// The ledger is a read surface, like GET /pipeline/runs/{id}: no admin token
// required, and no mutation verb exposed by this slice.
func TestHandlePipelineRunTransitions_IsAnOpenReadOnlyRoute(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	runID := seedTransitionRun(t, op)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		op.httpMux().ServeHTTP(rec, httptest.NewRequest(method, "/api/mills/pipeline/runs/"+runID+"/transitions", nil))
		if rec.Code == http.StatusOK {
			t.Errorf("%s on the transitions route returned 200; the ledger must be read-only in this slice", method)
		}
	}
}
