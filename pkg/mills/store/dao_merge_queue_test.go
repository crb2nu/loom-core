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

// 031 re-admission: an evicted run that re-passes enqueue-time authorization
// gets a FRESH candidate; a merged verdict stays authoritative; a mid-flight
// resume still re-finds its active row; readers take the newest row.
func TestMergeQueueEnqueue_ReadmitsAfterEviction(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	run := seedMergeQueueRun(t, st, "mq-readmit")

	first, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 200), 10)
	if err != nil || !created {
		t.Fatalf("first enqueue: created=%v err=%v", created, err)
	}
	// Mid-flight resume re-finds the active row.
	if again, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 200), 10); err != nil || created || again.ID != first.ID {
		t.Fatalf("active resume: created=%v id=%d err=%v", created, again.ID, err)
	}

	if _, err := st.MergeQueue.MarkEvicted(ctx, first.ID, MergeQueueEvictHeadMoved, map[string]any{"detail": "head moved"}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	// Re-enqueue after eviction inserts a fresh candidate.
	second, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 200), 10)
	if err != nil || !created {
		t.Fatalf("re-admission: created=%v err=%v", created, err)
	}
	if second.ID == first.ID || second.State != MergeQueueQueued {
		t.Fatalf("re-admission row = %+v (first %d)", second, first.ID)
	}
	// Get returns the newest row.
	got, err := st.MergeQueue.Get(ctx, run)
	if err != nil || got.ID != second.ID {
		t.Fatalf("Get newest: id=%d err=%v want %d", got.ID, err, second.ID)
	}
	// A merged verdict is final: enqueue re-finds it, never re-queues.
	if _, err := st.MergeQueue.MarkMerged(ctx, second.ID, MergeQueueQueued, "sha-final"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if third, created, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, 200), 10); err != nil || created || third.ID != second.ID {
		t.Fatalf("post-merge enqueue: created=%v id=%d err=%v", created, third.ID, err)
	}
}

func TestMergeQueueListSettled_FilterBoundsAndOrdering(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	var entries []*MergeQueueEntry
	for i := 0; i < 105; i++ {
		run := seedMergeQueueRun(t, st, fmt.Sprintf("settled-%03d", i))
		e, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(run, int64(600+i)), 0)
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		entries = append(entries, e)
		if i%2 == 0 {
			_, err = st.MergeQueue.MarkMerged(ctx, e.ID, MergeQueueQueued, "merged-sha")
		} else {
			_, err = st.MergeQueue.MarkEvicted(ctx, e.ID, MergeQueueEvictCIRed, nil)
		}
		if err != nil {
			t.Fatalf("settle %d: %v", i, err)
		}
		settled := base.Add(time.Duration(i) * time.Minute)
		if _, err := st.db.ExecContext(ctx, `UPDATE merge_queue SET settled_at = ?, updated_at = ? WHERE id = ?`,
			timeRFC3339(settled), timeRFC3339(settled), e.ID); err != nil {
			t.Fatalf("set settled_at %d: %v", i, err)
		}
	}

	// Leave one active row to prove terminal filtering.
	activeRun := seedMergeQueueRun(t, st, "settled-active")
	if _, _, err := st.MergeQueue.Enqueue(ctx, queueEntry(activeRun, 999), 0); err != nil {
		t.Fatalf("enqueue active: %v", err)
	}

	got, err := st.MergeQueue.ListSettled(ctx, base.Add(100*time.Minute), 10)
	if err != nil {
		t.Fatalf("ListSettled: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("since filter returned %d entries, want 5", len(got))
	}
	for i, e := range got {
		want := entries[104-i].PipelineRunID
		if e.PipelineRunID != want || !e.State.IsTerminal() {
			t.Fatalf("entry %d = %s/%s, want %s/terminal", i, e.PipelineRunID, e.State, want)
		}
	}

	for _, limit := range []int{0, -1} {
		got, err = st.MergeQueue.ListSettled(ctx, time.Time{}, limit)
		if err != nil || len(got) != 20 {
			t.Fatalf("limit %d: len=%d err=%v, want default 20", limit, len(got), err)
		}
	}
	got, err = st.MergeQueue.ListSettled(ctx, time.Time{}, 1000)
	if err != nil || len(got) != 100 {
		t.Fatalf("over-cap limit: len=%d err=%v, want cap 100", len(got), err)
	}
}
