package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedMergeQueueRun creates the backlog item + pipeline run the queue's
// foreign key requires and returns the run id.
func seedMergeQueueRun(t *testing.T, st *Store, id string) string {
	t.Helper()
	ctx := context.Background()
	item := &BacklogItem{
		ID:       "BL-" + id,
		Title:    "merge queue fixture",
		State:    BacklogRunning,
		Priority: P2,
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}
	run := &PipelineRun{
		ID:        "PIPE-" + id,
		BacklogID: item.ID,
		Template:  "mills-default-pipeline",
		State:     PipelineMerging,
		Attempts:  1,
		StartedAt: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC),
	}
	if err := st.Pipeline.PutRun(ctx, run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return run.ID
}

func queueEntry(runID string, iid int64) *MergeQueueEntry {
	return &MergeQueueEntry{
		PipelineRunID: runID,
		BacklogID:     "BL-x",
		Project:       "services/loom-core",
		MRIID:         iid,
		SourceBranch:  fmt.Sprintf("feat/mr-%d", iid),
		TargetBranch:  "main",
		EnqueuedSHA:   fmt.Sprintf("sha-%d", iid),
	}
}

func TestMergeQueueEnqueue_FIFOAndIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var runs []string
	for i := 0; i < 3; i++ {
		runs = append(runs, seedMergeQueueRun(t, st, fmt.Sprintf("mq-%d", i)))
	}
	for i, run := range runs {
		if _, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, int64(100+i)), 10); err != nil || !created {
			t.Fatalf("enqueue %d: created=%v err=%v", i, created, err)
		}
	}

	// Idempotent: re-enqueueing the first run returns its entry, no new row.
	again, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(runs[0], 100), 10)
	if err != nil || created {
		t.Fatalf("re-enqueue: created=%v err=%v", created, err)
	}
	if again.MRIID != 100 {
		t.Fatalf("re-enqueue returned wrong entry: %+v", again)
	}

	// One lane → one head, and it is the first enqueued run.
	heads, err := st.MergeQueue.Heads(ctx)
	if err != nil {
		t.Fatalf("heads: %v", err)
	}
	if len(heads) != 1 || heads[0].PipelineRunID != runs[0] {
		t.Fatalf("expected head %s, got %+v", runs[0], heads)
	}

	// Positions are FIFO.
	for i, run := range runs {
		pos, err := st.MergeQueue.Position(ctx, run)
		if err != nil || pos != i+1 {
			t.Fatalf("position %s: got %d err=%v want %d", run, pos, err, i+1)
		}
	}

	// Settling the head promotes the next enqueued run.
	if _, err := st.MergeQueue.MarkMerged(ctx, heads[0].ID, MergeQueueQueued, "merged-sha"); err != nil {
		t.Fatalf("mark merged: %v", err)
	}
	heads, err = st.MergeQueue.Heads(ctx)
	if err != nil || len(heads) != 1 || heads[0].PipelineRunID != runs[1] {
		t.Fatalf("expected promoted head %s, got %+v err=%v", runs[1], heads, err)
	}
}

func TestMergeQueueEnqueue_LaneFull(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		run := seedMergeQueueRun(t, st, fmt.Sprintf("full-%d", i))
		if _, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, int64(200+i)), 2); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	overflow := seedMergeQueueRun(t, st, "full-overflow")
	if _, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(overflow, 299), 2); !errors.Is(err, ErrMergeQueueFull) {
		t.Fatalf("expected ErrMergeQueueFull, got %v", err)
	}

	// A different lane is unaffected by the full one.
	other := seedMergeQueueRun(t, st, "full-otherlane")
	e := queueEntry(other, 300)
	e.TargetBranch = "release"
	if _, created, err := st.MergeQueue.Enqueue(ctx, e, 2); err != nil || !created {
		t.Fatalf("other-lane enqueue: created=%v err=%v", created, err)
	}
}

func TestMergeQueueTransition_CASAndTerminal(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := seedMergeQueueRun(t, st, "cas")
	e, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 400), 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := st.MergeQueue.Transition(ctx, MergeQueueTransition{
		ID: e.ID, From: MergeQueueQueued, To: MergeQueueRebasing,
		Detail: map[string]any{"ledger_seq": 1}, BumpAttempts: true,
	})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	if got.State != MergeQueueRebasing || got.Attempts != 1 {
		t.Fatalf("unexpected row after transition: %+v", got)
	}

	// Stale CAS: the row is no longer queued.
	if _, err := st.MergeQueue.Transition(ctx, MergeQueueTransition{
		ID: e.ID, From: MergeQueueQueued, To: MergeQueueMerging,
	}); !errors.Is(err, ErrMergeQueueConflict) {
		t.Fatalf("expected ErrMergeQueueConflict, got %v", err)
	}

	// SHA advance on the rebase-observed path.
	got, err = st.MergeQueue.Transition(ctx, MergeQueueTransition{
		ID: e.ID, From: MergeQueueRebasing, To: MergeQueueAwaitingPipeline,
		CurrentSHA: "sha-rebased",
	})
	if err != nil || got.CurrentSHA != "sha-rebased" {
		t.Fatalf("sha advance: %+v err=%v", got, err)
	}

	// Evict succeeds from any active state and settles the row.
	got, err = st.MergeQueue.MarkEvicted(ctx, e.ID, MergeQueueEvictCIRed, map[string]any{"detail": "pipeline failed"})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if got.State != MergeQueueEvicted || got.EvictionReason != MergeQueueEvictCIRed || got.SettledAt == nil {
		t.Fatalf("unexpected evicted row: %+v", got)
	}

	// Terminal rows refuse further settles.
	if _, err := st.MergeQueue.MarkMerged(ctx, e.ID, MergeQueueEvicted, "x"); !errors.Is(err, ErrMergeQueueConflict) {
		t.Fatalf("expected conflict on settled row, got %v", err)
	}
	// And drop out of Position (0 = not active).
	if pos, err := st.MergeQueue.Position(ctx, run); err != nil || pos != 0 {
		t.Fatalf("position after settle: %d err=%v", pos, err)
	}
}

func TestMergeQueueHeads_RestartResume(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := seedMergeQueueRun(t, st, "resume")
	e, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 500), 10)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := st.MergeQueue.Transition(ctx, MergeQueueTransition{
		ID: e.ID, From: MergeQueueQueued, To: MergeQueueRebasing,
		Detail: map[string]any{"ledger_seq": int64(3), "versions_cursor": int64(42)},
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	// A "restarted" processor reads the same head mid-flight with its
	// drive-state intact.
	heads, err := st.MergeQueue.Heads(ctx)
	if err != nil || len(heads) != 1 {
		t.Fatalf("heads: %+v err=%v", heads, err)
	}
	h := heads[0]
	if h.State != MergeQueueRebasing {
		t.Fatalf("expected rebasing head, got %s", h.State)
	}
	if got := h.Detail["ledger_seq"]; got != float64(3) && got != int64(3) {
		t.Fatalf("ledger_seq lost across reads: %#v", h.Detail)
	}
}
