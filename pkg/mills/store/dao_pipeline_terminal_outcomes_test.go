package store

import (
	"context"
	"testing"
	"time"
)

// TestPipeline_ListTerminalOutcomesSince guards the ground-truth projection:
// terminal runs only, newest-first, bounded by `since`, and carrying the cost
// and MR columns the config-outcome join reads. A run whose cost or MR is
// dropped here is silently re-attributed to "unknown" downstream.
func TestPipeline_ListTerminalOutcomesSince(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	item := &BacklogItem{
		ID: "MILLS-OUTCOME", Title: "outcomes", State: BacklogQueued, Priority: P2, CreatedBy: "test",
	}
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("put backlog: %v", err)
	}

	now := time.Now().UTC()
	mrIID := int64(4242)
	attempt := 0
	for _, r := range []*PipelineRun{
		{ID: "MERGED", BacklogID: item.ID, Template: "t", State: PipelineDone, StartedAt: now.Add(-1 * time.Hour), CostUSD: 3.50, MRIID: &mrIID},
		{ID: "ESCALATED", BacklogID: item.ID, Template: "t", State: PipelineEscalated, StartedAt: now.Add(-2 * time.Hour), CostUSD: 1.25},
		{ID: "IN-FLIGHT", BacklogID: item.ID, Template: "t", State: PipelineCI, StartedAt: now.Add(-3 * time.Hour), CostUSD: 9.99},
		{ID: "OLD", BacklogID: item.ID, Template: "t", State: PipelineDone, StartedAt: now.Add(-48 * time.Hour), CostUSD: 7.00},
	} {
		attempt++
		r.Attempts = attempt
		if err := st.Pipeline.PutRun(ctx, r); err != nil {
			t.Fatalf("put run %s: %v", r.ID, err)
		}
	}

	rows, err := st.Pipeline.ListTerminalOutcomesSince(ctx, now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("list-terminal-outcomes: %v", err)
	}
	wantOrder := []string{"MERGED", "ESCALATED"}
	if len(rows) != len(wantOrder) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(wantOrder), rows)
	}
	for i, want := range wantOrder {
		if rows[i].RunID != want {
			t.Fatalf("rows[%d].RunID = %q, want %q", i, rows[i].RunID, want)
		}
	}

	merged := rows[0]
	if merged.State != PipelineDone || merged.BacklogID != item.ID {
		t.Errorf("merged row = %+v", merged)
	}
	if merged.CostUSD != 3.50 {
		t.Errorf("merged cost_usd = %v, want 3.50", merged.CostUSD)
	}
	if merged.MRIID == nil || *merged.MRIID != mrIID {
		t.Errorf("merged mr_iid = %v, want %d", merged.MRIID, mrIID)
	}
	// A run that never opened an MR must read as nil, not as MR 0 — the
	// regression join keys on the iid.
	if rows[1].MRIID != nil {
		t.Errorf("escalated mr_iid = %v, want nil", *rows[1].MRIID)
	}
	if rows[1].CostUSD != 1.25 {
		t.Errorf("escalated cost_usd = %v, want 1.25", rows[1].CostUSD)
	}
}
