package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// seedWorkflowRun inserts a minimal imperative workflow run so step FKs
// resolve, and returns its id.
func seedWorkflowRun(t *testing.T, st *Store, id string) string {
	t.Helper()
	now := time.Now().UTC()
	run := &WorkflowRun{
		ID:                 id,
		Engine:             WorkflowEngineImperative,
		Template:           "implement-slice",
		TemplateVersion:    "v1",
		InterpreterVersion: "starlark-0.1",
		WorkflowParams:     `{"backlog_id":"X"}`,
		State:              WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := st.Workflow.PutWorkflowRun(context.Background(), run); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func TestWorkflowRun_RoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	paused := now.Add(time.Minute)

	run := &WorkflowRun{
		ID:                 "WF-2026-06-06-001",
		Engine:             WorkflowEngineImperative,
		Template:           "implement-slice",
		TemplateVersion:    "v3",
		InterpreterVersion: "starlark-0.2",
		WorkflowParams:     `{"k":"v"}`,
		State:              WorkflowRunRunning,
		StartedAt:          &now,
		PausedAt:           &paused,
		CostUSD:            1.25,
		ParentSessionID:    "sess-abc",
	}
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("put run: %v", err)
	}
	got, err := st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Engine != WorkflowEngineImperative || got.State != WorkflowRunRunning {
		t.Fatalf("engine/state mismatch: %+v", got)
	}
	if got.TemplateVersion != "v3" || got.InterpreterVersion != "starlark-0.2" {
		t.Fatalf("version fields mismatch: %+v", got)
	}
	if got.WorkflowParams != `{"k":"v"}` {
		t.Fatalf("params mismatch: %q", got.WorkflowParams)
	}
	if got.CostUSD != 1.25 || got.ParentSessionID != "sess-abc" {
		t.Fatalf("cost/session mismatch: %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(now) {
		t.Fatalf("started_at mismatch: %v want %v", got.StartedAt, now)
	}
	if got.PausedAt == nil || !got.PausedAt.Equal(paused) {
		t.Fatalf("paused_at mismatch: %v want %v", got.PausedAt, paused)
	}

	// Upsert: flip to done with ended_at.
	ended := now.Add(2 * time.Minute)
	run.State = WorkflowRunDone
	run.EndedAt = &ended
	run.CostUSD = 2.0
	if err := st.Workflow.PutWorkflowRun(ctx, run); err != nil {
		t.Fatalf("upsert run: %v", err)
	}
	got, err = st.Workflow.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("re-get run: %v", err)
	}
	if got.State != WorkflowRunDone || got.EndedAt == nil || got.CostUSD != 2.0 {
		t.Fatalf("upsert not applied: %+v", got)
	}

	if _, err := st.Workflow.GetWorkflowRun(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for missing run, got %v", err)
	}
}

// TestWorkflowAppendStep_Idempotency covers the three core AppendStep
// semantics: idempotent re-append, pending->success transition, and the
// call_hash mismatch detection (no silent overwrite).
func TestWorkflowAppendStep_Idempotency(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-IDEMPOTENT")

	started := time.Now().UTC().Truncate(time.Millisecond)
	step := &WorkflowStep{
		RunID:      runID,
		StepKey:    "spawn:plan:0",
		EventType:  WorkflowEventSpawnRequested,
		CallHash:   "hashA",
		Status:     WorkflowStepPending,
		StartedAt:  &started,
		CostSource: WorkflowCostUnavailable,
	}
	first, err := st.Workflow.AppendStep(ctx, step)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	if first.ID == 0 || first.Status != WorkflowStepPending {
		t.Fatalf("first append unexpected: %+v", first)
	}

	// Re-append the identical pending step -> single row, same id, no-op-ish.
	again := &WorkflowStep{
		RunID:     runID,
		StepKey:   "spawn:plan:0",
		EventType: WorkflowEventSpawnRequested,
		CallHash:  "hashA",
		Status:    WorkflowStepPending,
		StartedAt: &started,
	}
	second, err := st.Workflow.AppendStep(ctx, again)
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-append created a new row: %d vs %d", second.ID, first.ID)
	}
	if n := countSteps(t, st, runID); n != 1 {
		t.Fatalf("expected 1 row after re-append, got %d", n)
	}

	// Pending -> success transition (record-before-result completion).
	ended := started.Add(30 * time.Second)
	done := &WorkflowStep{
		RunID:       runID,
		StepKey:     "spawn:plan:0",
		EventType:   WorkflowEventSpawnResult,
		CallHash:    "hashA",
		Status:      WorkflowStepSuccess,
		StartedAt:   &started,
		EndedAt:     &ended,
		ResultBlob:  `{"ok":true}`,
		SpawnID:     "spawn-123",
		CostUSD:     0.42,
		CostSource:  WorkflowCostReal,
		EffectCount: 1,
	}
	completed, err := st.Workflow.AppendStep(ctx, done)
	if err != nil {
		t.Fatalf("complete append: %v", err)
	}
	if completed.ID != first.ID {
		t.Fatalf("completion created a new row: %d vs %d", completed.ID, first.ID)
	}
	if completed.Status != WorkflowStepSuccess {
		t.Fatalf("status not advanced to success: %+v", completed)
	}
	if completed.ResultBlob != `{"ok":true}` || completed.SpawnID != "spawn-123" {
		t.Fatalf("result/spawn not persisted: %+v", completed)
	}
	if completed.EventType != WorkflowEventSpawnResult || completed.CostUSD != 0.42 {
		t.Fatalf("event/cost not updated: %+v", completed)
	}
	if completed.CostSource != WorkflowCostReal || completed.EffectCount != 1 {
		t.Fatalf("cost_source/effect_count not updated: %+v", completed)
	}
	if n := countSteps(t, st, runID); n != 1 {
		t.Fatalf("expected still 1 row after completion, got %d", n)
	}

	// call_hash MISMATCH on the same step_key: must NOT overwrite, must
	// return the existing record plus ErrStepCallHashMismatch.
	bad := &WorkflowStep{
		RunID:      runID,
		StepKey:    "spawn:plan:0",
		EventType:  WorkflowEventSpawnResult,
		CallHash:   "hashB-DIFFERENT",
		Status:     WorkflowStepError,
		ResultBlob: `{"tampered":true}`,
	}
	existing, err := st.Workflow.AppendStep(ctx, bad)
	if !errors.Is(err, ErrStepCallHashMismatch) {
		t.Fatalf("expected ErrStepCallHashMismatch, got err=%v", err)
	}
	if existing == nil {
		t.Fatalf("mismatch must return the existing record")
	}
	if existing.CallHash != "hashA" {
		t.Fatalf("returned record should be the original (hashA), got %q", existing.CallHash)
	}
	// Verify the stored row was NOT overwritten.
	reread, err := st.Workflow.GetStep(ctx, runID, "spawn:plan:0")
	if err != nil {
		t.Fatalf("re-read after mismatch: %v", err)
	}
	if reread.CallHash != "hashA" || reread.Status != WorkflowStepSuccess {
		t.Fatalf("row was silently overwritten on mismatch: %+v", reread)
	}
	if reread.ResultBlob != `{"ok":true}` {
		t.Fatalf("result_blob clobbered on mismatch: %q", reread.ResultBlob)
	}
}

// TestWorkflowAppendStep_StructuredKeyDrift carries forward the S1 spike
// requirement: step_key is treated as an opaque string, and the store is
// insert/delete-tolerant when keys are stable. Recording extra unrelated keys
// and deleting one must not disturb the records of unchanged keys (no
// collision, no wrong-row consumption).
func TestWorkflowAppendStep_StructuredKeyDrift(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-KEYDRIFT")

	// Pass 1: record a stable set of keys K1..Kn with distinct call_hashes.
	keys := []string{
		"plan:0",
		"slice[0]:implement",
		"slice[0]:gate:tests",
		"slice[1]:implement",
		"loop[3]:tool_call:gitlab",
		"parallel:branch:audit",
	}
	want := make(map[string]string, len(keys)) // step_key -> call_hash
	for i, k := range keys {
		h := fmt.Sprintf("hash-%d", i)
		want[k] = h
		if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
			RunID:     runID,
			StepKey:   k,
			EventType: WorkflowEventToolCall,
			CallHash:  h,
			Status:    WorkflowStepSuccess,
		}); err != nil {
			t.Fatalf("pass1 append %q: %v", k, err)
		}
	}
	if n := countSteps(t, st, runID); n != len(keys) {
		t.Fatalf("pass1 expected %d rows, got %d", len(keys), n)
	}

	// Pass 2: INSERT an extra unrelated key AND DELETE one existing key.
	extra := "slice[2]:implement"
	if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID:     runID,
		StepKey:   extra,
		EventType: WorkflowEventToolCall,
		CallHash:  "hash-extra",
		Status:    WorkflowStepSuccess,
	}); err != nil {
		t.Fatalf("pass2 extra append: %v", err)
	}
	deleted := "slice[1]:implement"
	if _, err := st.DB().ExecContext(ctx,
		`DELETE FROM workflow_steps WHERE run_id = ? AND step_key = ?`, runID, deleted); err != nil {
		t.Fatalf("delete %q: %v", deleted, err)
	}

	// Assert every UNCHANGED key still returns its ORIGINAL record. The
	// deleted key must be gone; the rest must be intact and correctly keyed.
	for k, h := range want {
		got, err := st.Workflow.GetStep(ctx, runID, k)
		if k == deleted {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted key %q should be ErrNotFound, got %v / %+v", k, err, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unchanged key %q lookup failed: %v", k, err)
		}
		if got.StepKey != k {
			t.Fatalf("wrong-row consumption: asked %q got %q", k, got.StepKey)
		}
		if got.CallHash != h {
			t.Fatalf("key %q drifted: call_hash %q want %q", k, got.CallHash, h)
		}
	}

	// The extra key resolves to its own row, not colliding with any other.
	gotExtra, err := st.Workflow.GetStep(ctx, runID, extra)
	if err != nil || gotExtra.CallHash != "hash-extra" {
		t.Fatalf("extra key lookup wrong: %+v err=%v", gotExtra, err)
	}
}

// TestWorkflowListPending_AndCrashReconciliation verifies ListPending filters
// to pending-only and that a step interrupted between the pending write and
// the success update is recoverable as pending and completable on re-read.
func TestWorkflowListPending_AndCrashReconciliation(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-PENDING")

	// One completed step + one pending step.
	if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "done:0", EventType: WorkflowEventSpawnResult,
		CallHash: "h0", Status: WorkflowStepSuccess,
	}); err != nil {
		t.Fatalf("append done: %v", err)
	}
	started := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "spawn:1", EventType: WorkflowEventSpawnRequested,
		CallHash: "h1", Status: WorkflowStepPending, StartedAt: &started,
	}); err != nil {
		t.Fatalf("append pending: %v", err)
	}

	// Simulate a crash: NO success update for spawn:1. On re-read it must be
	// recoverable as pending.
	pending, err := st.Workflow.ListPending(ctx, runID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected exactly 1 pending step, got %d: %+v", len(pending), pending)
	}
	if pending[0].StepKey != "spawn:1" || pending[0].Status != WorkflowStepPending {
		t.Fatalf("wrong pending step: %+v", pending[0])
	}

	// ListByRun returns the full replay log (both steps) in append order.
	all, err := st.Workflow.ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("list by run: %v", err)
	}
	if len(all) != 2 || all[0].StepKey != "done:0" || all[1].StepKey != "spawn:1" {
		t.Fatalf("replay log wrong: %+v", all)
	}

	// Reconcile: complete the recovered pending step.
	ended := started.Add(10 * time.Second)
	completed, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "spawn:1", EventType: WorkflowEventSpawnResult,
		CallHash: "h1", Status: WorkflowStepSuccess, StartedAt: &started,
		EndedAt: &ended, ResultBlob: `{"recovered":true}`,
	})
	if err != nil {
		t.Fatalf("reconcile complete: %v", err)
	}
	if completed.Status != WorkflowStepSuccess || completed.ResultBlob != `{"recovered":true}` {
		t.Fatalf("reconciliation did not complete the step: %+v", completed)
	}

	// No pending steps remain.
	pending, err = st.Workflow.ListPending(ctx, runID)
	if err != nil {
		t.Fatalf("list pending after reconcile: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after reconcile, got %d", len(pending))
	}
}

// TestWorkflowMigration004_TablesExist confirms migration 004 is auto-applied
// by Open and that the dual source-of-truth invariant holds at the schema
// level (the tables exist alongside the advisory events table).
func TestWorkflowMigration004_TablesExist(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, table := range []string{"workflow_runs", "workflow_steps"} {
		var name string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name); err != nil {
			t.Fatalf("migration 004 table %s missing: %v", table, err)
		}
	}
	// Partial + replay indexes present.
	for _, idx := range []string{"idx_workflow_pending", "idx_workflow_replay"} {
		var name string
		if err := st.DB().QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&name); err != nil {
			t.Fatalf("migration 004 index %s missing: %v", idx, err)
		}
	}
}

func countSteps(t *testing.T, st *Store, runID string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workflow_steps WHERE run_id = ?`, runID).Scan(&n); err != nil {
		t.Fatalf("count steps: %v", err)
	}
	return n
}
