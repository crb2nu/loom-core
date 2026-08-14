package store

import (
	"context"
	"testing"
	"time"
)

// seedRelaunchItem stores one backlog item plus one pipeline run and returns
// the run so callers can layer more runs on the same item.
func seedRelaunchItem(
	t *testing.T,
	st *Store,
	itemID string,
	itemState BacklogState,
	run *PipelineRun,
) {
	t.Helper()
	ctx := context.Background()
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: itemID, Title: "title " + itemID,
		State: itemState, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put backlog %s: %v", itemID, err)
	}
	if run != nil {
		run.BacklogID = itemID
		if run.Template == "" {
			run.Template = "mills-default-pipeline"
		}
		if err := st.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("put run %s: %v", run.ID, err)
		}
	}
}

func TestListByEndedSince_ProjectsOnlyLatestRetryableEscalations(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	retryable := true
	notRetryable := false

	// Included: escalated item, latest run retryable=true with class metadata.
	endedA := base.Add(2 * time.Hour)
	seedRelaunchItem(t, st, "BL-RETRY-A", BacklogEscalated, &PipelineRun{
		ID: "PIPE-RETRY-A", State: PipelineEscalated, Attempts: 1,
		StartedAt: base, EndedAt: &endedA,
		EscalationClass: "infra", FailureClass: "infrastructure",
		EscalationRetryable: &retryable,
	})

	// Excluded: latest run explicitly not retryable.
	endedB := base.Add(time.Hour)
	seedRelaunchItem(t, st, "BL-NORETRY-B", BacklogEscalated, &PipelineRun{
		ID: "PIPE-NORETRY-B", State: PipelineEscalated, Attempts: 1,
		StartedAt: base, EndedAt: &endedB,
		EscalationClass: "config", FailureClass: "configuration",
		EscalationRetryable: &notRetryable,
	})

	// Excluded: latest run predates the metadata columns (retryable NULL).
	endedC := base.Add(time.Hour)
	seedRelaunchItem(t, st, "BL-LEGACY-C", BacklogEscalated, &PipelineRun{
		ID: "PIPE-LEGACY-C", State: PipelineEscalated, Attempts: 1,
		StartedAt: base, EndedAt: &endedC,
	})

	// Excluded: an OLDER run was retryable, but the LATEST run is not — the
	// latest-run contract must not resurrect the item on stale evidence.
	endedD1 := base.Add(30 * time.Minute)
	seedRelaunchItem(t, st, "BL-STALE-D", BacklogEscalated, &PipelineRun{
		ID: "PIPE-STALE-D-OLD", State: PipelineEscalated, Attempts: 1,
		StartedAt: base, EndedAt: &endedD1,
		EscalationClass: "infra", FailureClass: "infrastructure",
		EscalationRetryable: &retryable,
	})
	endedD2 := base.Add(90 * time.Minute)
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: "PIPE-STALE-D-NEW", BacklogID: "BL-STALE-D",
		Template: "mills-default-pipeline", State: PipelineEscalated, Attempts: 2,
		StartedAt: base.Add(time.Hour), EndedAt: &endedD2,
		EscalationClass: "config", FailureClass: "configuration",
		EscalationRetryable: &notRetryable,
	}); err != nil {
		t.Fatalf("put newer stale-D run: %v", err)
	}

	// Excluded: retryable latest run but the ITEM is not escalated.
	endedE := base.Add(time.Hour)
	seedRelaunchItem(t, st, "BL-RUNNING-E", BacklogRunning, &PipelineRun{
		ID: "PIPE-RUNNING-E", State: PipelineEscalated, Attempts: 1,
		StartedAt: base, EndedAt: &endedE,
		EscalationRetryable: &retryable,
	})

	got, err := st.Backlog.ListByEndedSince(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("list by ended since: %v", err)
	}
	if len(got) != 1 {
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		t.Fatalf("candidates = %v, want only BL-RETRY-A", ids)
	}
	c := got[0]
	if c.ID != "BL-RETRY-A" || c.Title != "title BL-RETRY-A" {
		t.Errorf("identity = {%s %s}, want {BL-RETRY-A title BL-RETRY-A}", c.ID, c.Title)
	}
	if c.EscalationClass != "infra" || c.FailureClass != "infrastructure" {
		t.Errorf("classes = {%s %s}, want {infra infrastructure}", c.EscalationClass, c.FailureClass)
	}
	if c.EndedAt == nil || !c.EndedAt.Equal(endedA) {
		t.Errorf("EndedAt = %v, want %v", c.EndedAt, endedA)
	}
}

func TestListByEndedSince_WindowOrderingAndLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	retryable := true

	endedOld := base.Add(-48 * time.Hour)
	seedRelaunchItem(t, st, "BL-OLD", BacklogEscalated, &PipelineRun{
		ID: "PIPE-OLD", State: PipelineEscalated, Attempts: 1,
		StartedAt: base.Add(-49 * time.Hour), EndedAt: &endedOld,
		EscalationRetryable: &retryable,
	})
	endedMid := base.Add(-2 * time.Hour)
	seedRelaunchItem(t, st, "BL-MID", BacklogEscalated, &PipelineRun{
		ID: "PIPE-MID", State: PipelineEscalated, Attempts: 1,
		StartedAt: base.Add(-3 * time.Hour), EndedAt: &endedMid,
		EscalationRetryable: &retryable,
	})
	endedNew := base.Add(-time.Hour)
	seedRelaunchItem(t, st, "BL-NEW", BacklogEscalated, &PipelineRun{
		ID: "PIPE-NEW", State: PipelineEscalated, Attempts: 1,
		StartedAt: base.Add(-90 * time.Minute), EndedAt: &endedNew,
		EscalationRetryable: &retryable,
	})

	// The window on ended_at excludes the 48h-old candidate.
	got, err := st.Backlog.ListByEndedSince(ctx, base.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("list windowed: %v", err)
	}
	if len(got) != 2 || got[0].ID != "BL-NEW" || got[1].ID != "BL-MID" {
		ids := make([]string, 0, len(got))
		for _, c := range got {
			ids = append(ids, c.ID)
		}
		t.Fatalf("windowed candidates = %v, want [BL-NEW BL-MID] (newest ended first)", ids)
	}

	// A zero since applies no window.
	all, err := st.Backlog.ListByEndedSince(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatalf("list unwindowed: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unwindowed candidates = %d, want 3", len(all))
	}

	// limit bounds the result after ordering.
	one, err := st.Backlog.ListByEndedSince(ctx, time.Time{}, 1)
	if err != nil {
		t.Fatalf("list limited: %v", err)
	}
	if len(one) != 1 || one[0].ID != "BL-NEW" {
		t.Fatalf("limited candidates = %+v, want only BL-NEW", one)
	}
}
