package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReadQuiescence_ExactDurableCounts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	empty, err := st.ReadQuiescence(ctx)
	if err != nil {
		t.Fatalf("read empty quiescence: %v", err)
	}
	if !empty.Quiescent() {
		t.Fatalf("empty store should be quiescent: %+v", empty)
	}

	// One transactional pipeline claim materializes a running backlog item,
	// active pipeline, running DAG workflow, and pending dispatch together.
	claimed := seedClaimBacklog(t, st, "MILLS-QUIESCENCE-ACTIVE")
	if _, err := st.ClaimPipelineStart(ctx, claimTestRequest(claimed.ID)); err != nil {
		t.Fatalf("claim active pipeline: %v", err)
	}

	// Exactly one queued backlog row. The terminal pipeline's parent is paused
	// so it cannot accidentally inflate the queued count.
	seedClaimBacklog(t, st, "MILLS-QUIESCENCE-QUEUED")
	pausedParent := seedClaimBacklog(t, st, "MILLS-QUIESCENCE-PAUSED")
	pausedParent.State = BacklogPaused
	if err := st.Backlog.Put(ctx, pausedParent); err != nil {
		t.Fatalf("pause terminal pipeline parent: %v", err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID:        "PIPE-QUIESCENCE-PAUSED",
		BacklogID: pausedParent.ID,
		Template:  "test",
		State:     PipelinePaused,
		Attempts:  1,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed paused pipeline: %v", err)
	}

	now := time.Now().UTC()
	for _, run := range []*WorkflowRun{
		{ID: "WF-QUIESCENCE-RUNNING", Engine: WorkflowEngineImperative, Template: "test", State: WorkflowRunRunning, StartedAt: &now},
		{ID: "WF-QUIESCENCE-PAUSED", Engine: WorkflowEngineImperative, Template: "test", State: WorkflowRunPaused, StartedAt: &now, PausedAt: &now},
		{ID: "WF-QUIESCENCE-DONE", Engine: WorkflowEngineImperative, Template: "test", State: WorkflowRunDone, StartedAt: &now, EndedAt: &now},
	} {
		if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
			t.Fatalf("seed workflow %s: %v", run.ID, err)
		}
	}

	for _, run := range []*SpinRun{
		{ID: "SPIN-QUIESCENCE-PENDING", Status: SpinPending, StartedAt: now},
		{ID: "SPIN-QUIESCENCE-RUNNING", Status: SpinRunning, StartedAt: now},
		{ID: "SPIN-QUIESCENCE-DONE", Status: SpinSucceeded, StartedAt: now, EndedAt: &now},
	} {
		if err := st.Spin.Put(ctx, run); err != nil {
			t.Fatalf("seed spin %s: %v", run.ID, err)
		}
	}

	for _, run := range []*CouncilRun{
		{ID: "COUNCIL-QUIESCENCE-ACTIVE", Trigger: CouncilTriggerCron, StartedAt: now, Outcome: CouncilOutcomeSuccess},
		{ID: "COUNCIL-QUIESCENCE-DONE", Trigger: CouncilTriggerCron, StartedAt: now, EndedAt: &now, Outcome: CouncilOutcomeSuccess},
	} {
		if err := st.Council.Put(ctx, run); err != nil {
			t.Fatalf("seed council %s: %v", run.ID, err)
		}
	}

	for _, run := range []*CrossRepoRun{
		{ID: "CROSS-QUIESCENCE-ACTIVE", BacklogItemID: pausedParent.ID, State: CrossRepoOpen},
		{ID: "CROSS-QUIESCENCE-DONE", BacklogItemID: pausedParent.ID, State: CrossRepoMerged},
	} {
		if err := st.CrossRepo.PutRun(ctx, run); err != nil {
			t.Fatalf("seed cross-repo run %s: %v", run.ID, err)
		}
	}

	got, err := st.ReadQuiescence(ctx)
	if err != nil {
		t.Fatalf("read quiescence: %v", err)
	}
	want := QuiescenceCounts{
		QueuedBacklog:          1,
		ActivePipelineRuns:     1,
		ActiveWorkflowRuns:     3, // claimed DAG + explicit running + paused
		ActiveSpinningRoomRuns: 2,
		ActiveCouncilRuns:      1,
		ActiveCrossRepoRuns:    1,
		PendingDispatches:      1,
	}
	if got != want {
		t.Fatalf("quiescence counts = %+v, want %+v", got, want)
	}
	if got.Quiescent() {
		t.Fatalf("active durable work reported quiescent: %+v", got)
	}
}

func TestReadQuiescence_UnknownStatesFailClosed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	item := seedClaimBacklog(t, st, "MILLS-QUIESCENCE-FUTURE")
	item.State = BacklogPaused
	if err := st.Backlog.Put(ctx, item); err != nil {
		t.Fatalf("pause fixture backlog: %v", err)
	}
	if err := st.Pipeline.PutRun(ctx, &PipelineRun{
		ID: "PIPE-QUIESCENCE-FUTURE", BacklogID: item.ID, Template: "test",
		State: PipelineState("future_pipeline_state"), Attempts: 1, StartedAt: now,
	}); err != nil {
		t.Fatalf("seed future pipeline state: %v", err)
	}
	if err := st.Workflow.PutWorkflowRun(ctx, &WorkflowRun{
		ID: "WF-QUIESCENCE-FUTURE", Engine: WorkflowEngineImperative,
		Template: "test", State: WorkflowRunState("future_workflow_state"), StartedAt: &now,
	}); err != nil {
		t.Fatalf("seed future workflow state: %v", err)
	}
	if err := st.Spin.Put(ctx, &SpinRun{
		ID: "SPIN-QUIESCENCE-FUTURE", Status: SpinStatus("future_spin_state"), StartedAt: now,
	}); err != nil {
		t.Fatalf("seed future spin state: %v", err)
	}
	if err := st.CrossRepo.PutRun(ctx, &CrossRepoRun{
		ID: "CROSS-QUIESCENCE-FUTURE", BacklogItemID: item.ID,
		State: CrossRepoState("future_cross_repo_state"),
	}); err != nil {
		t.Fatalf("seed future cross-repo state: %v", err)
	}

	// pending_dispatches has a current-version CHECK constraint. Simulate a
	// future schema value on one dedicated connection to prove the safety query
	// does not silently treat an as-yet-unknown dispatch state as terminal.
	dispatchItem := seedClaimBacklog(t, st, "MILLS-QUIESCENCE-FUTURE-DISPATCH")
	claim, err := st.ClaimPipelineStart(ctx, claimTestRequest(dispatchItem.ID))
	if err != nil {
		t.Fatalf("claim future dispatch fixture: %v", err)
	}
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatalf("open future dispatch fixture connection: %v", err)
	}
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{`UPDATE backlog_items SET state = 'paused' WHERE id = ?`, []any{dispatchItem.ID}},
		{`UPDATE pipeline_runs SET state = 'done' WHERE id = ?`, []any{claim.Run.ID}},
		{`UPDATE workflow_runs SET state = 'done' WHERE id = ?`, []any{claim.WorkflowRun.ID}},
	} {
		if _, err := conn.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatalf("terminalize future dispatch fixture: %v", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("disable check constraints for future dispatch fixture: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE pending_dispatches SET status = 'future_dispatch_state' WHERE id = ?`, claim.Dispatch.ID); err != nil {
		t.Fatalf("seed future dispatch state: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA ignore_check_constraints = OFF`); err != nil {
		t.Fatalf("restore check constraints: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close future dispatch fixture connection: %v", err)
	}

	got, err := st.ReadQuiescence(ctx)
	if err != nil {
		t.Fatalf("read quiescence: %v", err)
	}
	if got.ActivePipelineRuns != 1 || got.ActiveWorkflowRuns != 1 ||
		got.ActiveSpinningRoomRuns != 1 || got.ActiveCrossRepoRuns != 1 || got.PendingDispatches != 1 {
		t.Fatalf("future states were not counted fail-closed: %+v", got)
	}
	if got.Quiescent() {
		t.Fatalf("future states reported quiescent: %+v", got)
	}
}

func TestReadQuiescence_QueryFailureReturnsError(t *testing.T) {
	st := newTestStore(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if got, err := st.ReadQuiescence(context.Background()); err == nil {
		t.Fatalf("read after close = %+v, want error", got)
	}
}

func TestReadQuiescence_ActiveIndexesInstalled(t *testing.T) {
	st := newTestStore(t)
	want := []string{
		"idx_council_active",
		"idx_pipeline_quiescence_active",
		"idx_workflow_quiescence_active",
		"idx_spin_quiescence_active",
		"idx_cross_repo_quiescence_active",
		"idx_dispatch_quiescence_active",
	}
	for _, name := range want {
		var got string
		if err := st.db.QueryRowContext(context.Background(),
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, name,
		).Scan(&got); err != nil {
			t.Fatalf("quiescence index %s: %v", name, err)
		}
		if got != name {
			t.Fatalf("quiescence index = %q, want %q", got, name)
		}
	}

	// A focused plan assertion prevents predicate drift from silently turning
	// the destructive-safety read back into a full history scan.
	var detail string
	rows, err := st.db.QueryContext(context.Background(),
		`EXPLAIN QUERY PLAN SELECT COUNT(*) FROM spin_runs
		 WHERE status NOT IN ('succeeded', 'failed', 'timeout')`)
	if err != nil {
		t.Fatalf("explain spin quiescence query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var rowDetail string
		if err := rows.Scan(&id, &parent, &unused, &rowDetail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		detail += rowDetail
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	if detail == "" || !strings.Contains(detail, "idx_spin_quiescence_active") {
		t.Fatalf("spin quiescence query did not use partial index: %s", detail)
	}
}
