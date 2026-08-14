package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

func TestWorkflowRun_ImmutableMetadataMismatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"BACK-A", "BACK-B"} {
		if err := st.Backlog.Put(ctx, &BacklogItem{
			ID: id, Title: id, State: BacklogQueued, Priority: P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog %s: %v", id, err)
		}
	}

	tests := []struct {
		field  string
		mutate func(*WorkflowRun)
	}{
		{field: "backlog_id", mutate: func(r *WorkflowRun) { r.BacklogID = "BACK-B" }},
		{field: "engine", mutate: func(r *WorkflowRun) { r.Engine = WorkflowEngineDAG }},
		{field: "template", mutate: func(r *WorkflowRun) { r.Template = "different-template" }},
		{field: "template_version", mutate: func(r *WorkflowRun) { r.TemplateVersion = "v2" }},
		{field: "interpreter_version", mutate: func(r *WorkflowRun) { r.InterpreterVersion = "starlark-0.2" }},
		{field: "workflow_params", mutate: func(r *WorkflowRun) { r.WorkflowParams = `{"scope":"other"}` }},
		{field: "parent_session_id", mutate: func(r *WorkflowRun) { r.ParentSessionID = "session-b" }},
	}

	for i, tc := range tests {
		t.Run(tc.field, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Millisecond)
			original := &WorkflowRun{
				ID:                 fmt.Sprintf("WF-METADATA-%d", i),
				BacklogID:          "BACK-A",
				Engine:             WorkflowEngineImperative,
				Template:           "workflow-canary",
				TemplateVersion:    "v1",
				InterpreterVersion: "starlark-0.1",
				WorkflowParams:     `{"scope":"original"}`,
				State:              WorkflowRunRunning,
				StartedAt:          &now,
				CostUSD:            1.25,
				ParentSessionID:    "session-a",
			}
			if err := st.Workflow.PutWorkflowRun(ctx, original); err != nil {
				t.Fatalf("seed run: %v", err)
			}

			incoming := *original
			incoming.State = WorkflowRunDone
			incoming.CostUSD = 9.99
			tc.mutate(&incoming)
			err := st.Workflow.PutWorkflowRun(ctx, &incoming)
			if !errors.Is(err, ErrWorkflowRunMetadataMismatch) {
				t.Fatalf("error = %v, want ErrWorkflowRunMetadataMismatch", err)
			}
			var mismatch *WorkflowRunMetadataMismatchError
			if !errors.As(err, &mismatch) {
				t.Fatalf("error type = %T, want *WorkflowRunMetadataMismatchError", err)
			}
			if len(mismatch.Fields) != 1 || mismatch.Fields[0] != tc.field {
				t.Fatalf("mismatch fields = %v, want [%s]", mismatch.Fields, tc.field)
			}

			stored, getErr := st.Workflow.GetWorkflowRun(ctx, original.ID)
			if getErr != nil {
				t.Fatalf("get stored run: %v", getErr)
			}
			if stored.State != WorkflowRunRunning || stored.CostUSD != 1.25 {
				t.Fatalf("metadata mismatch partially updated mutable fields: %+v", stored)
			}
		})
	}
}

func TestEnsureWorkflowRun_InsertOnlyPreservesExistingRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	original := &WorkflowRun{
		ID:                 "WF-ENSURE-EXISTING",
		Engine:             WorkflowEngineImperative,
		Template:           "customer-workflow",
		TemplateVersion:    "v7",
		InterpreterVersion: "starlark-pinned",
		WorkflowParams:     `{"customer":true}`,
		State:              WorkflowRunPaused,
		StartedAt:          &now,
		CostUSD:            2.5,
		ParentSessionID:    "session-original",
	}
	if err := st.Workflow.PutWorkflowRun(ctx, original); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	seed := &WorkflowRun{
		ID:                 original.ID,
		Engine:             WorkflowEngineImperative,
		Template:           "workflow-seed",
		TemplateVersion:    "v0",
		InterpreterVersion: "host-current",
		State:              WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := st.Workflow.EnsureWorkflowRun(ctx, seed); err != nil {
		t.Fatalf("ensure existing run: %v", err)
	}

	stored, err := st.Workflow.GetWorkflowRun(ctx, original.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if stored.Template != original.Template ||
		stored.TemplateVersion != original.TemplateVersion ||
		stored.InterpreterVersion != original.InterpreterVersion ||
		stored.WorkflowParams != original.WorkflowParams ||
		stored.ParentSessionID != original.ParentSessionID {
		t.Fatalf("ensure overwrote immutable metadata: %+v", stored)
	}
	if stored.State != WorkflowRunPaused || stored.CostUSD != original.CostUSD {
		t.Fatalf("ensure overwrote existing mutable state: %+v", stored)
	}
}

func TestEnsureWorkflowRun_RejectsExistingEngineMismatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	existing := &WorkflowRun{
		ID:                 "WF-ENSURE-DAG",
		Engine:             WorkflowEngineDAG,
		Template:           "legacy-dag",
		TemplateVersion:    "v1",
		InterpreterVersion: "dag-runner-v1",
		State:              WorkflowRunRunning,
		StartedAt:          &now,
	}
	if err := st.Workflow.PutWorkflowRun(ctx, existing); err != nil {
		t.Fatalf("seed DAG run: %v", err)
	}

	err := st.Workflow.EnsureWorkflowRun(ctx, &WorkflowRun{
		ID:                 existing.ID,
		Engine:             WorkflowEngineImperative,
		Template:           "workflow-seed",
		TemplateVersion:    "v0",
		InterpreterVersion: "starlark-current",
		State:              WorkflowRunRunning,
		StartedAt:          &now,
	})
	if !errors.Is(err, ErrWorkflowRunMetadataMismatch) {
		t.Fatalf("ensure engine mismatch error = %v, want ErrWorkflowRunMetadataMismatch", err)
	}
	var mismatch *WorkflowRunMetadataMismatchError
	if !errors.As(err, &mismatch) || len(mismatch.Fields) != 1 || mismatch.Fields[0] != "engine" {
		t.Fatalf("ensure engine mismatch details = %#v, want [engine]", mismatch)
	}
}

func TestWorkflowRun_ProvisionalSeedPromotion(t *testing.T) {
	t.Run("promotes before the journal starts", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		if err := st.Backlog.Put(ctx, &BacklogItem{
			ID: "BACK-PROMOTE", Title: "promote", State: BacklogQueued, Priority: P2, CreatedBy: "test",
		}); err != nil {
			t.Fatalf("seed backlog: %v", err)
		}
		seedStarted := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
		if err := st.Workflow.EnsureWorkflowRun(ctx, &WorkflowRun{
			ID:                 "WF-SEED-PROMOTE",
			Engine:             WorkflowEngineImperative,
			Template:           "workflow-seed",
			TemplateVersion:    "v0",
			InterpreterVersion: "host-current",
			State:              WorkflowRunRunning,
			StartedAt:          &seedStarted,
		}); err != nil {
			t.Fatalf("ensure provisional seed: %v", err)
		}

		realStarted := seedStarted.Add(30 * time.Second)
		paused := realStarted.Add(time.Second)
		real := &WorkflowRun{
			ID:                 "WF-SEED-PROMOTE",
			BacklogID:          "BACK-PROMOTE",
			Engine:             WorkflowEngineImperative,
			Template:           "customer-workflow",
			TemplateVersion:    "v8",
			InterpreterVersion: "starlark-pinned",
			WorkflowParams:     `{"customer":"acme"}`,
			State:              WorkflowRunPaused,
			StartedAt:          &realStarted,
			PausedAt:           &paused,
			CostUSD:            1.75,
			ParentSessionID:    "session-real",
		}
		if err := st.Workflow.PutWorkflowRun(ctx, real); err != nil {
			t.Fatalf("promote provisional seed: %v", err)
		}

		stored, err := st.Workflow.GetWorkflowRun(ctx, real.ID)
		if err != nil {
			t.Fatalf("get promoted run: %v", err)
		}
		if stored.BacklogID != real.BacklogID || stored.Template != real.Template ||
			stored.TemplateVersion != real.TemplateVersion ||
			stored.InterpreterVersion != real.InterpreterVersion ||
			stored.WorkflowParams != real.WorkflowParams ||
			stored.ParentSessionID != real.ParentSessionID {
			t.Fatalf("provisional metadata not promoted: %+v", stored)
		}
		if stored.State != WorkflowRunPaused || stored.CostUSD != real.CostUSD ||
			stored.StartedAt == nil || !stored.StartedAt.Equal(realStarted) {
			t.Fatalf("provisional lifecycle not promoted: %+v", stored)
		}

		changed := *real
		changed.TemplateVersion = "v9"
		changed.State = WorkflowRunDone
		changed.CostUSD = 99
		if err := st.Workflow.PutWorkflowRun(ctx, &changed); !errors.Is(err, ErrWorkflowRunMetadataMismatch) {
			t.Fatalf("second identity promotion error = %v, want immutable metadata mismatch", err)
		}
		stored, err = st.Workflow.GetWorkflowRun(ctx, real.ID)
		if err != nil {
			t.Fatalf("get locked run: %v", err)
		}
		if stored.TemplateVersion != real.TemplateVersion || stored.State != real.State || stored.CostUSD != real.CostUSD {
			t.Fatalf("failed promotion partially changed locked run: %+v", stored)
		}
	})

	t.Run("step creation locks the provisional identity", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Millisecond)
		seed := &WorkflowRun{
			ID:                 "WF-SEED-LOCKED",
			Engine:             WorkflowEngineImperative,
			Template:           "workflow-seed",
			TemplateVersion:    "v0",
			InterpreterVersion: "host-current",
			State:              WorkflowRunRunning,
			StartedAt:          &now,
		}
		if err := st.Workflow.EnsureWorkflowRun(ctx, seed); err != nil {
			t.Fatalf("ensure seed: %v", err)
		}
		if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
			RunID: seed.ID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
			CallHash: "seed-step", Status: WorkflowStepPending,
		}); err != nil {
			t.Fatalf("append seed step: %v", err)
		}

		real := *seed
		real.Template = "too-late"
		real.TemplateVersion = "v1"
		if err := st.Workflow.PutWorkflowRun(ctx, &real); !errors.Is(err, ErrWorkflowRunMetadataMismatch) {
			t.Fatalf("late promotion error = %v, want immutable metadata mismatch", err)
		}
	})
}

func TestWorkflowRun_ConcurrentEnsureAndCreationPromotesSeed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const rounds = 32
	for i := 0; i < rounds; i++ {
		runID := fmt.Sprintf("WF-CREATE-ENSURE-%02d", i)
		now := time.Now().UTC().Truncate(time.Millisecond)
		seed := &WorkflowRun{
			ID: runID, Engine: WorkflowEngineImperative, Template: "workflow-seed",
			TemplateVersion: "v0", InterpreterVersion: "host-current",
			State: WorkflowRunRunning, StartedAt: &now,
		}
		real := &WorkflowRun{
			ID: runID, Engine: WorkflowEngineImperative, Template: "real-workflow",
			TemplateVersion: "v4", InterpreterVersion: "starlark-pinned",
			WorkflowParams: `{"round":true}`, State: WorkflowRunRunning,
			StartedAt: &now, ParentSessionID: "session-real",
		}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs <- st.Workflow.EnsureWorkflowRun(ctx, seed)
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- st.Workflow.PutWorkflowRun(ctx, real)
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("round %d concurrent create/ensure: %v", i, err)
			}
		}
		stored, err := st.Workflow.GetWorkflowRun(ctx, runID)
		if err != nil {
			t.Fatalf("round %d get run: %v", i, err)
		}
		if stored.Template != real.Template || stored.TemplateVersion != real.TemplateVersion ||
			stored.InterpreterVersion != real.InterpreterVersion ||
			stored.WorkflowParams != real.WorkflowParams || stored.ParentSessionID != real.ParentSessionID {
			t.Fatalf("round %d retained provisional identity: %+v", i, stored)
		}
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
		RunID:     runID,
		StepKey:   "spawn:plan:0",
		EventType: WorkflowEventSpawnRequested,
		CallHash:  "hashA",
		Status:    WorkflowStepPending,
		StartedAt: &started,
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

func TestWorkflowAppendStep_PendingEnrichmentPreservesFirstWriterHandles(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-PENDING-FIRST-WRITER")
	started := time.Now().UTC().Add(-time.Second).Truncate(time.Millisecond)
	first, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
		CallHash: "same-hash", IdempotencyKey: "idem-first", Status: WorkflowStepPending,
		SpawnID: "spawn-first", StartedAt: &started, CostUSD: 1.25, CostSource: WorkflowCostReal,
	})
	if err != nil {
		t.Fatalf("append first pending step: %v", err)
	}

	later := started.Add(time.Second)
	second, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
		CallHash: "same-hash", IdempotencyKey: "idem-second", Status: WorkflowStepPending,
		SpawnID: "spawn-second", StartedAt: &later, CostUSD: 9.99, CostSource: WorkflowCostEstimated,
	})
	if err != nil {
		t.Fatalf("enrich pending step: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("pending enrichment created another row: first=%d second=%d", first.ID, second.ID)
	}
	if second.IdempotencyKey != "idem-first" || second.SpawnID != "spawn-first" {
		t.Fatalf("pending enrichment replaced first-writer handles: %+v", second)
	}
	if second.CostUSD != 1.25 || second.CostSource != WorkflowCostReal {
		t.Fatalf("pending enrichment replaced first-writer cost provenance: %+v", second)
	}
	if second.StartedAt == nil || !second.StartedAt.Equal(started) {
		t.Fatalf("pending enrichment replaced first start time: %+v", second)
	}
}

func TestWorkflowAppendStep_TerminalProvenanceConflictPreservesPendingRecord(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*WorkflowStep)
	}{
		{
			name:  "idempotency key",
			field: "idempotency_key",
			mutate: func(step *WorkflowStep) {
				step.IdempotencyKey = "idem-terminal"
			},
		},
		{
			name:  "spawn id",
			field: "spawn_id",
			mutate: func(step *WorkflowStep) {
				step.SpawnID = "spawn-terminal"
			},
		},
		{
			name:  "cost source",
			field: "cost_source",
			mutate: func(step *WorkflowStep) {
				step.CostSource = WorkflowCostEstimated
			},
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			runID := seedWorkflowRun(t, st, fmt.Sprintf("WF-TERMINAL-PROVENANCE-%d", i))
			if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
				RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
				CallHash: "same-hash", IdempotencyKey: "idem-pending", Status: WorkflowStepPending,
				SpawnID: "spawn-pending", CostSource: WorkflowCostReal,
			}); err != nil {
				t.Fatalf("append pending step: %v", err)
			}

			terminal := &WorkflowStep{
				RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnResult,
				CallHash: "same-hash", IdempotencyKey: "idem-pending", Status: WorkflowStepSuccess,
				SpawnID: "spawn-pending", ResultBlob: `{"ok":true}`, CostUSD: 1.25,
				CostSource: WorkflowCostReal, EffectCount: 1,
			}
			tc.mutate(terminal)
			stored, err := st.Workflow.AppendStep(ctx, terminal)
			if !errors.Is(err, ErrStepTerminalConflict) {
				t.Fatalf("terminal provenance error = %v, want ErrStepTerminalConflict", err)
			}
			var conflict *WorkflowStepTerminalConflictError
			if !errors.As(err, &conflict) || len(conflict.Fields) != 1 || conflict.Fields[0] != tc.field {
				t.Fatalf("terminal conflict = %#v, want field %q", conflict, tc.field)
			}
			if stored == nil || stored.Status != WorkflowStepSuccess || stored.ResultBlob != terminal.ResultBlob {
				t.Fatalf("terminal result was not durably committed: %+v", stored)
			}
			if stored.EventType != WorkflowEventSpawnResult ||
				stored.IdempotencyKey != "idem-pending" ||
				stored.SpawnID != "spawn-pending" ||
				stored.CostSource != WorkflowCostReal {
				t.Fatalf("terminal writer replaced pending provenance: %+v", stored)
			}
		})
	}
}

func TestWorkflowAppendStep_ConcurrentTerminalConflict(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-CONCURRENT-TERMINAL-CONFLICT")
	if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
		CallHash: "same-hash", Status: WorkflowStepPending,
	}); err != nil {
		t.Fatalf("seed pending step: %v", err)
	}

	steps := []*WorkflowStep{
		{
			RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnResult,
			CallHash: "same-hash", Status: WorkflowStepSuccess, ResultBlob: `{"winner":"alpha"}`,
			SpawnID: "spawn-alpha", CostUSD: 1.25, CostSource: WorkflowCostReal, EffectCount: 1,
		},
		{
			RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnResult,
			CallHash: "same-hash", Status: WorkflowStepSuccess, ResultBlob: `{"winner":"beta"}`,
			SpawnID: "spawn-beta", CostUSD: 2.5, CostSource: WorkflowCostEstimated, EffectCount: 1,
		},
	}
	type appendResult struct {
		incoming *WorkflowStep
		stored   *WorkflowStep
		err      error
	}
	start := make(chan struct{})
	results := make(chan appendResult, len(steps))
	var wg sync.WaitGroup
	for _, step := range steps {
		step := step
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			stored, err := st.Workflow.AppendStep(ctx, step)
			results <- appendResult{incoming: step, stored: stored, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner *WorkflowStep
	conflicts := 0
	for result := range results {
		switch {
		case result.err == nil:
			winner = result.incoming
		case errors.Is(result.err, ErrStepTerminalConflict):
			conflicts++
			if result.stored == nil {
				t.Fatal("terminal conflict did not return the durable result")
			}
			var conflict *WorkflowStepTerminalConflictError
			if !errors.As(result.err, &conflict) || len(conflict.Fields) == 0 {
				t.Fatalf("terminal conflict details = %#v, err=%v", conflict, result.err)
			}
		default:
			t.Fatalf("terminal append returned unexpected error: %v", result.err)
		}
	}
	if winner == nil || conflicts != 1 {
		t.Fatalf("terminal race winner=%+v conflicts=%d, want one of each", winner, conflicts)
	}
	stored, err := st.Workflow.GetStep(ctx, runID, "agent:0")
	if err != nil {
		t.Fatalf("get terminal winner: %v", err)
	}
	if stored.ResultBlob != winner.ResultBlob || stored.SpawnID != winner.SpawnID ||
		stored.CostUSD != winner.CostUSD || stored.CostSource != winner.CostSource {
		t.Fatalf("durable terminal result = %+v, want first committed %+v", stored, winner)
	}
	if n := countSteps(t, st, runID); n != 1 {
		t.Fatalf("terminal race created %d rows, want 1", n)
	}
}

func TestWorkflowAppendStep_ConcurrentIdempotency(t *testing.T) {
	t.Run("identical pending appends create one row", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		runID := seedWorkflowRun(t, st, "WF-CONCURRENT-PENDING")
		const workers = 64
		start := make(chan struct{})
		errs := make(chan error, workers)
		ids := make(chan int64, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				got, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
					RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
					CallHash: "same-hash", Status: WorkflowStepPending,
				})
				errs <- err
				if got != nil {
					ids <- got.ID
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		close(ids)

		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent append returned error: %v", err)
			}
		}
		var firstID int64
		for id := range ids {
			if id == 0 {
				t.Fatal("concurrent append returned zero row id")
			}
			if firstID == 0 {
				firstID = id
			} else if id != firstID {
				t.Fatalf("concurrent append returned different row ids: %d and %d", firstID, id)
			}
		}
		if n := countSteps(t, st, runID); n != 1 {
			t.Fatalf("concurrent first append created %d rows, want 1", n)
		}
	})

	t.Run("pending and terminal race leaves one terminal row", func(t *testing.T) {
		st := newTestStore(t)
		ctx := context.Background()
		runID := seedWorkflowRun(t, st, "WF-CONCURRENT-COMPLETE")
		const workers = 64
		start := make(chan struct{})
		errs := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			terminal := i%2 == 1
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				step := &WorkflowStep{
					RunID: runID, StepKey: "agent:0", EventType: WorkflowEventSpawnRequested,
					CallHash: "same-hash", Status: WorkflowStepPending,
				}
				if terminal {
					step.EventType = WorkflowEventSpawnResult
					step.Status = WorkflowStepSuccess
					step.ResultBlob = `{"ok":true}`
					step.SpawnID = "spawn-once"
					step.EffectCount = 1
				}
				_, err := st.Workflow.AppendStep(ctx, step)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent pending/completion returned error: %v", err)
			}
		}
		stored, err := st.Workflow.GetStep(ctx, runID, "agent:0")
		if err != nil {
			t.Fatalf("get stored step: %v", err)
		}
		if stored.Status != WorkflowStepSuccess || stored.ResultBlob != `{"ok":true}` {
			t.Fatalf("race did not converge on terminal result: %+v", stored)
		}
		if n := countSteps(t, st, runID); n != 1 {
			t.Fatalf("pending/completion race created %d rows, want 1", n)
		}
	})
}

func TestWorkflowAppendStep_ConcurrentSuccessAndQuarantine(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	const rounds = 32
	for i := 0; i < rounds; i++ {
		runID := seedWorkflowRun(t, st, fmt.Sprintf("WF-SUCCESS-QUARANTINE-%02d", i))
		stepKey := "agent:0"
		callHash := "same-hash"
		if _, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
			RunID: runID, StepKey: stepKey, EventType: WorkflowEventSpawnRequested,
			CallHash: callHash, Status: WorkflowStepPending,
		}); err != nil {
			t.Fatalf("round %d seed pending step: %v", i, err)
		}

		start := make(chan struct{})
		successErr := make(chan error, 1)
		quarantineErr := make(chan error, 1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
				RunID: runID, StepKey: stepKey, EventType: WorkflowEventSpawnResult,
				CallHash: callHash, Status: WorkflowStepSuccess,
				ResultBlob: `{"ok":true}`, SpawnID: "spawn-once", EffectCount: 1,
			})
			successErr <- err
		}()
		go func() {
			defer wg.Done()
			<-start
			_, err := st.Workflow.QuarantineStep(ctx, runID, stepKey, callHash)
			quarantineErr <- err
		}()
		close(start)
		wg.Wait()
		if err := <-quarantineErr; err != nil {
			t.Fatalf("round %d quarantine: %v", i, err)
		}
		if err := <-successErr; err != nil && !errors.Is(err, ErrStepTerminalConflict) {
			t.Fatalf("round %d success error = %v, want nil or terminal conflict", i, err)
		}

		stored, err := st.Workflow.GetStep(ctx, runID, stepKey)
		if err != nil {
			t.Fatalf("round %d get frozen step: %v", i, err)
		}
		if stored.Status != WorkflowStepError {
			t.Fatalf("round %d final status = %s, want error quarantine", i, stored.Status)
		}
		if n := countSteps(t, st, runID); n != 1 {
			t.Fatalf("round %d success/quarantine race created %d rows, want 1", i, n)
		}
	}
}

func TestWorkflowQuarantineStep_ExplicitTerminalFreeze(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	runID := seedWorkflowRun(t, st, "WF-QUARANTINE")
	terminal, err := st.Workflow.AppendStep(ctx, &WorkflowStep{
		RunID: runID, StepKey: "gate:0", EventType: WorkflowEventGateEval,
		CallHash: "gate-hash", Status: WorkflowStepSuccess,
		ResultBlob: `{"pass":true}`, EffectCount: 1,
	})
	if err != nil {
		t.Fatalf("append terminal step: %v", err)
	}

	frozen, err := st.Workflow.QuarantineStep(ctx, runID, terminal.StepKey, terminal.CallHash)
	if err != nil {
		t.Fatalf("quarantine terminal step: %v", err)
	}
	if frozen.ID != terminal.ID || frozen.Status != WorkflowStepError {
		t.Fatalf("quarantine result = %+v, want same row in error", frozen)
	}
	if frozen.ResultBlob != terminal.ResultBlob || frozen.EffectCount != terminal.EffectCount {
		t.Fatalf("quarantine clobbered terminal provenance: %+v", frozen)
	}
	if n := countSteps(t, st, runID); n != 1 {
		t.Fatalf("quarantine created %d rows, want 1", n)
	}

	again, err := st.Workflow.QuarantineStep(ctx, runID, terminal.StepKey, terminal.CallHash)
	if err != nil || again.Status != WorkflowStepError {
		t.Fatalf("idempotent quarantine = %+v, err=%v", again, err)
	}
	stored, err := st.Workflow.QuarantineStep(ctx, runID, terminal.StepKey, "different-hash")
	if !errors.Is(err, ErrStepCallHashMismatch) || stored == nil {
		t.Fatalf("mismatched quarantine = %+v, err=%v; want existing + hash mismatch", stored, err)
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
