package store

import (
	"context"
	"testing"
	"time"
)

// TestPipeline_ListActive guards the single-query active-runs list that
// replaced the runs-list handler's 9 per-state SELECTs: every non-terminal
// run appears exactly once, terminal runs are excluded, ordering groups by
// pipeline progression (queued → merging) then oldest-first within a state,
// and the row count agrees with CountActive (same predicate).
func TestPipeline_ListActive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{
		ID: "MILLS-ACTIVE", Title: "active", State: BacklogQueued, Priority: P2, CreatedBy: "test",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}

	now := time.Now().UTC()
	attempt := 0
	put := func(id string, state PipelineState, startedAt time.Time) {
		t.Helper()
		attempt++
		if err := st.Pipeline.PutRun(ctx, &PipelineRun{
			ID: id, BacklogID: item.ID, Template: "t", State: state,
			Attempts: attempt, StartedAt: startedAt,
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
	}

	// Insert deliberately out of progression order, with two runs in the
	// same state to pin the oldest-first tiebreak.
	put("CI-NEW", PipelineCI, now.Add(-1*time.Minute))
	put("PLANNING", PipelinePlanning, now.Add(-10*time.Minute))
	put("CI-OLD", PipelineCI, now.Add(-20*time.Minute))
	put("QUEUED", PipelineQueued, now.Add(-2*time.Minute))
	// Terminal rows must not appear.
	put("DONE", PipelineDone, now.Add(-30*time.Minute))
	put("ESCALATED", PipelineEscalated, now.Add(-30*time.Minute))
	put("PAUSED", PipelinePaused, now.Add(-30*time.Minute))

	runs, err := st.Pipeline.ListActive(ctx)
	if err != nil {
		t.Fatalf("list-active: %v", err)
	}

	wantOrder := []string{"QUEUED", "PLANNING", "CI-OLD", "CI-NEW"}
	if len(runs) != len(wantOrder) {
		t.Fatalf("list-active returned %d runs, want %d: %+v", len(runs), len(wantOrder), runs)
	}
	for i, want := range wantOrder {
		if runs[i].ID != want {
			t.Errorf("runs[%d].ID = %q, want %q", i, runs[i].ID, want)
		}
	}

	n, err := st.Pipeline.CountActive(ctx)
	if err != nil {
		t.Fatalf("count-active: %v", err)
	}
	if n != len(runs) {
		t.Errorf("CountActive = %d, ListActive returned %d — predicates drifted", n, len(runs))
	}
}
