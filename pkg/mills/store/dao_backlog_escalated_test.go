package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestListEscalatedWithMRFiltersUnroutableRowsBeforeLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const limit = 128
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	success := StageOutcomeSuccess
	failure := StageOutcomeError

	for i := 0; i < limit; i++ {
		backlogID := fmt.Sprintf("MILLS-LEGACY-%03d", i)
		if err := st.Backlog.Put(ctx, &BacklogItem{
			ID: backlogID, Title: "legacy unroutable escalation",
			State: BacklogEscalated, Priority: P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("put legacy backlog %d: %v", i, err)
		}

		mrIID := int64(1000 + i)
		if i == 3 {
			mrIID = 0
		}
		run := &PipelineRun{
			ID: fmt.Sprintf("PIPE-LEGACY-%03d", i), BacklogID: backlogID,
			Template: "mills-default-pipeline", State: PipelineEscalated,
			Attempts: 1, MRIID: &mrIID, StartedAt: base.Add(time.Duration(i) * time.Minute),
		}
		if err := st.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("put legacy run %d: %v", i, err)
		}
		putProjectStage := func(stage, key, project string, outcome StageOutcome) {
			t.Helper()
			ended := run.StartedAt.Add(time.Second)
			if err := st.Pipeline.PutStage(ctx, &StageResult{
				PipelineRunID: run.ID, Stage: stage, Attempt: 1,
				StartedAt: run.StartedAt, EndedAt: &ended, Outcome: &outcome,
				Artifacts: map[string]any{key: project},
			}); err != nil {
				t.Fatalf("put %s project stage for legacy row %d: %v", stage, i, err)
			}
		}

		switch i {
		case 0:
			putProjectStage("mr", "mr_project", "   ", success)
		case 1:
			putProjectStage("mr", "mr_project", "services/loom-core", failure)
		case 2:
			putProjectStage("mr", "mr_project", "services/loom-core", success)
			// At the same start time, the higher attempt is the most-recent
			// run and it has no MR. The item must STAY a candidate through
			// its newest MR-BEARING run: a retry that dies before the mr
			// stage must not hide an earlier MR that later merged (rescue
			// trains, MWPS races — observed 2026-08-19). Closing still
			// requires GitLab to report that MR merged.
			if err := st.Pipeline.PutRun(ctx, &PipelineRun{
				ID: "PIPE-LEGACY-002-RETRY", BacklogID: backlogID,
				Template: "mills-default-pipeline", State: PipelineEscalated,
				Attempts: 2, StartedAt: run.StartedAt,
			}); err != nil {
				t.Fatalf("put latest legacy retry: %v", err)
			}
		case 3:
			// A zero IID is non-NULL in SQLite but is not a GitLab MR identity.
			putProjectStage("mr", "mr_project", "services/loom-core", success)
		case 4:
			putProjectStage("mr", "mr_project", "services/loom-core", success)
			if _, err := st.DB().ExecContext(ctx,
				`UPDATE stage_results SET artifacts_json = '{' WHERE pipeline_run_id = ?`, run.ID,
			); err != nil {
				t.Fatalf("corrupt legacy stage JSON: %v", err)
			}
		case 5:
			putProjectStage("mr", "mr_project", "services/loom-core", success)
			putProjectStage("ci_watch", "ci_project", "services/other", success)
		}

		if _, err := st.DB().ExecContext(ctx,
			`UPDATE backlog_items SET updated_at = ? WHERE id = ?`,
			timeRFC3339(base.Add(time.Duration(i)*time.Minute)), backlogID,
		); err != nil {
			t.Fatalf("age legacy backlog %d: %v", i, err)
		}
	}

	const validID = "MILLS-VALID-NEWER"
	if err := st.Backlog.Put(ctx, &BacklogItem{
		ID: validID, Title: "newer routable escalation",
		State: BacklogEscalated, Priority: P2, CreatedBy: "test",
	}); err != nil {
		t.Fatalf("put valid backlog: %v", err)
	}
	validMRIID := int64(9001)
	validRun := &PipelineRun{
		ID: "PIPE-VALID-NEWER", BacklogID: validID,
		Template: "mills-default-pipeline", State: PipelineEscalated,
		Attempts: 1, MRIID: &validMRIID, StartedAt: base.Add(24 * time.Hour),
	}
	if err := st.Pipeline.PutRun(ctx, validRun); err != nil {
		t.Fatalf("put valid run: %v", err)
	}
	ended := validRun.StartedAt.Add(time.Second)
	if err := st.Pipeline.PutStage(ctx, &StageResult{
		PipelineRunID: validRun.ID, Stage: "cleanup", Attempt: 1,
		StartedAt: validRun.StartedAt, EndedAt: &ended, Outcome: &success,
		Artifacts: map[string]any{"cleanup_project": "services/loom-core"},
	}); err != nil {
		t.Fatalf("put valid project stage: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE backlog_items SET updated_at = ? WHERE id = ?`,
		timeRFC3339(base.Add(48*time.Hour)), validID,
	); err != nil {
		t.Fatalf("age valid backlog: %v", err)
	}

	got, err := st.Backlog.ListEscalatedWithMR(ctx, limit)
	if err != nil {
		t.Fatalf("list escalated with MR: %v", err)
	}
	// Oldest updated_at first: LEGACY-002 (reachable via its newest MR-bearing
	// run despite the MR-less retry on top) drains before the newer valid item.
	want := []string{"MILLS-LEGACY-002", validID}
	ids := make([]string, 0, len(got))
	for _, item := range got {
		ids = append(ids, item.ID)
	}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("candidates = %v, want %v", ids, want)
	}

	// Disjointness: an item with ANY MR-bearing run belongs to the IID pass
	// alone; the branch pass's smaller budget is reserved for items where a
	// branch lookup is the only available evidence.
	without, err := st.Backlog.ListEscalatedWithoutMR(ctx, limit)
	if err != nil {
		t.Fatalf("list escalated without MR: %v", err)
	}
	for _, item := range without {
		if item.ID == "MILLS-LEGACY-002" {
			t.Fatalf("MILLS-LEGACY-002 leaked into the without-MR candidates")
		}
	}
}

func TestListEscalatedWithMRAcceptsEachDurableProjectKey(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	success := StageOutcomeSuccess
	started := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		stage string
		key   string
	}{
		{stage: "mr", key: "mr_project"},
		{stage: "ci_watch", key: "ci_project"},
		{stage: "merge", key: "merged_project"},
		{stage: "cleanup", key: "cleanup_project"},
	}
	want := make(map[string]bool, len(tests))

	for i, tt := range tests {
		backlogID := fmt.Sprintf("MILLS-DURABLE-PROJECT-%d", i)
		want[backlogID] = true
		if err := st.Backlog.Put(ctx, &BacklogItem{
			ID: backlogID, Title: tt.stage + " project provenance",
			State: BacklogEscalated, Priority: P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("put %s backlog: %v", tt.stage, err)
		}
		mrIID := int64(9100 + i)
		run := &PipelineRun{
			ID: fmt.Sprintf("PIPE-DURABLE-PROJECT-%d", i), BacklogID: backlogID,
			Template: "mills-default-pipeline", State: PipelineEscalated,
			Attempts: 1, MRIID: &mrIID, StartedAt: started.Add(time.Duration(i) * time.Minute),
		}
		if err := st.Pipeline.PutRun(ctx, run); err != nil {
			t.Fatalf("put %s run: %v", tt.stage, err)
		}
		ended := run.StartedAt.Add(time.Second)
		if err := st.Pipeline.PutStage(ctx, &StageResult{
			PipelineRunID: run.ID, Stage: tt.stage, Attempt: 1,
			StartedAt: run.StartedAt, EndedAt: &ended, Outcome: &success,
			Artifacts: map[string]any{tt.key: " services/loom-core "},
		}); err != nil {
			t.Fatalf("put %s stage: %v", tt.stage, err)
		}
	}

	got, err := st.Backlog.ListEscalatedWithMR(ctx, len(tests))
	if err != nil {
		t.Fatalf("list escalated with MR: %v", err)
	}
	for _, item := range got {
		delete(want, item.ID)
	}
	if len(got) != len(tests) || len(want) != 0 {
		t.Fatalf("got %d candidates with missing IDs %v", len(got), want)
	}
}

func TestAutoRequeueCursorRetainsOrderingAfterStateChange(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	for i, priority := range []Priority{P1, P2, P2, P3} {
		item := &BacklogItem{ID: fmt.Sprintf("cursor-%d", i), Title: "cursor", State: BacklogEscalated, Priority: priority, CreatedBy: "test", CreatedAt: base}
		if err := st.Backlog.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err := st.Backlog.Get(ctx, "cursor-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Backlog.SaveAutoRequeueCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE backlog_items SET state = 'queued', priority = 'P0' WHERE id = ?`, cursor.ID); err != nil {
		t.Fatal(err)
	}
	saved, err := st.Backlog.AutoRequeueCursor(ctx)
	if err != nil || saved == nil || saved.ID != cursor.ID || saved.Priority != cursor.Priority || !saved.CreatedAt.Equal(cursor.CreatedAt) {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	remaining, err := st.Backlog.CountAutoRequeueRemaining(ctx, saved)
	if err != nil || remaining != 2 {
		t.Fatalf("remaining=%d err=%v", remaining, err)
	}
	page, err := st.Backlog.ListByStatePage(ctx, BacklogEscalated, saved, 64)
	if err != nil || len(page) != 2 || page[0].ID != "cursor-2" || page[1].ID != "cursor-3" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	if err := st.Backlog.SaveAutoRequeueCursor(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if saved, err := st.Backlog.AutoRequeueCursor(ctx); err != nil || saved != nil {
		t.Fatalf("reset=%+v err=%v", saved, err)
	}
}
