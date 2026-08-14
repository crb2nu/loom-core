package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crb2nu/loom/pkg/mills/store"
)

// The open list read serves active entries in FIFO order with a per-lane
// depth summary — the HUD panel and fleet lane-pressure checks depend on
// the shape.
func TestHandleMergeQueueList(t *testing.T) {
	op, cleanup := newTestOperator(t)
	defer cleanup()
	ctx := context.Background()

	item := &store.BacklogItem{ID: "BL-MQL", Title: "x", State: store.BacklogRunning, Priority: store.P2}
	if err := op.store.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &store.PipelineRun{ID: "PIPE-MQL", BacklogID: item.ID, Template: "mills-default-pipeline",
		State: store.PipelineMerging, Attempts: 1, StartedAt: time.Now().UTC()}
	if err := op.store.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, _, err := op.store.MergeQueue.Enqueue(ctx, &store.MergeQueueEntry{
		PipelineRunID: run.ID, BacklogID: item.ID, Project: "services/loom-core",
		MRIID: 5, SourceBranch: "feat/mql", TargetBranch: "main", EnqueuedSHA: "sha-mql",
	}, 10); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	rec := httptest.NewRecorder()
	op.handleMergeQueueList(rec, httptest.NewRequest("GET", "/api/mills/merge-queue", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Active  []map[string]any `json:"active"`
		Summary struct {
			Depth int            `json:"depth"`
			Lanes map[string]int `json:"lanes"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Active) != 1 || got.Summary.Depth != 1 {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got.Summary.Lanes["services/loom-core→main"] != 1 {
		t.Fatalf("lane summary missing: %+v", got.Summary.Lanes)
	}
}
